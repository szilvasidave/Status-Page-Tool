package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // time zones independent of the host

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/directory"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminFollow performs a GET request to the redirect target of r (to see the
// flash message) and returns that response.
func adminFollow(t *testing.T, e *testEnv, r Response) Response {
	t.Helper()
	if r.Status != http.StatusSeeOther || r.Location == "" {
		t.Fatalf("expected a redirect (303), got %d: %s", r.Status, r.Body)
	}
	return e.get(r.Location)
}

// adminSettingsForm returns a valid settings form for the current settings
// (secrets empty = keep).
func adminSettingsForm(st *store.Settings) url.Values {
	f := url.Values{
		"site_name":         {st.SiteName},
		"base_url":          {st.BaseURL},
		"timezone":          {st.Timezone},
		"oidc_issuer":       {st.OIDCIssuer},
		"oidc_client_id":    {st.OIDCClientID},
		"oidc_scopes":       {st.OIDCScopes},
		"oidc_groups_claim": {st.OIDCGroupsClaim},
		"oidc_button_label": {st.OIDCButtonLabel},
		"smtp_host":         {st.SMTPHost},
		"smtp_port":         {strconv.Itoa(st.SMTPPort)},
		"smtp_security":     {st.SMTPSecurity},
		"smtp_username":     {st.SMTPUsername},
		"smtp_from":         {st.SMTPFrom},
	}
	if st.PublicAccess {
		f.Set("public_access", "on")
	}
	if st.LocalLoginEnabled {
		f.Set("local_login_enabled", "on")
	}
	if st.OIDCEnabled {
		f.Set("oidc_enabled", "on")
	}
	if st.DirectorySyncEnabled {
		f.Set("directory_sync_enabled", "on")
	}
	return f
}

func adminSettings(t *testing.T, e *testEnv) *store.Settings {
	t.Helper()
	st, err := e.Store.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAdminAccessControl(t *testing.T) {
	e := newTestEnv(t)
	pages := []string{"/admin", "/admin/settings", "/admin/services", "/admin/services/new", "/admin/groups",
		"/admin/groups/search?q=x", "/admin/users", "/admin/notifications"}
	posts := []string{"/admin/settings", "/admin/settings/test-email", "/admin/services", "/admin/groups", "/admin/groups/sync"}

	for _, p := range pages {
		if r := e.get(p); r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Location, "/login") {
			t.Errorf("anonymous GET %s: got %d %q, want redirect to /login", p, r.Status, r.Location)
		}
	}
	for _, role := range []store.Role{store.RoleViewer, store.RoleEditor} {
		e.logout()
		e.loginAs(role.String()+"@example.com", role)
		for _, p := range pages {
			if r := e.get(p); r.Status != http.StatusForbidden {
				t.Errorf("%s GET %s: got %d, want 403", role, p, r.Status)
			}
		}
		for _, p := range posts {
			if r := e.post(p, url.Values{"name": {"x"}, "external_id": {"x"}}); r.Status != http.StatusForbidden {
				t.Errorf("%s POST %s: got %d, want 403", role, p, r.Status)
			}
		}
	}
	if svcs, _ := e.Store.ListServices(context.Background()); len(svcs) != 0 {
		t.Errorf("a non-admin created a service")
	}

	// An SSO user in a group with the admin role is an administrator.
	e.logout()
	e.loginAs("boss@example.com", store.RoleAdmin)
	for _, p := range pages[1:] {
		r := e.get(p)
		if p == "/admin/groups/search?q=x" {
			// directory sync is not enabled: back to the groups page
			if r.Status != http.StatusSeeOther || r.Location != "/admin/groups" {
				t.Errorf("GET %s: got %d %q", p, r.Status, r.Location)
			}
			continue
		}
		if r.Status != http.StatusOK {
			t.Errorf("admin GET %s: got %d", p, r.Status)
			continue
		}
		mustContain(t, r.Body, `class="subnav"`, `href="/admin/notifications"`, `aria-current="page"`)
	}
	if r := e.get("/admin"); r.Status != http.StatusSeeOther || r.Location != "/admin/settings" {
		t.Errorf("GET /admin: got %d %q", r.Status, r.Location)
	}
}

func TestAdminSettingsSave(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()

	r := e.get("/admin/settings")
	mustContain(t, r.Body, `name="site_name"`, `value="Status"`, `<option value="UTC" selected>UTC</option>`,
		auth.OIDCCallbackPath, "set the base URL", `action="/admin/settings/test-email"`)

	f := adminSettingsForm(adminSettings(t, e))
	f.Set("site_name", "ACME IT Status")
	f.Set("base_url", "https://status.example.com/")
	f.Set("timezone", "Europe/Berlin")
	f.Del("public_access")
	f.Set("oidc_enabled", "on")
	f.Set("oidc_issuer", "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0")
	f.Set("oidc_client_id", "client-1")
	f.Set("oidc_client_secret", "very-secret-1")
	f.Set("oidc_scopes", "  openid   profile email ")
	f.Set("oidc_groups_claim", "")
	f.Set("oidc_button_label", "Sign in with Microsoft")
	f.Set("directory_sync_enabled", "on")
	f.Set("smtp_host", "smtp.example.com")
	f.Set("smtp_port", "")
	f.Set("smtp_security", "tls")
	f.Set("smtp_username", "mailer")
	f.Set("smtp_password", "mail-pass-1")
	f.Set("smtp_from", "IT Status <status@example.com>")
	r = e.post("/admin/settings", f)
	if r.Status != http.StatusSeeOther || r.Location != "/admin/settings" {
		t.Fatalf("save settings: got %d %s", r.Status, r.Body)
	}
	st := adminSettings(t, e)
	want := store.Settings{
		SiteName: "ACME IT Status", BaseURL: "https://status.example.com", PublicAccess: false, Timezone: "Europe/Berlin",
		OIDCEnabled: true, OIDCIssuer: "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0",
		OIDCClientID: "client-1", OIDCClientSecret: "very-secret-1", OIDCScopes: "openid profile email",
		OIDCGroupsClaim: "groups", OIDCButtonLabel: "Sign in with Microsoft", DirectorySyncEnabled: true,
		SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPSecurity: "tls", SMTPUsername: "mailer",
		SMTPPassword: "mail-pass-1", SMTPFrom: "IT Status <status@example.com>",
		LocalLoginEnabled: true, MailMethod: store.MailSMTP,
	}
	if *st != want {
		t.Fatalf("stored settings:\n got %+v\nwant %+v", *st, want)
	}

	// The page shows the flash and the redirect URI, but never the secrets.
	r = e.get("/admin/settings")
	mustContain(t, r.Body, "Settings saved.", "https://status.example.com"+auth.OIDCCallbackPath, "(stored)", "A secret is stored")
	if strings.Contains(r.Body, "very-secret-1") || strings.Contains(r.Body, "mail-pass-1") {
		t.Fatal("the settings page reveals a stored secret")
	}

	// Empty secret fields keep the stored secrets.
	f = adminSettingsForm(st)
	f.Set("site_name", "Renamed")
	if r := e.post("/admin/settings", f); r.Status != http.StatusSeeOther {
		t.Fatalf("save settings again: got %d %s", r.Status, r.Body)
	}
	st = adminSettings(t, e)
	if st.SiteName != "Renamed" || st.OIDCClientSecret != "very-secret-1" || st.SMTPPassword != "mail-pass-1" {
		t.Fatalf("secrets not kept: %+v", st)
	}

	// A new secret replaces the old one; no SMTP user name drops the password.
	f.Set("oidc_client_secret", "very-secret-2")
	f.Set("smtp_username", "")
	if r := e.post("/admin/settings", f); r.Status != http.StatusSeeOther {
		t.Fatalf("save settings again: got %d %s", r.Status, r.Body)
	}
	st = adminSettings(t, e)
	if st.OIDCClientSecret != "very-secret-2" || st.SMTPPassword != "" {
		t.Fatalf("secrets not replaced: %+v", st)
	}
}

func TestAdminSettingsValidation(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	before := adminSettings(t, e)

	tests := []struct {
		name   string
		change map[string]string
		want   string
	}{
		{"no site name", map[string]string{"site_name": " "}, "Enter a name for the status page."},
		{"relative base URL", map[string]string{"base_url": "status.example.com"}, "starting with http:// or https://"},
		{"base URL with path", map[string]string{"base_url": "https://example.com/status"}, "cannot be served from a sub-path"},
		{"unknown time zone", map[string]string{"timezone": "Mars/Olympus"}, "Unknown time zone"},
		{"OIDC without issuer", map[string]string{"oidc_enabled": "on", "oidc_client_id": "c", "oidc_client_secret": "s"}, "Enter the issuer URL"},
		{"OIDC http issuer", map[string]string{"oidc_enabled": "on", "oidc_issuer": "http://idp.example.com", "oidc_client_id": "c", "oidc_client_secret": "s"}, "must start with https://"},
		{"OIDC without secret", map[string]string{"oidc_enabled": "on", "oidc_issuer": "https://idp.example.com", "oidc_client_id": "c"}, "Enter the client secret"},
		{"OIDC without openid scope", map[string]string{"oidc_enabled": "on", "oidc_issuer": "https://idp.example.com", "oidc_client_id": "c", "oidc_client_secret": "s", "oidc_scopes": "profile"}, "must include"},
		{"directory sync with other IdP", map[string]string{"oidc_enabled": "on", "oidc_issuer": "https://keycloak.example.com/realms/x", "oidc_client_id": "c", "oidc_client_secret": "s", "directory_sync_enabled": "on"}, "only with Microsoft Entra ID"},
		{"SMTP port", map[string]string{"smtp_host": "smtp.example.com", "smtp_from": "a@example.com", "smtp_port": "99999"}, "port number between 1 and 65535"},
		{"SMTP security", map[string]string{"smtp_security": "ssl"}, "Choose STARTTLS, TLS or none."},
		{"SMTP without sender", map[string]string{"smtp_host": "smtp.example.com"}, "Enter the sender address"},
		{"SMTP bad sender", map[string]string{"smtp_host": "smtp.example.com", "smtp_from": "not an address"}, "Enter a valid sender address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := adminSettingsForm(before)
			for k, v := range tt.change {
				f.Set(k, v)
			}
			r := e.post("/admin/settings", f)
			if r.Status != http.StatusUnprocessableEntity {
				t.Fatalf("got %d, want 422", r.Status)
			}
			mustContain(t, r.Body, "Please correct the highlighted", `aria-invalid="true"`, adminEscape(tt.want))
			for k, v := range tt.change {
				if k != "oidc_enabled" && k != "directory_sync_enabled" && k != "smtp_security" && strings.TrimSpace(v) != "" {
					mustContain(t, r.Body, `value="`+adminEscape(v)+`"`) // entered values are kept
				}
			}
		})
	}
	if after := adminSettings(t, e); *after != *before {
		t.Fatalf("invalid input changed the settings: %+v", after)
	}
}

// adminEscape escapes s like html/template does for text and attributes.
func adminEscape(s string) string {
	return strings.NewReplacer(`&`, "&amp;", `"`, "&#34;", `'`, "&#39;", `<`, "&lt;", `>`, "&gt;", `+`, "&#43;").Replace(s)
}

// TestAdminSettingsResetOIDC checks that a changed issuer takes effect
// immediately, i.e. the cached OIDC client is dropped after saving.
func TestAdminSettingsResetOIDC(t *testing.T) {
	newIdP := func() *httptest.Server {
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/.well-known/openid-configuration" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                                srv.URL,
				"authorization_endpoint":                srv.URL + "/authorize",
				"token_endpoint":                        srv.URL + "/token",
				"jwks_uri":                              srv.URL + "/jwks",
				"response_types_supported":              []string{"code"},
				"subject_types_supported":               []string{"public"},
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	idp1, idp2 := newIdP(), newIdP()

	e := newTestEnv(t)
	e.loginAdmin()
	save := func(issuer string) {
		t.Helper()
		f := adminSettingsForm(adminSettings(t, e))
		f.Set("oidc_enabled", "on")
		f.Set("oidc_issuer", issuer)
		f.Set("oidc_client_id", "client")
		f.Set("oidc_client_secret", "secret")
		if r := e.post("/admin/settings", f); r.Status != http.StatusSeeOther {
			t.Fatalf("save settings: %d %s", r.Status, r.Body)
		}
	}
	start := func() string {
		t.Helper()
		r := e.get("/auth/oidc/login")
		if r.Status == http.StatusServiceUnavailable && strings.Contains(r.Body, "not configured") {
			t.Skip("OIDC login is not implemented in this build")
		}
		if r.Status != http.StatusFound && r.Status != http.StatusSeeOther {
			t.Fatalf("OIDC start: got %d %s", r.Status, r.Body)
		}
		return r.Location
	}

	save(idp1.URL)
	e.logout() // the OIDC login is for anonymous users
	if loc := start(); !strings.HasPrefix(loc, idp1.URL+"/authorize") {
		t.Fatalf("redirected to %q, want the first identity provider", loc)
	}
	e.loginAdmin()
	save(idp2.URL)
	e.logout()
	if loc := start(); !strings.HasPrefix(loc, idp2.URL+"/authorize") {
		t.Fatalf("redirected to %q after changing the issuer, want the second identity provider", loc)
	}
}

func TestAdminTestEmail(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()

	r := adminFollow(t, e, e.post("/admin/settings/test-email", url.Values{"to": {"nobody"}}))
	mustContain(t, r.Body, "Enter a valid e-mail address")

	r = adminFollow(t, e, e.post("/admin/settings/test-email", url.Values{"to": {"admin@example.com"}}))
	mustContain(t, r.Body, "E-mail is not configured yet")

	// A server that refuses connections: the error is shown.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	st := adminSettings(t, e)
	st.SMTPHost, st.SMTPPort, st.SMTPSecurity, st.SMTPFrom, st.MailMethod = "127.0.0.1", port, "none", "status@example.com", store.MailSMTP
	if err := e.Store.SaveSettings(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	r = adminFollow(t, e, e.post("/admin/settings/test-email", url.Values{"to": {"admin@example.com"}}))
	mustContain(t, r.Body, "flash-error", "The test e-mail could not be sent")
}

func TestAdminServices(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	g1, _ := e.Store.CreateGroup(ctx, "grp-ops", "Operations")
	g2, _ := e.Store.CreateGroup(ctx, "grp-all", "All Staff")

	r := e.get("/admin/services")
	mustContain(t, r.Body, "No services yet", `href="/admin/services/new"`)
	r = e.get("/admin/services/new")
	mustContain(t, r.Body, `name="groups" value="`+strconv.FormatInt(g1.ID, 10)+`"`, "Operations", "All Staff", "Groups appear",
		"Other groups (2)") // without a role or services

	// Validation.
	r = e.post("/admin/services", url.Values{"name": {""}, "sort_order": {"abc"}, "category": {"Infra"}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("create invalid service: got %d", r.Status)
	}
	mustContain(t, r.Body, "Enter a name for the service.", "Enter a whole number", `value="abc"`, `value="Infra"`)

	// Create.
	r = e.post("/admin/services", url.Values{
		"name": {"VPN"}, "description": {"Remote access"}, "category": {"Infrastructure"}, "sort_order": {"5"},
		"groups": {strconv.FormatInt(g1.ID, 10), strconv.FormatInt(g2.ID, 10)},
	})
	r = adminFollow(t, e, r)
	mustContain(t, r.Body, "Service “VPN” created.", "VPN", "Remote access", "Infrastructure", "All Staff, Operations", "status-operational")
	svcs, _ := e.Store.ListServices(ctx)
	if len(svcs) != 1 || svcs[0].Name != "VPN" || svcs[0].SortOrder != 5 || len(svcs[0].GroupIDs) != 2 {
		t.Fatalf("stored services: %+v", svcs)
	}
	id := strconv.FormatInt(svcs[0].ID, 10)

	// The list shows the derived status.
	inc := &store.Incident{Title: "Down", Status: store.IncidentInvestigating, Services: []store.ServiceImpact{{ServiceID: svcs[0].ID, Impact: store.StatusMajorOutage}}}
	if err := e.Store.CreateIncident(ctx, inc, &store.IncidentUpdate{Status: store.IncidentInvestigating, Message: "x"}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, e.get("/admin/services").Body, "status-major_outage")

	// Edit: the selected category and the checked groups are shown.
	r = e.get("/admin/services/" + id)
	mustContain(t, r.Body, `<option value="Infrastructure" selected>`, `value="VPN"`, `value="5"`, "checked", "Delete service", "data-confirm=")
	r = e.post("/admin/services/"+id, url.Values{"name": {"VPN gateway"}, "category": {""}, "sort_order": {""}, "groups": {strconv.FormatInt(g2.ID, 10)}})
	mustContain(t, adminFollow(t, e, r).Body, "Service “VPN gateway” saved.")
	svc, _ := e.Store.GetService(ctx, svcs[0].ID)
	if svc.Name != "VPN gateway" || svc.Category != "" || svc.SortOrder != 0 || len(svc.GroupIDs) != 1 || svc.GroupIDs[0] != g2.ID {
		t.Fatalf("updated service: %+v", svc)
	}
	if r := e.post("/admin/services/"+id, url.Values{"name": {""}}); r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid update: got %d", r.Status)
	}

	// Unknown services.
	if r := e.get("/admin/services/9999"); r.Status != http.StatusNotFound {
		t.Errorf("GET unknown service: %d", r.Status)
	}
	if r := e.post("/admin/services/9999", url.Values{"name": {"x"}}); r.Status != http.StatusNotFound {
		t.Errorf("update unknown service: %d", r.Status)
	}

	// Groups with services (and the checked ones) are listed first, the
	// others collapsed.
	r = e.get("/admin/services/" + id)
	mustContain(t, r.Body, "Other groups (1)")
	if i, j := strings.Index(r.Body, "All Staff"), strings.Index(r.Body, "Other groups"); i < 0 || i > j {
		t.Error("the group of the service is not listed before the other groups")
	}

	// Delete is refused while an open incident or a maintenance that has not
	// ended affects the service (an incident without services could not be
	// resolved properly).
	r = adminFollow(t, e, e.post("/admin/services/"+id+"/delete", nil))
	mustContain(t, r.Body, "cannot be deleted while the open incident “Down” affects it")
	if err := e.Store.AddIncidentUpdate(ctx, inc.ID, &store.IncidentUpdate{Status: store.IncidentResolved, Message: "ok"}, nil); err != nil {
		t.Fatal(err)
	}
	m := &store.Maintenance{Title: "Upgrade", StartsAt: time.Now().Add(time.Hour), EndsAt: time.Now().Add(2 * time.Hour),
		Services: []store.ServiceRef{{ID: svcs[0].ID}}}
	if err := e.Store.CreateMaintenance(ctx, m); err != nil {
		t.Fatal(err)
	}
	r = adminFollow(t, e, e.post("/admin/services/"+id+"/delete", nil))
	mustContain(t, r.Body, "cannot be deleted while the maintenance “Upgrade” affects it")
	if err := e.Store.CancelMaintenance(ctx, m.ID); err != nil {
		t.Fatal(err)
	}

	// Delete: the resolved incident stays.
	mustContain(t, adminFollow(t, e, e.post("/admin/services/"+id+"/delete", nil)).Body, "Service “VPN gateway” deleted.")
	if svcs, _ := e.Store.ListServices(ctx); len(svcs) != 0 {
		t.Fatalf("service not deleted")
	}
	if _, err := e.Store.GetIncident(ctx, inc.ID); err != nil {
		t.Fatalf("incident deleted with the service: %v", err)
	}
	if r := e.post("/admin/services/"+id+"/delete", nil); r.Status != http.StatusNotFound {
		t.Errorf("delete unknown service: %d", r.Status)
	}
}

func TestAdminGroups(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	svc := &store.Service{Name: "E-Mail"}
	if err := e.Store.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	r := e.get("/admin/groups")
	mustContain(t, r.Body, "No groups yet", `action="/admin/groups"`)
	if strings.Contains(r.Body, "/admin/groups/search") {
		t.Error("the directory search is shown although directory sync is disabled")
	}

	// Validation.
	r = e.post("/admin/groups", url.Values{"external_id": {" "}, "name": {"Typed name"}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("add group without id: got %d", r.Status)
	}
	mustContain(t, r.Body, "Enter the group ID", `value="Typed name"`)

	// Add: redirects to the edit page.
	r = e.post("/admin/groups", url.Values{"external_id": {"0c6d-guid"}, "name": {"Service Desk"}})
	if r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Location, "/admin/groups/") {
		t.Fatalf("add group: got %d %q", r.Status, r.Location)
	}
	editURL := r.Location
	r = e.get(editURL)
	mustContain(t, r.Body, "Group “Service Desk” added.", "0c6d-guid", `value="viewer" checked`, "E-Mail", "never", "No known members yet")

	// Adding it again does not create a duplicate.
	r = e.post("/admin/groups", url.Values{"external_id": {"0c6d-guid"}})
	if r.Location != editURL {
		t.Fatalf("add existing group: got %q, want %q", r.Location, editURL)
	}
	mustContain(t, e.get(editURL).Body, "already exists")
	groups, _ := e.Store.ListGroups(ctx)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	g := groups[0]

	// Edit role and services; members are listed.
	u, _ := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "s1", Email: "erin@example.com", Name: "Erin Editor"})
	if err := e.Store.SetUserGroups(ctx, u.ID, []store.GroupRef{{ExternalID: "0c6d-guid"}}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, e.get(editURL).Body, "Erin Editor", "erin@example.com")
	if r := e.post(editURL, url.Values{"name": {"Desk"}, "role": {"superuser"}}); r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid role: got %d", r.Status)
	}
	r = e.post(editURL, url.Values{"name": {"Desk"}, "role": {"editor"}, "services": {strconv.FormatInt(svc.ID, 10)}})
	mustContain(t, adminFollow(t, e, r).Body, "Group “Desk” saved.", "Editor")
	got, _ := e.Store.GetGroup(ctx, g.ID)
	if got.Name != "Desk" || got.Role != store.RoleEditor || len(got.ServiceIDs) != 1 || got.ServiceIDs[0] != svc.ID {
		t.Fatalf("updated group: %+v", got)
	}
	if role, _ := e.Store.UserRole(ctx, u.ID); role != store.RoleEditor {
		t.Fatalf("member role: %v", role)
	}
	// Groups in use are listed first, groups only seen in sign-ins collapsed.
	if err := e.Store.SetUserGroups(ctx, u.ID, []store.GroupRef{{ExternalID: "0c6d-guid"}, {ExternalID: "5a1f-unused"}}); err != nil {
		t.Fatal(err)
	}
	r = e.get("/admin/groups")
	mustContain(t, r.Body, "Desk", "0c6d-guid", "Editor", "Other groups (1)", "5a1f-unused")
	if i, j := strings.Index(r.Body, "0c6d-guid"), strings.Index(r.Body, "Other groups"); i > j {
		t.Error("the group in use is not listed before the other groups")
	}

	// Back to viewer: no group is in use any more.
	if r := e.post(editURL, url.Values{"name": {"Desk"}, "role": {"viewer"}}); r.Status != http.StatusSeeOther {
		t.Fatalf("set viewer: %d", r.Status)
	}
	if got, _ := e.Store.GetGroup(ctx, g.ID); got.Role != store.RoleNone || len(got.ServiceIDs) != 0 {
		t.Fatalf("group after reset: %+v", got)
	}
	mustContain(t, e.get("/admin/groups").Body, "No group has a role or services yet", "Other groups (2)")

	// Directory features are off.
	r = adminFollow(t, e, e.post("/admin/groups/sync", nil))
	mustContain(t, r.Body, "Directory sync is not enabled")
	if r := e.get("/admin/groups/search?q=desk"); r.Status != http.StatusSeeOther {
		t.Fatalf("search without directory sync: %d", r.Status)
	}

	// Delete.
	mustContain(t, adminFollow(t, e, e.post(editURL+"/delete", nil)).Body, "Group “Desk” deleted.")
	if _, err := e.Store.GetGroup(ctx, g.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("group not deleted: %v", err)
	}
	if r := e.get(editURL); r.Status != http.StatusNotFound {
		t.Fatalf("deleted group: %d", r.Status)
	}
}

func TestAdminUsers(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	u, _ := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "s1", Email: "sso@example.com", Name: "Sam SSO"})
	_ = e.Store.SetUserGroups(ctx, u.ID, []store.GroupRef{{ExternalID: "ops", Name: "Ops Team"}})
	g, _ := e.Store.CreateGroup(ctx, "dir-group", "Directory Group")
	if err := e.Store.SyncGroupMembers(ctx, g.ID, "", []store.DirectoryUser{{DirectoryID: "d1", Email: "dora@example.com", Name: "Dora Directory"}}); err != nil {
		t.Fatal(err)
	}

	r := e.get("/admin/users")
	if r.Status != http.StatusOK {
		t.Fatalf("users: %d", r.Status)
	}
	mustContain(t, r.Body, "Administrator", "Local administrator",
		"Sam SSO", "sso@example.com", "Ops Team", ">SSO<",
		"Dora Directory", "Directory Group", "Directory only (never signed in)", "never",
		"come from the identity provider")

	// SSO and directory users can be removed, the local administrator not.
	mustContain(t, r.Body, `action="/admin/users/`+strconv.FormatInt(u.ID, 10)+`/delete"`, "next sign-in or the next directory sync")
	if n := strings.Count(r.Body, `/delete"`); n != 2 {
		t.Errorf("%d remove buttons, want 2 (not for the local administrator)", n)
	}
	r = adminFollow(t, e, e.post("/admin/users/"+strconv.FormatInt(u.ID, 10)+"/delete", nil))
	mustContain(t, r.Body, "“Sam SSO” has been removed.", "Dora Directory")
	if strings.Contains(r.Body, "sso@example.com") {
		t.Error("the removed user is still listed")
	}
	if members, _ := e.Store.GroupMembers(ctx, g.ID); len(members) != 1 {
		t.Errorf("directory group members: %+v", members)
	}
	admin, err := e.Store.GetUserByUsername(ctx, testAdminUser)
	if err != nil {
		t.Fatal(err)
	}
	r = adminFollow(t, e, e.post("/admin/users/"+strconv.FormatInt(admin.ID, 10)+"/delete", nil))
	mustContain(t, r.Body, "The local administrator account cannot be removed.")
	for _, id := range []string{strconv.FormatInt(u.ID, 10), "9999", "x"} {
		if r := e.post("/admin/users/"+id+"/delete", nil); r.Status != http.StatusNotFound {
			t.Errorf("remove user %s: %d, want 404", id, r.Status)
		}
	}
	if users, _ := e.Store.ListUsers(ctx); len(users) != 2 {
		t.Errorf("%d users left, want 2", len(users))
	}
}

func TestAdminNotifications(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()

	r := e.get("/admin/notifications")
	mustContain(t, r.Body, "E-mail is not configured", "No e-mails have been sent yet.")

	err := e.Store.EnqueueEmails(ctx, []store.OutboundEmail{
		{Email: "a@example.com", Subject: "Incident: VPN down", BodyText: "x"},
		{Email: "b@example.com", Subject: "Maintenance scheduled", BodyText: "y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := e.Store.ListNotifications(ctx, 10)
	for _, n := range list {
		if n.Email == "a@example.com" {
			_ = e.Store.MarkNotificationAttemptFailed(ctx, n.ID, "550 mailbox unavailable", nil)
		} else {
			_ = e.Store.MarkNotificationSent(ctx, n.ID)
		}
	}
	st := adminSettings(t, e)
	st.SMTPHost, st.SMTPFrom, st.MailMethod = "smtp.example.com", "status@example.com", store.MailSMTP
	_ = e.Store.SaveSettings(ctx, st)

	r = e.get("/admin/notifications")
	mustContain(t, r.Body, "a@example.com", "Incident: VPN down", "notif-failed", "550 mailbox unavailable",
		"b@example.com", "Maintenance scheduled", "notif-sent", "latest 200")
	if strings.Contains(r.Body, "E-mail is not configured") {
		t.Error("SMTP note shown although SMTP is configured")
	}
}

// TestAdminDirectoryTemplates renders the pages that are only reachable with
// directory sync enabled (which needs Microsoft Graph) with sample data.
func TestAdminDirectoryTemplates(t *testing.T) {
	s := &Server{Deps: Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	s.loc.Store(time.UTC)
	if err := s.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	render := func(name string, data any) string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/admin/x", nil)
		req = req.WithContext(auth.WithCSRFToken(req.Context(), "tok-123"))
		s.render(rec, req, name, "Test", data)
		if rec.Code != http.StatusOK {
			t.Fatalf("render %s: %d %s", name, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}

	body := render("admin_group_search", &adminGroupSearchData{Query: "IT", Searched: true, Results: []adminGroupResult{
		{Group: directory.Group{ID: "guid-1", Name: "IT Operations", Description: "Ops team"}, ExistingID: 7},
		{Group: directory.Group{ID: "guid-2", Name: "IT Security"}},
	}})
	mustContain(t, body, `href="/admin/groups/7"`, "Already added", "Ops team", `name="external_id" value="guid-2"`,
		`name="name" value="IT Security"`, `name="csrf_token" value="tok-123"`)
	body = render("admin_group_search", &adminGroupSearchData{Query: "x", Searched: true, Error: "The search failed: boom"})
	mustContain(t, body, "The search failed: boom")
	body = render("admin_group_search", &adminGroupSearchData{Query: "zzz", Searched: true})
	mustContain(t, body, "No groups found")

	body = render("admin_groups", &adminGroupsData{DirectoryEnabled: true, Errors: adminFormErrors{}})
	mustContain(t, body, `action="/admin/groups/search"`, `action="/admin/groups/sync"`, "Sync now")
	body = render("admin_settings", &adminSettingsData{St: store.DefaultSettings(), Sync: adminSyncData{DirectoryEnabled: true}, Errors: adminFormErrors{}})
	mustContain(t, body, `form="directory-sync-form"`, `id="directory-sync-form"`, `name="next" value="/admin/settings"`)
}

// TestAdminLengthLimitsCountCharacters: length limits count characters like
// the maxlength of the inputs, not bytes, so accented names up to the limit
// are accepted.
func TestAdminLengthLimitsCountCharacters(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	o := func(n int) string { return strings.Repeat("ő", n) } // 2 bytes each

	r := e.post("/admin/services", url.Values{"name": {o(100)}, "category": {o(100)}, "description": {o(300)}})
	mustContain(t, adminFollow(t, e, r).Body, "Service “"+o(100)+"” created.")
	r = e.post("/admin/services", url.Values{"name": {o(101)}, "category": {o(101)}, "description": {o(301)}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("service with too long fields: %d", r.Status)
	}
	mustContain(t, r.Body, "The name is too long (at most 100 characters).", "The category is too long (at most 100 characters).",
		"The description is too long (at most 300 characters).")

	f := adminSettingsForm(adminSettings(t, e))
	f.Set("site_name", o(100))
	mustContain(t, adminFollow(t, e, e.post("/admin/settings", f)).Body, "Settings saved.")
	f.Set("site_name", o(101))
	r = e.post("/admin/settings", f)
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("too long site name: %d", r.Status)
	}
	mustContain(t, r.Body, "The name is too long (at most 100 characters).")

	r = e.post("/admin/groups", url.Values{"external_id": {o(255)}, "name": {o(200)}})
	if r.Status != http.StatusSeeOther {
		t.Fatalf("add group with 200 characters: %d %s", r.Status, r.Body)
	}
	r = e.post("/admin/groups", url.Values{"external_id": {o(256)}, "name": {o(201)}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("group with too long fields: %d", r.Status)
	}
	mustContain(t, r.Body, "The group ID is too long", "The display name is too long")
}

// TestAdminSettingsLocalhostBaseURL: a base URL that only works on the server
// itself is pointed out.
func TestAdminSettingsLocalhostBaseURL(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	const warning = "only works on the server itself"
	for base, warn := range map[string]bool{
		"http://localhost:8080": true, "http://127.0.0.1:8080": true, "http://status-srv:8080": false, "": false,
	} {
		f := adminSettingsForm(adminSettings(t, e))
		f.Set("base_url", base)
		r := adminFollow(t, e, e.post("/admin/settings", f))
		if strings.Contains(r.Body, warning) != warn {
			t.Errorf("base URL %q: warning shown = %v, want %v", base, !warn, warn)
		}
	}
}
