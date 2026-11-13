package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

// maxErrorLen limits the stored length of a delivery error message.
const maxErrorLen = 1000

const notificationCols = `id, user_id, email, subject, body_text, body_html, status, attempts, last_error,
	next_attempt_at, created_at, sent_at`

// notificationListCols are notificationCols with empty bodies, for lists
// that do not show them (one e-mail can be several megabytes).
const notificationListCols = `id, user_id, email, subject, '', '', status, attempts, last_error,
	next_attempt_at, created_at, sent_at`

func scanNotification(sc scanner) (Notification, error) {
	var (
		n              Notification
		userID, sentAt sql.NullInt64
		next, created  int64
	)
	err := sc.Scan(&n.ID, &userID, &n.Email, &n.Subject, &n.BodyText, &n.BodyHTML, &n.Status, &n.Attempts,
		&n.LastError, &next, &created, &sentAt)
	n.UserID = userID.Int64
	n.NextAttemptAt = fromUnix(next)
	n.CreatedAt = fromUnix(created)
	n.SentAt = fromNullUnix(sentAt)
	return n, err
}

// RecipientsForServices returns the distinct users (with a non-empty e-mail)
// that are members of a group assigned to any of the given services, ordered
// by e-mail.
func (s *Store) RecipientsForServices(ctx context.Context, serviceIDs []int64) ([]Recipient, error) {
	ids := uniqueIDs(serviceIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	in, args := inList(ids)
	out, err := queryAll(ctx, s.db, func(sc scanner) (Recipient, error) {
		var r Recipient
		return r, sc.Scan(&r.UserID, &r.Email, &r.Name)
	}, `SELECT DISTINCT u.id, u.email, u.name
	    FROM users u
	    JOIN user_groups ug ON ug.user_id = u.id
	    JOIN service_groups sg ON sg.group_id = ug.group_id
	    WHERE sg.service_id IN (`+in+`) AND u.email <> ''
	    ORDER BY u.email, u.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("find e-mail recipients: %w", err)
	}
	return out, nil
}

// EnqueueEmails inserts pending notifications (next_attempt_at=now).
func (s *Store) EnqueueEmails(ctx context.Context, msgs []OutboundEmail) error {
	if len(msgs) == 0 {
		return nil
	}
	now := db.Now()
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		for _, m := range msgs {
			if m.Email == "" {
				return fmt.Errorf("message %q has no recipient address", m.Subject)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO notifications (user_id, email, subject, body_text, body_html, status, attempts, last_error, next_attempt_at, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, 0, '', ?, ?)`,
				nullID(m.UserID), m.Email, m.Subject, m.BodyText, m.BodyHTML, NotificationPending, now, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("queue e-mails: %w", err)
	}
	return nil
}

// DueNotifications returns up to limit pending notifications whose
// next_attempt_at <= now, oldest first. A limit <= 0 means 100.
func (s *Store) DueNotifications(ctx context.Context, now time.Time, limit int) ([]Notification, error) {
	if limit <= 0 {
		limit = 100
	}
	out, err := queryAll(ctx, s.db, scanNotification,
		`SELECT `+notificationCols+` FROM notifications WHERE status = ? AND next_attempt_at <= ?
		 ORDER BY created_at, id LIMIT ?`, NotificationPending, now.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("list due e-mails: %w", err)
	}
	return out, nil
}

// MarkNotificationSent marks a notification as sent (sent_at=now, attempts+1).
// Returns ErrNotFound for an unknown notification.
func (s *Store) MarkNotificationSent(ctx context.Context, id int64) error {
	err := affected(s.db.ExecContext(ctx,
		`UPDATE notifications SET status = ?, sent_at = ?, attempts = attempts + 1 WHERE id = ?`,
		NotificationSent, db.Now(), id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("mark e-mail %d as sent: %w", id, err)
	}
	return err
}

// MarkNotificationAttemptFailed records a failed attempt: attempts+1,
// last_error=errMsg; if retryAt is nil the status becomes failed, otherwise it
// stays pending with next_attempt_at=*retryAt. Returns ErrNotFound for an
// unknown notification.
func (s *Store) MarkNotificationAttemptFailed(ctx context.Context, id int64, errMsg string, retryAt *time.Time) error {
	if r := []rune(errMsg); len(r) > maxErrorLen {
		errMsg = string(r[:maxErrorLen]) + "…"
	}
	var res sql.Result
	var err error
	if retryAt == nil {
		res, err = s.db.ExecContext(ctx,
			`UPDATE notifications SET status = ?, attempts = attempts + 1, last_error = ? WHERE id = ?`,
			NotificationFailed, errMsg, id)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE notifications SET status = ?, attempts = attempts + 1, last_error = ?, next_attempt_at = ? WHERE id = ?`,
			NotificationPending, errMsg, retryAt.Unix(), id)
	}
	if err = affected(res, err); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("record failed e-mail %d: %w", id, err)
	}
	return err
}

// ListNotifications returns the most recent notifications, newest first,
// without their bodies (BodyText and BodyHTML are empty). A limit <= 0
// means 100.
func (s *Store) ListNotifications(ctx context.Context, limit int) ([]Notification, error) {
	if limit <= 0 {
		limit = 100
	}
	out, err := queryAll(ctx, s.db, scanNotification,
		`SELECT `+notificationListCols+` FROM notifications ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list e-mails: %w", err)
	}
	return out, nil
}

// DeleteNotificationsBefore removes sent/failed notifications created before t
// (housekeeping). Pending notifications are kept.
func (s *Store) DeleteNotificationsBefore(ctx context.Context, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notifications WHERE status IN (?, ?) AND created_at < ?`,
		NotificationSent, NotificationFailed, t.Unix())
	if err != nil {
		return fmt.Errorf("delete old e-mails: %w", err)
	}
	return nil
}
