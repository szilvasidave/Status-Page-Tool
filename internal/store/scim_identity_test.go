package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Tests of who gets a role: the external id of a group that provisioning
// took over, users whose user name was reused, and users activated again.

// TestSCIMKeepsRealExternalID: provisioning cannot move a group's role to
// another identity provider group by changing its external id; only a
// generated external id is replaced.
func TestSCIMKeepsRealExternalID(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		const adminsID, allStaffID = "11111111-0000-4000-8000-000000000001", "eeeeeeee-0000-4000-8000-0000000000ee"
		admins := mustGroup(t, s, adminsID, "Admins")
		check(t, s.UpdateGroup(ctx, admins.ID, "Admins", RoleAdmin))
		_, adopted, err := s.CreateSCIMGroup(ctx, SCIMGroupData{DisplayName: "Admins", ExternalID: adminsID})
		check(t, err)
		equal(t, "adopted", adopted, true)

		g, err := s.UpdateSCIMGroup(ctx, admins.ID, func(d *SCIMGroupData) error {
			d.ExternalID = strings.ToUpper(allStaffID)
			return nil
		})
		check(t, err)
		equal(t, "external id kept", g.ExternalID, adminsID)
		check(t, s.DeleteSCIMGroup(ctx, admins.ID))
		check(t, s.SetSCIMToken(ctx, "", time.Time{}))

		// The role stays with the real group of the identity provider.
		employee, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-eve", Email: "eve@contoso.com"})
		check(t, err)
		check(t, s.SetUserGroups(ctx, employee.ID, []GroupRef{{ExternalID: allStaffID}}))
		admin, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-ann", Email: "ann@contoso.com"})
		check(t, err)
		check(t, s.SetUserGroups(ctx, admin.ID, []GroupRef{{ExternalID: adminsID}}))
		for _, c := range []struct {
			who  string
			id   int64
			want Role
		}{{"employee", employee.ID, RoleViewer}, {"administrator", admin.ID, RoleAdmin}} {
			role, err := s.UserRole(ctx, c.id)
			check(t, err)
			equal(t, "role of the "+c.who, role, c.want)
		}

		// A group created without an external id gets the first real one
		// sent; that one is kept.
		noExt := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "No External ID"})
		g, err = s.UpdateSCIMGroup(ctx, noExt.ID, func(d *SCIMGroupData) error { d.ExternalID = "AAAAAAAA-0000-4000-8000-0000000000AA"; return nil })
		check(t, err)
		equal(t, "generated id replaced", g.ExternalID, "aaaaaaaa-0000-4000-8000-0000000000aa")
		g, err = s.UpdateSCIMGroup(ctx, noExt.ID, func(d *SCIMGroupData) error { d.ExternalID = "bbbbbbbb-0000-4000-8000-0000000000bb"; return nil })
		check(t, err)
		equal(t, "real id kept", g.ExternalID, "aaaaaaaa-0000-4000-8000-0000000000aa")
	})
}

// TestSCIMReactivationUnlinksSignIn: Microsoft Entra ID finds a deactivated
// user by the user name and activates the row again, also when the user
// name now belongs to another person. The sign-in identity of the previous
// person is unlinked: the new person gets the provisioned user at the first
// sign-in, the previous one cannot use it any more.
func TestSCIMReactivationUnlinksSignIn(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		john, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-john", DirectoryID: "oid-john", PrincipalName: "j.smith@contoso.com",
			Email: "j.smith@contoso.com", Name: "John Smith"})
		check(t, err)
		u, adopted, err := s.CreateSCIMUser(ctx, SCIMUserData{UserName: "j.smith@contoso.com", Email: "j.smith@contoso.com", Name: "John Smith", Active: true})
		check(t, err)
		equal(t, "adopted", [2]any{adopted, u.ID}, [2]any{true, john.ID})
		editors := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "Editors", ExternalID: "editors", Members: []int64{u.ID}})
		check(t, s.UpdateGroup(ctx, editors.ID, "Editors", RoleEditor))

		// John leaves the group: removed and deactivated.
		_, err = s.UpdateSCIMGroup(ctx, editors.ID, func(d *SCIMGroupData) error { d.Members = nil; return nil })
		check(t, err)
		_, err = s.UpdateSCIMUser(ctx, u.ID, replaceUser(SCIMUserData{UserName: "j.smith@contoso.com", Email: "j.smith@contoso.com", Name: "John Smith"}))
		check(t, err)
		_, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-john", DirectoryID: "oid-john", PrincipalName: "j.smith@contoso.com"})
		if !errors.Is(err, ErrUserDeactivated) {
			t.Fatalf("deactivated John signs in: %v", err)
		}

		// The user name goes to Jane: activated again for her, added to the
		// group again.
		check(t, s.CreateSession(ctx, &Session{ID: "sess-old", UserID: u.ID, CreatedAt: nowTime(), ExpiresAt: nowTime().Add(time.Hour)}))
		u, err = s.UpdateSCIMUser(ctx, u.ID, replaceUser(SCIMUserData{UserName: "j.smith@contoso.com", Email: "j.smith@contoso.com", Name: "Jane Smith", Active: true}))
		check(t, err)
		equal(t, "sign-in identity unlinked", [2]string{u.OIDCSubject, u.DirectoryID}, [2]string{"", ""})
		_, err = s.GetSession(ctx, "sess-old", time.Now())
		wantNotFound(t, "session of the previous person", err)
		_, err = s.UpdateSCIMGroup(ctx, editors.ID, func(d *SCIMGroupData) error { d.Members = []int64{u.ID}; return nil })
		check(t, err)

		jane, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-jane", DirectoryID: "oid-jane", PrincipalName: "J.Smith@contoso.com",
			Email: "j.smith@contoso.com", Name: "Jane"})
		check(t, err)
		equal(t, "Jane gets the provisioned user", [4]any{jane.ID, jane.Email, jane.Name, jane.OIDCSubject},
			[4]any{u.ID, "j.smith@contoso.com", "Jane Smith", "sub-jane"})
		role, err := s.UserRole(ctx, jane.ID)
		check(t, err)
		equal(t, "Jane's role", role, RoleEditor)

		// John still exists under another user name: a user of his own.
		john, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-john", DirectoryID: "oid-john", PrincipalName: "john.smith@contoso.com",
			Email: "john.smith@contoso.com", Name: "John Smith"})
		check(t, err)
		if john.ID == u.ID {
			t.Fatal("John signed in as Jane's user")
		}
		role, err = s.UserRole(ctx, john.ID)
		check(t, err)
		equal(t, "John's role", role, RoleViewer)

		// The same person activated again is linked again by the user name.
		_, err = s.UpdateSCIMUser(ctx, u.ID, replaceUser(SCIMUserData{UserName: "j.smith@contoso.com", Email: "j.smith@contoso.com", Name: "Jane Smith"}))
		check(t, err)
		_, err = s.UpdateSCIMUser(ctx, u.ID, replaceUser(SCIMUserData{UserName: "j.smith@contoso.com", Email: "j.smith@contoso.com", Name: "Jane Smith", Active: true}))
		check(t, err)
		again, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-jane", DirectoryID: "oid-jane", PrincipalName: "j.smith@contoso.com"})
		check(t, err)
		equal(t, "linked again", [2]any{again.ID, again.DirectoryID}, [2]any{u.ID, "oid-jane"})

		// Changes of an active user keep the sign-in identity.
		u, err = s.UpdateSCIMUser(ctx, u.ID, replaceUser(SCIMUserData{UserName: "j.smith@contoso.com", Name: "Jane S.", Active: true}))
		check(t, err)
		equal(t, "sign-in identity kept", [2]string{u.OIDCSubject, u.DirectoryID}, [2]string{"sub-jane", "oid-jane"})
	})
}

// TestSignInReusedUserName: a user name (preferred_username) alone does not
// take over a user that another sign-in identity uses, unless provisioning
// manages that user.
func TestSignInReusedUserName(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		// A provider without oid claims (e.g. Keycloak).
		john, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "kc-john", PrincipalName: "jsmith", Email: "john.smith@corp.example", Name: "John Smith"})
		check(t, err)
		check(t, s.CreateSession(ctx, &Session{ID: "sess-john", UserID: john.ID, CreatedAt: nowTime(), ExpiresAt: nowTime().Add(time.Hour)}))

		jane, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "kc-jane", PrincipalName: "JSmith", Name: "Jane Smith"})
		check(t, err)
		if jane.ID == john.ID {
			t.Fatal("Jane took over John's user by the user name")
		}
		equal(t, "Jane's user", [2]string{jane.Email, jane.Name}, [2]string{"", "Jane Smith"})
		got, err := s.GetUser(ctx, john.ID)
		check(t, err)
		equal(t, "John's user unchanged", [3]string{got.OIDCSubject, got.Name, got.Email}, [3]string{"kc-john", "John Smith", "john.smith@corp.example"})
		_, err = s.GetSession(ctx, "sess-john", time.Now())
		check(t, err)

		// A provisioned user is found by the user name even with another
		// subject (provisioning is authoritative for the user name).
		p := mustSCIMUser(t, s, SCIMUserData{UserName: "max@contoso.com", Email: "max@contoso.com", Active: true})
		_, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-max-1", PrincipalName: "max@contoso.com"})
		check(t, err)
		u, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-max-2", PrincipalName: "Max@contoso.com"})
		check(t, err)
		equal(t, "provisioned user", [2]any{u.ID, u.OIDCSubject}, [2]any{p.ID, "sub-max-2"})
	})
}

// TestSCIMTakeoverReusedUserName: provisioning takes over a row that another
// person's sign-in created under a user name that now belongs to the
// provisioned person. The earlier sign-in identity loses the row: its
// session, its app roles and the link; it gets a row of its own at its next
// sign-in, and the provisioned person gets the provisioned row.
func TestSCIMTakeoverReusedUserName(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		a, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-a", DirectoryID: "oid-a", PrincipalName: "j.smith@contoso.com",
			Email: "j.smith@contoso.com", AppRoles: []string{"Admin"}})
		check(t, err)
		check(t, s.CreateSession(ctx, &Session{ID: "sess-a", UserID: a.ID, CreatedAt: nowTime(), ExpiresAt: nowTime().Add(time.Hour)}))
		// A's sign-in recorded a group with the Administrator role.
		admins := mustGroup(t, s, "it-admins", "")
		check(t, s.UpdateGroup(ctx, admins.ID, "", RoleAdmin))
		check(t, s.SetUserGroups(ctx, a.ID, []GroupRef{{ExternalID: "it-admins"}}))

		// The user name now belongs to B, whom Entra ID provisions.
		b, adopted, err := s.CreateSCIMUser(ctx, SCIMUserData{UserName: "j.smith@contoso.com", Email: "j.smith@contoso.com", Name: "B Smith", Active: true})
		check(t, err)
		equal(t, "taken over", [2]any{adopted, b.ID}, [2]any{true, a.ID})
		equal(t, "unlinked", [3]any{b.OIDCSubject, b.DirectoryID, b.AppRoles}, [3]any{"", "", []string(nil)})
		_, err = s.GetSession(ctx, "sess-a", time.Now())
		wantNotFound(t, "A's session", err)
		// … and A's memberships: B gets neither A's groups nor their role
		// (also not at a sign-in that leaves the memberships unchanged,
		// e.g. with Entra group overage).
		groups, err := s.UserGroups(ctx, b.ID)
		check(t, err)
		equal(t, "A's groups after the takeover", len(groups), 0)
		role, err := s.UserRole(ctx, b.ID)
		check(t, err)
		equal(t, "role after the takeover", role, RoleViewer)
		staff := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "Staff", ExternalID: "staff", Members: []int64{b.ID}})
		check(t, s.UpdateGroup(ctx, staff.ID, "Staff", RoleEditor))
		// Memberships of provisioned groups stay with provisioning, also
		// when the user is deactivated and activated again.
		for _, active := range []bool{false, true} {
			_, err := s.UpdateSCIMUser(ctx, b.ID, func(cur *User) (SCIMUserData, error) {
				return SCIMUserData{UserName: cur.PrincipalName, Email: cur.Email, Name: cur.Name, Active: active}, nil
			})
			check(t, err)
		}
		groups, err = s.UserGroups(ctx, b.ID)
		check(t, err)
		equal(t, "provisioned memberships after reactivation", len(groups), 1)

		// A signs in under the new user name: a row of its own, no roles of B.
		a2, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-a", DirectoryID: "oid-a", PrincipalName: "john.smith@contoso.com", Email: "john.smith@contoso.com"})
		check(t, err)
		if a2.ID == b.ID {
			t.Fatal("the earlier identity got the provisioned row")
		}
		role, err = s.UserRole(ctx, a2.ID)
		check(t, err)
		equal(t, "A's role", role, RoleViewer)

		// B signs in: the provisioned row, with its group's role.
		b2, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-b", DirectoryID: "oid-b", PrincipalName: "J.Smith@contoso.com", Email: "j.smith@contoso.com"})
		check(t, err)
		equal(t, "B gets the provisioned row", [2]any{b2.ID, b2.Name}, [2]any{b.ID, "B Smith"})
		role, err = s.UserRole(ctx, b2.ID)
		check(t, err)
		equal(t, "B's role", role, RoleEditor)
	})
}
