package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestLocalAdmin(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		has, err := s.HasLocalAdmin(ctx)
		check(t, err)
		equal(t, "has admin before", has, false)

		u, err := s.CreateLocalAdmin(ctx, "  Admin ", " The Admin ", "hash1")
		check(t, err)
		equal(t, "username", u.Username, "admin")
		equal(t, "name", u.Name, "The Admin")
		equal(t, "hash", u.PasswordHash, "hash1")
		equal(t, "is admin", u.IsLocalAdmin, true)
		equal(t, "email", u.Email, "")
		equal(t, "subject", u.OIDCSubject, "")
		if u.ID == 0 || !recent(u.CreatedAt) || u.LastLoginAt != nil {
			t.Fatalf("unexpected user %+v", u)
		}

		has, err = s.HasLocalAdmin(ctx)
		check(t, err)
		equal(t, "has admin after", has, true)

		got, err := s.GetUserByUsername(ctx, " ADMIN ")
		check(t, err)
		equal(t, "by username", *got, *u)
		got, err = s.GetUser(ctx, u.ID)
		check(t, err)
		equal(t, "by id", *got, *u)

		_, err = s.GetUserByUsername(ctx, "nobody")
		wantNotFound(t, "unknown username", err)
		_, err = s.GetUserByUsername(ctx, "  ")
		wantNotFound(t, "empty username", err)
		_, err = s.GetUser(ctx, 999)
		wantNotFound(t, "unknown id", err)

		_, err = s.CreateLocalAdmin(ctx, "ADMIN", "", "x")
		wantError(t, "duplicate username", err)
		if !strings.Contains(err.Error(), "already taken") {
			t.Fatalf("unhelpful error: %v", err)
		}
		_, err = s.CreateLocalAdmin(ctx, " ", "", "x")
		wantError(t, "empty username", err)
		_, err = s.CreateLocalAdmin(ctx, "nopw", "", "")
		wantError(t, "empty password", err)

		// a second admin: no unique conflicts on the empty/NULL columns
		u2, err := s.CreateLocalAdmin(ctx, "backup", "", "hash2")
		check(t, err)
		role, err := s.UserRole(ctx, u2.ID)
		check(t, err)
		equal(t, "role", role, RoleAdmin)

		check(t, s.SetPasswordHash(ctx, u.ID, "hash3"))
		got, err = s.GetUser(ctx, u.ID)
		check(t, err)
		equal(t, "new hash", got.PasswordHash, "hash3")
		wantNotFound(t, "set password unknown user", s.SetPasswordHash(ctx, 999, "x"))
		oidc := mustUser(t, s, "sub", "a@example.com", "A")
		wantNotFound(t, "set password of OIDC user", s.SetPasswordHash(ctx, oidc.ID, "x"))
	})
}

func TestUpsertOIDCUser(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		admin, err := s.CreateLocalAdmin(ctx, "admin", "Admin", "hash")
		check(t, err)

		_, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: " ", Email: "x@example.com"})
		wantError(t, "empty subject", err)

		// create
		alice, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-alice", Email: " Alice@Example.COM ", Name: " Alice ", DirectoryID: "d-alice"})
		check(t, err)
		equal(t, "email", alice.Email, "alice@example.com")
		equal(t, "name", alice.Name, "Alice")
		equal(t, "subject", alice.OIDCSubject, "s-alice")
		equal(t, "dir", alice.DirectoryID, "d-alice")
		equal(t, "local admin", alice.IsLocalAdmin, false)
		equal(t, "username", alice.Username, "")
		if alice.LastLoginAt == nil || !recent(*alice.LastLoginAt) || !recent(alice.CreatedAt) {
			t.Fatalf("times not set: %+v", alice)
		}

		// same subject again, without e-mail and directory id: kept
		again, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-alice", Name: "Alice Smith"})
		check(t, err)
		equal(t, "same user", again.ID, alice.ID)
		equal(t, "name updated", again.Name, "Alice Smith")
		equal(t, "email kept", again.Email, "alice@example.com")
		equal(t, "dir kept", again.DirectoryID, "d-alice")

		// changed subject, matched by directory id: subject taken over
		again, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-alice-2", DirectoryID: "d-alice"})
		check(t, err)
		equal(t, "matched by dir", again.ID, alice.ID)
		equal(t, "subject taken over", again.OIDCSubject, "s-alice-2")
		equal(t, "name kept", again.Name, "Alice Smith")

		// matched by e-mail (user known from a directory sync, no dir id given)
		g := mustGroup(t, s, "g", "")
		check(t, s.SyncGroupMembers(ctx, g.ID, "", []DirectoryUser{
			{Email: "carol@example.com", Name: "Carol"},
			{DirectoryID: "d-dave", Email: "dave@example.com"},
		}))
		members, err := s.GroupMembers(ctx, g.ID)
		check(t, err)
		equal(t, "synced members", len(members), 2)
		carol, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-carol", Email: "CAROL@example.com"})
		check(t, err)
		equal(t, "matched by email", carol.ID, members[0].ID)
		equal(t, "name kept", carol.Name, "Carol")
		equal(t, "subject set", carol.OIDCSubject, "s-carol")

		// matched by directory id even though the e-mail differs
		dave, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-dave", DirectoryID: "d-dave", Email: "david@example.com", Name: "Dave"})
		check(t, err)
		equal(t, "matched by dir", dave.ID, members[1].ID)
		equal(t, "email updated", dave.Email, "david@example.com")

		// conflict: alice now has the e-mail that carol holds -> cleared at carol
		again, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-alice-2", Email: "carol@example.com"})
		check(t, err)
		equal(t, "alice", again.ID, alice.ID)
		equal(t, "alice email", again.Email, "carol@example.com")
		c, err := s.GetUser(ctx, carol.ID)
		check(t, err)
		equal(t, "carol email cleared", c.Email, "")

		// conflict: alice (matched by subject) reports dave's directory id
		again, err = s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-alice-2", DirectoryID: "d-dave"})
		check(t, err)
		equal(t, "alice", again.ID, alice.ID)
		equal(t, "alice dir", again.DirectoryID, "d-dave")
		d, err := s.GetUser(ctx, dave.ID)
		check(t, err)
		equal(t, "dave dir cleared", d.DirectoryID, "")
		equal(t, "dave subject kept", d.OIDCSubject, "s-dave")

		// several users without directory id / e-mail (NULL / '' in unique columns)
		mustUser(t, s, "anon-1", "", "")
		mustUser(t, s, "anon-2", "", "")

		// the local admin is never touched
		a, err := s.GetUser(ctx, admin.ID)
		check(t, err)
		equal(t, "admin unchanged", *a, *admin)
	})
}

func TestUserGroupsAndRole(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		u := mustUser(t, s, "sub", "u@example.com", "U")
		role, err := s.UserRole(ctx, u.ID)
		check(t, err)
		equal(t, "role without groups", role, RoleViewer)
		_, err = s.UserRole(ctx, 999)
		wantNotFound(t, "role of unknown user", err)

		check(t, s.SetUserGroups(ctx, u.ID, []GroupRef{
			{ExternalID: "g1", Name: "Group One"},
			{ExternalID: " g2 "},
			{ExternalID: "g1", Name: "duplicate"},
			{ExternalID: "  ", Name: "no id"},
		}))
		groups, err := s.UserGroups(ctx, u.ID)
		check(t, err)
		equal(t, "groups", len(groups), 2)
		equal(t, "ordered by name", groups[0].ExternalID, "g2")
		equal(t, "name defaults to external id", groups[0].Name, "g2")
		equal(t, "second", groups[1].Name, "Group One")
		equal(t, "member count", groups[1].MemberCount, 1)
		g1, g2 := groups[1], groups[0]

		// roles
		check(t, s.UpdateGroup(ctx, g1.ID, "Group One", RoleEditor))
		role, err = s.UserRole(ctx, u.ID)
		check(t, err)
		equal(t, "editor", role, RoleEditor)
		check(t, s.UpdateGroup(ctx, g2.ID, "g2", RoleAdmin))
		role, err = s.UserRole(ctx, u.ID)
		check(t, err)
		equal(t, "admin wins", role, RoleAdmin)

		// names: not overwritten by the external id or an empty name
		check(t, s.SetUserGroups(ctx, u.ID, []GroupRef{{ExternalID: "g1", Name: "g1"}, {ExternalID: "g2", Name: ""}}))
		g, err := s.GetGroup(ctx, g1.ID)
		check(t, err)
		equal(t, "name kept", g.Name, "Group One")
		check(t, s.SetUserGroups(ctx, u.ID, []GroupRef{{ExternalID: "g1", Name: "Renamed"}}))
		g, err = s.GetGroup(ctx, g1.ID)
		check(t, err)
		equal(t, "renamed", g.Name, "Renamed")
		equal(t, "role kept", g.Role, RoleEditor)

		// memberships are replaced
		groups, err = s.UserGroups(ctx, u.ID)
		check(t, err)
		equal(t, "only g1", len(groups), 1)
		equal(t, "only g1", groups[0].ID, g1.ID)
		role, err = s.UserRole(ctx, u.ID)
		check(t, err)
		equal(t, "editor again", role, RoleEditor)

		check(t, s.SetUserGroups(ctx, u.ID, nil))
		groups, err = s.UserGroups(ctx, u.ID)
		check(t, err)
		equal(t, "no groups", len(groups), 0)
		role, err = s.UserRole(ctx, u.ID)
		check(t, err)
		equal(t, "viewer", role, RoleViewer)

		// a local admin is admin regardless of groups
		admin, err := s.CreateLocalAdmin(ctx, "root", "", "h")
		check(t, err)
		role, err = s.UserRole(ctx, admin.ID)
		check(t, err)
		equal(t, "local admin", role, RoleAdmin)

		wantError(t, "unknown user", s.SetUserGroups(ctx, 999, []GroupRef{{ExternalID: "g1"}}))
	})
}

// TestConcurrentLogins runs several logins at the same time that report the
// same (new) groups in different orders.
func TestConcurrentLogins(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		const n = 8
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			go func() {
				u, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: fmt.Sprint("sub-", i), Email: fmt.Sprintf("user%d@example.com", i)})
				if err == nil {
					refs := []GroupRef{{ExternalID: "a"}, {ExternalID: "b"}, {ExternalID: "c"}}
					if i%2 == 1 {
						slices.Reverse(refs)
					}
					err = s.SetUserGroups(ctx, u.ID, refs)
				}
				if err == nil {
					_, err = s.UserRole(ctx, u.ID)
				}
				if err == nil {
					_, err = s.GetSettings(ctx)
				}
				errs <- err
			}()
		}
		for i := 0; i < n; i++ {
			check(t, <-errs)
		}
		groups, err := s.ListGroups(ctx)
		check(t, err)
		equal(t, "groups", len(groups), 3)
		for _, g := range groups {
			equal(t, "members of "+g.ExternalID, g.MemberCount, n)
		}
	})
}

func TestListUsers(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		users, err := s.ListUsers(ctx)
		check(t, err)
		equal(t, "empty", len(users), 0)

		bob := mustUser(t, s, "b", "bob@example.com", "bob")
		alice := mustUser(t, s, "a", "alice@example.com", "Alice")
		noname := mustUser(t, s, "c", "carl@example.com", "")
		admin, err := s.CreateLocalAdmin(ctx, "zz-admin", "", "h")
		check(t, err)

		check(t, s.SetUserGroups(ctx, bob.ID, []GroupRef{{ExternalID: "ops", Name: "Ops"}, {ExternalID: "dev"}}))
		check(t, s.SetUserGroups(ctx, alice.ID, []GroupRef{{ExternalID: "dev"}}))
		ops, err := s.CreateGroup(ctx, "ops", "")
		check(t, err)
		check(t, s.UpdateGroup(ctx, ops.ID, "Ops", RoleEditor))

		users, err = s.ListUsers(ctx)
		check(t, err)
		var order []int64
		for _, u := range users {
			order = append(order, u.ID)
		}
		equal(t, "order by display name", order, []int64{alice.ID, bob.ID, noname.ID, admin.ID})
		equal(t, "alice groups", users[0].Groups, []GroupRef{{ExternalID: "dev", Name: "dev"}})
		equal(t, "alice role", users[0].Role, RoleViewer)
		equal(t, "bob groups", users[1].Groups, []GroupRef{{ExternalID: "dev", Name: "dev"}, {ExternalID: "ops", Name: "Ops"}})
		equal(t, "bob role", users[1].Role, RoleEditor)
		equal(t, "carl groups", len(users[2].Groups), 0)
		equal(t, "admin role", users[3].Role, RoleAdmin)
		equal(t, "user fields", users[0].User.Email, "alice@example.com")
	})
}
