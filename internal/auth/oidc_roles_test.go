package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// roleOf returns the role of the session that the sign-in res started, as
// a request with its cookie gets it.
func (e *oidcEnv) roleOf(res loginResult) store.Role {
	e.t.Helper()
	for _, c := range res.cookies {
		if c.Name == SessionCookie && c.Value != "" {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(c)
			if id := e.m.loadIdentity(req); id != nil {
				return id.Role
			}
		}
	}
	e.t.Fatal("no valid session")
	return store.RoleNone
}

// signIn runs a sign-in with claims that must succeed.
func (e *oidcEnv) signIn(claims map[string]any) loginResult {
	e.t.Helper()
	e.idp.setUser(claims)
	res := e.login("/")
	if res.err != nil {
		e.t.Fatalf("sign-in failed: %v", res.err)
	}
	return res
}

func TestOIDCAppRoles(t *testing.T) {
	e := newOIDCEnv(t)
	appRoles := func(on bool) {
		e.saveSettings(func(s *store.Settings) { s.OIDCAppRoles = on })
	}

	// Recorded while roles from app roles are off, without effect.
	res := e.signIn(map[string]any{"sub": "user-1", "groups": []string{"staff"}, "roles": []string{"Admin", " Reader ", "Admin", ""}})
	u := e.sessionUser(res)
	if !slices.Equal(u.AppRoles, []string{"Admin", "Reader"}) {
		t.Errorf("app roles = %q, want [Admin Reader]", u.AppRoles)
	}
	if r := e.roleOf(res); r != store.RoleViewer {
		t.Errorf("role while off = %v, want viewer", r)
	}
	// Turned on: the session of the last sign-in has the role at once.
	appRoles(true)
	if r := e.roleOf(res); r != store.RoleAdmin {
		t.Errorf("role after turning it on = %v, want admin", r)
	}
	if log := e.logBuf.String(); !strings.Contains(log, "app_roles=") {
		t.Errorf("sign-in log without app roles:\n%s", log)
	}

	// Exact values only ("editor" may be another application's role); a
	// single string works.
	if r := e.roleOf(e.signIn(map[string]any{"sub": "user-1", "groups": []string{}, "roles": "editor"})); r != store.RoleViewer {
		t.Errorf("role with editor = %v, want viewer", r)
	}
	if r := e.roleOf(e.signIn(map[string]any{"sub": "user-1", "groups": []string{}, "roles": "Editor"})); r != store.RoleEditor {
		t.Errorf("role with Editor = %v", r)
	}
	// The roles of groups count as well: the higher role wins.
	g, err := e.store.CreateGroup(context.Background(), "admins", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.UpdateGroup(context.Background(), g.ID, "", store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if r := e.roleOf(e.signIn(map[string]any{"sub": "user-1", "groups": []string{"admins"}, "roles": []string{"Editor"}})); r != store.RoleAdmin {
		t.Errorf("role with an admin group and the editor app role = %v", r)
	}

	// Without the claim the app roles are gone (the assignment was removed;
	// userinfo has no roles either).
	e.idp.setUserinfo(map[string]any{"name": "User One"})
	res = e.signIn(map[string]any{"sub": "user-1", "groups": []string{"staff"}})
	if u := e.sessionUser(res); u.AppRoles != nil {
		t.Errorf("app roles without the claim = %q", u.AppRoles)
	}
	if r := e.roleOf(res); r != store.RoleViewer {
		t.Errorf("role without the claim = %v", r)
	}
	if _, hits := e.idp.hits(); hits != 1 {
		// The ID token had the groups but no roles: userinfo was asked for them.
		t.Errorf("userinfo hits = %d, want 1", hits)
	}

	// A claim of the wrong type counts as no app roles and is logged.
	if r := e.roleOf(e.signIn(map[string]any{"sub": "user-1", "groups": []string{}, "roles": map[string]any{"Admin": true}})); r != store.RoleViewer {
		t.Errorf("role with a malformed claim = %v", r)
	}
	if log := e.logBuf.String(); !strings.Contains(log, "the roles claim is neither a list of strings nor a string; the user is treated as having no app roles") {
		t.Errorf("malformed roles claim not logged:\n%s", log)
	}

	// Another claim name and other values.
	e.saveSettings(func(s *store.Settings) {
		s.OIDCAppRoles = true
		s.OIDCRolesClaim = "app_roles"
		s.OIDCAdminRole = "StatusPage.Admin"
		s.OIDCEditorRole = ""
	})
	if r := e.roleOf(e.signIn(map[string]any{"sub": "user-1", "groups": []string{}, "roles": []string{"Admin"}, "app_roles": []string{"StatusPage.Admin"}})); r != store.RoleAdmin {
		t.Errorf("role with a custom claim = %v", r)
	}
	if r := e.roleOf(e.signIn(map[string]any{"sub": "user-1", "groups": []string{}, "app_roles": []string{"Editor"}})); r != store.RoleViewer {
		t.Errorf("role with an app role that is not set = %v", r)
	}
}

// The roles claim is read from userinfo when the ID token has none, while
// roles from app roles are on.
func TestOIDCAppRolesFromUserinfo(t *testing.T) {
	e := newOIDCEnv(t)
	e.idp.setUserinfo(map[string]any{"roles": []string{"Admin"}})

	// Off: userinfo is not asked for the roles alone.
	res := e.signIn(map[string]any{"sub": "user-1", "groups": []string{"staff"}})
	if _, hits := e.idp.hits(); hits != 0 {
		t.Errorf("userinfo hits while off = %d, want 0", hits)
	}
	if u := e.sessionUser(res); u.AppRoles != nil {
		t.Errorf("app roles = %q", u.AppRoles)
	}
	// It is asked for the groups, and then its roles are recorded.
	res = e.signIn(map[string]any{"sub": "user-1"})
	if u := e.sessionUser(res); !slices.Equal(u.AppRoles, []string{"Admin"}) {
		t.Errorf("app roles from userinfo (for the groups) = %q", u.AppRoles)
	}

	e.saveSettings(func(s *store.Settings) { s.OIDCAppRoles = true })
	res = e.signIn(map[string]any{"sub": "user-1", "groups": []string{"staff"}})
	if r := e.roleOf(res); r != store.RoleAdmin {
		t.Errorf("role from userinfo = %v", r)
	}
	if got := e.groupsOf(e.sessionUser(res).ID); !slices.Equal(got, []string{"staff"}) {
		t.Errorf("groups = %v", got)
	}

	// Userinfo fails: no app roles until the next sign-in (fail closed); the
	// groups of the ID token are used as usual.
	e.idp.setUserinfo(nil)
	res = e.signIn(map[string]any{"sub": "user-1", "groups": []string{"ops"}})
	if r := e.roleOf(res); r != store.RoleViewer {
		t.Errorf("role when userinfo failed = %v, want viewer", r)
	}
	if got := e.groupsOf(e.sessionUser(res).ID); !slices.Equal(got, []string{"ops"}) {
		t.Errorf("groups = %v, want [ops]", got)
	}
	if log := e.logBuf.String(); !strings.Contains(log, "could not read the userinfo endpoint for the roles claim; this sign-in gives no app roles") {
		t.Errorf("userinfo failure not logged:\n%s", log)
	}

	// Neither groups nor roles in the ID token, userinfo fails: the groups
	// stay, the app roles are gone.
	res = e.signIn(map[string]any{"sub": "user-1"})
	if got := e.groupsOf(e.sessionUser(res).ID); !slices.Equal(got, []string{"ops"}) {
		t.Errorf("groups = %v, want them unchanged", got)
	}
	if log := e.logBuf.String(); !strings.Contains(log, "group memberships are left unchanged and this sign-in gives no app roles") {
		t.Errorf("userinfo failure not logged:\n%s", log)
	}
}

// Microsoft Entra ID leaves the groups claim out for a user in no groups,
// and its userinfo has no roles claim: the app roles of the ID token are
// kept, the groups come from userinfo.
func TestOIDCAppRolesIDTokenFirst(t *testing.T) {
	e := newOIDCEnv(t)
	e.saveSettings(func(s *store.Settings) { s.OIDCAppRoles = true })
	e.idp.setUserinfo(map[string]any{"name": "Boss"})
	res := e.signIn(map[string]any{"sub": "boss", "roles": []string{"Admin"}})
	if _, hits := e.idp.hits(); hits != 1 {
		t.Errorf("userinfo hits = %d, want 1 (for the groups)", hits)
	}
	u := e.sessionUser(res)
	if !slices.Equal(u.AppRoles, []string{"Admin"}) || e.roleOf(res) != store.RoleAdmin || len(e.groupsOf(u.ID)) != 0 {
		t.Errorf("app roles %q, role %v, groups %v; want [Admin], admin, none", u.AppRoles, e.roleOf(res), e.groupsOf(u.ID))
	}

	// Userinfo with groups and other roles: the roles of the ID token win.
	e.idp.setUserinfo(map[string]any{"groups": []string{"staff"}, "roles": []string{"Reader"}})
	res = e.signIn(map[string]any{"sub": "boss", "roles": []string{"Admin"}})
	u = e.sessionUser(res)
	if !slices.Equal(u.AppRoles, []string{"Admin"}) || e.roleOf(res) != store.RoleAdmin || !slices.Equal(e.groupsOf(u.ID), []string{"staff"}) {
		t.Errorf("app roles %q, role %v, groups %v; want [Admin], admin, [staff]", u.AppRoles, e.roleOf(res), e.groupsOf(u.ID))
	}
}

// rolesFromUserinfo: the userinfo endpoint of Microsoft Entra ID (on
// Microsoft Graph) never has a roles claim, so it is not asked for one.
func TestRolesFromUserinfo(t *testing.T) {
	const entra = "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0"
	cases := []struct {
		issuer, endpoint string
		want             bool
	}{
		{entra, "https://graph.microsoft.com/oidc/userinfo", false},
		{entra, "https://userinfo.example.com/userinfo", false},
		{"https://sts.windows.net/11111111-2222-3333-4444-555555555555/", "https://graph.microsoft.com/oidc/userinfo", false},
		{"https://login.microsoftonline.com/common/v2.0", "https://graph.microsoft.com/oidc/userinfo", false},
		{"https://keycloak.example.com/realms/it", "https://GRAPH.microsoft.com/oidc/userinfo", false},
		{"https://keycloak.example.com/realms/it", "https://keycloak.example.com/realms/it/protocol/openid-connect/userinfo", true},
		{"https://keycloak.example.com/realms/it", "https://graph.microsoft.com.example.com/userinfo", true},
		{"https://keycloak.example.com/realms/it", "", true},
	}
	for _, c := range cases {
		if got := rolesFromUserinfo(c.issuer, c.endpoint); got != c.want {
			t.Errorf("rolesFromUserinfo(%q, %q) = %v, want %v", c.issuer, c.endpoint, got, c.want)
		}
	}
}
