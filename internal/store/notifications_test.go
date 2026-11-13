package store

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRecipientsForServices(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		ops := mustGroup(t, s, "ops", "")
		dev := mustGroup(t, s, "dev", "")
		mustGroup(t, s, "other", "")
		mail := mustService(t, s, "Mail", "", 0, ops.ID)
		vpn := mustService(t, s, "VPN", "", 0, ops.ID, dev.ID)
		wiki := mustService(t, s, "Wiki", "", 0)

		zed := mustUser(t, s, "1", "zed@example.com", "Zed")
		amy := mustUser(t, s, "2", "amy@example.com", "Amy")
		nomail := mustUser(t, s, "3", "", "No Mail")
		outsider := mustUser(t, s, "4", "out@example.com", "Out")
		check(t, s.SetUserGroups(ctx, zed.ID, []GroupRef{{ExternalID: "ops"}, {ExternalID: "dev"}}))
		check(t, s.SetUserGroups(ctx, amy.ID, []GroupRef{{ExternalID: "dev"}}))
		check(t, s.SetUserGroups(ctx, nomail.ID, []GroupRef{{ExternalID: "ops"}}))
		check(t, s.SetUserGroups(ctx, outsider.ID, []GroupRef{{ExternalID: "other"}}))

		r, err := s.RecipientsForServices(ctx, nil)
		check(t, err)
		equal(t, "no services", len(r), 0)

		r, err = s.RecipientsForServices(ctx, []int64{mail.ID})
		check(t, err)
		equal(t, "mail", r, []Recipient{{UserID: zed.ID, Email: "zed@example.com", Name: "Zed"}})

		r, err = s.RecipientsForServices(ctx, []int64{mail.ID, vpn.ID, vpn.ID, wiki.ID})
		check(t, err)
		equal(t, "distinct, ordered by email", r, []Recipient{
			{UserID: amy.ID, Email: "amy@example.com", Name: "Amy"},
			{UserID: zed.ID, Email: "zed@example.com", Name: "Zed"},
		})

		r, err = s.RecipientsForServices(ctx, []int64{wiki.ID, 999})
		check(t, err)
		equal(t, "no groups", len(r), 0)
	})
}

func TestNotificationOutbox(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		u := mustUser(t, s, "1", "u@example.com", "U")
		check(t, s.EnqueueEmails(ctx, nil))
		check(t, s.EnqueueEmails(ctx, []OutboundEmail{
			{UserID: u.ID, Email: "u@example.com", Subject: "first", BodyText: "text", BodyHTML: "<p>html</p>"},
			{Email: "admin@example.com", Subject: "second", BodyText: "text"},
			{UserID: u.ID, Email: "u@example.com", Subject: "third", BodyText: "text"},
		}))
		wantError(t, "missing address", s.EnqueueEmails(ctx, []OutboundEmail{{Subject: "x"}}))

		now := time.Now()
		due, err := s.DueNotifications(ctx, now, 2)
		check(t, err)
		equal(t, "limit", len(due), 2)
		n := due[0]
		equal(t, "oldest first", []string{due[0].Subject, due[1].Subject}, []string{"first", "second"})
		equal(t, "fields", [5]string{n.Email, n.BodyText, n.BodyHTML, n.Status, n.LastError}, [5]string{"u@example.com", "text", "<p>html</p>", NotificationPending, ""})
		equal(t, "user", n.UserID, u.ID)
		equal(t, "no user", due[1].UserID, int64(0))
		equal(t, "attempts", n.Attempts, 0)
		if !recent(n.CreatedAt) || !recent(n.NextAttemptAt) || n.SentAt != nil {
			t.Fatalf("times: %+v", n)
		}
		all, err := s.DueNotifications(ctx, now, 0)
		check(t, err)
		equal(t, "default limit", len(all), 3)
		none, err := s.DueNotifications(ctx, now.Add(-time.Hour), 10)
		check(t, err)
		equal(t, "not due yet", len(none), 0)

		// sent
		check(t, s.MarkNotificationSent(ctx, due[0].ID))
		// failed with retry
		retry := now.Add(10 * time.Minute)
		check(t, s.MarkNotificationAttemptFailed(ctx, due[1].ID, "connection refused", &retry))
		// failed for good, very long error message
		long := strings.Repeat("é", 3000)
		check(t, s.MarkNotificationAttemptFailed(ctx, all[2].ID, long, nil))

		due, err = s.DueNotifications(ctx, now, 10)
		check(t, err)
		equal(t, "nothing due", len(due), 0)
		due, err = s.DueNotifications(ctx, retry, 10)
		check(t, err)
		equal(t, "retry due", len(due), 1)
		equal(t, "retry", [2]any{due[0].Subject, due[0].Attempts}, [2]any{"second", 1})
		equal(t, "retry error", due[0].LastError, "connection refused")
		equal(t, "retry time", due[0].NextAttemptAt, truncate(retry))

		list, err := s.ListNotifications(ctx, 0)
		check(t, err)
		equal(t, "list newest first", []string{list[0].Subject, list[1].Subject, list[2].Subject}, []string{"third", "second", "first"})
		equal(t, "list without bodies", [2]string{list[2].BodyText, list[2].BodyHTML}, [2]string{"", ""})
		equal(t, "list fields", [2]any{list[2].Email, list[2].UserID}, [2]any{"u@example.com", u.ID})
		sent := list[2]
		equal(t, "sent status", sent.Status, NotificationSent)
		equal(t, "sent attempts", sent.Attempts, 1)
		if sent.SentAt == nil || !recent(*sent.SentAt) {
			t.Fatalf("sent_at not set: %+v", sent)
		}
		failed := list[0]
		equal(t, "failed status", failed.Status, NotificationFailed)
		equal(t, "failed attempts", failed.Attempts, 1)
		if !utf8.ValidString(failed.LastError) || utf8.RuneCountInString(failed.LastError) > maxErrorLen+1 {
			t.Fatalf("error not truncated properly: %d runes", utf8.RuneCountInString(failed.LastError))
		}
		list, err = s.ListNotifications(ctx, 1)
		check(t, err)
		equal(t, "list limit", len(list), 1)

		wantNotFound(t, "sent unknown", s.MarkNotificationSent(ctx, 999))
		wantNotFound(t, "failed unknown", s.MarkNotificationAttemptFailed(ctx, 999, "x", nil))
		wantNotFound(t, "retry unknown", s.MarkNotificationAttemptFailed(ctx, 999, "x", &retry))

		// housekeeping keeps pending messages and newer ones
		check(t, s.DeleteNotificationsBefore(ctx, now.Add(-time.Hour)))
		list, err = s.ListNotifications(ctx, 10)
		check(t, err)
		equal(t, "nothing old", len(list), 3)
		check(t, s.DeleteNotificationsBefore(ctx, now.Add(time.Hour)))
		list, err = s.ListNotifications(ctx, 10)
		check(t, err)
		equal(t, "pending kept", len(list), 1)
		equal(t, "pending kept", list[0].Status, NotificationPending)

		// deleting the user keeps the notification
		mustExec(t, s, `DELETE FROM users WHERE id = ?`, u.ID)
		list, err = s.ListNotifications(ctx, 10)
		check(t, err)
		equal(t, "still there", len(list), 1)
	})
}

func TestSessions(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		u := mustUser(t, s, "1", "u@example.com", "U")
		other := mustUser(t, s, "2", "o@example.com", "O")
		now := time.Now()

		sess := &Session{ID: "hash-1", UserID: u.ID, ExpiresAt: now.Add(time.Hour)}
		check(t, s.CreateSession(ctx, sess))
		if !recent(sess.CreatedAt) {
			t.Fatal("created_at not set")
		}
		got, err := s.GetSession(ctx, "hash-1", now)
		check(t, err)
		equal(t, "session", *got, Session{ID: "hash-1", UserID: u.ID, ExpiresAt: truncate(sess.ExpiresAt), CreatedAt: truncate(sess.CreatedAt)})

		_, err = s.GetSession(ctx, "hash-1", now.Add(time.Hour+time.Second))
		wantNotFound(t, "expired", err)
		_, err = s.GetSession(ctx, "unknown", now)
		wantNotFound(t, "unknown", err)
		wantError(t, "duplicate id", s.CreateSession(ctx, &Session{ID: "hash-1", UserID: u.ID, ExpiresAt: now}))
		wantError(t, "empty id", s.CreateSession(ctx, &Session{UserID: u.ID, ExpiresAt: now}))
		wantError(t, "unknown user", s.CreateSession(ctx, &Session{ID: "x", UserID: 999, ExpiresAt: now}))

		check(t, s.DeleteSession(ctx, "hash-1"))
		_, err = s.GetSession(ctx, "hash-1", now)
		wantNotFound(t, "deleted", err)
		check(t, s.DeleteSession(ctx, "hash-1")) // idempotent

		check(t, s.CreateSession(ctx, &Session{ID: "a", UserID: u.ID, ExpiresAt: now.Add(time.Hour)}))
		check(t, s.CreateSession(ctx, &Session{ID: "b", UserID: u.ID, ExpiresAt: now.Add(time.Hour)}))
		check(t, s.CreateSession(ctx, &Session{ID: "c", UserID: other.ID, ExpiresAt: now.Add(time.Hour)}))
		check(t, s.CreateSession(ctx, &Session{ID: "old", UserID: other.ID, ExpiresAt: now.Add(-time.Minute)}))
		check(t, s.DeleteUserSessions(ctx, u.ID))
		_, err = s.GetSession(ctx, "a", now)
		wantNotFound(t, "user sessions deleted", err)
		_, err = s.GetSession(ctx, "c", now)
		check(t, err)

		check(t, s.DeleteExpiredSessions(ctx, now))
		var n int
		check(t, s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n))
		equal(t, "only valid left", n, 1)

		// sessions go away with their user
		mustExec(t, s, `DELETE FROM users WHERE id = ?`, other.ID)
		_, err = s.GetSession(ctx, "c", now)
		wantNotFound(t, "user deleted", err)
	})
}
