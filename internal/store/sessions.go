package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CreateSession stores a session (ID is the token hash). A zero CreatedAt is
// set to now.
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	if sess.ID == "" {
		return errors.New("create session: empty session id")
	}
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = nowTime()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, user_id, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		sess.ID, sess.UserID, sess.ExpiresAt.Unix(), sess.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession returns a non-expired session by id hash or ErrNotFound.
func (s *Store) GetSession(ctx context.Context, id string, now time.Time) (*Session, error) {
	var (
		sess             Session
		expires, created int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, expires_at, created_at FROM sessions WHERE id = ? AND expires_at > ?`,
		id, now.Unix()).Scan(&sess.ID, &sess.UserID, &expires, &created)
	if err != nil {
		if err = notFound(err); errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("get session: %w", err)
	}
	sess.ExpiresAt = fromUnix(expires)
	sess.CreatedAt = fromUnix(created)
	return &sess, nil
}

// DeleteSession removes a session. Deleting an unknown session is not an
// error.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteUserSessions removes all sessions of a user.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete sessions of user %d: %w", userID, err)
	}
	return nil
}

// DeleteExpiredSessions removes sessions with expires_at <= now.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.Unix()); err != nil {
		return fmt.Errorf("delete expired sessions: %w", err)
	}
	return nil
}
