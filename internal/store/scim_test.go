package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests of the data access for SCIM provisioning (scim.go) and of how sign-in
// treats provisioned users and groups (users.go).

func mustSCIMUser(t *testing.T, s *Store, in SCIMUserData) *User {
	t.Helper()
	u, _, err := s.CreateSCIMUser(ctx, in)
	check(t, err)
	return u
}

func mustSCIMGroup(t *testing.T, s *Store, in SCIMGroupData) *Group {
	t.Helper()
	g, _, err := s.CreateSCIMGroup(ctx, in)
	check(t, err)
	return g
}

func wantConflict(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrSCIMConflict) {
		t.Fatalf("%s: want ErrSCIMConflict, got %v", what, err)
	}
}

// replaceUser returns a change function for UpdateSCIMUser that sets in.
func replaceUser(in SCIMUserData) func(*User) (SCIMUserData, error) {
	return func(*User) (SCIMUserData, error) { return in, nil }
}

func TestSCIMCreateUser(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		admin, err := s.CreateLocalAdmin(ctx, "admin", "Admin", "hash")
		check(t, err)

		u, adopted, err := s.CreateSCIMUser(ctx, SCIMUserData{UserName: " Jane.Doe@Contoso.com ", ExternalID: "jdoe",
			Email: "Jane.Doe@Contoso.com", Name: " Jane Doe ", Active: true})
		check(t, err)
		equal(t, "adopted", adopted, false)
		equal(t, "user", [5]string{u.PrincipalName, u.SCIMExternalID, u.Email, u.Name, u.OIDCSubject},
			[5]string{"Jane.Doe@Contoso.com", "jdoe", "jane.doe@contoso.com", "Jane Doe", ""})
		if !u.Active || !u.Provisioned || u.IsLocalAdmin || u.LastLoginAt != nil || !recent(u.CreatedAt) {
			t.Fatalf("flags: %+v", u)
		}

		// The same user name again (any case): conflict, nothing changes.
		_, _, err = s.CreateSCIMUser(ctx, SCIMUserData{UserName: "jane.doe@contoso.com", Email: "other@contoso.com", Active: true})
		wantConflict(t, "duplicate user name", err)
		_, _, err = s.CreateSCIMUser(ctx, SCIMUserData{UserName: " ", Active: true})
		wantError(t, "empty user name", err)

		// A user who signed in before is taken over: by user name …
		signedIn, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-max", PrincipalName: "Max@Contoso.com", Email: "max@old.example", Name: "Max"})
		check(t, err)
		session := &Session{ID: "sess-max", UserID: signedIn.ID, CreatedAt: nowTime(), ExpiresAt: nowTime().Add(time.Hour)}
		check(t, s.CreateSession(ctx, session))
		u, adopted, err = s.CreateSCIMUser(ctx, SCIMUserData{UserName: "max@contoso.com", Email: "max@contoso.com", Name: "Max Mustermann", Active: true})
		check(t, err)
		equal(t, "adopted by user name", [2]any{adopted, u.ID}, [2]any{true, signedIn.ID})
		equal(t, "provisioned values", [3]string{u.PrincipalName, u.Email, u.Name}, [3]string{"max@contoso.com", "max@contoso.com", "Max Mustermann"})
		equal(t, "sign-in kept", u.OIDCSubject, "s-max")
		_, err = s.GetSession(ctx, "sess-max", time.Now())
		check(t, err) // still signed in

		// … or by e-mail address.
		byMail, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-eve", Email: "Eve@contoso.com"})
		check(t, err)
		u, adopted, err = s.CreateSCIMUser(ctx, SCIMUserData{UserName: "eve.smith@contoso.com", Email: "eve@contoso.com", Active: true})
		check(t, err)
		equal(t, "adopted by e-mail", [2]any{adopted, u.ID}, [2]any{true, byMail.ID})

		// A provisioned user is not taken over by e-mail: a new user gets the
		// address, which the other user no longer has.
		u, adopted, err = s.CreateSCIMUser(ctx, SCIMUserData{UserName: "jane.new@contoso.com", Email: "jane.doe@contoso.com", Active: true})
		check(t, err)
		equal(t, "new user", adopted, false)
		equal(t, "address moved", u.Email, "jane.doe@contoso.com")
		jane, _, err := s.SCIMUsers(ctx, SCIMUserQuery{UserName: "Jane.Doe@contoso.com"}, 0, 10)
		check(t, err)
		equal(t, "old row lost the address", jane[0].Email, "")

		// The local administrator is never taken over.
		u, adopted, err = s.CreateSCIMUser(ctx, SCIMUserData{UserName: "admin", Active: true})
		check(t, err)
		if adopted || u.ID == admin.ID {
			t.Fatal("the local administrator was taken over")
		}
		got, err := s.GetUser(ctx, admin.ID)
		check(t, err)
		equal(t, "admin unchanged", *got, *admin)
	})
}

func TestSCIMUpdateUser(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		admin, err := s.CreateLocalAdmin(ctx, "admin", "Admin", "hash")
		check(t, err)
		a := mustSCIMUser(t, s, SCIMUserData{UserName: "a@contoso.com", Email: "a@contoso.com", Name: "A", Active: true})
		b := mustSCIMUser(t, s, SCIMUserData{UserName: "b@contoso.com", Email: "b@contoso.com", Active: true})
		signedIn, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-c", PrincipalName: "c@contoso.com", Email: "c@contoso.com"})
		check(t, err)

		u, err := s.UpdateSCIMUser(ctx, a.ID, func(cur *User) (SCIMUserData, error) {
			equal(t, "current user", cur.ID, a.ID)
			return SCIMUserData{UserName: "a2@contoso.com", ExternalID: "a2", Email: "A2@contoso.com", Name: "A Two", Active: true}, nil
		})
		check(t, err)
		equal(t, "updated", [4]string{u.PrincipalName, u.SCIMExternalID, u.Email, u.Name}, [4]string{"a2@contoso.com", "a2", "a2@contoso.com", "A Two"})

		// The change function's error is returned unchanged; nothing is stored.
		boom := errors.New("boom")
		_, err = s.UpdateSCIMUser(ctx, a.ID, func(*User) (SCIMUserData, error) { return SCIMUserData{}, boom })
		if err != boom {
			t.Fatalf("got %v, want the change function's error", err)
		}

		// Another provisioned user has the name: conflict. A user who only
		// signed in does not count.
		_, err = s.UpdateSCIMUser(ctx, a.ID, replaceUser(SCIMUserData{UserName: "B@contoso.com", Active: true}))
		wantConflict(t, "rename to a taken user name", err)
		_, err = s.UpdateSCIMUser(ctx, a.ID, replaceUser(SCIMUserData{UserName: "c@contoso.com", Active: true}))
		check(t, err)

		// Users who signed in can be changed (they become provisioned).
		u, err = s.UpdateSCIMUser(ctx, signedIn.ID, replaceUser(SCIMUserData{UserName: "carl@contoso.com", Email: "carl@contoso.com", Active: true}))
		check(t, err)
		equal(t, "provisioned", u.Provisioned, true)
		equal(t, "subject kept", u.OIDCSubject, "s-c")

		// Deactivation ends the sessions; reactivation works.
		check(t, s.CreateSession(ctx, &Session{ID: "sess-b", UserID: b.ID, CreatedAt: nowTime(), ExpiresAt: nowTime().Add(time.Hour)}))
		u, err = s.UpdateSCIMUser(ctx, b.ID, replaceUser(SCIMUserData{UserName: "b@contoso.com", Email: "b@contoso.com", Active: false}))
		check(t, err)
		equal(t, "deactivated", u.Active, false)
		_, err = s.GetSession(ctx, "sess-b", time.Now())
		wantNotFound(t, "session of the deactivated user", err)
		u, err = s.UpdateSCIMUser(ctx, b.ID, replaceUser(SCIMUserData{UserName: "b@contoso.com", Email: "b@contoso.com", Active: true}))
		check(t, err)
		equal(t, "reactivated", u.Active, true)

		_, err = s.UpdateSCIMUser(ctx, admin.ID, replaceUser(SCIMUserData{UserName: "x", Active: true}))
		wantNotFound(t, "local administrator", err)
		_, err = s.UpdateSCIMUser(ctx, 9999, replaceUser(SCIMUserData{UserName: "x", Active: true}))
		wantNotFound(t, "unknown user", err)
		_, err = s.SCIMUser(ctx, admin.ID)
		wantNotFound(t, "get local administrator", err)
		got, err := s.SCIMUser(ctx, b.ID)
		check(t, err)
		equal(t, "get user", got.PrincipalName, "b@contoso.com")
	})
}

func TestSCIMUserQueries(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		_, err := s.CreateLocalAdmin(ctx, "admin", "Admin", "hash")
		check(t, err)
		var ids []int64
		for i := 1; i <= 5; i++ {
			u := mustSCIMUser(t, s, SCIMUserData{UserName: fmt.Sprintf("User%d@Contoso.com", i), ExternalID: fmt.Sprint("ext-", i),
				Email: fmt.Sprintf("user%d@contoso.com", i), Active: true})
			ids = append(ids, u.ID)
		}
		query := func(q SCIMUserQuery, offset, limit int) ([]int64, int) {
			t.Helper()
			users, total, err := s.SCIMUsers(ctx, q, offset, limit)
			check(t, err)
			var got []int64
			for _, u := range users {
				got = append(got, u.ID)
			}
			return got, total
		}
		got, total := query(SCIMUserQuery{}, 0, 100)
		equal(t, "all users, not the local administrator", [2]any{got, total}, [2]any{ids, 5})
		got, total = query(SCIMUserQuery{}, 1, 2)
		equal(t, "page", [2]any{got, total}, [2]any{ids[1:3], 5})
		got, total = query(SCIMUserQuery{}, 5, 2)
		equal(t, "after the end", [2]any{len(got), total}, [2]any{0, 5})
		got, total = query(SCIMUserQuery{}, 0, 0)
		equal(t, "count only", [2]any{len(got), total}, [2]any{0, 5})

		got, _ = query(SCIMUserQuery{UserName: "user3@CONTOSO.com"}, 0, 10)
		equal(t, "user name, any case", got, ids[2:3])
		got, _ = query(SCIMUserQuery{ExternalID: "ext-4"}, 0, 10)
		equal(t, "external id", got, ids[3:4])
		got, _ = query(SCIMUserQuery{ExternalID: "EXT-4"}, 0, 10)
		equal(t, "external id is case-sensitive", len(got), 0)
		got, _ = query(SCIMUserQuery{Email: "USER5@contoso.com"}, 0, 10)
		equal(t, "e-mail", got, ids[4:5])
		got, _ = query(SCIMUserQuery{ID: ids[0], UserName: "user1@contoso.com", ExternalID: "ext-1"}, 0, 10)
		equal(t, "all conditions", got, ids[0:1])
		got, _ = query(SCIMUserQuery{ID: ids[0], UserName: "user2@contoso.com"}, 0, 10)
		equal(t, "conditions that contradict each other", len(got), 0)
		got, _ = query(SCIMUserQuery{UserName: "admin"}, 0, 10)
		equal(t, "local administrator", len(got), 0)
	})
}

func TestSCIMGroupsCreateUpdateDelete(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		admin, err := s.CreateLocalAdmin(ctx, "admin", "Admin", "hash")
		check(t, err)
		u1 := mustSCIMUser(t, s, SCIMUserData{UserName: "u1@contoso.com", Email: "u1@contoso.com", Active: true})
		u2 := mustSCIMUser(t, s, SCIMUserData{UserName: "u2@contoso.com", Email: "u2@contoso.com", Active: true})
		u3, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-3", Email: "u3@contoso.com"})
		check(t, err)
		members := func(g *Group) []int64 {
			t.Helper()
			m, err := s.SCIMGroupMembers(ctx, []int64{g.ID})
			check(t, err)
			var ids []int64
			for _, x := range m[g.ID] {
				ids = append(ids, x.UserID)
			}
			return ids
		}

		// Created with members; unknown users and the local administrator
		// are skipped.
		const objectID = "AAAAAAAA-0000-4000-8000-000000000001"
		g, adopted, err := s.CreateSCIMGroup(ctx, SCIMGroupData{DisplayName: " Status Editors ", ExternalID: objectID,
			Members: []int64{u2.ID, u1.ID, 9999, admin.ID, u1.ID}})
		check(t, err)
		equal(t, "adopted", adopted, false)
		equal(t, "group", [2]string{g.Name, g.ExternalID}, [2]string{"Status Editors", strings.ToLower(objectID)})
		if !g.Provisioned || g.LastSyncedAt == nil || !recent(*g.LastSyncedAt) || g.MemberCount != 2 {
			t.Fatalf("group: %+v", g)
		}
		equal(t, "members", members(g), []int64{u1.ID, u2.ID})

		_, _, err = s.CreateSCIMGroup(ctx, SCIMGroupData{DisplayName: "status editors", ExternalID: "other"})
		wantConflict(t, "same display name", err)
		_, _, err = s.CreateSCIMGroup(ctx, SCIMGroupData{DisplayName: " "})
		wantError(t, "empty display name", err)

		// Without an external id the group gets a generated one.
		noExt := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "No External ID"})
		if !strings.HasPrefix(noExt.ExternalID, SCIMGeneratedIDPrefix) || len(noExt.ExternalID) < 10 {
			t.Fatalf("generated external id %q", noExt.ExternalID)
		}

		// A group recorded from sign-ins is taken over by its external id: it
		// keeps role and services, its members are replaced.
		const claimID = "bbbbbbbb-0000-4000-8000-000000000002"
		check(t, s.SetUserGroups(ctx, u3.ID, []GroupRef{{ExternalID: claimID}}))
		old := mustGroup(t, s, claimID, "")
		check(t, s.UpdateGroup(ctx, old.ID, "", RoleAdmin))
		svc := mustService(t, s, "VPN", "", 0, old.ID)
		g2, adopted, err := s.CreateSCIMGroup(ctx, SCIMGroupData{DisplayName: "Status Admins", ExternalID: strings.ToUpper(claimID),
			Members: []int64{u1.ID}})
		check(t, err)
		equal(t, "adopted", [2]any{adopted, g2.ID}, [2]any{true, old.ID})
		equal(t, "role kept", g2.Role, RoleAdmin)
		equal(t, "name", g2.Name, "Status Admins")
		equal(t, "members replaced", members(g2), []int64{u1.ID})
		gs, err := s.GetGroup(ctx, g2.ID)
		check(t, err)
		equal(t, "services kept", gs.ServiceIDs, []int64{svc.ID})

		// Update: name, external id and members.
		up, err := s.UpdateSCIMGroup(ctx, g.ID, func(d *SCIMGroupData) error {
			equal(t, "current", [2]any{d.DisplayName, d.Members}, [2]any{"Status Editors", []int64{u1.ID, u2.ID}})
			d.DisplayName = "Editors"
			d.ExternalID = "cccccccc-0000-4000-8000-000000000003"
			d.Members = []int64{u2.ID, u3.ID}
			return nil
		})
		check(t, err)
		equal(t, "updated", [2]string{up.Name, up.ExternalID}, [2]string{"Editors", "cccccccc-0000-4000-8000-000000000003"})
		equal(t, "members", members(up), []int64{u2.ID, u3.ID})
		up, err = s.UpdateSCIMGroup(ctx, g.ID, func(d *SCIMGroupData) error { d.ExternalID = ""; return nil })
		check(t, err)
		equal(t, "empty external id keeps it", up.ExternalID, "cccccccc-0000-4000-8000-000000000003")

		_, err = s.UpdateSCIMGroup(ctx, g.ID, func(d *SCIMGroupData) error { d.DisplayName = "STATUS ADMINS"; return nil })
		wantConflict(t, "rename to a taken name", err)
		_, err = s.UpdateSCIMGroup(ctx, g.ID, func(d *SCIMGroupData) error { d.ExternalID = claimID; return nil })
		wantConflict(t, "external id of another group", err)
		boom := errors.New("boom")
		if _, err = s.UpdateSCIMGroup(ctx, g.ID, func(*SCIMGroupData) error { return boom }); err != boom {
			t.Fatalf("got %v, want the change function's error", err)
		}

		// Only provisioned groups are visible.
		other := mustGroup(t, s, "not-provisioned", "Other")
		_, err = s.SCIMGroup(ctx, other.ID)
		wantNotFound(t, "group that is not provisioned", err)
		_, err = s.UpdateSCIMGroup(ctx, other.ID, func(*SCIMGroupData) error { return nil })
		wantNotFound(t, "update a group that is not provisioned", err)
		list, total, err := s.SCIMGroups(ctx, SCIMGroupQuery{}, 0, 100)
		check(t, err)
		equal(t, "provisioned groups", [2]int{len(list), total}, [2]int{3, 3})
		list, _, err = s.SCIMGroups(ctx, SCIMGroupQuery{DisplayName: "editors"}, 0, 100)
		check(t, err)
		equal(t, "by display name", list[0].ID, g.ID)
		list, _, err = s.SCIMGroups(ctx, SCIMGroupQuery{ExternalID: strings.ToUpper(claimID)}, 0, 100)
		check(t, err)
		equal(t, "by external id (GUID in any case)", list[0].ID, g2.ID)
		list, total, err = s.SCIMGroups(ctx, SCIMGroupQuery{}, 1, 1)
		check(t, err)
		equal(t, "page", [2]any{list[0].ID, total}, [2]any{noExt.ID, 3})

		// Delete: the members go, the group stays with role and services
		// but is no longer provisioned.
		check(t, s.DeleteSCIMGroup(ctx, g2.ID))
		gs, err = s.GetGroup(ctx, g2.ID)
		check(t, err)
		if gs.Provisioned || gs.MemberCount != 0 || gs.Role != RoleAdmin || len(gs.ServiceIDs) != 1 {
			t.Fatalf("group after delete: %+v", gs)
		}
		wantNotFound(t, "delete again", s.DeleteSCIMGroup(ctx, g2.ID))
		wantNotFound(t, "delete unknown", s.DeleteSCIMGroup(ctx, 9999))

		// Provisioning it again restores it.
		g3, adopted, err := s.CreateSCIMGroup(ctx, SCIMGroupData{DisplayName: "Status Admins", ExternalID: claimID, Members: []int64{u2.ID}})
		check(t, err)
		equal(t, "restored", [3]any{adopted, g3.ID, g3.Role}, [3]any{true, g2.ID, RoleAdmin})
	})
}

func TestSCIMTokenAndTurnOff(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		created := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
		check(t, s.SetSCIMToken(ctx, "abc123", created))
		st, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "token", [2]any{st.SCIMTokenHash, st.SCIMTokenCreatedAt}, [2]any{"abc123", created})
		equal(t, "enabled", st.SCIMEnabled(), true)

		// Saving the other settings keeps the token.
		st.SiteName = "Changed"
		check(t, s.SaveSettings(ctx, st))
		st, err = s.GetSettings(ctx)
		check(t, err)
		equal(t, "token kept", st.SCIMTokenHash, "abc123")

		u := mustSCIMUser(t, s, SCIMUserData{UserName: "u@contoso.com", Name: "Provisioned Name", Active: true})
		g := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "G", ExternalID: "g-1", Members: []int64{u.ID}})

		// Turning provisioning off hands the groups and users back to sign-in.
		check(t, s.SetSCIMToken(ctx, "", created))
		st, err = s.GetSettings(ctx)
		check(t, err)
		equal(t, "token removed", [3]any{st.SCIMTokenHash, st.SCIMTokenCreatedAt.IsZero(), st.SCIMEnabled()}, [3]any{"", true, false})
		gs, err := s.GetGroup(ctx, g.ID)
		check(t, err)
		equal(t, "group no longer provisioned, members kept", [2]any{gs.Provisioned, gs.MemberCount}, [2]any{false, 1})
		got, err := s.GetUser(ctx, u.ID)
		check(t, err)
		equal(t, "user no longer provisioned", got.Provisioned, false)
		signedIn, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-u", PrincipalName: "u@contoso.com", Name: "Name From Sign-in"})
		check(t, err)
		equal(t, "sign-in updates the name again", [2]any{signedIn.ID, signedIn.Name}, [2]any{u.ID, "Name From Sign-in"})
		check(t, s.SetUserGroups(ctx, u.ID, nil))
		gs, err = s.GetGroup(ctx, g.ID)
		check(t, err)
		equal(t, "sign-in manages the members again", gs.MemberCount, 0)
	})
}

// TestSCIMRecipientsSkipDeactivated: users deactivated by provisioning get
// no e-mails, but keep their groups for when they are activated again.
func TestSCIMRecipientsSkipDeactivated(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		a := mustSCIMUser(t, s, SCIMUserData{UserName: "a@contoso.com", Email: "a@contoso.com", Active: true})
		b := mustSCIMUser(t, s, SCIMUserData{UserName: "b@contoso.com", Email: "b@contoso.com", Active: true})
		g := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "All", ExternalID: "all", Members: []int64{a.ID, b.ID}})
		svc := mustService(t, s, "Mail", "", 0, g.ID)
		recipients := func() []string {
			t.Helper()
			r, err := s.RecipientsForServices(ctx, []int64{svc.ID})
			check(t, err)
			var out []string
			for _, x := range r {
				out = append(out, x.Email)
			}
			return out
		}
		equal(t, "recipients", recipients(), []string{"a@contoso.com", "b@contoso.com"})
		_, err := s.UpdateSCIMUser(ctx, b.ID, replaceUser(SCIMUserData{UserName: "b@contoso.com", Email: "b@contoso.com", Active: false}))
		check(t, err)
		equal(t, "without the deactivated user", recipients(), []string{"a@contoso.com"})
		gs, err := s.GetGroup(ctx, g.ID)
		check(t, err)
		equal(t, "still a member", gs.MemberCount, 2)
		_, err = s.UpdateSCIMUser(ctx, b.ID, replaceUser(SCIMUserData{UserName: "b@contoso.com", Email: "b@contoso.com", Active: true}))
		check(t, err)
		equal(t, "activated again", recipients(), []string{"a@contoso.com", "b@contoso.com"})
	})
}

// TestSignInProvisionedUser: sign-in finds a provisioned user by the user
// name, keeps the provisioned name, e-mail and user name, and refuses a
// deactivated user.
func TestSignInProvisionedUser(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		p := mustSCIMUser(t, s, SCIMUserData{UserName: "Jane.Doe@contoso.com", Email: "jane@contoso.com", Name: "Jane Doe", Active: true})

		u, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-jane", DirectoryID: "oid-jane", PrincipalName: "jane.doe@CONTOSO.com",
			Email: "jane.other@contoso.com", Name: "J. Doe"})
		check(t, err)
		equal(t, "same user", u.ID, p.ID)
		equal(t, "sign-in recorded", [2]string{u.OIDCSubject, u.DirectoryID}, [2]string{"sub-jane", "oid-jane"})
		if u.LastLoginAt == nil {
			t.Fatal("last sign-in not recorded")
		}
		equal(t, "provisioned values kept", [3]string{u.PrincipalName, u.Email, u.Name}, [3]string{"Jane.Doe@contoso.com", "jane@contoso.com", "Jane Doe"})
		u, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-jane", PrincipalName: "renamed@contoso.com"})
		check(t, err)
		equal(t, "matched by subject", [2]any{u.ID, u.PrincipalName}, [2]any{p.ID, "Jane.Doe@contoso.com"})

		// Users who are not provisioned get the user name from the sign-in.
		o, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-otto", PrincipalName: "otto@contoso.com", Email: "otto@contoso.com"})
		check(t, err)
		equal(t, "user name stored", o.PrincipalName, "otto@contoso.com")
		o, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-otto", PrincipalName: "otto.new@contoso.com"})
		check(t, err)
		equal(t, "user name updated", o.PrincipalName, "otto.new@contoso.com")
		o, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-otto"})
		check(t, err)
		equal(t, "user name kept", o.PrincipalName, "otto.new@contoso.com")

		// The user name of another directory object is not matched.
		other, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-x", DirectoryID: "oid-x", PrincipalName: "jane.doe@contoso.com"})
		check(t, err)
		if other.ID == p.ID {
			t.Fatal("another directory object took over the user by its user name")
		}

		// A sign-in never takes the e-mail address of a provisioned user.
		thief, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-t", DirectoryID: "oid-t", Email: "JANE@contoso.com"})
		check(t, err)
		equal(t, "address not taken", thief.Email, "")
		got, err := s.GetUser(ctx, p.ID)
		check(t, err)
		equal(t, "provisioned address kept", got.Email, "jane@contoso.com")

		// Deactivated: no sign-in, nothing changes.
		_, err = s.UpdateSCIMUser(ctx, p.ID, func(cur *User) (SCIMUserData, error) {
			return SCIMUserData{UserName: cur.PrincipalName, Email: cur.Email, Name: cur.Name, Active: false}, nil
		})
		check(t, err)
		before, err := s.GetUser(ctx, p.ID)
		check(t, err)
		_, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-jane-2", PrincipalName: "jane.doe@contoso.com"})
		if !errors.Is(err, ErrUserDeactivated) {
			t.Fatalf("sign-in of a deactivated user: got %v, want ErrUserDeactivated", err)
		}
		_, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-jane"})
		if !errors.Is(err, ErrUserDeactivated) {
			t.Fatalf("sign-in of a deactivated user by subject: got %v", err)
		}
		after, err := s.GetUser(ctx, p.ID)
		check(t, err)
		equal(t, "unchanged", *after, *before)
	})
}

// TestSignInLeavesProvisionedGroups: the groups claim decides about the
// memberships of other groups only; provisioning alone adds and removes
// members of provisioned groups and names them.
func TestSignInLeavesProvisionedGroups(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		alice, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-alice", PrincipalName: "alice@contoso.com"})
		check(t, err)
		const staffID, adminsID = "aaaaaaaa-0000-4000-8000-000000000001", "bbbbbbbb-0000-4000-8000-000000000002"
		staff := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "All Staff", ExternalID: staffID, Members: []int64{alice.ID}})
		admins := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "Status Admins", ExternalID: adminsID})
		check(t, s.UpdateGroup(ctx, admins.ID, "Status Admins", RoleAdmin))
		groups := func() []string {
			t.Helper()
			gs, err := s.UserGroups(ctx, alice.ID)
			check(t, err)
			var out []string
			for _, g := range gs {
				out = append(out, g.Name)
			}
			return out
		}
		equal(t, "after provisioning", groups(), []string{"All Staff"})

		// Without the provisioned group in the claim it is kept; a
		// provisioned group in the claim is not added; a claim name does not
		// rename a provisioned group.
		check(t, s.SetUserGroups(ctx, alice.ID, []GroupRef{{ExternalID: "security-group"}, {ExternalID: strings.ToUpper(adminsID), Name: "Renamed"}}))
		equal(t, "after sign-in", groups(), []string{"All Staff", "security-group"})
		role, err := s.UserRole(ctx, alice.ID)
		check(t, err)
		equal(t, "no admin role from the claim", role, RoleViewer)
		g, err := s.GetGroup(ctx, admins.ID)
		check(t, err)
		equal(t, "name kept", g.Name, "Status Admins")
		check(t, s.SetUserGroups(ctx, alice.ID, nil))
		equal(t, "sign-in without groups", groups(), []string{"All Staff"})

		// Provisioning removes the membership.
		_, err = s.UpdateSCIMGroup(ctx, staff.ID, func(d *SCIMGroupData) error { d.Members = nil; return nil })
		check(t, err)
		equal(t, "after provisioning removed it", len(groups()), 0)
	})
}

// TestSCIMConcurrentWithSignIns provisions users and group members while the
// same people sign in for the first time. Every person must end up as one
// user (PostgreSQL: the lookups and inserts of concurrent transactions must
// not create duplicates or fail with duplicate keys).
func TestSCIMConcurrentWithSignIns(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		const n = 12
		g := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "Everybody", ExternalID: "everybody"})
		var wg sync.WaitGroup
		errs := make(chan error, 3*n)
		for i := 0; i < n; i++ {
			upn := fmt.Sprintf("user%d@contoso.com", i)
			wg.Add(3)
			go func() {
				defer wg.Done()
				u, _, err := s.CreateSCIMUser(ctx, SCIMUserData{UserName: upn, Email: upn, Active: true})
				if errors.Is(err, ErrSCIMConflict) {
					err = nil
				} else if err == nil {
					_, err = s.UpdateSCIMGroup(ctx, g.ID, func(d *SCIMGroupData) error { d.Members = append(d.Members, u.ID); return nil })
				}
				errs <- err
			}()
			go func() {
				defer wg.Done()
				u, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-" + upn, PrincipalName: upn, Email: upn})
				if err == nil {
					err = s.SetUserGroups(ctx, u.ID, []GroupRef{{ExternalID: "claim-group"}, {ExternalID: "everybody"}})
				}
				errs <- err
			}()
			go func() {
				defer wg.Done()
				_, _, err := s.SCIMUsers(ctx, SCIMUserQuery{UserName: upn}, 0, 10)
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			check(t, err)
		}
		users, total, err := s.SCIMUsers(ctx, SCIMUserQuery{}, 0, 100)
		check(t, err)
		equal(t, "one user per person", total, n)
		for _, u := range users {
			if !u.Provisioned || u.OIDCSubject == "" || u.Email != strings.ToLower(u.PrincipalName) {
				t.Errorf("user %+v", u)
			}
		}
		gs, err := s.GetGroup(ctx, g.ID)
		check(t, err)
		equal(t, "provisioned members", gs.MemberCount, n)
	})
}

// TestSCIMQueriesUseIndexes: on SQLite the lookups of provisioning and
// sign-in must use the indexes (a provisioning cycle looks up every user;
// table scans hold the write lock long with many users).
func TestSCIMQueriesUseIndexes(t *testing.T) {
	s := openSQLite(t)
	for _, q := range []string{
		`SELECT ` + userCols + ` FROM users u WHERE LOWER(u.principal_name) = LOWER(?) AND u.provisioned = ? AND u.is_local_admin = ? ORDER BY u.id LIMIT 1`,
		`SELECT ` + userCols + ` FROM users u WHERE LOWER(u.principal_name) = LOWER(?) AND u.principal_name <> '' AND u.directory_id IS NULL AND u.is_local_admin = ? ORDER BY u.id LIMIT 1`,
		`SELECT ` + userCols + ` FROM users u WHERE u.email = ? AND u.email <> '' AND u.provisioned = ? AND u.is_local_admin = ? ORDER BY u.id LIMIT 1`,
		`SELECT COUNT(*) FROM users u WHERE u.is_local_admin = ? AND u.scim_external_id = ?`,
		`SELECT COUNT(*) FROM users WHERE email = ? AND email <> '' AND provisioned = ? AND id <> ?`,
		`UPDATE users SET email = '' WHERE email = ? AND email <> '' AND id <> ?`,
	} {
		rows, err := s.DB().QueryContext(ctx, `EXPLAIN QUERY PLAN `+q, "x", true, false, 1)
		check(t, err)
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			check(t, rows.Scan(&id, &parent, &unused, &detail))
			plan = append(plan, detail)
		}
		check(t, rows.Err())
		rows.Close()
		text := strings.Join(plan, "; ")
		if !strings.Contains(text, "USING INDEX") || strings.Contains(text, "SCAN u") || strings.Contains(text, "SCAN users") {
			t.Errorf("query does not use an index (%s):\n%s", text, q)
		}
	}
}
