package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

const maintenanceCols = `m.id, m.title, m.description, m.starts_at, m.ends_at, m.cancelled, m.notify,
	m.notified_start, m.notified_end, m.created_by, m.created_at, m.updated_at`

func scanMaintenance(sc scanner) (Maintenance, error) {
	var (
		m                              Maintenance
		starts, ends, created, updated int64
		createdBy                      sql.NullInt64
	)
	err := sc.Scan(&m.ID, &m.Title, &m.Description, &starts, &ends, &m.Cancelled, &m.Notify,
		&m.NotifiedStart, &m.NotifiedEnd, &createdBy, &created, &updated)
	m.StartsAt = fromUnix(starts)
	m.EndsAt = fromUnix(ends)
	m.CreatedBy = createdBy.Int64
	m.CreatedAt = fromUnix(created)
	m.UpdatedAt = fromUnix(updated)
	return m, err
}

// validateMaintenance checks the fields that are written to the database.
func validateMaintenance(m *Maintenance) error {
	if strings.TrimSpace(m.Title) == "" {
		return errors.New("the title must not be empty")
	}
	if m.StartsAt.IsZero() || m.EndsAt.IsZero() {
		return errors.New("start and end time are required")
	}
	if !m.EndsAt.After(m.StartsAt) {
		return errors.New("the maintenance must end after it starts")
	}
	return nil
}

// setMaintenanceServices replaces the services of a maintenance. Unknown
// service ids are ignored.
func setMaintenanceServices(ctx context.Context, tx *db.Tx, maintenanceID int64, services []ServiceRef) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM maintenance_services WHERE maintenance_id = ?`, maintenanceID); err != nil {
		return err
	}
	ids := make([]int64, len(services))
	for i, svc := range services {
		ids[i] = svc.ID
	}
	for _, sid := range uniqueIDs(ids) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO maintenance_services (maintenance_id, service_id) SELECT ?, id FROM services WHERE id = ?`,
			maintenanceID, sid); err != nil {
			return err
		}
	}
	return nil
}

// CreateMaintenance inserts m with its Services (only IDs are used; unknown
// ids are ignored). ID, CreatedAt and UpdatedAt are set on m.
// notified_start/notified_end are initialised to true when the window
// already started/ended at creation time, so that no "started"/"completed"
// e-mails are sent for past windows. The window must end after it starts.
func (s *Store) CreateMaintenance(ctx context.Context, m *Maintenance) error {
	if err := validateMaintenance(m); err != nil {
		return fmt.Errorf("create maintenance: %w", err)
	}
	now := nowTime()
	starts, ends := truncate(m.StartsAt), truncate(m.EndsAt)
	notifiedStart := !now.Before(starts)
	notifiedEnd := !now.Before(ends)
	var id int64
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		err := tx.QueryRowContext(ctx,
			`INSERT INTO maintenances (title, description, starts_at, ends_at, cancelled, notify, notified_start, notified_end,
			                           created_by, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			m.Title, m.Description, starts.Unix(), ends.Unix(), m.Cancelled, m.Notify, notifiedStart, notifiedEnd,
			nullID(m.CreatedBy), now.Unix(), now.Unix()).Scan(&id)
		if err != nil {
			return err
		}
		return setMaintenanceServices(ctx, tx, id, m.Services)
	})
	if err != nil {
		return fmt.Errorf("create maintenance: %w", err)
	}
	m.ID = id
	m.StartsAt, m.EndsAt = starts, ends
	m.NotifiedStart, m.NotifiedEnd = notifiedStart, notifiedEnd
	m.CreatedAt, m.UpdatedAt = now, now
	return nil
}

// UpdateMaintenance updates title, description, starts_at, ends_at, notify
// and replaces the services. If the new window starts in the future,
// notified_start and notified_end are reset to false; if it ends in the
// future notified_end is reset to false. UpdatedAt and the notified flags are
// set on m. Returns ErrNotFound for an unknown maintenance.
func (s *Store) UpdateMaintenance(ctx context.Context, m *Maintenance) error {
	if err := validateMaintenance(m); err != nil {
		return fmt.Errorf("update maintenance: %w", err)
	}
	now := nowTime()
	starts, ends := truncate(m.StartsAt), truncate(m.EndsAt)
	var notifiedStart, notifiedEnd bool
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT notified_start, notified_end FROM maintenances WHERE id = ?`, m.ID).
			Scan(&notifiedStart, &notifiedEnd)
		if err != nil {
			return notFound(err)
		}
		if now.Before(starts) {
			notifiedStart, notifiedEnd = false, false
		} else if now.Before(ends) {
			notifiedEnd = false
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE maintenances SET title = ?, description = ?, starts_at = ?, ends_at = ?, notify = ?,
			        notified_start = ?, notified_end = ?, updated_at = ? WHERE id = ?`,
			m.Title, m.Description, starts.Unix(), ends.Unix(), m.Notify, notifiedStart, notifiedEnd, now.Unix(), m.ID)
		if err != nil {
			return err
		}
		return setMaintenanceServices(ctx, tx, m.ID, m.Services)
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return fmt.Errorf("update maintenance %d: %w", m.ID, err)
	}
	m.StartsAt, m.EndsAt = starts, ends
	m.NotifiedStart, m.NotifiedEnd = notifiedStart, notifiedEnd
	m.UpdatedAt = now
	return nil
}

// CancelMaintenance marks a maintenance as cancelled. Returns ErrNotFound for
// an unknown maintenance.
func (s *Store) CancelMaintenance(ctx context.Context, id int64) error {
	err := affected(s.db.ExecContext(ctx, `UPDATE maintenances SET cancelled = ?, updated_at = ? WHERE id = ?`, true, db.Now(), id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("cancel maintenance %d: %w", id, err)
	}
	return err
}

// DeleteMaintenance deletes a maintenance. Returns ErrNotFound for an unknown
// maintenance.
func (s *Store) DeleteMaintenance(ctx context.Context, id int64) error {
	err := affected(s.db.ExecContext(ctx, `DELETE FROM maintenances WHERE id = ?`, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("delete maintenance %d: %w", id, err)
	}
	return err
}

// GetMaintenance returns a maintenance with Services or ErrNotFound.
func (s *Store) GetMaintenance(ctx context.Context, id int64) (*Maintenance, error) {
	ms, err := s.listMaintenances(ctx, `WHERE m.id = ?`, ``, id)
	if err != nil {
		return nil, fmt.Errorf("get maintenance %d: %w", id, err)
	}
	if len(ms) == 0 {
		return nil, ErrNotFound
	}
	return &ms[0], nil
}

// ListCurrentMaintenances returns non-cancelled maintenances that have not
// ended at now (in progress or upcoming), ordered by starts_at, with Services.
func (s *Store) ListCurrentMaintenances(ctx context.Context, now time.Time) ([]Maintenance, error) {
	ms, err := s.listMaintenances(ctx, `WHERE m.cancelled = ? AND m.ends_at > ?`, `m.starts_at, m.id`, false, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("list current maintenances: %w", err)
	}
	return ms, nil
}

// ListMaintenancesBetween returns maintenances (including cancelled) whose
// window overlaps [from, to), newest first, with Services.
func (s *Store) ListMaintenancesBetween(ctx context.Context, from, to time.Time) ([]Maintenance, error) {
	ms, err := s.listMaintenances(ctx, `WHERE m.starts_at < ? AND m.ends_at > ?`, `m.starts_at DESC, m.id DESC`, to.Unix(), from.Unix())
	if err != nil {
		return nil, fmt.Errorf("list maintenances: %w", err)
	}
	return ms, nil
}

// MaintenancesToNotify returns non-cancelled maintenances with notify=true
// that started at or before now but have notified_start=false, or that ended
// at or before now but have notified_end=false. With Services, ordered by
// starts_at.
func (s *Store) MaintenancesToNotify(ctx context.Context, now time.Time) ([]Maintenance, error) {
	n := now.Unix()
	ms, err := s.listMaintenances(ctx,
		`WHERE m.cancelled = ? AND m.notify = ? AND
		       ((m.starts_at <= ? AND m.notified_start = ?) OR (m.ends_at <= ? AND m.notified_end = ?))`,
		`m.starts_at, m.id`, false, true, n, false, n, false)
	if err != nil {
		return nil, fmt.Errorf("list maintenances to notify: %w", err)
	}
	return ms, nil
}

// MarkMaintenanceNotified sets notified_start (start=true) or notified_end
// (start=false) of maintenance m.ID to true, but only if its start (or end)
// is still m.StartsAt (or m.EndsAt): a window that an editor moved
// meanwhile keeps its flags, so that its new start or end is announced.
// It does nothing for an unknown maintenance.
func (s *Store) MarkMaintenanceNotified(ctx context.Context, m *Maintenance, start bool) error {
	q, at := `UPDATE maintenances SET notified_end = ? WHERE id = ? AND ends_at = ?`, m.EndsAt
	if start {
		q, at = `UPDATE maintenances SET notified_start = ? WHERE id = ? AND starts_at = ?`, m.StartsAt
	}
	if _, err := s.db.ExecContext(ctx, q, true, m.ID, at.Unix()); err != nil {
		return fmt.Errorf("mark maintenance %d as notified: %w", m.ID, err)
	}
	return nil
}

// listMaintenances returns the maintenances matching where (alias m) in the
// given order (default: starts_at), with Services.
func (s *Store) listMaintenances(ctx context.Context, where, order string, args ...any) ([]Maintenance, error) {
	if order == "" {
		order = "m.starts_at, m.id"
	}
	ms, err := queryAll(ctx, s.db, scanMaintenance, `SELECT `+maintenanceCols+` FROM maintenances m `+where+` ORDER BY `+order, args...)
	if err != nil || len(ms) == 0 {
		return ms, err
	}
	ids := make([]int64, len(ms))
	pos := make(map[int64]int, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
		pos[m.ID] = i
	}
	in, inArgs := inList(ids)
	type row struct {
		maintenance int64
		ref         ServiceRef
	}
	rows, err := queryAll(ctx, s.db, func(sc scanner) (row, error) {
		var r row
		return r, sc.Scan(&r.maintenance, &r.ref.ID, &r.ref.Name)
	}, `SELECT x.maintenance_id, sv.id, sv.name
	    FROM maintenance_services x JOIN services sv ON sv.id = x.service_id
	    WHERE x.maintenance_id IN (`+in+`) ORDER BY `+serviceOrderSQL, inArgs...)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		p := pos[r.maintenance]
		ms[p].Services = append(ms[p].Services, r.ref)
	}
	return ms, nil
}
