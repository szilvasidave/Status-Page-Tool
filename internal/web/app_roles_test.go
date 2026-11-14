package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

const entraIssuer = "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0"

// ssoForm returns the settings form with single sign-on set up for Entra ID.
func ssoForm(t *testing.T, e *testEnv) url.Values {
	t.Helper()
	f := adminSettingsForm(adminSettings(t, e))
	f.Set("oidc_enabled", "on")
	f.Set("oidc_issuer", entraIssuer)
	f.Set("oidc_client_id", "client-1")
	f.Set("oidc_client_secret", "secret-1")
	return f
}

func TestSettingsAppRolesAndGroupNames(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	r := e.get("/admin/settings")
	mustContain(t, r.Body, `name="entra_group_names"`, `name="oidc_app_roles"`, `id="app-role-fields" hidden`,
		`name="oidc_admin_role" type="text" value="Admin"`, `name="oidc_editor_role" type="text" value="Editor"`,
		`name="oidc_roles_claim" type="text" value="roles"`, "GroupMember.Read.All")

	invalid := []struct {
		name   string
		change func(url.Values)
		want   string
	}{
		{"no app role", func(f url.Values) { f.Set("oidc_admin_role", ""); f.Set("oidc_editor_role", " ") },
			"Enter the value of the app role for administrators, for editors, or both."},
		{"same app roles", func(f url.Values) { f.Set("oidc_admin_role", "Admin"); f.Set("oidc_editor_role", "ADMIN") },
			"Administrators and editors need different app roles."},
		{"too long", func(f url.Values) { f.Set("oidc_editor_role", strings.Repeat("x", 257)) },
			"The app role is too long (at most 256 characters)."},
		{"group names without Entra ID", func(f url.Values) {
			f.Set("entra_group_names", "on")
			f.Set("oidc_issuer", "https://keycloak.example.com/realms/it")
		}, "Group names can only be looked up in Microsoft Entra ID"},
		{"group names without tenant", func(f url.Values) {
			f.Set("entra_group_names", "on")
			f.Set("oidc_issuer", "https://login.microsoftonline.com/common/v2.0")
		}, "To look up group names, the issuer must name your tenant"},
	}
	for _, tc := range invalid {
		f := ssoForm(t, e)
		f.Set("oidc_app_roles", "on")
		tc.change(f)
		r := e.post("/admin/settings", f)
		if r.Status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", tc.name, r.Status)
			continue
		}
		mustContain(t, r.Body, tc.want)
	}
	if st := adminSettings(t, e); st.OIDCAppRoles || st.EntraGroupNames {
		t.Fatalf("invalid settings were saved: %+v", st)
	}

	f := ssoForm(t, e)
	f.Set("oidc_app_roles", "on")
	f.Set("entra_group_names", "on")
	f.Set("oidc_admin_role", " StatusPage.Admin ")
	f.Set("oidc_editor_role", "")
	f.Set("oidc_roles_claim", " ")
	if r := e.post("/admin/settings", f); r.Status != http.StatusSeeOther {
		t.Fatalf("save: %d %s", r.Status, r.Body)
	}
	st := adminSettings(t, e)
	if !st.OIDCAppRoles || !st.EntraGroupNames || st.OIDCAdminRole != "StatusPage.Admin" || st.OIDCEditorRole != "" || st.OIDCRolesClaim != "roles" {
		t.Errorf("saved: %+v", st)
	}
	r = e.get("/admin/settings")
	mustContain(t, r.Body, `name="oidc_app_roles" data-show-when="on" data-target="#app-role-fields" aria-describedby="oidc_app_roles_help" checked`,
		`<div id="app-role-fields">`, `name="entra_group_names" aria-describedby="entra_group_names_help" checked`)
}

// The local administrator sign-in can be turned off when somebody signed in
// with the app role for administrators, instead of a group with the
// Administrator role; then roles from app roles cannot be turned off.
func TestLocalLoginOffWithAppRoleAdmin(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.loginAdmin()
	if _, err := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "s-boss", Email: "boss@example.com", AppRoles: []string{"Admin"}}); err != nil {
		t.Fatal(err)
	}

	f := ssoForm(t, e)
	f.Del("local_login_enabled")
	r := e.post("/admin/settings", f)
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("local sign-in off without any administrator: %d", r.Status)
	}
	mustContain(t, r.Body, "The local administrator sign-in can only be turned off when at least one group has the Administrator role")

	f.Set("oidc_app_roles", "on")
	if r := e.post("/admin/settings", f); r.Status != http.StatusSeeOther {
		t.Fatalf("local sign-in off with an app role administrator: %d %s", r.Status, r.Body)
	}
	if adminSettings(t, e).LocalLoginEnabled {
		t.Fatal("the local sign-in is still on")
	}

	// From now on the local administrator is signed out: change the
	// settings as the app role administrator.
	e.logout()
	boss, err := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "s-boss", AppRoles: []string{"Admin"}})
	if err != nil {
		t.Fatal(err)
	}
	e.loginUserID(boss.ID)
	f.Del("oidc_app_roles")
	f.Del(auth.CSRFField) // set by the posts above, for the previous session
	r = e.post("/admin/settings", f)
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("turning app roles off would lock everybody out, but: %d %s", r.Status, r.Body)
	}
	// The error is at the field this save changed, not at the local
	// sign-in, which was off already.
	mustContain(t, r.Body, `aria-describedby="oidc_app_roles_help" aria-invalid="true"`,
		"nobody has signed in with the app role entered here in the last 30 days")
	if strings.Contains(r.Body, `aria-describedby="local_login_help" aria-invalid="true"`) {
		t.Error("the error is at the local sign-in, which this save did not change")
	}
	// Another value for administrators: refused as well, at that field.
	f.Set("oidc_app_roles", "on")
	f.Set("oidc_admin_role", "StatusPage.Admin")
	r = e.post("/admin/settings", f)
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("another value for administrators would lock everybody out, but: %d", r.Status)
	}
	mustContain(t, r.Body, `value="StatusPage.Admin" maxlength="256" spellcheck="false" autocomplete="off" aria-invalid="true"`)
	if strings.Contains(r.Body, `id="app-role-fields" hidden`) {
		t.Error("the app role fields are hidden with an error in them")
	}
	f.Set("oidc_admin_role", "Admin")

	// The only group with the Administrator role can lose it while somebody
	// has the app role for administrators …
	g, err := e.Store.CreateGroup(ctx, "admins", "Admins")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.UpdateGroup(ctx, g.ID, "Admins", store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	carl, err := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "s-carl", Email: "carl@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SetUserGroups(ctx, carl.ID, []store.GroupRef{{ExternalID: "admins"}}); err != nil {
		t.Fatal(err)
	}
	path := "/admin/groups/" + strconv.FormatInt(g.ID, 10)
	if r := e.post(path, url.Values{"name": {"Admins"}, "role": {"editor"}}); r.Status != http.StatusSeeOther {
		t.Errorf("demote the admin group: %d", r.Status)
	}
	if err := e.Store.UpdateGroup(ctx, g.ID, "Admins", store.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	// … but not when nobody has it any more (the assignment was removed).
	if _, err := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "s-boss"}); err != nil {
		t.Fatal(err)
	}
	e.logout()
	e.loginUserID(carl.ID)
	r = e.post(path, url.Values{"name": {"Admins"}, "role": {"editor"}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("demote the last way to administer: %d, want 422", r.Status)
	}
	mustContain(t, r.Body, "nobody has signed in with the app role for administrators",
		"Give another group the Administrator role or sign in with the app role for administrators first")
	r = adminFollow(t, e, e.post(path+"/delete", nil))
	mustContain(t, r.Body, "nobody has signed in with the app role for administrators")
	if _, err := e.Store.GetGroup(ctx, g.ID); err != nil {
		t.Errorf("the group was deleted: %v", err)
	}
}

func TestUsersAndAccountShowAppRoles(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	u, err := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "s-ann", Email: "ann@example.com", Name: "Ann Approle", AppRoles: []string{"Admin", "Reader"}})
	if err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	r := e.get("/admin/users")
	if strings.Contains(r.Body, "App roles:") {
		t.Error("app roles shown while roles from app roles are off")
	}
	annRow := func(body string) string {
		return regexp.MustCompile(`(?s)<tr>\s*<td>[^<]*Ann Approle.*?</tr>`).FindString(body)
	}
	if row := annRow(r.Body); !strings.Contains(row, `<span class="muted">Viewer</span>`) {
		t.Errorf("Ann's row while app roles are off: %q", row)
	}
	e.logout()
	e.loginUserID(u.ID)
	if r := e.get("/admin/settings"); r.Status != http.StatusForbidden {
		t.Errorf("admin page while off: %d, want 403", r.Status)
	}
	r = e.get("/account")
	mustContain(t, r.Body, "Viewer", "Your role is the highest role of your groups.")
	if strings.Contains(r.Body, "App roles") {
		t.Error("account page shows app roles while they are off")
	}

	saveSettings(t, e, func(s *store.Settings) { s.OIDCAppRoles = true })
	if r := e.get("/admin/settings"); r.Status != http.StatusOK {
		t.Errorf("admin page with the app role: %d, want 200", r.Status)
	}
	r = e.get("/account")
	mustContain(t, r.Body, "Administrator", "App roles", "Admin, Reader", "Your role is the highest role of your groups and app roles.")
	r = e.get("/admin/users")
	mustContain(t, r.Body, "App roles: Admin, Reader", "and with app roles")
	if row := annRow(r.Body); !strings.Contains(row, `<span class="badge">Administrator</span>`) {
		t.Errorf("Ann's row with app roles on: %q", row)
	}
	mustContain(t, e.get("/admin/groups").Body, "highest role of their groups\n  and app roles")
}

// Without a value for the app role for administrators, the messages do not
// suggest signing in with it.
func TestLastAdminGroupWithoutAdminAppRole(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	g, err := e.Store.CreateGroup(ctx, "admins", "Admins")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.UpdateGroup(ctx, g.ID, "Admins", store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	carl, err := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "s-carl", Email: "carl@example.com", AppRoles: []string{"Admin"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SetUserGroups(ctx, carl.ID, []store.GroupRef{{ExternalID: "admins"}}); err != nil {
		t.Fatal(err)
	}
	saveSettings(t, e, func(s *store.Settings) {
		s.OIDCEnabled, s.OIDCIssuer, s.OIDCClientID, s.OIDCClientSecret = true, "https://idp.example.com", "c", "s"
		s.OIDCAppRoles, s.OIDCAdminRole, s.OIDCEditorRole = true, "", "Editor"
		s.LocalLoginEnabled = false
	})
	e.loginUserID(carl.ID)
	r := e.post("/admin/groups/"+strconv.FormatInt(g.ID, 10), url.Values{"name": {"Admins"}, "role": {"editor"}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("demote the only admin group: %d, want 422", r.Status)
	}
	mustContain(t, r.Body, "Give another group the Administrator role first")
	if strings.Contains(r.Body, "sign in with the app role") {
		t.Error("the message suggests the app role for administrators, which has no value")
	}
}
