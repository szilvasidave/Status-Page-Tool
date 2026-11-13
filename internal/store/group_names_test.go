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
		ga := mustGroup(t, s, strings.ToUpper(a), a) // a name equal to the id is no name
		equal(t, "no name", ga.Name, "")
		gb := mustGroup(t, s, b, "")
		gc := mustGroup(t, s, c, "Zeta (own name)")
		mustGroup(t, s, "keycloak-name", "")
		prov := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "Provisioned", ExternalID: "dddddddd-0000-4000-8000-000000000004"})

		ids := func(groups []Group) []int64 {
			var out []int64
			for _, g := range groups {
				out = append(out, g.ID)
			}
			return out
		}
		due, err := s.GroupsForNameLookup(ctx, nowTime())
		check(t, err)
		equal(t, "due: groups with object IDs, not provisioned", ids(due), []int64{ga.ID, gb.ID, gc.ID})

		at := nowTime().Add(-time.Hour)
		long := strings.Repeat("é", 300)
		check(t, s.SetGroupDirectoryNames(ctx, []int64{ga.ID, gb.ID, gc.ID, prov.ID + 100}, map[int64]string{ga.ID: " Alpha ", gc.ID: long}, at))
		got, err := s.GetGroup(ctx, ga.ID)
		check(t, err)
		equal(t, "directory name", [3]any{got.DirectoryName, got.DisplayName(), *got.DirectoryCheckedAt}, [3]any{"Alpha", "Alpha", at})
		got, err = s.GetGroup(ctx, gb.ID)
		check(t, err)
		equal(t, "not found", [3]any{got.DirectoryName, got.DisplayName(), got.DirectoryCheckedAt != nil}, [3]any{"", b, true})
		got, err = s.GetGroup(ctx, gc.ID)
		check(t, err)
		equal(t, "own name wins", got.DisplayName(), "Zeta (own name)")
		equal(t, "long name shortened", len([]rune(got.DirectoryName)), 256)

		// Due again only after the refresh time; the zero time lists all.
		due, err = s.GroupsForNameLookup(ctx, at)
		check(t, err)
		equal(t, "nothing due", len(due), 0)
		due, err = s.GroupsForNameLookup(ctx, at.Add(time.Second))
		check(t, err)
		equal(t, "due after the refresh time", ids(due), []int64{ga.ID, gb.ID, gc.ID})
		due, err = s.GroupsForNameLookup(ctx, time.Time{})
		check(t, err)
		equal(t, "all", len(due), 3)

		// A group deleted in the directory keeps its last name.
		check(t, s.SetGroupDirectoryNames(ctx, []int64{ga.ID}, nil, nowTime()))
		got, err = s.GetGroup(ctx, ga.ID)
		check(t, err)
		equal(t, "last name kept", got.DisplayName(), "Alpha")

		// Lists are ordered by the name shown, and users see it too.
		groups, err := s.ListGroups(ctx)
		check(t, err)
		var names []string
		for _, g := range groups {
			names = append(names, g.DisplayName())
		}
		equal(t, "ordered by display name", names, []string{"Alpha", b, "keycloak-name", "Provisioned", "Zeta (own name)"})
		u := mustUser(t, s, "s-u", "u@example.com", "U")
		check(t, s.SetUserGroups(ctx, u.ID, []GroupRef{{ExternalID: c}, {ExternalID: a}, {ExternalID: b}}))
		users, err := s.ListUsers(ctx)
		check(t, err)
		equal(t, "user's groups", users[0].Groups, []GroupRef{{ExternalID: a, Name: "Alpha"}, {ExternalID: b, Name: b}, {ExternalID: c, Name: "Zeta (own name)"}})

		// Sign-ins and provisioning leave the directory name alone.
		check(t, s.SetUserGroups(ctx, u.ID, []GroupRef{{ExternalID: a}}))
		got, err = s.GetGroup(ctx, ga.ID)
		check(t, err)
		equal(t, "after a sign-in", [2]string{got.Name, got.DirectoryName}, [2]string{"", "Alpha"})
	})
}
