package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// localLoginSSOForm returns the settings form with single sign-on configured
// and the local administrator sign-in on or off.
func localLoginSSOForm(st *store.Settings, local bool) url.Values {
	f := adminSettingsForm(st)
	f.Set("oidc_enabled", "on")
	f.Set("oidc_issuer", "https://idp.example.com")
	f.Set("oidc_client_id", "status")
	if st.OIDCClientSecret == "" {
		f.Set("oidc_client_secret", "secret")
	}
	if local {
		f.Set("local_login_enabled", "on")
	} else {
		f.Del("local_login_enabled")
	}
	return f
}

func TestLocalLoginSwitch(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.loginAdmin()

	r := e.get("/admin/settings")
	mustContain(t, r.Body, `name="local_login_enabled" aria-describedby="local_login_help" checked`,
		"Allow sign-in with the local administrator account", "statuspage local-login enable")

	// It cannot be turned off without single sign-on, nor without a group
	// with the Administrator role.
	f := adminSettingsForm(adminSettings(t, e))
	f.Del("local_login_enabled")
	r = e.post("/admin/settings", f)
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("turn off without single sign-on: %d", r.Status)
	}
	mustContain(t, r.Body, "can only be turned off while single sign-on is enabled and configured",
		`name="local_login_enabled" aria-describedby="local_login_help" aria-invalid="true"`)
	r = e.post("/admin/settings", localLoginSSOForm(adminSettings(t, e), false))
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("turn off without an administrator group: %d", r.Status)
	}
	mustContain(t, r.Body, "at least one group has the Administrator role")
	if !adminSettings(t, e).LocalLoginEnabled {
		t.Fatal("turned off despite the errors")
	}
	g, err := e.Store.CreateGroup(ctx, "it-admins", "IT admins")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.UpdateGroup(ctx, g.ID, g.Name, store.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	// The local administrator turns it off and is signed out: the sign-in
	// page says why (once) and offers single sign-on only.
	r = e.post("/admin/settings", localLoginSSOForm(adminSettings(t, e), false))
	if r.Status != http.StatusSeeOther {
		t.Fatalf("turn off: %d %s", r.Status, r.Body)
	}
	if adminSettings(t, e).LocalLoginEnabled {
		t.Fatal("not turned off")
	}
	r = e.get(r.Location)
	if r.Status != http.StatusSeeOther || r.Location != "/login?next=%2Fadmin%2Fsettings" {
		t.Fatalf("settings page after turning off: %d %q", r.Status, r.Location)
	}
	r = e.get(r.Location)
	mustContain(t, r.Body, "Settings saved.", "has been turned off, so you were signed out", `href="/auth/oidc/login?next=%2fadmin%2fsettings"`)
	if strings.Contains(r.Body, `action="/login"`) {
		t.Error("the sign-in page shows the local form")
	}
	if r := e.get("/login"); strings.Contains(r.Body, "you were signed out") {
		t.Error("the sign-out notice is shown again")
	}

	// The local sign-in is refused, also with ?local=1.
	r = e.post("/login", url.Values{"username": {testAdminUser}, "password": {testAdminPass}})
	if r.Status != http.StatusForbidden {
		t.Fatalf("local sign-in while turned off: %d", r.Status)
	}
	mustContain(t, r.Body, "Sign-in with the local administrator account is turned off", "statuspage local-login enable")
	if strings.Contains(r.Body, `action="/login"`) {
		t.Error("the refusal shows the local form")
	}
	r = e.get("/login?local=1")
	mustContain(t, r.Body, "is turned off", "statuspage local-login enable")
	if strings.Contains(r.Body, `action="/login"`) {
		t.Error("?local=1 shows the local form")
	}

	// A single sign-on administrator sees it off and cannot turn single
	// sign-on off while it is off; turning it on again lets the local
	// administrator sign in again.
	e.logout()
	e.loginAs("boss@example.com", store.RoleAdmin)
	r = e.get("/admin/settings")
	if !strings.Contains(r.Body, `name="local_login_enabled" aria-describedby="local_login_help">`) {
		t.Error("the checkbox is not shown unticked")
	}
	f = localLoginSSOForm(adminSettings(t, e), false)
	f.Del("oidc_enabled")
	if r := e.post("/admin/settings", f); r.Status != http.StatusUnprocessableEntity || !strings.Contains(r.Body, "can only be turned off while single sign-on") {
		t.Fatalf("turn off single sign-on: %d", r.Status)
	}
	r = e.post("/admin/settings", localLoginSSOForm(adminSettings(t, e), true))
	if r.Status != http.StatusSeeOther || !adminSettings(t, e).LocalLoginEnabled {
		t.Fatalf("turn on again: %d", r.Status)
	}
	if r := e.get("/admin/settings"); r.Status != http.StatusOK {
		t.Errorf("the single sign-on administrator was signed out: %d", r.Status)
	}
	e.logout()
	if r := e.post("/login", url.Values{"username": {testAdminUser}, "password": {testAdminPass}}); r.Status != http.StatusSeeOther {
		t.Fatalf("local sign-in after turning it on again: %d", r.Status)
	}
	r = e.get("/login?local=1&next=%2F")
	if r.Status != http.StatusSeeOther {
		t.Fatalf("sign-in page while signed in: %d", r.Status)
	}
}

// Sessions of the local administrator stop working as soon as the sign-in is
// turned off by somebody else (e.g. on the command line); other users stay
// signed in.
func TestLocalLoginSessions(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.loginAdmin()
	adminCookies := e.Client.Jar

	st := adminSettings(t, e)
	st.LocalLoginEnabled = false
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	if r := e.get("/account"); r.Status != http.StatusSeeOther {
		t.Fatalf("local administrator after turning off: %d", r.Status)
	}
	// Not configured single sign-on: the page says that nobody can sign in.
	r := e.get("/login")
	mustContain(t, r.Body, "you were signed out", "Signing in is not possible", "statuspage local-login enable")

	e.logout()
	e.loginAs("viewer@example.com", store.RoleViewer)
	if r := e.get("/account"); r.Status != http.StatusOK {
		t.Fatalf("single sign-on user: %d", r.Status)
	}

	// Turned on again, the session ended by the sign-in page above does not
	// come back. (The settings page and "statuspage local-login enable" end
	// the other sessions of the local administrator too, see
	// TestLocalLoginOnAgainEndsOldSessions.)
	st.LocalLoginEnabled = true
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	e.Client.Jar = adminCookies
	if r := e.get("/account"); r.Status != http.StatusSeeOther {
		t.Errorf("the session ended by the sign-in page came back: %d", r.Status)
	}
}
