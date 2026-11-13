package store

import (
	"context"
	"errors"
	"fmt"
)

// ErrLocalAdmin is returned by DeleteUser for the local administrator
// account, which cannot be removed.
var ErrLocalAdmin = errors.New("the local administrator account cannot be removed")

// DeleteUser removes a user who signs in with single sign-on or was found
// by the directory sync, together with the user's group memberships and
// sessions (ON DELETE CASCADE); incidents, updates and e-mails the user
// created or received are kept. Returns ErrNotFound for an unknown user and
// ErrLocalAdmin for the local administrator. A user who is still a member of
// a group at the identity provider is created again at the next sign-in or
// directory sync.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	var localAdmin bool
	err := s.db.QueryRowContext(ctx, `SELECT is_local_admin FROM users WHERE id = ?`, id).Scan(&localAdmin)
	if err = notFound(err); errors.Is(err, ErrNotFound) {
		return err
	} else if err != nil {
		return fmt.Errorf("delete user %d: %w", id, err)
	}
	if localAdmin {
		return ErrLocalAdmin
	}
	err = affected(s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ? AND is_local_admin = ?`, id, false))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("delete user %d: %w", id, err)
	}
	return err
}
