package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSettingsAppRole(t *testing.T) {
	on := DefaultSettings()
	on.OIDCAppRoles = true
	cases := []struct {
		values []string
		want   Role
	}{
		{nil, RoleNone},
		{[]string{"Reader"}, RoleNone},
		{[]string{"Editor"}, RoleEditor},
		{[]string{"Reader", "Editor"}, RoleEditor},
		{[]string{"Editor", "Admin"}, RoleAdmin},
		// Exact values only: "admin" may be a role of another application.
		{[]string{"admin", "editor", "ADMIN"}, RoleNone},
		{[]string{"Admin "}, RoleNone}, // values are compared as stored (trimmed at sign-in)
		{[]string{"Administrator"}, RoleNone},
	}
	for _, tc := range cases {
		if got := on.AppRole(tc.values); got != tc.want {
			t.Errorf("AppRole(%q) = %v, want %v", tc.values, got, tc.want)
		}
	}
	off := DefaultSettings()
	if got := off.AppRole([]string{"Admin"}); got != RoleNone {
		t.Errorf("AppRole while off = %v", got)
	}
	none := on
	none.OIDCAdminRole, none.OIDCEditorRole = " ", ""
	if got := none.AppRole([]string{"", " ", "Admin"}); got != RoleNone {
		t.Errorf("AppRole without values set = %v", got)
	}
	var nilSettings *Settings
	if nilSettings.AppRole([]string{"Admin"}) != RoleNone || nilSettings.GroupNamesActive() || nilSettings.AppRoleAdminSet() {
		t.Error("nil settings")
	}
	if !on.AppRoleAdminSet() || none.AppRoleAdminSet() || off.AppRoleAdminSet() {
		t.Error("AppRoleAdminSet")
	}

	active := DefaultSettings()
	active.OIDCEnabled, active.OIDCIssuer, active.OIDCClientID, active.OIDCClientSecret = true, "https://issuer", "c", "s"
	if active.GroupNamesActive() {
		t.Error("GroupNamesActive without EntraGroupNames")
	}
	active.EntraGroupNames = true
	if !active.GroupNamesActive() {
		t.Error("GroupNamesActive = false")
	}
	active.OIDCClientSecret = ""
	if active.GroupNamesActive() {
		t.Error("GroupNamesActive without a client secret")
	}
}

func TestAppRolesSettingsStored(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		st, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "defaults", [5]any{st.EntraGroupNames, st.OIDCAppRoles, st.OIDCRolesClaim, st.OIDCAdminRole, st.OIDCEditorRole},
			[5]any{false, false, "roles", "Admin", "Editor"})
		st.EntraGroupNames, st.OIDCAppRoles, st.OIDCRolesClaim, st.OIDCAdminRole, st.OIDCEditorRole = true, true, "app_roles", "SP.Admin", ""
		check(t, s.SaveSettings(ctx, st))
		got, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "stored", *got, *st)
	})
}

func TestUserAppRoles(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		role := func(id int64) Role {
			t.Helper()
			r, err := s.UserRole(ctx, id)
			check(t, err)
			return r
		}
		many := make([]string, 0, 120)
		for i := range 120 {
			many = append(many, fmt.Sprint("r", i))
		}
		u, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s1", Email: "u@example.com",
			AppRoles: append([]string{" Admin ", "", "Admin", strings.Repeat("x", 257), "Reader"}, many...)})
		check(t, err)
		if len(u.AppRoles) != 100 || u.AppRoles[0] != "Admin" || u.AppRoles[1] != "Reader" || u.AppRoles[99] != "r97" {
			t.Fatalf("app roles = %d values %q…", len(u.AppRoles), u.AppRoles[:3])
		}

		// Off: no effect, also in the list of users.
		equal(t, "role while off", role(u.ID), RoleViewer)
		offUsers, err := s.ListUsers(ctx)
		check(t, err)
		equal(t, "listed role while off", [2]any{len(offUsers), offUsers[0].Role}, [2]any{1, RoleViewer})
		st, err := s.GetSettings(ctx)
		check(t, err)
		st.OIDCAppRoles = true
		check(t, s.SaveSettings(ctx, st))
		equal(t, "role while on", role(u.ID), RoleAdmin)

		// The next sign-in replaces the app roles; none clears them.
		u, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s1", AppRoles: []string{"editor"}})
		check(t, err)
		equal(t, "replaced", u.AppRoles, []string{"editor"})
		equal(t, "a value in another case", role(u.ID), RoleViewer)
		u, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s1", AppRoles: []string{"Editor"}})
		check(t, err)
		equal(t, "editor", role(u.ID), RoleEditor)
		g := mustGroup(t, s, "admins", "")
		check(t, s.UpdateGroup(ctx, g.ID, "", RoleAdmin))
		check(t, s.SetUserGroups(ctx, u.ID, []GroupRef{{ExternalID: "admins"}}))
		equal(t, "group role wins", role(u.ID), RoleAdmin)
		check(t, s.SetUserGroups(ctx, u.ID, nil))
		u, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s1"})
		check(t, err)
		equal(t, "cleared", u.AppRoles, []string(nil))
		equal(t, "viewer", role(u.ID), RoleViewer)

		// A provisioned user keeps the name from provisioning, but the app
		// roles come from the sign-in.
		p, _, err := s.CreateSCIMUser(ctx, SCIMUserData{UserName: "p@example.com", Name: "Prov", Active: true})
		check(t, err)
		got, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-p", PrincipalName: "p@example.com", Name: "Other", AppRoles: []string{"Admin"}})
		check(t, err)
		equal(t, "provisioned user", [2]any{got.ID, got.Name}, [2]any{p.ID, "Prov"})
		equal(t, "app roles of a provisioned user", got.AppRoles, []string{"Admin"})

		users, err := s.ListUsers(ctx)
		check(t, err)
		for _, lu := range users {
			want := RoleViewer
			if lu.ID == p.ID {
				want = RoleAdmin
			}
			equal(t, "listed role of "+lu.Email, lu.Role, want)
		}
	})
}

// The local sign-in can be turned off when an active user signed in with the
// app role for administrators (with roles from app roles on), instead of a
// group with the Administrator role.
func TestHasSSOAdminWithAppRoles(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		st := DefaultSettings()
		st.OIDCEnabled, st.OIDCIssuer, st.OIDCClientID, st.OIDCClientSecret = true, "https://issuer", "c", "s"
		has := func(except int64) bool {
			t.Helper()
			ok, err := s.HasSSOAdmin(ctx, &st, except)
			check(t, err)
			return ok
		}
		boss, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-boss", AppRoles: []string{"Admin"}})
		check(t, err)
		_, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-ed", AppRoles: []string{"Editor"}})
		check(t, err)
		if has(0) {
			t.Error("an app role administrator counts while roles from app roles are off")
		}
		wantNoAdmin := func(what string) {
			t.Helper()
			if err := s.CheckLocalLoginOff(ctx, &st); err != ErrNoAdmin {
				t.Errorf("%s: CheckLocalLoginOff = %v, want ErrNoAdmin", what, err)
			}
		}
		wantNoAdmin("off")
		st.OIDCAppRoles = true
		if !has(0) {
			t.Error("the app role administrator does not count")
		}
		check(t, s.CheckLocalLoginOff(ctx, &st))
		n, err := s.AppRoleAdminCount(ctx, &st)
		check(t, err)
		equal(t, "app role administrators", n, 1)

		// Another value for administrators, or none: nobody.
		st.OIDCAdminRole = "StatusPage.Admin"
		wantNoAdmin("other value")
		st.OIDCAdminRole = ""
		wantNoAdmin("no value")
		st.OIDCAdminRole = "Admin"

		// Deactivated users do not count, nor those who last signed in
		// with it longer ago than AppRoleAdminWindow.
		mustExec(t, s, `UPDATE users SET active = ? WHERE id = ?`, false, boss.ID)
		wantNoAdmin("deactivated")
		mustExec(t, s, `UPDATE users SET active = ? WHERE id = ?`, true, boss.ID)
		mustExec(t, s, `UPDATE users SET last_login_at = ? WHERE id = ?`, time.Now().Add(-AppRoleAdminWindow-time.Hour).Unix(), boss.ID)
		wantNoAdmin("signed in too long ago")
		mustExec(t, s, `UPDATE users SET last_login_at = ? WHERE id = ?`, time.Now().Add(-AppRoleAdminWindow+time.Hour).Unix(), boss.ID)
		if !has(0) {
			t.Error("a sign-in within the window does not count")
		}

		// A group with the Administrator role counts, except the given one.
		check(t, s.SetUserGroups(ctx, boss.ID, nil))
		_, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-boss"})
		check(t, err)
		g := mustGroup(t, s, "admins", "")
		check(t, s.UpdateGroup(ctx, g.ID, "", RoleAdmin))
		equal(t, "with the group", has(0), true)
		equal(t, "without the group", has(g.ID), false)
	})
}

// Provisioning activates a user again: the app roles of the identity that
// signed in before go with it (the user name may belong to somebody else
// by now), so the row no longer counts as an administrator.
func TestSCIMReactivationClearsAppRoles(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		st := DefaultSettings()
		st.OIDCEnabled, st.OIDCIssuer, st.OIDCClientID, st.OIDCClientSecret, st.OIDCAppRoles = true, "https://issuer", "c", "s", true
		check(t, s.SaveSettings(ctx, &st))
		p := mustSCIMUser(t, s, SCIMUserData{UserName: "jane@example.com", Name: "Jane", Active: true})
		signedIn, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-jane", PrincipalName: "jane@example.com", AppRoles: []string{"Admin"}})
		check(t, err)
		equal(t, "signed in as the provisioned user", signedIn.ID, p.ID)
		n, err := s.AppRoleAdminCount(ctx, &st)
		check(t, err)
		equal(t, "app role administrators", n, 1)

		setActive := func(active bool) {
			t.Helper()
			_, err := s.UpdateSCIMUser(ctx, p.ID, func(cur *User) (SCIMUserData, error) {
				return SCIMUserData{UserName: cur.PrincipalName, Name: cur.Name, Email: cur.Email, Active: active}, nil
			})
			check(t, err)
		}
		setActive(false)
		setActive(true)
		got, err := s.GetUser(ctx, p.ID)
		check(t, err)
		equal(t, "after reactivation", [2]any{got.AppRoles, got.OIDCSubject}, [2]any{[]string(nil), ""})
		n, err = s.AppRoleAdminCount(ctx, &st)
		check(t, err)
		equal(t, "app role administrators after reactivation", n, 0)
		if err := s.CheckLocalLoginOff(ctx, &st); err != ErrNoAdmin {
			t.Errorf("CheckLocalLoginOff = %v, want ErrNoAdmin", err)
		}
		users, err := s.ListUsers(ctx)
		check(t, err)
		for _, u := range users {
			if u.ID == p.ID && u.Role != RoleViewer {
				t.Errorf("listed role after reactivation = %v", u.Role)
			}
		}
	})
}
