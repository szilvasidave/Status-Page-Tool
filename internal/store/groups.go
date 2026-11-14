package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

// groupCols are the columns read by scanGroup (table alias g).
const groupCols = `g.id, g.external_id, g.name, g.role, g.last_synced_at, g.created_at,
	(SELECT COUNT(*) FROM user_groups ug WHERE ug.group_id = g.id), g.provisioned,
	g.directory_name, g.directory_checked_at, g.directory_found_at`

// groupNameSQL is the SQL expression for the display name of a group row
// with alias g, like Group.DisplayName.
const groupNameSQL = "COALESCE(NULLIF(g.name, ''), NULLIF(g.directory_name, ''), g.external_id)"

const groupOrderSQL = "LOWER(" + groupNameSQL + "), g.id"

func scanGroup(sc scanner) (Group, error) {
	var (
		g       Group
		role    string
		synced  sql.NullInt64
		created int64
		checked sql.NullInt64
		found   sql.NullInt64
	)
	err := sc.Scan(&g.ID, &g.ExternalID, &g.Name, &role, &synced, &created, &g.MemberCount, &g.Provisioned,
		&g.DirectoryName, &checked, &found)
	g.Role = ParseRole(role)
	g.LastSyncedAt = fromNullUnix(synced)
	g.CreatedAt = fromUnix(created)
	g.DirectoryCheckedAt = fromNullUnix(checked)
	g.DirectoryFoundAt = fromNullUnix(found)
	return g, err
}

// ownGroupName returns the name to store as a group's own name: trimmed, and
// "" if it is just the external id (see NormExternalID), so that the group
// shows its directory name, else its external id. Any other name, also one
// equal to the directory name, is kept: an administrator may pin a name
// against renames in the directory.
func ownGroupName(name, externalID string) string {
	name = strings.TrimSpace(name)
	if NormExternalID(name) == externalID {
		return ""
	}
	return name
}

// roleValue is the value stored in idp_groups.role: "" for groups without
// a special role (members are viewers), otherwise "editor" or "admin".
func roleValue(r Role) string {
	if r == RoleEditor || r == RoleAdmin {
		return r.String()
	}
	return ""
}

// listGroups returns the groups matching where (SQL with alias g, may be
// empty) ordered by display name, with ServiceIDs filled.
func (s *Store) listGroups(ctx context.Context, where string, args ...any) ([]Group, error) {
	groups, err := queryAll(ctx, s.db, scanGroup, `SELECT `+groupCols+` FROM idp_groups g `+where+` ORDER BY `+groupOrderSQL, args...)
	if err != nil || len(groups) == 0 {
		return groups, err
	}
	ids := make([]int64, len(groups))
	for i, g := range groups {
		ids[i] = g.ID
	}
	in, inArgs := inList(ids)
	type link struct{ group, service int64 }
	links, err := queryAll(ctx, s.db, func(sc scanner) (link, error) {
		var l link
		return l, sc.Scan(&l.group, &l.service)
	}, `SELECT group_id, service_id FROM service_groups WHERE group_id IN (`+in+`) ORDER BY service_id`, inArgs...)
	if err != nil {
		return nil, err
	}
	byGroup := make(map[int64][]int64)
	for _, l := range links {
		byGroup[l.group] = append(byGroup[l.group], l.service)
	}
	for i := range groups {
		groups[i].ServiceIDs = byGroup[groups[i].ID]
	}
	return groups, nil
}

// ListGroups returns all groups ordered by display name, with MemberCount and
// ServiceIDs filled.
func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	groups, err := s.listGroups(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	return groups, nil
}

// GetGroup returns one group (MemberCount and ServiceIDs filled) or ErrNotFound.
func (s *Store) GetGroup(ctx context.Context, id int64) (*Group, error) {
	groups, err := s.listGroups(ctx, `WHERE g.id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("get group %d: %w", id, err)
	}
	if len(groups) == 0 {
		return nil, ErrNotFound
	}
	return &groups[0], nil
}

// GroupMembers returns the users in a group ordered by name/email.
func (s *Store) GroupMembers(ctx context.Context, groupID int64) ([]User, error) {
	users, err := queryAll(ctx, s.db, scanUser,
		`SELECT `+userCols+` FROM users u JOIN user_groups ug ON ug.user_id = u.id
		 WHERE ug.group_id = ? ORDER BY `+userOrderSQL, groupID)
	if err != nil {
		return nil, fmt.Errorf("list members of group %d: %w", groupID, err)
	}
	return users, nil
}

// IsGUID reports whether s looks like a GUID such as a Microsoft Entra ID
// object id (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx, any case).
func IsGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// NormExternalID returns a group's external id as it is stored and looked
// up: trimmed, and in lower case if it is a GUID. Entra ID puts object ids
// into the groups claim in lower case, but tools often show them in upper
// case. Other ids (e.g. Keycloak group names) keep their case.
func NormExternalID(s string) string {
	s = strings.TrimSpace(s)
	if IsGUID(s) {
		return strings.ToLower(s)
	}
	return s
}

// CreateGroup adds a group manually (before anyone of it signed in). The
// name is optional and stored as its own name (see ownGroupName). If a
// group with the same external id (see NormExternalID) exists it is
// returned unchanged (its name is set if it was empty/equal to the external
// id and name is given).
func (s *Store) CreateGroup(ctx context.Context, externalID, name string) (*Group, error) {
	externalID = NormExternalID(externalID)
	if externalID == "" {
		return nil, errors.New("add group: the group id must not be empty")
	}
	name = ownGroupName(name, externalID)
	var id int64
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO idp_groups (external_id, name, role, created_at) VALUES (?, ?, '', ?)
			 ON CONFLICT (external_id) DO NOTHING`, externalID, name, db.Now()); err != nil {
			return err
		}
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT id, name FROM idp_groups WHERE external_id = ?`, externalID).Scan(&id, &current); err != nil {
			return err
		}
		if name != "" && name != current && (current == "" || current == externalID) {
			_, err := tx.ExecContext(ctx, `UPDATE idp_groups SET name = ? WHERE id = ?`, name, id)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("add group %q: %w", externalID, err)
	}
	return s.GetGroup(ctx, id)
}

// UpdateGroup sets the own name and the role of a group. RoleNone and
// RoleViewer both mean "no special role". The name of a group that is not
// provisioned is stored as its own name (see ownGroupName); the name of a
// provisioned group is stored as given (provisioning owns it, and SCIM
// needs it). Returns ErrNotFound for an unknown group.
func (s *Store) UpdateGroup(ctx context.Context, id int64, name string, role Role) error {
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		var (
			ext         string
			provisioned bool
		)
		if err := tx.QueryRowContext(ctx, `SELECT external_id, provisioned FROM idp_groups WHERE id = ?`, id).Scan(&ext, &provisioned); err != nil {
			return notFound(err)
		}
		name = strings.TrimSpace(name)
		if !provisioned {
			name = ownGroupName(name, ext)
		}
		return affected(tx.ExecContext(ctx, `UPDATE idp_groups SET name = ?, role = ? WHERE id = ?`, name, roleValue(role), id))
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("update group %d: %w", id, err)
	}
	return err
}

// SetGroupServices replaces the services assigned to a group. Unknown
// service ids are ignored. Returns ErrNotFound for an unknown group.
func (s *Store) SetGroupServices(ctx context.Context, groupID int64, serviceIDs []int64) error {
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM idp_groups WHERE id = ?`, groupID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM service_groups WHERE group_id = ?`, groupID); err != nil {
			return err
		}
		for _, sid := range uniqueIDs(serviceIDs) {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO service_groups (service_id, group_id) SELECT id, ? FROM services WHERE id = ?`, groupID, sid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("assign services to group %d: %w", groupID, err)
	}
	return err
}

// DeleteGroup deletes a group with its memberships and service assignments.
// Returns ErrNotFound for an unknown group.
func (s *Store) DeleteGroup(ctx context.Context, id int64) error {
	err := affected(s.db.ExecContext(ctx, `DELETE FROM idp_groups WHERE id = ?`, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("delete group %d: %w", id, err)
	}
	return err
}

// maxDirectoryName is the longest directory name stored (characters): the
// limit of Microsoft Entra ID.
const maxDirectoryName = 256

// GroupsForNameLookup returns the groups whose names are looked up in
// Microsoft Entra ID and that were not looked up since checkedBefore (the
// zero time: all of them): the groups that are not provisioned
// (provisioning sends the names of its groups), whose external id is an
// object id (GUID) and that have members, a role or services, or were
// recorded after recordedAfter (e.g. added by an administrator a moment
// ago; the zero time: none). A group nobody uses any more keeps its last
// name. Groups never looked up come first, then those looked up longest
// ago.
func (s *Store) GroupsForNameLookup(ctx context.Context, checkedBefore, recordedAfter time.Time) ([]Group, error) {
	used := `g.role <> '' OR EXISTS (SELECT 1 FROM user_groups ug WHERE ug.group_id = g.id)
		OR EXISTS (SELECT 1 FROM service_groups sg WHERE sg.group_id = g.id)`
	args := []any{false}
	if !recordedAfter.IsZero() {
		used += ` OR g.created_at > ?`
		args = append(args, recordedAfter.Unix())
	}
	where := `WHERE g.provisioned = ? AND (` + used + `)`
	if !checkedBefore.IsZero() {
		where += ` AND (g.directory_checked_at IS NULL OR g.directory_checked_at < ?)`
		args = append(args, checkedBefore.Unix())
	}
	groups, err := queryAll(ctx, s.db, scanGroup, `SELECT `+groupCols+` FROM idp_groups g `+where+`
		ORDER BY CASE WHEN g.directory_checked_at IS NULL THEN 0 ELSE 1 END, g.directory_checked_at, g.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list the groups to look up: %w", err)
	}
	return slices.DeleteFunc(groups, func(g Group) bool { return !IsGUID(g.ExternalID) }), nil
}

// SetGroupDirectoryNames stores the outcome of a name lookup made at time
// at: the groups in checked were looked up, and names has the display names
// (by group id) of those that were found. The others keep their directory
// name (e.g. of a group that was deleted in the directory since) and count
// as missing (Group.DirectoryMissing). Control characters are removed from
// the names (tabs and line breaks become spaces). Unknown and provisioned
// groups are left alone: provisioning names those.
func (s *Store) SetGroupDirectoryNames(ctx context.Context, checked []int64, names map[int64]string, at time.Time) error {
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		for _, id := range uniqueIDs(checked) {
			var err error
			if name, found := names[id]; found {
				name = strings.TrimSpace(strings.Map(func(r rune) rune {
					switch {
					case r == '\t' || r == '\n' || r == '\r':
						return ' '
					case unicode.IsControl(r):
						return -1
					}
					return r
				}, name))
				if r := []rune(name); len(r) > maxDirectoryName {
					name = string(r[:maxDirectoryName])
				}
				_, err = tx.ExecContext(ctx, `UPDATE idp_groups SET directory_name = ?, directory_checked_at = ?, directory_found_at = ?
					WHERE id = ? AND provisioned = ?`, name, at.Unix(), at.Unix(), id, false)
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE idp_groups SET directory_checked_at = ? WHERE id = ? AND provisioned = ?`, at.Unix(), id, false)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("save the names of groups: %w", err)
	}
	return nil
}
