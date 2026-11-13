package web

import (
	"net/http"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// Turning the local sign-in off and on again on the settings page signs the
// local administrator out for good, like "statuspage local-login": a session
// from before (an idle browser, a copied cookie) does not come back. While it
// is off, the sign-in page still tells each browser of the local
// administrator why it was signed out.
func TestLocalLoginOnAgainEndsOldSessions(t *testing.T) {
	e := newTestEnv(t)

	// Two browsers of the local administrator.
	e.loginAdmin()
	idle := e.Client.Jar
	e.logout()
	e.loginAdmin()
	other := e.Client.Jar
	e.logout()

	// A single sign-on administrator turns the local sign-in off.
	e.loginAs("boss@example.com", store.RoleAdmin)
	boss := e.Client.Jar
	r := e.post("/admin/settings", localLoginSSOForm(adminSettings(t, e), false))
	if r.Status != http.StatusSeeOther || adminSettings(t, e).LocalLoginEnabled {
		t.Fatalf("turn off: %d", r.Status)
	}

	// One browser of the local administrator opens a page meanwhile and is
	// told why it was signed out; the other one stays idle.
	e.Client.Jar = other
	if r := e.get("/account"); r.Status != http.StatusSeeOther {
		t.Fatalf("local administrator while turned off: %d", r.Status)
	}
	mustContain(t, e.get("/login").Body, "has been turned off, so you were signed out")

	// Turned on again on the settings page: the idle session is gone too,
	// while the single sign-on administrator stays signed in.
	e.Client.Jar = boss
	r = e.post("/admin/settings", localLoginSSOForm(adminSettings(t, e), true))
	if r.Status != http.StatusSeeOther || !adminSettings(t, e).LocalLoginEnabled {
		t.Fatalf("turn on again: %d", r.Status)
	}
	if r := e.get("/admin/settings"); r.Status != http.StatusOK {
		t.Errorf("the single sign-on administrator was signed out: %d", r.Status)
	}
	e.Client.Jar = idle
	if r := e.get("/account"); r.Status != http.StatusSeeOther {
		t.Errorf("a session from before turning the local sign-in off came back: %d", r.Status)
	}

	// The local administrator signs in again. Saving the settings without
	// changing the switch keeps the session.
	e.loginAdmin()
	if r := e.post("/admin/settings", localLoginSSOForm(adminSettings(t, e), true)); r.Status != http.StatusSeeOther {
		t.Fatalf("save as the local administrator: %d", r.Status)
	}
	if r := e.get("/account"); r.Status != http.StatusOK {
		t.Errorf("saving the settings signed the local administrator out: %d", r.Status)
	}
}
