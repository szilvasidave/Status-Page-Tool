package store

// The local administrator account: its details (account page) and the switch
// that allows or forbids its sign-in (Settings.LocalLoginEnabled, set on the
// settings page and with "statuspage local-login").

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

// ErrUsernameTaken is returned by UpdateLocalAdmin when another local account
// already has the user name.
var ErrUsernameTaken = errors.New("another local account already has this user name")

// Reasons returned by CheckLocalLoginOff.
var (
	// ErrSSONotConfigured: single sign-on is not enabled and configured.
	ErrSSONotConfigured = errors.New("single sign-on is not enabled and configured")
	// ErrNoAdminGroup: no group has the Administrator role.
	ErrNoAdminGroup = errors.New("no group has the Administrator role")
)

// localLoginKey is the settings key of Settings.LocalLoginEnabled (see
// settingFields).
const localLoginKey = "local_login_enabled"

// deleteLocalAdminSessionsSQL signs out all local administrator accounts.
const deleteLocalAdminSessionsSQL = `DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE is_local_admin = ?)`

// UpdateLocalAdmin changes the user name and the display name of a local
// administrator account; the caller validates them. The user name is stored
// in lower case, like CreateLocalAdmin does. Returns ErrNotFound if id is not
// a local administrator and ErrUsernameTaken if another local account has
// the user name (upper and lower case do not matter).
func (s *Store) UpdateLocalAdmin(ctx context.Context, id int64, username, name string) error {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return errors.New("change administrator: the username must not be empty")
	}
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		if _, err := findUser(ctx, tx, "LOWER(u.username) = ? AND u.id <> ?", username, id); err == nil {
			return ErrUsernameTaken
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		return affected(tx.ExecContext(ctx, `UPDATE users SET username = ?, name = ? WHERE id = ? AND is_local_admin = ?`,
			username, strings.TrimSpace(name), id, true))
	})
	switch {
	case err == nil, errors.Is(err, ErrNotFound), errors.Is(err, ErrUsernameTaken):
		return err
	case isConflict(err):
		return ErrUsernameTaken // PostgreSQL: taken by a concurrent change after the check
	}
	return fmt.Errorf("change administrator %d: %w", id, err)
}

// CheckLocalLoginOff reports whether sign-in with the local administrator
// account may be turned off with the settings st without locking everybody
// out: single sign-on must be enabled and configured (issuer URL, client ID
// and client secret), and at least one group must have the Administrator
// role. It returns nil, ErrSSONotConfigured, ErrNoAdminGroup or a database
// error.
func (s *Store) CheckLocalLoginOff(ctx context.Context, st *Settings) error {
	if !st.OIDCConfigured() || st.OIDCClientSecret == "" {
		return ErrSSONotConfigured
	}
	n, err := s.AdminGroupCount(ctx, 0)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNoAdminGroup
	}
	return nil
}

// AdminGroupCount returns the number of groups with the Administrator role,
// not counting the group except (0 = count all). While the sign-in with the
// local administrator account is off, the groups page uses it to keep the
// last of them (see CheckLocalLoginOff).
func (s *Store) AdminGroupCount(ctx context.Context, except int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM idp_groups WHERE role = ? AND id <> ?`,
		roleValue(RoleAdmin), except).Scan(&n); err != nil {
		return 0, fmt.Errorf("check for groups with the Administrator role: %w", err)
	}
	return n, nil
}

// DeleteLocalAdminSessions signs out all local administrator accounts. The
// settings page calls it when it turns their sign-in on again (like
// SetLocalLoginEnabled), so that sessions from before it was turned off do
// not come back to life.
func (s *Store) DeleteLocalAdminSessions(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, deleteLocalAdminSessionsSQL, true); err != nil {
		return fmt.Errorf("sign out the local administrator: %w", err)
	}
	return nil
}

// LocalAdminUsernames returns the user names of the local administrator
// accounts, sorted. The command line names them when it does not know a
// name, since the name can be changed on the account page.
func (s *Store) LocalAdminUsernames(ctx context.Context) ([]string, error) {
	names, err := queryAll(ctx, s.db, func(sc scanner) (string, error) {
		var name string
		err := sc.Scan(&name)
		return name, err
	}, `SELECT username FROM users WHERE is_local_admin = ? AND username <> '' ORDER BY username`, true)
	if err != nil {
		return nil, fmt.Errorf("list the local administrator accounts: %w", err)
	}
	return names, nil
}

// SetLocalLoginEnabled turns sign-in with the local administrator account on
// or off without changing other settings (used by "statuspage local-login",
// which checks CheckLocalLoginOff before turning it off). It reports whether
// the setting changed. When it changes, all sessions of local administrator
// accounts are deleted: turned off, they would not work anyway, and turned
// on again, sessions from before must not come back to life. A running
// server picks up the change within settingsTTL.
func (s *Store) SetLocalLoginEnabled(ctx context.Context, on bool) (changed bool, err error) {
	err = s.db.InTx(ctx, func(tx *db.Tx) error {
		var v string
		err := notFound(tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, localLoginKey).Scan(&v))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		current := DefaultSettings().LocalLoginEnabled // not stored yet, or invalid (like GetSettings)
		if b, perr := strconv.ParseBool(v); err == nil && perr == nil {
			current = b
		}
		if current == on {
			return nil
		}
		changed = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, localLoginKey, strconv.FormatBool(on)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, deleteLocalAdminSessionsSQL, true)
		return err
	})
	s.settingsMu.Lock()
	s.settingsCache = nil
	s.settingsMu.Unlock()
	if err != nil {
		return false, fmt.Errorf("save the local administrator sign-in setting: %w", err)
	}
	return changed, nil
}
