package store

// Data access for SCIM provisioning from Microsoft Entra ID (package
// internal/scim). Provisioning is authoritative for what it writes:
//
//   - users: user name (principal_name), external id, e-mail, name and
//     active. Every write sets provisioned; sign-in then no longer changes the
//     name, e-mail or user name. A deactivated user (active = false) cannot
//     sign in and receives no e-mails.
//   - groups: display name, external id and members. Every write sets
//     provisioned and last_synced_at; sign-in neither adds nor removes
//     members of provisioned groups.
//
// Provisioning sees every user except the local administrator accounts (so
// that it can take over users who signed in before), but only provisioned
// groups (a group is taken over by creating it, which replaces the members
// recorded from sign-ins).

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/secret"
)

// ErrSCIMConflict is returned, as a *SCIMConflictError, when provisioning
// would create a second user with the same user name or a second group with
// the same display name or external id.
var ErrSCIMConflict = errors.New("conflict")

// SCIMConflictError describes a conflict; errors.Is(err, ErrSCIMConflict)
// reports it.
type SCIMConflictError struct {
	Detail string // e.g. `A user with the user name "jane@contoso.com" exists already.`
}

func (e *SCIMConflictError) Error() string { return e.Detail }
func (e *SCIMConflictError) Unwrap() error { return ErrSCIMConflict }

func scimConflict(format string, args ...any) error {
	return &SCIMConflictError{Detail: fmt.Sprintf(format, args...)}
}

// SCIMGeneratedIDPrefix starts the external id that CreateSCIMGroup gives a
// group created without one; such ids never appear in a groups claim.
const SCIMGeneratedIDPrefix = "scim:"

// provisioningLockID identifies the PostgreSQL advisory lock taken by
// lockProvisioning.
const provisioningLockID = 7395000

// lockProvisioning serialises the transactions that find users or groups by
// name before creating or changing them (provisioning and sign-ins), so that
// two of them cannot both decide that a user does not exist yet, and the
// provisioning writes with SetSCIMToken (see lockSCIMWrite). PostgreSQL
// needs a lock for that; SQLite runs writing transactions one at a time
// anyway (BEGIN IMMEDIATE). Call it first in the transaction.
func (s *Store) lockProvisioning(ctx context.Context, tx *db.Tx) error {
	if !s.db.IsPostgres() {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, provisioningLockID)
	return err
}

// ErrProvisioningOff is returned by the provisioning writes of a request
// whose token was deleted ("Turn off provisioning") or replaced after the
// request was authenticated.
var ErrProvisioningOff = errors.New("provisioning was turned off or its token was replaced")

type scimTokenKey struct{}

// WithSCIMToken returns a context for the provisioning writes of a request
// that was authenticated with the token of this hash: they fail with
// ErrProvisioningOff unless it is still the stored token.
func WithSCIMToken(ctx context.Context, hash string) context.Context {
	return context.WithValue(ctx, scimTokenKey{}, hash)
}

// lockSCIMWrite starts the transaction of a provisioning write: it calls
// lockProvisioning and then checks the token of the request (WithSCIMToken)
// against the stored one. SetSCIMToken takes the same lock, so once turning
// provisioning off has cleared the provisioned flags, no request that was
// authenticated before can set them again. Without WithSCIMToken (tests,
// tools) there is no token to check.
func (s *Store) lockSCIMWrite(ctx context.Context, tx *db.Tx) error {
	if err := s.lockProvisioning(ctx, tx); err != nil {
		return err
	}
	want, ok := ctx.Value(scimTokenKey{}).(string)
	if !ok {
		return nil
	}
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, "scim_token_hash").Scan(&stored)
	if err = notFound(err); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if stored == "" || stored != want {
		return ErrProvisioningOff
	}
	return nil
}

// inTxRetry runs fn in a transaction and repeats it when PostgreSQL reports a
// conflict with a concurrent transaction (see isConflict).
func (s *Store) inTxRetry(ctx context.Context, fn func(tx *db.Tx) error) error {
	err := s.db.InTx(ctx, fn)
	for attempt := 1; attempt < 3 && isConflict(err); attempt++ {
		err = s.db.InTx(ctx, fn)
	}
	return err
}

// ===================================================================== users

// SCIMUserQuery selects users for provisioning; every non-empty field must
// match. Local administrator accounts never match.
type SCIMUserQuery struct {
	ID         int64  // 0 = any
	UserName   string // principal_name, case-insensitive
	ExternalID string // scim_external_id, exact
	Email      string // e-mail address, case-insensitive
}

func (q SCIMUserQuery) where() (string, []any) {
	conds := []string{"u.is_local_admin = ?"}
	args := []any{false}
	if q.ID != 0 {
		conds = append(conds, "u.id = ?")
		args = append(args, q.ID)
	}
	if q.UserName != "" {
		conds = append(conds, "LOWER(u.principal_name) = LOWER(?)")
		args = append(args, q.UserName)
	}
	if q.ExternalID != "" {
		conds = append(conds, "u.scim_external_id = ?")
		args = append(args, q.ExternalID)
	}
	if q.Email != "" {
		// The repeated "email <> ''" lets SQLite use users_email_idx.
		conds = append(conds, "u.email = ? AND u.email <> ''")
		args = append(args, normEmail(q.Email))
	}
	return strings.Join(conds, " AND "), args
}

// SCIMUsers returns at most limit users matching q, ordered by id and
// starting after the first offset matches, and the number of all matches.
func (s *Store) SCIMUsers(ctx context.Context, q SCIMUserQuery, offset, limit int) ([]User, int, error) {
	where, args := q.where()
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users u WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("list users for provisioning: %w", err)
	}
	if limit <= 0 || offset >= total {
		return nil, total, nil
	}
	users, err := queryAll(ctx, s.db, scanUser,
		`SELECT `+userCols+` FROM users u WHERE `+where+` ORDER BY u.id LIMIT ? OFFSET ?`, append(args, limit, max(offset, 0))...)
	if err != nil {
		return nil, 0, fmt.Errorf("list users for provisioning: %w", err)
	}
	return users, total, nil
}

// SCIMUser returns the user with id unless it is a local administrator
// account (ErrNotFound).
func (s *Store) SCIMUser(ctx context.Context, id int64) (*User, error) {
	u, err := findUser(ctx, s.db, "u.id = ? AND u.is_local_admin = ?", id, false)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("get user %d: %w", id, err)
	}
	return u, err
}

// SCIMUserData are the attributes provisioning stores for a user.
type SCIMUserData struct {
	UserName   string // stored as sent (trimmed); required
	ExternalID string
	Email      string // stored in lower case; "" = none
	Name       string
	Active     bool
	// EmailFromUserName: no address was sent, Email was derived from
	// UserName (User.EmailFromUserName).
	EmailFromUserName bool
}

func (in SCIMUserData) clean() SCIMUserData {
	in.UserName = strings.TrimSpace(in.UserName)
	in.ExternalID = strings.TrimSpace(in.ExternalID)
	in.Email = normEmail(in.Email)
	in.Name = strings.TrimSpace(in.Name)
	return in
}

// CreateSCIMUser stores a user created by provisioning. A user who is not
// provisioned yet (e.g. created by an earlier sign-in) with the same user
// name (case-insensitive) or, failing that, the same e-mail address is taken
// over instead of creating a second one (adopted = true). A provisioned user
// with the same user name already exists: an error wrapping ErrSCIMConflict.
func (s *Store) CreateSCIMUser(ctx context.Context, in SCIMUserData) (u *User, adopted bool, err error) {
	in = in.clean()
	if in.UserName == "" {
		return nil, false, errors.New("create user: the user name must not be empty")
	}
	err = s.inTxRetry(ctx, func(tx *db.Tx) error {
		adopted = false
		if err := s.lockSCIMWrite(ctx, tx); err != nil {
			return err
		}
		const notAdmin = " AND u.is_local_admin = ?"
		if _, err := findUser(ctx, tx, "LOWER(u.principal_name) = LOWER(?) AND u.provisioned = ?"+notAdmin, in.UserName, true, false); err == nil {
			return scimConflict("A user with the user name %q exists already.", in.UserName)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		existing, err := findUser(ctx, tx, "LOWER(u.principal_name) = LOWER(?) AND u.principal_name <> '' AND u.provisioned = ?"+notAdmin,
			in.UserName, false, false)
		if errors.Is(err, ErrNotFound) && in.Email != "" {
			existing, err = findUser(ctx, tx, "u.email = ? AND u.email <> '' AND u.provisioned = ?"+notAdmin, in.Email, false, false)
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		var id int64
		wasActive := true // a new row
		if existing != nil {
			id, adopted, wasActive = existing.ID, true, existing.Active
		} else if err := tx.QueryRowContext(ctx, `INSERT INTO users (created_at) VALUES (?) RETURNING id`, db.Now()).Scan(&id); err != nil {
			return err
		}
		if err := writeSCIMUser(ctx, tx, id, wasActive, in); err != nil {
			return err
		}
		u, err = findUser(ctx, tx, "u.id = ?", id)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("provision user %q: %w", in.UserName, err)
	}
	return u, adopted, nil
}

// UpdateSCIMUser changes a user for provisioning (PUT, PATCH): change gets
// the current user and returns the new attributes; its error is returned
// unchanged and nothing is stored. The read and the write happen in one
// transaction, so concurrent changes are not lost. Returns ErrNotFound for
// an unknown user or a local administrator account, and an error wrapping
// ErrSCIMConflict if another provisioned user has the new user name.
func (s *Store) UpdateSCIMUser(ctx context.Context, id int64, change func(cur *User) (SCIMUserData, error)) (*User, error) {
	var (
		out       *User
		changeErr error
	)
	err := s.inTxRetry(ctx, func(tx *db.Tx) error {
		changeErr = nil
		if err := s.lockSCIMWrite(ctx, tx); err != nil {
			return err
		}
		cur, err := findUser(ctx, tx, "u.id = ? AND u.is_local_admin = ?", id, false)
		if err != nil {
			return err
		}
		in, err := change(cur)
		if err != nil {
			changeErr = err
			return err
		}
		in = in.clean()
		if in.UserName == "" {
			return errors.New("the user name must not be empty")
		}
		if _, err := findUser(ctx, tx, "LOWER(u.principal_name) = LOWER(?) AND u.provisioned = ? AND u.is_local_admin = ? AND u.id <> ?",
			in.UserName, true, false, id); err == nil {
			return scimConflict("Another user has the user name %q.", in.UserName)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := writeSCIMUser(ctx, tx, id, cur.Active, in); err != nil {
			return err
		}
		out, err = findUser(ctx, tx, "u.id = ?", id)
		return err
	})
	switch {
	case changeErr != nil:
		return nil, changeErr
	case errors.Is(err, ErrNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("provision user %d: %w", id, err)
	}
	return out, nil
}

// writeSCIMUser stores the provisioned attributes of user id (wasActive: its
// active value before). The e-mail address is taken from any other user that
// still has it (provisioning is authoritative for it). Deactivating a user
// ends their sessions. Activating a user again also unlinks the sign-in
// identity (subject and directory id): Microsoft Entra ID finds users by
// their user name, which may belong to another person by now, so the next
// sign-in links the user again by user name or e-mail address.
func writeSCIMUser(ctx context.Context, tx *db.Tx, id int64, wasActive bool, in SCIMUserData) error {
	if in.Email != "" {
		// The repeated "email <> ''" lets SQLite use users_email_idx.
		if _, err := tx.ExecContext(ctx, `UPDATE users SET email = '' WHERE email = ? AND email <> '' AND id <> ?`, in.Email, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET principal_name = ?, scim_external_id = ?, email = ?, email_from_user_name = ?, name = ?, active = ?, provisioned = ?
		 WHERE id = ?`,
		in.UserName, in.ExternalID, in.Email, in.EmailFromUserName && in.Email != "", in.Name, in.Active, true, id); err != nil {
		return err
	}
	if in.Active && !wasActive {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET oidc_subject = NULL, directory_id = NULL WHERE id = ?`, id); err != nil {
			return err
		}
	}
	if !in.Active || !wasActive {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// ===================================================================== groups

// SCIMGroupQuery selects provisioned groups; every non-empty field must
// match.
type SCIMGroupQuery struct {
	ID          int64   // 0 = any
	DisplayName string  // case-insensitive
	ExternalID  string  // see NormExternalID
	MemberIDs   []int64 // users who are all members
}

func (q SCIMGroupQuery) where() (string, []any) {
	conds := []string{"g.provisioned = ?"}
	args := []any{true}
	if q.ID != 0 {
		conds = append(conds, "g.id = ?")
		args = append(args, q.ID)
	}
	if q.DisplayName != "" {
		conds = append(conds, "LOWER(g.name) = LOWER(?)")
		args = append(args, strings.TrimSpace(q.DisplayName))
	}
	if q.ExternalID != "" {
		conds = append(conds, "g.external_id = ?")
		args = append(args, NormExternalID(q.ExternalID))
	}
	for _, uid := range q.MemberIDs {
		conds = append(conds, "EXISTS (SELECT 1 FROM user_groups ug WHERE ug.group_id = g.id AND ug.user_id = ?)")
		args = append(args, uid)
	}
	return strings.Join(conds, " AND "), args
}

// SCIMGroups returns at most limit provisioned groups matching q (ordered by
// id, starting after the first offset matches; MemberCount filled,
// ServiceIDs not) and the number of all matches.
func (s *Store) SCIMGroups(ctx context.Context, q SCIMGroupQuery, offset, limit int) ([]Group, int, error) {
	where, args := q.where()
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM idp_groups g WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("list groups for provisioning: %w", err)
	}
	if limit <= 0 || offset >= total {
		return nil, total, nil
	}
	groups, err := queryAll(ctx, s.db, scanGroup,
		`SELECT `+groupCols+` FROM idp_groups g WHERE `+where+` ORDER BY g.id LIMIT ? OFFSET ?`, append(args, limit, max(offset, 0))...)
	if err != nil {
		return nil, 0, fmt.Errorf("list groups for provisioning: %w", err)
	}
	return groups, total, nil
}

// SCIMGroup returns the provisioned group with id or ErrNotFound.
func (s *Store) SCIMGroup(ctx context.Context, id int64) (*Group, error) {
	return scimGroup(ctx, s.db, id)
}

func scimGroup(ctx context.Context, q db.Querier, id int64) (*Group, error) {
	g, err := scanGroup(q.QueryRowContext(ctx, `SELECT `+groupCols+` FROM idp_groups g WHERE g.id = ? AND g.provisioned = ?`, id, true))
	if err = notFound(err); errors.Is(err, ErrNotFound) {
		return nil, err
	} else if err != nil {
		return nil, fmt.Errorf("get group %d: %w", id, err)
	}
	return &g, nil
}

// SCIMMember is a member of a group as provisioning sees it.
type SCIMMember struct {
	UserID  int64
	Display string // display name of the user
}

// SCIMGroupMembers returns the members of the given groups by group id,
// ordered by user id. Local administrator accounts are left out.
func (s *Store) SCIMGroupMembers(ctx context.Context, groupIDs []int64) (map[int64][]SCIMMember, error) {
	out := make(map[int64][]SCIMMember)
	ids := uniqueIDs(groupIDs)
	if len(ids) == 0 {
		return out, nil
	}
	in, args := inList(ids)
	type row struct {
		group int64
		m     SCIMMember
	}
	rows, err := queryAll(ctx, s.db, func(sc scanner) (row, error) {
		var r row
		return r, sc.Scan(&r.group, &r.m.UserID, &r.m.Display)
	}, `SELECT ug.group_id, u.id, `+displayNameSQL("u")+`
	    FROM user_groups ug JOIN users u ON u.id = ug.user_id
	    WHERE ug.group_id IN (`+in+`) AND u.is_local_admin = ?
	    ORDER BY ug.group_id, u.id`, append(args, false)...)
	if err != nil {
		return nil, fmt.Errorf("list group members for provisioning: %w", err)
	}
	for _, r := range rows {
		out[r.group] = append(out[r.group], r.m)
	}
	return out, nil
}

// SCIMGroupData are the attributes provisioning stores for a group.
type SCIMGroupData struct {
	DisplayName string  // required
	ExternalID  string  // "" = generated (create) or unchanged (update)
	Members     []int64 // user ids; unknown users and local administrators are ignored
}

// CreateSCIMGroup stores a group created by provisioning. An existing group
// with the same external id (recorded from sign-ins or added by an
// administrator) is taken over (adopted = true) with its role and services;
// its members are replaced by in.Members. A group without external id gets a
// generated one (SCIMGeneratedIDPrefix). Another provisioned group with the
// same display name: an error wrapping ErrSCIMConflict.
func (s *Store) CreateSCIMGroup(ctx context.Context, in SCIMGroupData) (g *Group, adopted bool, err error) {
	name := strings.TrimSpace(in.DisplayName)
	if name == "" {
		return nil, false, errors.New("create group: the display name must not be empty")
	}
	ext := NormExternalID(in.ExternalID)
	if ext == "" {
		ext = SCIMGeneratedIDPrefix + secret.RandomToken(12)
	}
	err = s.inTxRetry(ctx, func(tx *db.Tx) error {
		if err := s.lockSCIMWrite(ctx, tx); err != nil {
			return err
		}
		var existing int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM idp_groups WHERE LOWER(name) = LOWER(?) AND provisioned = ? ORDER BY id LIMIT 1`,
			name, true).Scan(&existing)
		if err == nil {
			return scimConflict("A group with the display name %q exists already.", name)
		} else if err = notFound(err); !errors.Is(err, ErrNotFound) {
			return err
		}
		now := db.Now()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO idp_groups (external_id, name, role, created_at, provisioned, last_synced_at) VALUES (?, ?, '', ?, ?, ?)
			 ON CONFLICT (external_id) DO NOTHING`, ext, name, now, true, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		adopted = n == 0
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM idp_groups WHERE external_id = ?`, ext).Scan(&id); err != nil {
			return err
		}
		if adopted {
			if _, err := tx.ExecContext(ctx, `UPDATE idp_groups SET name = ?, provisioned = ?, last_synced_at = ? WHERE id = ?`,
				name, true, now, id); err != nil {
				return err
			}
		}
		if err := setSCIMMembers(ctx, tx, id, in.Members); err != nil {
			return err
		}
		g, err = scimGroup(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("provision group %q: %w", name, err)
	}
	return g, adopted, nil
}

// UpdateSCIMGroup changes a provisioned group (PUT, PATCH): change gets the
// current attributes and members and modifies them; its error is returned
// unchanged and nothing is stored. The external id is only changed if it was
// generated (SCIMGeneratedIDPrefix); a real one is kept. Returns ErrNotFound
// for an unknown or not provisioned group, and an error wrapping
// ErrSCIMConflict if another provisioned group has the new display name or
// another group has the new external id.
func (s *Store) UpdateSCIMGroup(ctx context.Context, id int64, change func(g *SCIMGroupData) error) (*Group, error) {
	var (
		out       *Group
		changeErr error
	)
	err := s.inTxRetry(ctx, func(tx *db.Tx) error {
		changeErr = nil
		if err := s.lockSCIMWrite(ctx, tx); err != nil {
			return err
		}
		cur, err := scimGroup(ctx, tx, id)
		if err != nil {
			return err
		}
		members, err := groupMemberIDs(ctx, tx, id)
		if err != nil {
			return err
		}
		data := SCIMGroupData{DisplayName: cur.DisplayName(), ExternalID: cur.ExternalID, Members: members}
		if err := change(&data); err != nil {
			changeErr = err
			return err
		}
		name := strings.TrimSpace(data.DisplayName)
		if name == "" {
			return errors.New("the display name must not be empty")
		}
		var other int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM idp_groups WHERE LOWER(name) = LOWER(?) AND provisioned = ? AND id <> ? ORDER BY id LIMIT 1`,
			name, true, id).Scan(&other)
		if err == nil {
			return scimConflict("Another group has the display name %q.", name)
		} else if err = notFound(err); !errors.Is(err, ErrNotFound) {
			return err
		}
		ext := NormExternalID(data.ExternalID)
		if ext == "" || !strings.HasPrefix(cur.ExternalID, SCIMGeneratedIDPrefix) {
			// The external id is the group's id in the groups claim: it
			// decides who gets the group's role once provisioning ends. Only
			// a generated one is replaced (by the first one sent).
			ext = cur.ExternalID
		}
		if ext != cur.ExternalID {
			err = tx.QueryRowContext(ctx, `SELECT id FROM idp_groups WHERE external_id = ?`, ext).Scan(&other)
			if err == nil {
				return scimConflict("Another group has the external id %q.", ext)
			} else if err = notFound(err); !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE idp_groups SET name = ?, external_id = ?, provisioned = ?, last_synced_at = ? WHERE id = ?`,
			name, ext, true, db.Now(), id); err != nil {
			return err
		}
		if err := setSCIMMembers(ctx, tx, id, data.Members); err != nil {
			return err
		}
		out, err = scimGroup(ctx, tx, id)
		return err
	})
	switch {
	case changeErr != nil:
		return nil, changeErr
	case errors.Is(err, ErrNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("provision group %d: %w", id, err)
	}
	return out, nil
}

// DeleteSCIMGroup handles the deletion of a provisioned group: its members
// are removed and it is no longer provisioned, but the group stays with its
// role and services (provisioning it again restores everything). Returns
// ErrNotFound for an unknown or not provisioned group.
func (s *Store) DeleteSCIMGroup(ctx context.Context, id int64) error {
	err := s.inTxRetry(ctx, func(tx *db.Tx) error {
		if err := s.lockSCIMWrite(ctx, tx); err != nil {
			return err
		}
		if _, err := scimGroup(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_groups WHERE group_id = ?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE idp_groups SET provisioned = ?, last_synced_at = ? WHERE id = ?`, false, db.Now(), id)
		return err
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("remove provisioned group %d: %w", id, err)
	}
	return err
}

// groupMemberIDs returns the ids of the members of a group, ascending.
func groupMemberIDs(ctx context.Context, q db.Querier, groupID int64) ([]int64, error) {
	return queryAll(ctx, q, func(sc scanner) (int64, error) {
		var id int64
		return id, sc.Scan(&id)
	}, `SELECT user_id FROM user_groups WHERE group_id = ? ORDER BY user_id`, groupID)
}

// setSCIMMembers makes members the members of group id: memberships of other
// users are removed, new ones are added (unknown users and local
// administrator accounts are skipped).
func setSCIMMembers(ctx context.Context, tx *db.Tx, groupID int64, members []int64) error {
	current, err := groupMemberIDs(ctx, tx, groupID)
	if err != nil {
		return err
	}
	want := make(map[int64]bool, len(members))
	for _, uid := range uniqueIDs(members) {
		want[uid] = true
	}
	for _, uid := range current {
		if want[uid] {
			delete(want, uid) // already a member
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_groups WHERE group_id = ? AND user_id = ?`, groupID, uid); err != nil {
			return err
		}
	}
	added := slices.Sorted(maps.Keys(want))
	for _, uid := range added {
		// A sign-in may add the same membership at the same time.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_groups (user_id, group_id) SELECT id, ? FROM users WHERE id = ? AND is_local_admin = ?
			 ON CONFLICT DO NOTHING`, groupID, uid, false); err != nil {
			return err
		}
	}
	return nil
}

// ===================================================================== token

// SetSCIMToken stores the hash of the provisioning token and when it was
// created (Settings.SCIMTokenHash, SCIMTokenCreatedAt; SaveSettings does not
// store them). An empty hash turns provisioning off: all groups and users are
// then no longer provisioned, so that sign-ins manage the group memberships,
// names and e-mail addresses again. Provisioning writes of requests that were
// authenticated with the previous token fail from then on (lockSCIMWrite).
func (s *Store) SetSCIMToken(ctx context.Context, hash string, created time.Time) error {
	if hash == "" {
		created = time.Time{}
	}
	st := Settings{SCIMTokenHash: hash, SCIMTokenCreatedAt: created}
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		// Waits for running provisioning writes (see lockSCIMWrite).
		if err := s.lockProvisioning(ctx, tx); err != nil {
			return err
		}
		for _, f := range settingFields {
			if f.key != "scim_token_hash" && f.key != "scim_token_created_at" {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO settings (key, value) VALUES (?, ?)
				 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, f.key, f.encode(&st)); err != nil {
				return err
			}
		}
		if hash != "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE idp_groups SET provisioned = ? WHERE provisioned = ?`, false, true); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE users SET provisioned = ? WHERE provisioned = ?`, false, true)
		return err
	})
	s.settingsMu.Lock()
	s.settingsCache = nil
	s.settingsMu.Unlock()
	if err != nil {
		return fmt.Errorf("save the provisioning token: %w", err)
	}
	return nil
}
