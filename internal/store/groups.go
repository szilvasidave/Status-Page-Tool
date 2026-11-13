package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

// groupCols are the columns read by scanGroup (table alias g).
const groupCols = `g.id, g.external_id, g.name, g.role, g.last_synced_at, g.created_at,
	(SELECT COUNT(*) FROM user_groups ug WHERE ug.group_id = g.id), g.provisioned`

const groupOrderSQL = "LOWER(COALESCE(NULLIF(g.name, ''), g.external_id)), g.id"

func scanGroup(sc scanner) (Group, error) {
	var (
		g       Group
		role    string
		synced  sql.NullInt64
		created int64
	)
	err := sc.Scan(&g.ID, &g.ExternalID, &g.Name, &role, &synced, &created, &g.MemberCount, &g.Provisioned)
	g.Role = ParseRole(role)
	g.LastSyncedAt = fromNullUnix(synced)
	g.CreatedAt = fromUnix(created)
	return g, err
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

// CreateGroup adds a group manually (before anyone of it signed in). If a
// group with the same external id (see NormExternalID) exists it is returned
// unchanged (its name is set if it was empty/equal to the external id and
// name is given).
func (s *Store) CreateGroup(ctx context.Context, externalID, name string) (*Group, error) {
	externalID = NormExternalID(externalID)
	name = strings.TrimSpace(name)
	if externalID == "" {
		return nil, errors.New("add group: the group id must not be empty")
	}
	var id int64
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		initial := name
		if initial == "" {
			initial = externalID
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO idp_groups (external_id, name, role, created_at) VALUES (?, ?, '', ?)
			 ON CONFLICT (external_id) DO NOTHING`, externalID, initial, db.Now()); err != nil {
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

// UpdateGroup sets the display name and role of a group. RoleNone and
// RoleViewer both mean "no special role". Returns ErrNotFound for an unknown
// group.
func (s *Store) UpdateGroup(ctx context.Context, id int64, name string, role Role) error {
	err := affected(s.db.ExecContext(ctx, `UPDATE idp_groups SET name = ?, role = ? WHERE id = ?`,
		strings.TrimSpace(name), roleValue(role), id))
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
