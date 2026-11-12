package store

import (
	"testing"
)

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
