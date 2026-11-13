package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestGroupExternalIDCase: Entra ID object ids are matched case-insensitively
// (the claim has them in lower case, an admin may paste them in upper case);
// other ids keep their case.
func TestGroupExternalIDCase(t *testing.T) {
	const lower = "8f3a1c2d-0000-4000-8000-00000000abcd"
	upper := strings.ToUpper(lower)
	equal(t, "IsGUID", []bool{IsGUID(lower), IsGUID(upper), IsGUID("it-admins"), IsGUID(lower[1:] + "x"), IsGUID(strings.ReplaceAll(lower, "-", "_"))},
		[]bool{true, true, false, false, false})
	equal(t, "NormExternalID", []string{NormExternalID(" " + upper + " "), NormExternalID(" IT-Admins ")}, []string{lower, "IT-Admins"})

	forEachDB(t, func(t *testing.T, s *Store) {
		g := mustGroup(t, s, " "+upper+" ", "")
		equal(t, "stored in lower case", g.ExternalID, lower)
		check(t, s.UpdateGroup(ctx, g.ID, "Editors", RoleEditor))
		equal(t, "added again in upper case", mustGroup(t, s, upper, "").ID, g.ID)

		u := mustUser(t, s, "sub", "u@example.com", "U")
		check(t, s.SetUserGroups(ctx, u.ID, []GroupRef{{ExternalID: lower}, {ExternalID: upper}}))
		groups, err := s.UserGroups(ctx, u.ID)
		check(t, err)
		equal(t, "one group", len(groups), 1)
		equal(t, "the added group", groups[0].ID, g.ID)
		role, err := s.UserRole(ctx, u.ID)
		check(t, err)
		equal(t, "role of the added group", role, RoleEditor)

		// Group names (Keycloak) may differ only in case.
		a := mustGroup(t, s, "IT-Admins", "")
		b := mustGroup(t, s, "it-admins", "")
		if a.ID == b.ID {
			t.Fatal("group names that differ in case were merged")
		}
	})
}

// TestSyncGroupMembersDirectoryIsAuthoritative: the directory's e-mail
// address and name win for users who never signed in (directory sync exists
// for them), and an address that moved to another directory object never
// moves that object onto an existing user's row.
func TestSyncGroupMembersDirectoryIsAuthoritative(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		g := mustGroup(t, s, "grp", "")
		svc := mustService(t, s, "Payroll", "", 0, g.ID)
		recipients := func() []string {
			t.Helper()
			r, err := s.RecipientsForServices(ctx, []int64{svc.ID})
			check(t, err)
			var out []string
			for _, x := range r {
				out = append(out, x.Email+"|"+x.Name)
			}
			return out
		}
		user := func(id int64) *User {
			t.Helper()
			u, err := s.GetUser(ctx, id)
			check(t, err)
			return u
		}

		// John changes his name and address in the directory.
		check(t, s.SyncGroupMembers(ctx, g.ID, "", []DirectoryUser{{DirectoryID: "d-john", Email: "john.smith@corp.example", Name: "John Smith"}}))
		check(t, s.SyncGroupMembers(ctx, g.ID, "", []DirectoryUser{{DirectoryID: "d-john", Email: "John.Doe@corp.example", Name: "John Doe"}}))
		equal(t, "refreshed", recipients(), []string{"john.doe@corp.example|John Doe"})

		// His old address now belongs to Jane: she gets her own row.
		check(t, s.SyncGroupMembers(ctx, g.ID, "", []DirectoryUser{
			{DirectoryID: "d-john", Email: "john.doe@corp.example", Name: "John Doe"},
			{DirectoryID: "d-jane", Email: "john.smith@corp.example", Name: "Jane"},
		}))
		equal(t, "reassigned address", recipients(), []string{"john.doe@corp.example|John Doe", "john.smith@corp.example|Jane"})

		// Users who signed in keep the values from the sign-in.
		alice, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-alice", DirectoryID: "d-alice", Email: "alice@corp.example", Name: "Alice"})
		check(t, err)
		check(t, s.SyncGroupMembers(ctx, g.ID, "", []DirectoryUser{{DirectoryID: "d-alice", Email: "alice.mail@corp.example", Name: "Alice Directory"}}))
		got := user(alice.ID)
		equal(t, "signed-in user", [2]string{got.Email, got.Name}, [2]string{"alice@corp.example", "Alice"})

		// Max's old address is reassigned to Moritz before Max's own group is
		// synchronised again: the stale address moves from Max's row to
		// Moritz instead of Moritz being merged into Max's row.
		g2 := mustGroup(t, s, "grp2", "")
		check(t, s.SyncGroupMembers(ctx, g2.ID, "", []DirectoryUser{{DirectoryID: "d-max", Email: "max@corp.example", Name: "Max"}}))
		maxRow := mustMember(t, s, g2.ID)
		check(t, s.SyncGroupMembers(ctx, g.ID, "", []DirectoryUser{{DirectoryID: "d-moritz", Email: "max@corp.example", Name: "Moritz"}}))
		moritz := mustMember(t, s, g.ID)
		if moritz.ID == maxRow.ID {
			t.Fatal("Moritz was matched to Max's row by e-mail")
		}
		equal(t, "Moritz has the address", [2]string{moritz.Email, moritz.DirectoryID}, [2]string{"max@corp.example", "d-moritz"})
		equal(t, "Max's stale address removed", user(maxRow.ID).Email, "")
		check(t, s.SyncGroupMembers(ctx, g2.ID, "", []DirectoryUser{{DirectoryID: "d-max", Email: "max.new@corp.example", Name: "Max"}}))
		equal(t, "Max's new address", user(maxRow.ID).Email, "max.new@corp.example")
	})
}

// TestSyncLargeGroupSQLite: on SQLite the sync of a group holds the write
// lock, so it must stay fast for large groups. E-mail statements that cannot
// use the partial index users_email_idx (they must repeat its condition) scan
// the users table for every member: re-synchronising 5,000 members took
// about 1.8 s instead of 0.2 s, 10,000 about 6 s, and sign-ins waiting for
// the lock failed after the busy timeout (10 s).
func TestSyncLargeGroupSQLite(t *testing.T) {
	if testing.Short() {
		t.Skip("large group")
	}
	s := openSQLite(t)
	g := mustGroup(t, s, "all-staff", "All Staff")
	const n = 5000
	members := make([]DirectoryUser, n)
	for i := range members {
		members[i] = DirectoryUser{DirectoryID: fmt.Sprint("d-", i), Email: fmt.Sprintf("user%d@example.com", i), Name: fmt.Sprint("User ", i)}
	}
	check(t, s.SyncGroupMembers(ctx, g.ID, "", members))
	start := time.Now()
	check(t, s.SyncGroupMembers(ctx, g.ID, "", members))
	if d := time.Since(start); d > time.Second {
		t.Errorf("re-synchronising %d members took %v", n, d)
	}
	got, err := s.GroupMembers(ctx, g.ID)
	check(t, err)
	equal(t, "members", len(got), n)
	for _, u := range got {
		if want := "user" + strings.TrimPrefix(u.DirectoryID, "d-") + "@example.com"; u.Email != want {
			t.Fatalf("member %s has e-mail %q, want %q", u.DirectoryID, u.Email, want)
		}
	}
}

// mustMember returns the only member of a group.
func mustMember(t *testing.T, s *Store, groupID int64) User {
	t.Helper()
	members, err := s.GroupMembers(ctx, groupID)
	check(t, err)
	if len(members) != 1 {
		t.Fatalf("group %d has %d members, want 1", groupID, len(members))
	}
	return members[0]
}

func TestGroupsCRUD(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		groups, err := s.ListGroups(ctx)
		check(t, err)
		equal(t, "empty", len(groups), 0)

		ops, err := s.CreateGroup(ctx, " ext-ops ", " Operations ")
		check(t, err)
		equal(t, "external id", ops.ExternalID, "ext-ops")
		equal(t, "name", ops.Name, "Operations")
		equal(t, "role", ops.Role, RoleNone)
		equal(t, "members", ops.MemberCount, 0)
		equal(t, "services", len(ops.ServiceIDs), 0)
		if ops.LastSyncedAt != nil || !recent(ops.CreatedAt) {
			t.Fatalf("unexpected times %+v", ops)
		}

		// existing group is returned unchanged
		same, err := s.CreateGroup(ctx, "ext-ops", "Other name")
		check(t, err)
		equal(t, "existing", *same, *ops)

		// name defaults to the external id and is set later
		dev, err := s.CreateGroup(ctx, "ext-dev", "")
		check(t, err)
		equal(t, "default name", dev.Name, "ext-dev")
		dev, err = s.CreateGroup(ctx, "ext-dev", "Developers")
		check(t, err)
		equal(t, "name set", dev.Name, "Developers")

		_, err = s.CreateGroup(ctx, "  ", "x")
		wantError(t, "empty external id", err)

		_, err = s.GetGroup(ctx, 999)
		wantNotFound(t, "unknown group", err)

		// update name and role; viewer is stored as "no special role"
		check(t, s.UpdateGroup(ctx, dev.ID, " Dev team ", RoleEditor))
		g, err := s.GetGroup(ctx, dev.ID)
		check(t, err)
		equal(t, "updated name", g.Name, "Dev team")
		equal(t, "updated role", g.Role, RoleEditor)
		check(t, s.UpdateGroup(ctx, dev.ID, "Dev team", RoleViewer))
		var stored string
		check(t, s.DB().QueryRowContext(ctx, `SELECT role FROM idp_groups WHERE id = ?`, dev.ID).Scan(&stored))
		equal(t, "stored viewer role", stored, "")
		check(t, s.UpdateGroup(ctx, dev.ID, "Dev team", RoleAdmin))
		wantNotFound(t, "update unknown", s.UpdateGroup(ctx, 999, "x", RoleNone))

		// services
		s1 := mustService(t, s, "Mail", "", 0)
		s2 := mustService(t, s, "VPN", "", 0)
		check(t, s.SetGroupServices(ctx, ops.ID, []int64{s2.ID, s1.ID, s2.ID, 999}))
		g, err = s.GetGroup(ctx, ops.ID)
		check(t, err)
		equal(t, "service ids", g.ServiceIDs, []int64{s1.ID, s2.ID})
		svc, err := s.GetService(ctx, s1.ID)
		check(t, err)
		equal(t, "service group ids", svc.GroupIDs, []int64{ops.ID})
		check(t, s.SetGroupServices(ctx, ops.ID, []int64{s2.ID}))
		g, err = s.GetGroup(ctx, ops.ID)
		check(t, err)
		equal(t, "replaced", g.ServiceIDs, []int64{s2.ID})
		wantNotFound(t, "services of unknown group", s.SetGroupServices(ctx, 999, []int64{s1.ID}))

		// members
		u1 := mustUser(t, s, "1", "zed@example.com", "")
		u2 := mustUser(t, s, "2", "amy@example.com", "Amy")
		check(t, s.SetUserGroups(ctx, u1.ID, []GroupRef{{ExternalID: "ext-ops"}}))
		check(t, s.SetUserGroups(ctx, u2.ID, []GroupRef{{ExternalID: "ext-ops"}, {ExternalID: "ext-dev"}}))
		members, err := s.GroupMembers(ctx, ops.ID)
		check(t, err)
		equal(t, "members", len(members), 2)
		equal(t, "members order", []int64{members[0].ID, members[1].ID}, []int64{u2.ID, u1.ID})
		members, err = s.GroupMembers(ctx, 999)
		check(t, err)
		equal(t, "members of unknown group", len(members), 0)

		groups, err = s.ListGroups(ctx)
		check(t, err)
		equal(t, "count", len(groups), 2)
		equal(t, "order", []string{groups[0].Name, groups[1].Name}, []string{"Dev team", "Operations"})
		equal(t, "member count", []int{groups[0].MemberCount, groups[1].MemberCount}, []int{1, 2})
		equal(t, "service ids", groups[1].ServiceIDs, []int64{s2.ID})
		equal(t, "no service ids", len(groups[0].ServiceIDs), 0)

		// delete removes memberships and service assignments
		check(t, s.DeleteGroup(ctx, ops.ID))
		_, err = s.GetGroup(ctx, ops.ID)
		wantNotFound(t, "deleted", err)
		ug, err := s.UserGroups(ctx, u1.ID)
		check(t, err)
		equal(t, "memberships removed", len(ug), 0)
		svc, err = s.GetService(ctx, s2.ID)
		check(t, err)
		equal(t, "service link removed", len(svc.GroupIDs), 0)
		wantNotFound(t, "delete again", s.DeleteGroup(ctx, ops.ID))
	})
}

func TestGroupsToSync(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		mustGroup(t, s, "plain", "Plain")
		editors := mustGroup(t, s, "editors", "Editors")
		check(t, s.UpdateGroup(ctx, editors.ID, "Editors", RoleEditor))
		viewers := mustGroup(t, s, "viewers", "Viewers")
		check(t, s.UpdateGroup(ctx, viewers.ID, "Viewers", RoleViewer))
		subscribers := mustGroup(t, s, "subscribers", "Subscribers")
		svc := mustService(t, s, "Mail", "", 0, subscribers.ID)

		groups, err := s.GroupsToSync(ctx)
		check(t, err)
		var names []string
		for _, g := range groups {
			names = append(names, g.Name)
		}
		equal(t, "groups to sync", names, []string{"Editors", "Subscribers"})
		equal(t, "service ids", groups[1].ServiceIDs, []int64{svc.ID})
	})
}

func TestSyncGroupMembers(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		g := mustGroup(t, s, "grp", "Old name")
		wantNotFound(t, "unknown group", s.SyncGroupMembers(ctx, 999, "x", nil))

		existing := mustUser(t, s, "sub-e", "e@example.com", "Existing")
		noDir, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-d", DirectoryID: "dir-d"})
		check(t, err)
		taken := mustUser(t, s, "sub-t", "taken@example.com", "Taken")
		check(t, s.SetUserGroups(ctx, taken.ID, []GroupRef{{ExternalID: "grp"}}))

		check(t, s.SyncGroupMembers(ctx, g.ID, " New name ", []DirectoryUser{
			{DirectoryID: "dir-new", Email: " New@Example.com ", Name: "New"},
			{DirectoryID: "dir-e", Email: "E@EXAMPLE.COM", Name: "ignored, has a name"},
			{DirectoryID: "dir-d", Email: "taken@example.com", Name: "D"}, // e-mail held by another user
			{Name: "cannot be identified"},
			{DirectoryID: "dir-new", Email: "new@example.com"}, // duplicate
			{DirectoryID: "dir-noemail", Name: "No Mail"},
		}))

		got, err := s.GetGroup(ctx, g.ID)
		check(t, err)
		equal(t, "name", got.Name, "New name")
		if got.LastSyncedAt == nil || !recent(*got.LastSyncedAt) {
			t.Fatalf("last synced not set: %+v", got)
		}
		members, err := s.GroupMembers(ctx, g.ID)
		check(t, err)
		byDir := map[string]User{}
		for _, m := range members {
			byDir[m.DirectoryID] = m
		}
		equal(t, "member count", len(members), 4) // taken is no longer a member
		equal(t, "new user email", byDir["dir-new"].Email, "new@example.com")
		equal(t, "new user name", byDir["dir-new"].Name, "New")
		equal(t, "existing matched by email", byDir["dir-e"].ID, existing.ID)
		equal(t, "existing name kept", byDir["dir-e"].Name, "Existing")
		equal(t, "matched by dir", byDir["dir-d"].ID, noDir.ID)
		equal(t, "conflicting email not taken", byDir["dir-d"].Email, "")
		equal(t, "name filled", byDir["dir-d"].Name, "D")
		equal(t, "member without email", byDir["dir-noemail"].Email, "")
		equal(t, "member without email", byDir["dir-noemail"].Name, "No Mail")
		tk, err := s.GetUser(ctx, taken.ID)
		check(t, err)
		equal(t, "taken keeps email", tk.Email, "taken@example.com")

		// a later sync replaces the memberships; an empty name keeps the name
		check(t, s.SyncGroupMembers(ctx, g.ID, "", []DirectoryUser{{DirectoryID: "dir-e"}}))
		members, err = s.GroupMembers(ctx, g.ID)
		check(t, err)
		equal(t, "one member", len(members), 1)
		equal(t, "one member", members[0].ID, existing.ID)
		got, err = s.GetGroup(ctx, g.ID)
		check(t, err)
		equal(t, "name kept", got.Name, "New name")

		check(t, s.SyncGroupMembers(ctx, g.ID, "", nil))
		got, err = s.GetGroup(ctx, g.ID)
		check(t, err)
		equal(t, "no members", got.MemberCount, 0)
	})
}

// TestSyncGroupMembersBadMember checks that a member that cannot be stored
// does not fail the whole group. PostgreSQL rejects invalid UTF-8 (which
// aborts the statement and exercises the savepoint); SQLite stores it.
func TestSyncGroupMembersBadMember(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		g := mustGroup(t, s, "grp", "")
		check(t, s.SyncGroupMembers(ctx, g.ID, "", []DirectoryUser{
			{DirectoryID: "ok-1", Email: "one@example.com"},
			{DirectoryID: "bad", Email: "bad\xff@example.com", Name: "\xff"},
			{DirectoryID: "ok-2", Email: "two@example.com"},
		}))
		members, err := s.GroupMembers(ctx, g.ID)
		check(t, err)
		dirs := map[string]bool{}
		for _, m := range members {
			dirs[m.DirectoryID] = true
		}
		if !dirs["ok-1"] || !dirs["ok-2"] {
			t.Fatalf("good members missing: %+v", members)
		}
		if s.DB().IsPostgres() && dirs["bad"] {
			t.Fatal("invalid member stored on PostgreSQL")
		}
	})
}
