package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

func TestAccountRequiresLogin(t *testing.T) {
	e := newTestEnv(t)
	r := e.get("/account")
	if r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Location, "/login?next=%2Faccount") {
		t.Errorf("anonymous GET /account: %d %q", r.Status, r.Location)
	}
	r = e.post("/account/password", url.Values{"current_password": {testAdminPass}})
	if r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Location, "/login") {
		t.Errorf("anonymous POST /account/password: %d %q", r.Status, r.Location)
	}
}

func TestAccountPageSSOUser(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	u := e.loginAs("editor@example.com", store.RoleEditor)

	// A second group (without a role) with a display name; services assigned
	// to the groups are listed, others are not.
	other, err := e.Store.CreateGroup(ctx, "11111111-aaaa", "IT Operations")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SetUserGroups(ctx, u.ID, []store.GroupRef{{ExternalID: "test-group-editor"}, {ExternalID: other.ExternalID}}); err != nil {
		t.Fatal(err)
	}
	editors, err := e.Store.CreateGroup(ctx, "test-group-editor", "")
	if err != nil {
		t.Fatal(err)
	}
	mail := &store.Service{Name: "E-mail", Category: "Communication", GroupIDs: []int64{editors.ID}}
	vpn := &store.Service{Name: "VPN", GroupIDs: []int64{other.ID, editors.ID}}
	erp := &store.Service{Name: "Secret ERP"}
	for _, svc := range []*store.Service{mail, vpn, erp} {
		if err := e.Store.CreateService(ctx, svc); err != nil {
			t.Fatal(err)
		}
	}

	r := e.get("/account")
	if r.Status != http.StatusOK {
		t.Fatalf("GET /account: %d", r.Status)
	}
	mustContain(t, r.Body,
		"Your account", "editor@example.com", "Editor", "Single sign-on",
		"IT Operations", "test-group-editor",
		"refreshed every time you sign in",
		"E-mail", "(Communication)", "VPN",
		`aria-current="page"`, // account nav link
	)
	if strings.Contains(r.Body, "Secret ERP") {
		t.Error("service of another group listed")
	}
	if strings.Contains(r.Body, "Change password") || strings.Contains(r.Body, "/account/password") {
		t.Error("password form shown to an SSO user")
	}

	// SSO users cannot use the password form.
	r = e.post("/account/password", url.Values{"current_password": {"x"}, "new_password": {"a-new-password"}, "confirm_password": {"a-new-password"}})
	if r.Status != http.StatusForbidden {
		t.Errorf("POST /account/password as SSO user: %d", r.Status)
	}
}

func TestAccountPageWithoutGroupsOrEmail(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	u, err := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "no-mail", Name: "No Mail"})
	if err != nil {
		t.Fatal(err)
	}
	e.loginUserID(u.ID)
	r := e.get("/account")
	if r.Status != http.StatusOK {
		t.Fatalf("GET /account: %d", r.Status)
	}
	mustContain(t, r.Body, "No Mail", "Viewer", "You are not a member of any group", "no e-mail address")

	e.loginAs("viewer@example.com", store.RoleViewer)
	r = e.get("/account")
	mustContain(t, r.Body, "viewer@example.com", "Viewer", "test-group-viewer", "None of your groups is assigned to a service")
}

func TestAccountPageLocalAdmin(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	r := e.get("/account")
	if r.Status != http.StatusOK {
		t.Fatalf("GET /account: %d", r.Status)
	}
	mustContain(t, r.Body, "Administrator", "Local administrator account", "Change password",
		`action="/account/password"`, `name="csrf_token"`, `minlength="10"`)
	if strings.Contains(r.Body, "Notifications") {
		t.Error("notifications section shown to the local administrator")
	}
}

func TestAccountChangePassword(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.loginAdmin()
	admin, err := e.Store.GetUserByUsername(ctx, testAdminUser)
	if err != nil {
		t.Fatal(err)
	}

	// Another session of the administrator (e.g. another browser).
	rec := httptest.NewRecorder()
	if err := e.Auth.StartSession(ctx, rec, httptest.NewRequest(http.MethodGet, "/", nil), admin.ID); err != nil {
		t.Fatal(err)
	}
	otherSession := rec.Result().Cookies()[0]

	const newPass = "a-brand-new-password"
	cases := []struct {
		name              string
		current, pw, conf string
		want              string
	}{
		{"wrong current", "wrong-password", newPass, newPass, "The current password is not correct."},
		{"empty current", "", newPass, newPass, "The current password is not correct."},
		{"too short", testAdminPass, "short", "short", "at least 10 characters"},
		{"too long", testAdminPass, strings.Repeat("x", 73), strings.Repeat("x", 73), "too long"},
		{"mismatch", testAdminPass, newPass, newPass + "x", "do not match"},
	}
	for _, tc := range cases {
		r := e.post("/account/password", url.Values{"current_password": {tc.current}, "new_password": {tc.pw}, "confirm_password": {tc.conf}})
		if r.Status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", tc.name, r.Status)
			continue
		}
		mustContain(t, r.Body, tc.want, "Change password")
		if strings.Contains(r.Body, tc.pw) && len(tc.pw) > 5 {
			t.Errorf("%s: entered password echoed back in the page", tc.name)
		}
	}

	r := e.post("/account/password", url.Values{"current_password": {testAdminPass}, "new_password": {newPass}, "confirm_password": {newPass}})
	if r.Status != http.StatusSeeOther || r.Location != "/account" {
		t.Fatalf("change password: %d %q %s", r.Status, r.Location, r.Body)
	}

	// This browser stays signed in and sees the confirmation.
	r = e.get("/account")
	if r.Status != http.StatusOK {
		t.Fatalf("GET /account after change: %d", r.Status)
	}
	mustContain(t, r.Body, "Your password was changed")

	// Other sessions are signed out.
	srvReq, _ := http.NewRequest(http.MethodGet, e.Server.URL+"/account", nil)
	srvReq.AddCookie(otherSession)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Do(srvReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Errorf("other session after the password change: %d %q, want redirect to /login", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Only the new password works.
	e.logout()
	if r := e.post("/login", url.Values{"username": {testAdminUser}, "password": {testAdminPass}}); r.Status != http.StatusUnauthorized {
		t.Errorf("old password: status %d", r.Status)
	}
	if r := e.post("/login", url.Values{"username": {testAdminUser}, "password": {newPass}}); r.Status != http.StatusSeeOther {
		t.Errorf("new password: status %d", r.Status)
	}
	u, err := e.Store.GetUser(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckPassword(u.PasswordHash, newPass) {
		t.Error("stored hash does not match the new password")
	}
}
