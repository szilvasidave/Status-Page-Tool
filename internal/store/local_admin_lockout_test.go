package store

import (
	"errors"
	"testing"
	"time"
)

func TestAdminGroupCount(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		count := func(except int64) int {
			t.Helper()
			n, err := s.AdminGroupCount(ctx, except)
			check(t, err)
			return n
		}
		equal(t, "no groups", count(0), 0)
		admins := mustGroup(t, s, "admins", "Admins")
		check(t, s.UpdateGroup(ctx, admins.ID, admins.Name, RoleAdmin))
		editors := mustGroup(t, s, "editors", "Editors")
		check(t, s.UpdateGroup(ctx, editors.ID, editors.Name, RoleEditor))
		mustGroup(t, s, "viewers", "")
		equal(t, "all groups", count(0), 1)
		equal(t, "without the administrator group", count(admins.ID), 0)
		equal(t, "without another group", count(editors.ID), 1)
	})
}

func TestDeleteLocalAdminSessions(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		admin, err := s.CreateLocalAdmin(ctx, "admin", "", "hash")
		check(t, err)
		sso := mustUser(t, s, "sub-1", "ann@example.com", "Ann")
		sessions := map[string]int64{"admin-1": admin.ID, "admin-2": admin.ID, "sso-1": sso.ID}
		for id, userID := range sessions {
			check(t, s.CreateSession(ctx, &Session{ID: id, UserID: userID, ExpiresAt: time.Now().Add(time.Hour)}))
		}
		check(t, s.DeleteLocalAdminSessions(ctx))
		for id, userID := range sessions {
			_, err := s.GetSession(ctx, id, time.Now())
			if err != nil && !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
			equal(t, "session "+id+" kept", err == nil, userID == sso.ID)
		}
	})
}

func TestLocalAdminUsernames(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		names, err := s.LocalAdminUsernames(ctx)
		check(t, err)
		equal(t, "without local administrators", len(names), 0)

		admin, err := s.CreateLocalAdmin(ctx, "admin", "", "hash")
		check(t, err)
		_, err = s.CreateLocalAdmin(ctx, "Backup", "", "hash")
		check(t, err)
		mustUser(t, s, "sub-1", "ann@example.com", "Ann")
		check(t, s.UpdateLocalAdmin(ctx, admin.ID, "ops-breakglass", ""))
		names, err = s.LocalAdminUsernames(ctx)
		check(t, err)
		equal(t, "names", names, []string{"backup", "ops-breakglass"})
	})
}
