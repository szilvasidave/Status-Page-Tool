package web

// Tests of SCIM provisioning in the web UI: the settings section with the
// token, the /scim/v2 route in the middleware chain, and the badges on the
// groups and users pages.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/notify"
	"github.com/szilvasidave/status-page-tool/internal/scim"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// provLogBuffer collects the log of a test server.
type provLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *provLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *provLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// provEnv is newTestEnv whose server logs into the returned buffer.
func provEnv(t *testing.T) (*testEnv, *provLogBuffer) {
	t.Helper()
	e := newTestEnv(t)
	logs := &provLogBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h, err := NewHandler(Deps{DataDir: e.DataDir, Store: e.Store, Auth: e.Auth, Notifier: notify.New(e.Store, log), Version: "test", Log: log})
	if err != nil {
		t.Fatal(err)
	}
	e.Server.Config.Handler = h
	return e, logs
}

// provSCIM sends a SCIM request with a bearer token (no cookies, no CSRF
// token).
func provSCIM(t *testing.T, e *testEnv, method, path, token, body string) Response {
	t.Helper()
	req, _ := http.NewRequest(method, e.Server.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/scim+json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return Response{Status: resp.StatusCode, Location: resp.Header.Get("Location"), Body: b.String(), Header: resp.Header}
}

var provTokenRe = regexp.MustCompile(`id="scim-token">([^<]+)<`)

func TestProvisioningToken(t *testing.T) {
	e, logs := provEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	st := adminSettings(t, e)
	st.BaseURL = "https://status.example.com"
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	// Off: the Tenant URL and "Generate token" (no confirmation needed).
	r := e.get("/admin/settings")
	mustContain(t, r.Body, `id="provisioning"`, "SCIM provisioning", "https://status.example.com/scim/v2",
		"No token – provisioning is off.", `form="scim-token-form"`, "Generate token",
		`<form id="scim-token-form" method="post" action="/admin/settings/scim-token#provisioning" class="inline">`)
	if strings.Contains(r.Body, "Turn off provisioning") {
		t.Error("turn off is offered although provisioning is off")
	}
	provSCIM(t, e, http.MethodGet, "/scim/v2/Users", "anything", "").expectSCIMStatus(t, http.StatusUnauthorized)

	// Generate: the token is shown once on the page the POST redirects to,
	// only its hash is stored, and it is in no URL, cookie, header or log
	// line.
	token, gen, r := provGenerateToken(t, e)
	mustContain(t, r.Body, "New token generated.", "shown only once", "Provisioning is on.", "Generate new token", "Turn off provisioning")
	if strings.Contains(r.Body, "previous token no longer works") {
		t.Error("the first token claims to replace one")
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", r.Header.Get("Cache-Control"))
	}
	for _, resp := range []Response{gen, r} {
		for k, v := range resp.Header {
			if strings.Contains(strings.Join(v, " "), token) {
				t.Errorf("the token is in the header %s", k)
			}
		}
	}
	if strings.Contains(logs.String(), token) {
		t.Error("the token is in the log")
	}
	st = adminSettings(t, e)
	if st.SCIMTokenHash != scim.HashToken(token) || st.SCIMTokenCreatedAt.IsZero() || strings.Contains(st.SCIMTokenHash, token) {
		t.Fatalf("stored token: %+v", st)
	}

	// The settings page does not show it again; replacing asks first.
	r = e.get("/admin/settings")
	if strings.Contains(r.Body, token) || strings.Contains(r.Body, `id="scim-token"`) {
		t.Error("the token is shown again")
	}
	mustContain(t, r.Body, "Provisioning is on.", `data-confirm="Generate a new token?`, `action="/admin/settings/scim-token/delete"`,
		`data-confirm="Turn off provisioning?`)

	// SCIM through the whole middleware chain: no session, no CSRF token.
	provSCIM(t, e, http.MethodGet, "/scim/v2/Users", token, "").expectSCIMStatus(t, http.StatusOK)
	created := provSCIM(t, e, http.MethodPost, "/scim/v2/Users", token, `{"userName":"jane@example.com","displayName":"Jane"}`)
	created.expectSCIMStatus(t, http.StatusCreated)
	if !strings.HasPrefix(created.Location, "https://status.example.com/scim/v2/Users/") {
		t.Errorf("Location %q", created.Location)
	}
	// The session of a browser does not authenticate SCIM.
	req, _ := http.NewRequest(http.MethodGet, e.Server.URL+"/scim/v2/Users", nil)
	resp, err := e.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("SCIM with the admin's session cookie: %d, want 401", resp.StatusCode)
	}

	// A new token replaces the old one.
	newToken, _, r := provGenerateToken(t, e)
	if newToken == token {
		t.Fatal("no new token")
	}
	mustContain(t, r.Body, "The previous token no longer works.")
	provSCIM(t, e, http.MethodGet, "/scim/v2/Users", token, "").expectSCIMStatus(t, http.StatusUnauthorized)
	token = newToken
	provSCIM(t, e, http.MethodGet, "/scim/v2/Users", token, "").expectSCIMStatus(t, http.StatusOK)

	// Turn off: the token is deleted, provisioned groups are handed back to
	// sign-in.
	g, _, err := e.Store.CreateSCIMGroup(ctx, store.SCIMGroupData{DisplayName: "Ops", ExternalID: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	r = e.post("/admin/settings/scim-token/delete", nil)
	if r.Status != http.StatusSeeOther || r.Location != "/admin/settings" {
		t.Fatalf("turn off: %d %q", r.Status, r.Location)
	}
	mustContain(t, e.get(r.Location).Body, "Provisioning has been turned off.", "No token – provisioning is off.")
	provSCIM(t, e, http.MethodGet, "/scim/v2/Users", token, "").expectSCIMStatus(t, http.StatusUnauthorized)
	if got, _ := e.Store.GetGroup(ctx, g.ID); got == nil || got.Provisioned {
		t.Errorf("group still provisioned: %+v", got)
	}
	if st := adminSettings(t, e); st.SCIMEnabled() || !st.SCIMTokenCreatedAt.IsZero() {
		t.Errorf("token not deleted: %+v", st)
	}
}

// expectSCIMStatus checks the status of a SCIM response and that errors are
// SCIM JSON (not an HTML page or a redirect).
func (r Response) expectSCIMStatus(t *testing.T, status int) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("SCIM status %d, want %d: %s", r.Status, status, r.Body)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/scim+json" {
		t.Errorf("SCIM Content-Type %q", ct)
	}
}

func TestProvisioningTokenAccess(t *testing.T) {
	e := newTestEnv(t)
	for _, p := range []string{"/admin/settings/scim-token", "/admin/settings/scim-token/delete"} {
		if r := e.post(p, nil); r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Location, "/login") {
			t.Errorf("anonymous POST %s: %d %q", p, r.Status, r.Location)
		}
	}
	for _, role := range []store.Role{store.RoleViewer, store.RoleEditor} {
		e.logout()
		e.loginAs(role.String()+"@example.com", role)
		for _, p := range []string{"/admin/settings/scim-token", "/admin/settings/scim-token/delete"} {
			if r := e.post(p, nil); r.Status != http.StatusForbidden {
				t.Errorf("%s POST %s: %d, want 403", role, p, r.Status)
			}
		}
	}
	e.logout()
	e.loginAdmin()
	// Without the CSRF token the request is refused.
	req, _ := http.NewRequest(http.MethodPost, e.Server.URL+"/admin/settings/scim-token", strings.NewReader(url.Values{}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if r := e.do(req); r.Status != http.StatusForbidden {
		t.Errorf("POST without CSRF token: %d", r.Status)
	}
	if st := adminSettings(t, e); st.SCIMEnabled() {
		t.Error("a token was generated without permission")
	}
}

// TestProvisioningBeforeSetup: SCIM answers with its own errors, not with
// the redirect to the setup wizard.
func TestProvisioningBeforeSetup(t *testing.T) {
	e := newTestEnvNoAdmin(t)
	r := provSCIM(t, e, http.MethodGet, "/scim/v2/Users", "x", "")
	r.expectSCIMStatus(t, http.StatusUnauthorized)
	if e.get("/admin/settings").Location != "/setup" {
		t.Error("the setup redirect is gone")
	}
}

func TestProvisioningBadges(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	active, _, err := e.Store.CreateSCIMUser(ctx, store.SCIMUserData{UserName: "pia@example.com", Email: "pia@example.com", Name: "Pia Provisioned", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	gone, _, err := e.Store.CreateSCIMUser(ctx, store.SCIMUserData{UserName: "dan@example.com", Email: "dan@example.com", Name: "Dan Deactivated", Active: false})
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := e.Store.CreateSCIMGroup(ctx, store.SCIMGroupData{DisplayName: "Provisioned Team", ExternalID: "team", Members: []int64{active.ID, gone.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.CreateGroup(ctx, "claims-only", "Claims Only"); err != nil {
		t.Fatal(err)
	}

	// Groups: the provisioned group is listed (not collapsed) with a badge;
	// no directory search or sync any more.
	r := e.get("/admin/groups")
	mustContain(t, r.Body, "Provisioned Team</bdi></a> <span class=\"badge\">Provisioned</span>", "Other groups (1)")
	if i, j := strings.Index(r.Body, "Provisioned Team"), strings.Index(r.Body, "Other groups"); i < 0 || i > j {
		t.Error("the provisioned group is not listed before the other groups")
	}
	for _, gone := range []string{"/admin/groups/search", "/admin/groups/sync", "Sync now"} {
		if strings.Contains(r.Body, gone) {
			t.Errorf("the groups page still contains %q", gone)
		}
	}
	for _, p := range []string{"/admin/groups/search?q=x"} {
		if r := e.get(p); r.Status != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, r.Status)
		}
	}
	if r := e.post("/admin/groups/sync", nil); r.Status != http.StatusNotFound && r.Status != http.StatusMethodNotAllowed {
		t.Errorf("POST /admin/groups/sync: %d", r.Status)
	}

	r = e.get("/admin/groups/" + strconv.FormatInt(g.ID, 10))
	mustContain(t, r.Body, "come from Microsoft Entra ID", "Last provisioning change", "Pia Provisioned",
		"Dan Deactivated <span class=\"badge badge-muted\">Deactivated</span>",
		"The name comes from Microsoft Entra ID with provisioning")
	if strings.Contains(r.Body, `id="name"`) {
		t.Error("the name of a provisioned group can be edited")
	}

	// Users: badges and "Never signed in".
	r = e.get("/admin/users")
	mustContain(t, r.Body, "Pia Provisioned <span class=\"badge\">Provisioned</span>",
		"Dan Deactivated <span class=\"badge\">Provisioned</span> <span class=\"badge badge-muted\">Deactivated</span>", "Never signed in")
}
