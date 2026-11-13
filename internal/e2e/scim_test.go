package e2e

// SCIM provisioning end to end: the administrator generates a token in the
// settings, Microsoft Entra ID (simulated with its request shapes) provisions
// users and a group, the group is assigned to a service and the provisioned
// users, who never signed in, get the incident e-mails. A provisioned user
// then signs in with single sign-on (matched by the user name) and keeps the
// provisioned group; deactivating the user ends the session, the e-mails and
// the sign-in.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakesmtp"
	"github.com/szilvasidave/status-page-tool/internal/testutil/mockidp"
)

const (
	scimSite     = "SCIM E2E Status"
	scimAdminPW  = "provisioning-admin-pw"
	scimMailFrom = "status@scim.example"
	scimMailWait = 15 * time.Second
)

func TestSCIMProvisioningSQLite(t *testing.T) {
	runSCIMProvisioning(t, &config.Config{DBDriver: config.DriverSQLite, DBDSN: "statuspage.db"})
}

// TestSCIMProvisioningPostgres runs against STATUSPAGE_TEST_PG_DSN; the
// public schema of that database is dropped and recreated first.
func TestSCIMProvisioningPostgres(t *testing.T) {
	dsn := os.Getenv("STATUSPAGE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("STATUSPAGE_TEST_PG_DSN not set")
	}
	d, err := db.Open(context.Background(), config.DriverPostgres, dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.ExecContext(context.Background(), "DROP SCHEMA public CASCADE; CREATE SCHEMA public")
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	runSCIMProvisioning(t, &config.Config{DBDriver: config.DriverPostgres, DBDSN: dsn})
}

// scimEntra is a SCIM client that sends requests like the Microsoft Entra
// provisioning service.
type scimEntra struct {
	t      *testing.T
	tenant string // Tenant URL
	token  string
}

// do sends a request and decodes the JSON response (nil for 204).
func (c *scimEntra) do(method, path string, body string, wantStatus int) map[string]any {
	c.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.tenant+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/scim+json; charset=utf-8")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		c.t.Fatalf("SCIM %s %s: status %d, want %d: %s", method, path, resp.StatusCode, wantStatus, raw)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("SCIM %s %s: %v: %s", method, path, err, raw)
		}
	}
	return out
}

// query runs GET path?filter=… and returns the resources.
func (c *scimEntra) query(path, filter string, extra ...string) []map[string]any {
	c.t.Helper()
	q := url.Values{"filter": {filter}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	res := c.do(http.MethodGet, path+"?"+q.Encode(), "", http.StatusOK)
	list, _ := res["Resources"].([]any)
	out := make([]map[string]any, len(list))
	for i, r := range list {
		out[i] = r.(map[string]any)
	}
	return out
}

// provisionUser creates a user like Entra ID: query by userName, then POST.
func (c *scimEntra) provisionUser(upn, mailNickname, given, family string) string {
	c.t.Helper()
	if found := c.query("/Users", `userName eq "`+upn+`"`); len(found) != 0 {
		c.t.Fatalf("user %s exists already", upn)
	}
	body, _ := json.Marshal(map[string]any{
		"schemas":     []string{"urn:ietf:params:scim:schemas:core:2.0:User", "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"},
		"externalId":  mailNickname,
		"userName":    upn,
		"active":      true,
		"displayName": given + " " + family,
		"emails":      []map[string]any{{"primary": true, "type": "work", "value": upn}},
		"meta":        map[string]any{"resourceType": "User"},
		"name":        map[string]any{"formatted": given + " " + family, "familyName": family, "givenName": given},
		"title":       nil, "preferredLanguage": nil, "addresses": nil, "phoneNumbers": nil,
		"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User": map[string]any{"department": "IT", "employeeNumber": "4711"},
	})
	res := c.do(http.MethodPost, "/Users", string(body), http.StatusCreated)
	id, _ := res["id"].(string)
	if id == "" {
		c.t.Fatalf("created user without id: %v", res)
	}
	return id
}

var scimTokenRe = regexp.MustCompile(`id="scim-token">([^<]+)<`)

func runSCIMProvisioning(t *testing.T, cfg *config.Config) {
	logs := &logBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("application log:\n%s", logs)
		}
	})
	idp, smtp := mockidp.New(t), fakesmtp.Start(t)
	dataDir, addr := t.TempDir(), freeAddr(t)
	base := "http://" + addr

	// A configured installation: local administrator, single sign-on with
	// the mock provider, e-mail with the fake SMTP server.
	cfg.Listen, cfg.SecretKey = addr, config.NewSecretKey()
	if err := cfg.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d, err := db.OpenConfig(ctx, cfg, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := cfg.Key()
	st := store.New(d, key)
	hash, err := auth.HashPassword(scimAdminPW)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateLocalAdmin(ctx, "admin", "Ada Admin", hash); err != nil {
		t.Fatal(err)
	}
	settings, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.SiteName, settings.BaseURL, settings.PublicAccess = scimSite, base, true
	settings.OIDCEnabled, settings.OIDCIssuer, settings.OIDCClientID, settings.OIDCClientSecret = true, idp.Issuer(), idp.ClientID, idp.ClientSecret
	settings.MailMethod, settings.SMTPHost, settings.SMTPPort, settings.SMTPSecurity, settings.SMTPFrom =
		store.MailSMTP, smtp.Host(), smtp.Port(), "none", "IT Status <"+scimMailFrom+">"
	if err := st.SaveSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	d.Close()
	startApp(t, dataDir, addr, logs)
	idp.RequireRedirectURI(base + "/auth/oidc/callback")

	// The administrator generates the token: it is shown once (on the page
	// the POST redirects to), with the Tenant URL.
	admin := newBrowser(t, "admin")
	p := admin.get(base+"/login?local=1&next=%2Fadmin%2Fsettings").expectStatus(http.StatusOK).
		form("/login").set("username", "admin").set("password", scimAdminPW).post()
	p.expectRedirect("/admin/settings")
	p = p.follow().expectStatus(http.StatusOK).expectText(base+"/scim/v2", "No token – provisioning is off.")
	p = p.form("/admin/settings/scim-token#provisioning").post().follow().expectStatus(http.StatusOK).expectText("New token generated.")
	m := scimTokenRe.FindStringSubmatch(p.Body)
	if m == nil {
		t.Fatalf("no token on the page:\n%s", truncate(p.text(), 3000))
	}
	entra := &scimEntra{t: t, tenant: base + "/scim/v2", token: m[1]}
	// Reloading that page shows the token no more and does not replace it
	// (the requests below use it).
	admin.get(p.URL.String()).expectStatus(http.StatusOK).expectText("Provisioning is on.").expectNoText(m[1])

	// "Test connection", then the provisioning cycle.
	if found := entra.query("/Users", `userName eq "1d4e3f9a-0000-4000-8000-0123456789ab"`); len(found) != 0 {
		t.Fatalf("test connection found users: %v", found)
	}
	aliceID := entra.provisionUser("alice@contoso.example", "alice", "Alice", "Provisioned")
	bobID := entra.provisionUser("bob@contoso.example", "bob", "Bob", "Provisioned")
	carolID := entra.provisionUser("carol@contoso.example", "carol", "Carol", "Provisioned")
	const groupObjectID = "5f8c0d2e-7a41-4b9e-9c3d-2a1b0c9d8e7f"
	if found := entra.query("/Groups", `displayName eq "Status Subscribers"`, "excludedAttributes", "members"); len(found) != 0 {
		t.Fatalf("group exists already: %v", found)
	}
	group := entra.do(http.MethodPost, "/Groups", `{
    "schemas": ["urn:ietf:params:scim:schemas:core:2.0:Group", "http://schemas.microsoft.com/2006/11/ResourceManagement/ADSCIM/2.0/Group"],
    "externalId": "`+groupObjectID+`",
    "displayName": "Status Subscribers",
    "meta": {"resourceType": "Group"}
}`, http.StatusCreated)
	groupID, _ := group["id"].(string)
	entra.do(http.MethodPatch, "/Groups/"+groupID, `{
    "schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
    "Operations": [{"op": "Add", "path": "members", "value": [
        {"$ref": null, "value": "`+aliceID+`"}, {"$ref": null, "value": "`+bobID+`"}, {"$ref": null, "value": "`+carolID+`"}]}]
}`, http.StatusNoContent)

	// The administrator sees the provisioned group and users and assigns the
	// group to a service.
	admin.get(base+"/admin/users").expectStatus(http.StatusOK).
		expectText("Alice Provisioned", "Bob Provisioned", "Carol Provisioned", "Provisioned", "Never signed in")
	p = admin.get(base + "/admin/services/new").expectStatus(http.StatusOK)
	p = p.form("/admin/services").set("name", "ERP").post()
	p.expectRedirect("/admin/services")
	var api struct {
		Services []struct {
			ID int64 `json:"id"`
		} `json:"services"`
	}
	admin.get(base + "/api/status").expectStatus(http.StatusOK).json(&api)
	if len(api.Services) != 1 {
		t.Fatalf("services: %+v", api.Services)
	}
	erp := strconv.FormatInt(api.Services[0].ID, 10)
	admin.get(base+"/admin/groups").expectStatus(http.StatusOK).expectText("Status Subscribers Provisioned", groupObjectID)
	p = admin.get(base+"/admin/groups/"+groupID).expectStatus(http.StatusOK).
		expectText("The members of this group come from Microsoft Entra ID", "Alice Provisioned", "Carol Provisioned")
	p = p.form("/admin/groups/"+groupID).check("services", erp).post()
	p.expectRedirect("/admin/groups")

	// An incident e-mails the provisioned users, who never signed in.
	p = admin.get(base + "/manage/incidents/new").expectStatus(http.StatusOK)
	p = p.form("/manage/incidents").set("title", "ERP is slow").choose("impact_"+erp, "degraded").
		set("message", "We are looking into it.").post()
	incident := p.redirect()
	if !strings.Contains(incident, "/manage/incidents/") {
		t.Fatalf("reporting the incident: %d\n%s", p.Status, truncate(p.text(), 2000))
	}
	p.follow().expectFlash("success", "3 notification e-mails queued")
	scimExpectMails(t, smtp, "["+scimSite+"] Investigating: ERP is slow",
		"alice@contoso.example", "bob@contoso.example", "carol@contoso.example")

	// Alice signs in for the first time: the identity provider has another
	// subject and no e-mail, only her user name (preferred_username). She is
	// the provisioned user and keeps the provisioned group, which her groups
	// claim does not contain.
	alice := newBrowser(t, "alice")
	idp.SetUser(mockidp.User{Subject: "f0e1d2c3-pairwise-subject", OID: "oid-alice", Name: "A. P. from the token",
		PreferredUsername: "Alice@Contoso.example", Groups: []string{"some-security-group"}})
	p = alice.open(base + "/auth/oidc/login?next=%2Faccount")
	if p.URL.Path != "/account" {
		t.Fatalf("alice: sign-in ends at %s (%d):\n%s", p.URL, p.Status, truncate(p.text(), 2000))
	}
	p.expectStatus(http.StatusOK).expectText("Alice Provisioned", "alice@contoso.example", "Status Subscribers", "some-security-group", "ERP")
	users := entra.do(http.MethodGet, "/Users", "", http.StatusOK)
	if users["totalResults"] != float64(3) {
		t.Fatalf("users after the sign-in: %v", users)
	}
	found := entra.query("/Users", `userName eq "alice@contoso.example"`)
	if len(found) != 1 || found[0]["id"] != aliceID || found[0]["displayName"] != "Alice Provisioned" {
		t.Fatalf("alice after the sign-in: %v", found)
	}
	members, _ := entra.do(http.MethodGet, "/Groups/"+groupID, "", http.StatusOK)["members"].([]any)
	if len(members) != 3 {
		t.Fatalf("group members after the sign-in: %v", members)
	}
	admin.get(base + "/admin/users").expectStatus(http.StatusOK).expectText("SSO")

	// Entra ID deactivates Alice (string "False", as sent without the
	// aadOptscim062020 flag): her session ends, she gets no e-mails and
	// cannot sign in again.
	res := entra.do(http.MethodPatch, "/Users/"+aliceID, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
"Operations":[{"op":"Replace","path":"active","value":"False"}]}`, http.StatusOK)
	if res["active"] != false {
		t.Fatalf("deactivation: %v", res)
	}
	alice.get(base + "/account").expectRedirect("/login?next=%2Faccount")
	admin.get(base + "/admin/users").expectStatus(http.StatusOK).expectText("Deactivated")

	p = admin.get(base + strings.TrimPrefix(incident, base)).expectStatus(http.StatusOK)
	p = p.form(strings.TrimPrefix(incident, base)+"/updates").choose("status", "identified").
		set("message", "A database index is missing.").post()
	p.expectRedirect(strings.TrimPrefix(incident, base))
	p.follow().expectFlash("success", "2 notification e-mails queued")
	scimExpectMails(t, smtp, "["+scimSite+"] Update – Identified: ERP is slow", "bob@contoso.example", "carol@contoso.example")

	idp.SetUser(mockidp.User{Subject: "f0e1d2c3-pairwise-subject", OID: "oid-alice", PreferredUsername: "alice@contoso.example"})
	p = alice.open(base + "/auth/oidc/login?next=%2Faccount")
	p.expectStatus(http.StatusUnauthorized).expectText("deactivated by your organisation's user provisioning")
	alice.get(base + "/account").expectRedirect("/login?next=%2Faccount")

	// Activated again, she can sign in.
	entra.do(http.MethodPatch, "/Users/"+aliceID, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
"Operations":[{"op":"replace","value":{"active":true}}]}`, http.StatusOK)
	p = alice.open(base + "/auth/oidc/login?next=%2Faccount")
	if p.URL.Path != "/account" {
		t.Fatalf("alice after the reactivation: %s %d", p.URL, p.Status)
	}
	p.expectStatus(http.StatusOK).expectText("Status Subscribers")
}

// scimExpectMails waits for one e-mail per recipient with the subject and
// resets the fake server; other e-mails fail the test.
func scimExpectMails(t *testing.T, smtp *fakesmtp.Server, subject string, recipients ...string) {
	t.Helper()
	smtp.WaitFor(len(recipients), scimMailWait)
	time.Sleep(200 * time.Millisecond) // catch unexpected extra e-mails
	msgs := smtp.Messages()
	smtp.Reset()
	var got []string
	for _, m := range msgs {
		if m.Subject != subject {
			t.Errorf("e-mail to %v has subject %q, want %q", m.To, m.Subject, subject)
		}
		got = append(got, m.To...)
	}
	slices.Sort(got)
	want := slices.Sorted(slices.Values(recipients))
	if !slices.Equal(got, want) {
		t.Fatalf("e-mails %q went to %q, want %q", subject, got, want)
	}
}
