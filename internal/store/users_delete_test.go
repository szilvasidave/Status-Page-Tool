package store

import (
	"errors"
	"testing"
	"time"
)

func TestDeleteUser(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		admin, err := s.CreateLocalAdmin(ctx, "admin", "Admin", "hash")
		if err != nil {
			t.Fatal(err)
		}
		u, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "sub-1", Email: "bob@example.com", Name: "Bob"})
		if err != nil {
			t.Fatal(err)
		}
		g := mustGroup(t, s, "network-team", "Network")
		if _, err := s.db.ExecContext(ctx, `INSERT INTO user_groups (user_id, group_id) VALUES (?, ?)`, u.ID, g.ID); err != nil {
			t.Fatal(err)
		}
		sess := &Session{ID: "session-hash", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
		inc := &Incident{Title: "Down", Status: IncidentInvestigating, CreatedBy: u.ID}
		if err := s.CreateIncident(ctx, inc, &IncidentUpdate{Status: IncidentInvestigating, Message: "x", CreatedBy: u.ID}); err != nil {
			t.Fatal(err)
		}

		if err := s.DeleteUser(ctx, admin.ID); !errors.Is(err, ErrLocalAdmin) {
			t.Fatalf("delete local admin: %v, want ErrLocalAdmin", err)
		}
		if err := s.DeleteUser(ctx, 99999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete unknown user: %v, want ErrNotFound", err)
		}
		if err := s.DeleteUser(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetUser(ctx, u.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("user still exists: %v", err)
		}
		if _, err := s.GetSession(ctx, sess.ID, time.Now()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session still exists: %v", err)
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_groups WHERE user_id = ?`, u.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("memberships left: %d %v", n, err)
		}
		if got, err := s.GetIncident(ctx, inc.ID); err != nil || got.CreatedBy != 0 {
			t.Fatalf("incident after deleting its author: %+v %v", got, err)
		}
		if _, err := s.GetUser(ctx, admin.ID); err != nil {
			t.Fatalf("local admin: %v", err)
		}
		if err := s.DeleteUser(ctx, u.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete twice: %v, want ErrNotFound", err)
		}
	})
}
