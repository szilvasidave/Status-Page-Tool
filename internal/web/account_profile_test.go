package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

func TestAccountProfile(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if r := e.post("/account/profile", url.Values{"username": {"x"}}); r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Location, "/login") {
		t.Errorf("anonymous POST /account/profile: %d %q", r.Status, r.Location)
	}
	e.loginAdmin()
	r := e.get("/account")
	mustContain(t, r.Body, `action="/account/profile"`, `name="name" type="text" value="Administrator"`,
		`name="username" type="text" value="admin"`, "Save details")

	hash, _ := auth.HashPassword("backup-password")
	if _, err := e.Store.CreateLocalAdmin(ctx, "backup", "", hash); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, username, want string
	}{
		{"Ada", " ", "Enter a user name."},
		{"Ada", "has space", "(no spaces)"},
		{"Ada", strings.Repeat("a", 65), "at most 64 letters"},
		{strings.Repeat("ő", 101), "admin", "The display name is too long (at most 100 characters)."},
		{"Ada", "BACKUP", "Another local account already has this user name."},
	} {
		r := e.post("/account/profile", url.Values{"name": {tc.name}, "username": {tc.username}})
		if r.Status != http.StatusUnprocessableEntity {
			t.Fatalf("%q/%q: status %d, want 422", tc.name, tc.username, r.Status)
		}
		// The entered values are kept (the user name in lower case).
		mustContain(t, r.Body, tc.want, "Please correct the highlighted field", `aria-invalid="true"`,
			`value="`+adminEscape(strings.TrimSpace(tc.name))+`"`, `value="`+adminEscape(strings.ToLower(strings.TrimSpace(tc.username)))+`"`)
	}
	if u, _ := e.Store.GetUserByUsername(ctx, testAdminUser); u == nil || u.Name != "Administrator" {
		t.Fatalf("changed despite the errors: %+v", u)
	}

	// Saved: the user name is stored in lower case, and this browser stays
	// signed in.
	r = e.post("/account/profile", url.Values{"name": {" Ada Admin "}, "username": {" It-Admin "}})
	if r.Status != http.StatusSeeOther || r.Location != "/account" {
		t.Fatalf("save: %d %q %s", r.Status, r.Location, r.Body)
	}
	r = e.get("/account")
	mustContain(t, r.Body, "Your account details were saved. From now on, sign in with the user name “it-admin”.",
		`class="user-name"`, "Ada Admin", `value="it-admin"`)
	u, err := e.Store.GetUserByUsername(ctx, "IT-ADMIN")
	if err != nil || u.Name != "Ada Admin" || u.Username != "it-admin" || !auth.CheckPassword(u.PasswordHash, testAdminPass) {
		t.Fatalf("saved account: %+v %v", u, err)
	}

	// Only the display name: no hint about the user name. An empty display
	// name shows the user name.
	r = e.post("/account/profile", url.Values{"name": {""}, "username": {"it-admin"}})
	r = adminFollow(t, e, r)
	mustContain(t, r.Body, "Your account details were saved.", `<a href="/account" class="user-name" title="Your account" aria-current="page">it-admin</a>`)
	if strings.Contains(r.Body, "From now on") {
		t.Error("user name hint shown although the user name did not change")
	}

	// The new user name signs in, the old one does not.
	e.logout()
	if r := e.post("/login", url.Values{"username": {testAdminUser}, "password": {testAdminPass}}); r.Status != http.StatusUnauthorized {
		t.Errorf("old user name: %d", r.Status)
	}
	if r := e.post("/login", url.Values{"username": {"IT-Admin"}, "password": {testAdminPass}}); r.Status != http.StatusSeeOther {
		t.Errorf("new user name: %d", r.Status)
	}
}

// Users who sign in with single sign-on get their name from the identity
// provider and have no account details form.
func TestAccountProfileSSOUser(t *testing.T) {
	e := newTestEnv(t)
	u := e.loginAs("editor@example.com", store.RoleEditor)
	if r := e.get("/account"); strings.Contains(r.Body, "/account/profile") || strings.Contains(r.Body, "Account details") {
		t.Error("account details form shown to a single sign-on user")
	}
	r := e.post("/account/profile", url.Values{"name": {"Mallory"}, "username": {"mallory"}})
	if r.Status != http.StatusForbidden {
		t.Errorf("POST /account/profile as single sign-on user: %d", r.Status)
	}
	got, err := e.Store.GetUser(context.Background(), u.ID)
	if err != nil || got.Name != "editor@example.com" || got.Username != "" {
		t.Errorf("single sign-on user changed: %+v %v", got, err)
	}
}
