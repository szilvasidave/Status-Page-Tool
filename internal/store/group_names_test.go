package store

import (
	"strings"
	"testing"
	"time"
)

func TestGroupDisplayName(t *testing.T) {
	cases := []struct {
		g    Group
		want string
	}{
		{Group{ExternalID: "id"}, "id"},
		{Group{ExternalID: "id", DirectoryName: "Dir"}, "Dir"},
		{Group{ExternalID: "id", Name: "Own", DirectoryName: "Dir"}, "Own"},
	}
	for _, tc := range cases {
		if got := tc.g.DisplayName(); got != tc.want {
			t.Errorf("DisplayName(%+v) = %q, want %q", tc.g, got, tc.want)
		}
	}
}

func TestGroupDirectoryNames(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		const a, b, c = "aaaaaaaa-0000-4000-8000-000000000001", "bbbbbbbb-0000-4000-8000-000000000002", "cccccccc-0000-4000-8000-000000000003"
		const unused = "eeeeeeee-0000-4000-8000-000000000005"
		ga := mustGroup(t, s, strings.ToUpper(a), strings.ToUpper(a)) // a name equal to the id is no name
		equal(t, "no name", ga.Name, "")
		gb := mustGroup(t, s, b, "")
		gc := mustGroup(t, s, c, "Zeta (own name)")
		mustGroup(t, s, unused, "") // no members, role or services: not looked up
		mustGroup(t, s, "keycloak-name", "")
		prov := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "Provisioned", ExternalID: "dddddddd-0000-4000-8000-000000000004"})
		member := mustUser(t, s, "s-m", "m@example.com", "M")
		check(t, s.SetUserGroups(ctx, member.ID, []GroupRef{{ExternalID: a}, {ExternalID: "keycloak-name"}}))
		check(t, s.UpdateGroup(ctx, gb.ID, "", RoleEditor))
		svc := &Service{Name: "ERP", GroupIDs: []int64{gc.ID}}
		check(t, s.CreateService(ctx, svc))

		ids := func(groups []Group) []int64 {
			var out []int64
			for _, g := range groups {
				out = append(out, g.ID)
			}
			return out
		}
		due, err := s.GroupsForNameLookup(ctx, nowTime(), time.Time{})
		check(t, err)
		equal(t, "due: used groups with object IDs, not provisioned", ids(due), []int64{ga.ID, gb.ID, gc.ID})
		// Groups recorded a moment ago count as used (e.g. one an administrator just added).
		due, err = s.GroupsForNameLookup(ctx, nowTime(), nowTime().Add(-time.Minute))
		check(t, err)
		equal(t, "with the groups recorded since", len(due), 4)
		due, err = s.GroupsForNameLookup(ctx, nowTime(), nowTime().Add(time.Minute))
		check(t, err)
		equal(t, "none recorded since", len(due), 3)

		at := nowTime().Add(-time.Hour)
		long := strings.Repeat("é", 300)
		check(t, s.SetGroupDirectoryNames(ctx, []int64{ga.ID, gb.ID, gc.ID, prov.ID, prov.ID + 100},
			map[int64]string{ga.ID: " Al\x00pha\x1b\tTeam ", gc.ID: long, prov.ID: "Must not be stored"}, at))
		got, err := s.GetGroup(ctx, ga.ID)
		check(t, err)
		equal(t, "directory name cleaned", [4]any{got.DirectoryName, got.DisplayName(), *got.DirectoryCheckedAt, got.DirectoryMissing()},
			[4]any{"Alpha Team", "Alpha Team", at, false})
		got, err = s.GetGroup(ctx, gb.ID)
		check(t, err)
		equal(t, "not found", [3]any{got.DirectoryName, got.DisplayName(), got.DirectoryMissing()}, [3]any{"", b, true})
		got, err = s.GetGroup(ctx, gc.ID)
		check(t, err)
		equal(t, "own name wins", got.DisplayName(), "Zeta (own name)")
		equal(t, "long name shortened", len([]rune(got.DirectoryName)), 256)
		got, err = s.GetGroup(ctx, prov.ID)
		check(t, err)
		equal(t, "provisioned group untouched", [2]any{got.DirectoryName, got.DirectoryCheckedAt == nil}, [2]any{"", true})

		// Due again only after the refresh time, longest unchecked first; the
		// zero time lists all.
		due, err = s.GroupsForNameLookup(ctx, at, time.Time{})
		check(t, err)
		equal(t, "nothing due", len(due), 0)
		check(t, s.SetGroupDirectoryNames(ctx, []int64{ga.ID}, map[int64]string{ga.ID: "Alpha Team"}, at.Add(30*time.Minute)))
		due, err = s.GroupsForNameLookup(ctx, nowTime(), time.Time{})
		check(t, err)
		equal(t, "oldest first", ids(due), []int64{gb.ID, gc.ID, ga.ID})
		due, err = s.GroupsForNameLookup(ctx, time.Time{}, time.Time{})
		check(t, err)
		equal(t, "all", len(due), 3)

		// A group deleted in the directory keeps its last name, marked missing.
		check(t, s.SetGroupDirectoryNames(ctx, []int64{ga.ID}, nil, nowTime()))
		got, err = s.GetGroup(ctx, ga.ID)
		check(t, err)
		equal(t, "last name kept", [2]any{got.DisplayName(), got.DirectoryMissing()}, [2]any{"Alpha Team", true})

		// Lists are ordered by the name shown, and users see it too.
		groups, err := s.ListGroups(ctx)
		check(t, err)
		var names []string
		for _, g := range groups {
			names = append(names, g.DisplayName())
		}
		equal(t, "ordered by display name", names, []string{"Alpha Team", b, unused, "keycloak-name", "Provisioned", "Zeta (own name)"})
		u := mustUser(t, s, "s-u", "u@example.com", "U")
		check(t, s.SetUserGroups(ctx, u.ID, []GroupRef{{ExternalID: c}, {ExternalID: a}, {ExternalID: b}}))
		users, err := s.ListUsers(ctx)
		check(t, err)
		for _, lu := range users {
			if lu.ID == u.ID {
				equal(t, "user's groups", lu.Groups, []GroupRef{{ExternalID: a, Name: "Alpha Team"}, {ExternalID: b, Name: b}, {ExternalID: c, Name: "Zeta (own name)"}})
			}
		}

		// Sign-ins leave the directory name alone.
		check(t, s.SetUserGroups(ctx, u.ID, []GroupRef{{ExternalID: a}}))
		got, err = s.GetGroup(ctx, ga.ID)
		check(t, err)
		equal(t, "after a sign-in", [2]string{got.Name, got.DirectoryName}, [2]string{"", "Alpha Team"})
	})
}

// A name provisioning gave a group is its name in the directory: when
// provisioning takes a group over, the looked-up name goes; when it stops,
// the provisioning name becomes the directory name, which the lookup then
// keeps up to date.
func TestGroupNamesAndProvisioning(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		const id1, id2 = "11111111-0000-4000-8000-000000000001", "22222222-0000-4000-8000-000000000002"
		g := mustGroup(t, s, id1, "")
		check(t, s.SetGroupDirectoryNames(ctx, []int64{g.ID}, map[int64]string{g.ID: "Sales"}, nowTime()))

		adopted, isAdopted, err := s.CreateSCIMGroup(ctx, SCIMGroupData{DisplayName: "Sales EMEA", ExternalID: id1})
		check(t, err)
		equal(t, "adopted", isAdopted, true)
		equal(t, "directory name dropped", [3]any{adopted.DisplayName(), adopted.DirectoryName, adopted.DirectoryCheckedAt == nil},
			[3]any{"Sales EMEA", "", true})

		// Entra ID deletes the group (or unassigns it): its name is the directory name now.
		check(t, s.DeleteSCIMGroup(ctx, adopted.ID))
		got, err := s.GetGroup(ctx, adopted.ID)
		check(t, err)
		equal(t, "after deprovisioning", [4]any{got.Name, got.DirectoryName, got.DisplayName(), got.DirectoryCheckedAt == nil},
			[4]any{"", "Sales EMEA", "Sales EMEA", true})
		due, err := s.GroupsForNameLookup(ctx, nowTime(), time.Time{})
		check(t, err)
		equal(t, "not due without members, role or services", len(due), 0)
		check(t, s.UpdateGroup(ctx, got.ID, "", RoleEditor))
		due, err = s.GroupsForNameLookup(ctx, nowTime(), time.Time{})
		check(t, err)
		equal(t, "due at once", len(due), 1)
		check(t, s.SetGroupDirectoryNames(ctx, []int64{got.ID}, map[int64]string{got.ID: "Sales Europe"}, nowTime()))
		got, err = s.GetGroup(ctx, got.ID)
		check(t, err)
		equal(t, "follows renames", got.DisplayName(), "Sales Europe")

		// Turning provisioning off does the same for all provisioned groups.
		other := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "Finance", ExternalID: id2})
		check(t, s.SetSCIMToken(ctx, "", time.Time{}))
		got, err = s.GetGroup(ctx, other.ID)
		check(t, err)
		equal(t, "after turning provisioning off", [3]any{got.Provisioned, got.Name, got.DisplayName()}, [3]any{false, "", "Finance"})

		// A provisioned group whose name is its id leaves no directory name.
		third := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "33333333-0000-4000-8000-000000000003", ExternalID: "33333333-0000-4000-8000-000000000003"})
		check(t, s.DeleteSCIMGroup(ctx, third.ID))
		got, err = s.GetGroup(ctx, third.ID)
		check(t, err)
		equal(t, "id as name", [2]any{got.Name, got.DirectoryName}, [2]any{"", ""})
	})
}

// A members-only change of a provisioned group is not refused because
// another provisioned group already has the same name.
func TestSCIMGroupDuplicateNameKeepsMembersWorking(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		a := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "Admins", ExternalID: "a"})
		b := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "Other", ExternalID: "b"})
		check(t, s.UpdateGroup(ctx, b.ID, "admins", RoleViewer)) // e.g. renamed before 1.1
		u := mustUser(t, s, "s-u", "u@example.com", "U")
		_, err := s.UpdateSCIMGroup(ctx, a.ID, func(d *SCIMGroupData) error { d.Members = []int64{u.ID}; return nil })
		check(t, err)
		_, err = s.UpdateSCIMGroup(ctx, b.ID, func(d *SCIMGroupData) error { d.Members = []int64{u.ID}; return nil })
		check(t, err)
		// A real rename to a taken name is still refused.
		_, err = s.UpdateSCIMGroup(ctx, b.ID, func(d *SCIMGroupData) error { d.DisplayName = "ADMINS "; return nil })
		wantConflict(t, "rename to a taken name", err)
	})
}
