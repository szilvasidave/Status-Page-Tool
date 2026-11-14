package store

import (
	"strings"
	"testing"
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

		// no name: the external id is shown; the name is set later
		dev, err := s.CreateGroup(ctx, "ext-dev", "")
		check(t, err)
		equal(t, "no name", dev.Name, "")
		equal(t, "shown as", dev.DisplayName(), "ext-dev")
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
