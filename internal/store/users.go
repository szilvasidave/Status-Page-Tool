package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

// userCols are the columns read by scanUser (table alias u).
const userCols = `u.id, u.email, u.name, u.oidc_subject, u.directory_id, u.username,
	u.password_hash, u.is_local_admin, u.last_login_at, u.created_at,
	u.principal_name, u.scim_external_id, u.active, u.provisioned`

// userOrderSQL orders users by display name (table alias u).
var userOrderSQL = "LOWER(" + displayNameSQL("u") + "), u.id"

func scanUser(sc scanner) (User, error) {
	var (
		u                         User
		subject, dir, uname, hash sql.NullString
		lastLogin                 sql.NullInt64
		created                   int64
	)
	err := sc.Scan(&u.ID, &u.Email, &u.Name, &subject, &dir, &uname, &hash, &u.IsLocalAdmin, &lastLogin, &created,
		&u.PrincipalName, &u.SCIMExternalID, &u.Active, &u.Provisioned)
	if err != nil {
		return u, err
	}
	u.OIDCSubject = subject.String
	u.DirectoryID = dir.String
	u.Username = uname.String
	u.PasswordHash = hash.String
	u.LastLoginAt = fromNullUnix(lastLogin)
	u.CreatedAt = fromUnix(created)
	return u, nil
}

// findUser returns the first user matching cond (SQL with alias u) or
// ErrNotFound.
func findUser(ctx context.Context, q db.Querier, cond string, args ...any) (*User, error) {
	u, err := scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE `+cond+` ORDER BY u.id LIMIT 1`, args...))
	if err != nil {
		return nil, notFound(err)
	}
	return &u, nil
}

// GetUser returns the user with id or ErrNotFound.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	u, err := findUser(ctx, s.db, "u.id = ?", id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("get user %d: %w", id, err)
	}
	return u, err
}

// GetUserByUsername returns the local account with the given username
// (case-insensitive) or ErrNotFound.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return nil, ErrNotFound
	}
	u, err := findUser(ctx, s.db, "LOWER(u.username) = ?", username)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("get user %q: %w", username, err)
	}
	return u, err
}

// HasLocalAdmin reports whether at least one local administrator exists.
// The first-run setup is complete once this is true.
func (s *Store) HasLocalAdmin(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE is_local_admin = ?`, true).Scan(&n); err != nil {
		return false, fmt.Errorf("check for local administrator: %w", err)
	}
	return n > 0, nil
}

// CreateLocalAdmin creates a local administrator account (username stored
// lower case). Returns an error if the username is empty or taken.
func (s *Store) CreateLocalAdmin(ctx context.Context, username, name, passwordHash string) (*User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return nil, errors.New("create administrator: the username must not be empty")
	}
	if passwordHash == "" {
		return nil, errors.New("create administrator: a password is required")
	}
	var u *User
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		if _, err := findUser(ctx, tx, "LOWER(u.username) = ?", username); err == nil {
			return fmt.Errorf("the username %q is already taken", username)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		var id int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO users (email, name, username, password_hash, is_local_admin, created_at)
			 VALUES ('', ?, ?, ?, ?, ?) RETURNING id`,
			strings.TrimSpace(name), username, passwordHash, true, db.Now()).Scan(&id)
		if err != nil {
			return err
		}
		u, err = findUser(ctx, tx, "u.id = ?", id)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("create administrator: %w", err)
	}
	return u, nil
}

// SetPasswordHash replaces the password hash of a local account. Returns
// ErrNotFound if there is no local account with that id.
func (s *Store) SetPasswordHash(ctx context.Context, userID int64, hash string) error {
	err := affected(s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE id = ? AND username IS NOT NULL`, hash, userID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("set password of user %d: %w", userID, err)
	}
	return err
}

// UpsertOIDCUser finds the user for a successful OIDC login and updates it,
// or creates it. Matching order: oidc_subject, then directory_id (if given),
// then email (if given, case-insensitive; with a directory id only rows
// without one, so that the row of another directory object is never taken
// over). The matched user gets oidc_subject (taken over if it changed),
// email (lower case, if given), name (if given), directory_id (if given) and
// last_login_at=now updated. When the subject of a row changes, the row's
// sessions are deleted (they belong to the previous identity). Local admin
// accounts are never matched or modified (they have no e-mail).
// Must not fail on unique constraints when another row already holds the
// e-mail/directory id: the first match wins and conflicting values are
// cleared from the other row.
func (s *Store) UpsertOIDCUser(ctx context.Context, in OIDCUser) (*User, error) {
	subject := strings.TrimSpace(in.Subject)
	if subject == "" {
		return nil, errors.New("sign-in: the identity provider did not return a subject (sub claim)")
	}
	var out *User
	upsert := func(tx *db.Tx) error {
		email := normEmail(in.Email)
		name := strings.TrimSpace(in.Name)
		dir := strings.TrimSpace(in.DirectoryID)

		const notAdmin = " AND u.is_local_admin = ?"
		u, err := findUser(ctx, tx, "u.oidc_subject = ?"+notAdmin, subject, false)
		if errors.Is(err, ErrNotFound) && dir != "" {
			u, err = findUser(ctx, tx, "u.directory_id = ?"+notAdmin, dir, false)
		}
		if errors.Is(err, ErrNotFound) && email != "" {
			cond := "u.email = ? AND u.email <> ''"
			if dir != "" {
				cond += " AND u.directory_id IS NULL"
			}
			u, err = findUser(ctx, tx, cond+notAdmin, email, false)
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		var id int64 // 0 = create a new user
		if u != nil {
			id = u.ID
			if u.OIDCSubject != "" && u.OIDCSubject != subject {
				if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
					return err
				}
			}
		}

		// Free unique values that are held by other rows. (The subject, and
		// the directory id of a new user, can only be held by a row that a
		// concurrent transaction created after the lookup: then the write
		// below fails and the lookup is repeated.) The repeated
		// "email <> ''" lets SQLite use the partial index users_email_idx.
		if dir != "" && u != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET directory_id = NULL WHERE directory_id = ? AND id <> ?`, dir, id); err != nil {
				return err
			}
		}
		if email != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET email = '' WHERE email = ? AND email <> '' AND id <> ?`, email, id); err != nil {
				return err
			}
		}

		now := db.Now()
		if u == nil {
			err = tx.QueryRowContext(ctx,
				`INSERT INTO users (email, name, oidc_subject, directory_id, last_login_at, created_at)
				 VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
				email, name, subject, nullString(dir), now, now).Scan(&id)
			if err != nil {
				return err
			}
		} else {
			if email == "" {
				email = u.Email
			}
			if name == "" {
				name = u.Name
			}
			if dir == "" {
				dir = u.DirectoryID
			}
			_, err = tx.ExecContext(ctx,
				`UPDATE users SET oidc_subject = ?, email = ?, name = ?, directory_id = ?, last_login_at = ? WHERE id = ?`,
				subject, email, name, nullString(dir), now, id)
			if err != nil {
				return err
			}
		}
		out, err = findUser(ctx, tx, "u.id = ?", id)
		return err
	}
	// PostgreSQL: a concurrent sign-in or directory sync may create or
	// change the same user after the lookup (duplicate key, or a deadlock).
	// Look again; then the row is found.
	err := s.db.InTx(ctx, upsert)
	for attempt := 1; attempt < 3 && isConflict(err); attempt++ {
		err = s.db.InTx(ctx, upsert)
	}
	if err != nil {
		return nil, fmt.Errorf("save signed-in user %q: %w", subject, err)
	}
	return out, nil
}

// insertMembershipSQL adds a user to a group. A sign-in and a directory sync
// may insert the same membership at the same time (PostgreSQL), and a
// membership kept from the sync may be in the claim, too.
const insertMembershipSQL = `INSERT INTO user_groups (user_id, group_id) VALUES (?, ?) ON CONFLICT DO NOTHING`

// SetUserGroups replaces all group memberships of a user with the given
// groups (SetLoginGroups without keepSynced).
func (s *Store) SetUserGroups(ctx context.Context, userID int64, groups []GroupRef) error {
	return s.SetLoginGroups(ctx, userID, groups, false)
}

// SetLoginGroups sets the group memberships of a user to the groups reported
// by the identity provider at sign-in: memberships of other groups are
// removed. Groups are created if they do not exist yet (keyed by ExternalID,
// see NormExternalID; Name defaults to ExternalID). An existing group's name
// is only overwritten when ref.Name is non-empty and differs from
// ExternalID. Refs with an empty ExternalID are ignored.
//
// With keepSynced (directory sync enabled), memberships of groups the
// directory sync manages are not removed: only the sync removes those. These
// are the groups it currently synchronises (see GroupsToSync) and has
// synchronised at least once (last_synced_at set).
func (s *Store) SetLoginGroups(ctx context.Context, userID int64, groups []GroupRef, keepSynced bool) error {
	// Unique refs (the first name wins), sorted so that concurrent logins
	// lock the group rows in the same order.
	names := make(map[string]string)
	var exts []string
	for _, ref := range groups {
		ext := NormExternalID(ref.ExternalID)
		if _, dup := names[ext]; ext == "" || dup {
			continue
		}
		names[ext] = strings.TrimSpace(ref.Name)
		exts = append(exts, ext)
	}
	slices.Sort(exts)

	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		var ids []int64
		for _, ext := range exts {
			g, err := ensureGroup(ctx, tx, ext, names[ext])
			if err != nil {
				return err
			}
			ids = append(ids, g)
		}
		del := `DELETE FROM user_groups WHERE user_id = ?`
		if keepSynced {
			del += ` AND group_id NOT IN (SELECT g.id FROM idp_groups g WHERE g.last_synced_at IS NOT NULL AND ` + groupInUseSQL + `)`
		}
		if _, err := tx.ExecContext(ctx, del, userID); err != nil {
			return err
		}
		for _, gid := range ids {
			if _, err := tx.ExecContext(ctx, insertMembershipSQL, userID, gid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("save groups of user %d: %w", userID, err)
	}
	return nil
}

// ensureGroup returns the id of the group with external id ext, creating it
// if needed. The name is updated when name is non-empty and differs from ext.
func ensureGroup(ctx context.Context, tx *db.Tx, ext, name string) (int64, error) {
	initial := name
	if initial == "" {
		initial = ext
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO idp_groups (external_id, name, role, created_at) VALUES (?, ?, '', ?)
		 ON CONFLICT (external_id) DO NOTHING`, ext, initial, db.Now()); err != nil {
		return 0, err
	}
	var (
		id      int64
		current string
	)
	if err := tx.QueryRowContext(ctx, `SELECT id, name FROM idp_groups WHERE external_id = ?`, ext).Scan(&id, &current); err != nil {
		return 0, err
	}
	if name != "" && name != ext && name != current {
		if _, err := tx.ExecContext(ctx, `UPDATE idp_groups SET name = ? WHERE id = ?`, name, id); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// effectiveRole computes a user's role from the local admin flag and the
// roles of the user's groups.
func effectiveRole(localAdmin bool, groupRoles []Role) Role {
	if localAdmin {
		return RoleAdmin
	}
	r := RoleViewer
	for _, gr := range groupRoles {
		if gr > r {
			r = gr
		}
	}
	return r
}

// UserRole returns the effective role of a user: RoleAdmin for local admins,
// otherwise the highest role of the user's groups, but at least RoleViewer.
// Returns ErrNotFound for an unknown user.
func (s *Store) UserRole(ctx context.Context, userID int64) (Role, error) {
	var localAdmin bool
	err := s.db.QueryRowContext(ctx, `SELECT is_local_admin FROM users WHERE id = ?`, userID).Scan(&localAdmin)
	if err != nil {
		if err = notFound(err); errors.Is(err, ErrNotFound) {
			return RoleNone, err
		}
		return RoleNone, fmt.Errorf("get role of user %d: %w", userID, err)
	}
	if localAdmin {
		return RoleAdmin, nil
	}
	roles, err := queryAll(ctx, s.db, func(sc scanner) (Role, error) {
		var r string
		err := sc.Scan(&r)
		return ParseRole(r), err
	}, `SELECT g.role FROM idp_groups g JOIN user_groups ug ON ug.group_id = g.id WHERE ug.user_id = ?`, userID)
	if err != nil {
		return RoleNone, fmt.Errorf("get role of user %d: %w", userID, err)
	}
	return effectiveRole(false, roles), nil
}

// UserGroups returns the groups a user is a member of, ordered by name
// (MemberCount and ServiceIDs filled).
func (s *Store) UserGroups(ctx context.Context, userID int64) ([]Group, error) {
	groups, err := s.listGroups(ctx, `WHERE g.id IN (SELECT group_id FROM user_groups WHERE user_id = ?)`, userID)
	if err != nil {
		return nil, fmt.Errorf("list groups of user %d: %w", userID, err)
	}
	return groups, nil
}

// ListUsers returns all users with their groups and effective role, ordered
// by name/email.
func (s *Store) ListUsers(ctx context.Context) ([]UserWithGroups, error) {
	users, err := queryAll(ctx, s.db, scanUser, `SELECT `+userCols+` FROM users u ORDER BY `+userOrderSQL)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	type membership struct {
		userID int64
		ref    GroupRef
		role   Role
	}
	members, err := queryAll(ctx, s.db, func(sc scanner) (membership, error) {
		var m membership
		var role string
		err := sc.Scan(&m.userID, &m.ref.ExternalID, &m.ref.Name, &role)
		if m.ref.Name == "" {
			m.ref.Name = m.ref.ExternalID
		}
		m.role = ParseRole(role)
		return m, err
	}, `SELECT ug.user_id, g.external_id, g.name, g.role
	    FROM user_groups ug JOIN idp_groups g ON g.id = ug.group_id
	    ORDER BY LOWER(COALESCE(NULLIF(g.name, ''), g.external_id)), g.id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	groups := make(map[int64][]GroupRef)
	roles := make(map[int64][]Role)
	for _, m := range members {
		groups[m.userID] = append(groups[m.userID], m.ref)
		roles[m.userID] = append(roles[m.userID], m.role)
	}
	out := make([]UserWithGroups, len(users))
	for i, u := range users {
		out[i] = UserWithGroups{User: u, Role: effectiveRole(u.IsLocalAdmin, roles[u.ID]), Groups: groups[u.ID]}
	}
	return out, nil
}
