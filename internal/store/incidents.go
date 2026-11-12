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

const incidentCols = `i.id, i.title, i.status, i.started_at, i.resolved_at, i.created_by, i.created_at, i.updated_at`

func scanIncident(sc scanner) (Incident, error) {
	var (
		inc                       Incident
		status                    string
		started, created, updated int64
		resolved, createdBy       sql.NullInt64
	)
	err := sc.Scan(&inc.ID, &inc.Title, &status, &started, &resolved, &createdBy, &created, &updated)
	inc.Status = IncidentStatus(status)
	inc.StartedAt = fromUnix(started)
	inc.ResolvedAt = fromNullUnix(resolved)
	inc.CreatedBy = createdBy.Int64
	inc.CreatedAt = fromUnix(created)
	inc.UpdatedAt = fromUnix(updated)
	return inc, err
}

// updateCols are the columns read by scanUpdate (alias iu, author alias a).
var updateCols = `iu.id, iu.incident_id, iu.status, iu.message, iu.created_by, iu.created_at, ` + displayNameSQL("a")

const updateFrom = ` FROM incident_updates iu LEFT JOIN users a ON a.id = iu.created_by `

func scanUpdate(sc scanner) (IncidentUpdate, error) {
	var (
		u         IncidentUpdate
		status    string
		createdBy sql.NullInt64
		created   int64
	)
	err := sc.Scan(&u.ID, &u.IncidentID, &status, &u.Message, &createdBy, &created, &u.AuthorName)
	u.Status = IncidentStatus(status)
	u.CreatedBy = createdBy.Int64
	u.CreatedAt = fromUnix(created)
	return u, err
}

// validateImpacts checks the impacts and removes duplicate services (the
// first entry wins).
func validateImpacts(impacts []ServiceImpact) ([]ServiceImpact, error) {
	out := make([]ServiceImpact, 0, len(impacts))
	seen := make(map[int64]bool)
	for _, si := range impacts {
		if !ValidImpact(si.Impact) {
			return nil, fmt.Errorf("invalid impact %q", si.Impact)
		}
		if si.ServiceID <= 0 || seen[si.ServiceID] {
			continue
		}
		seen[si.ServiceID] = true
		out = append(out, si)
	}
	return out, nil
}

// setIncidentServices replaces the affected services of an incident.
// Unknown service ids are ignored.
func setIncidentServices(ctx context.Context, tx *db.Tx, incidentID int64, impacts []ServiceImpact) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM incident_services WHERE incident_id = ?`, incidentID); err != nil {
		return err
	}
	for _, si := range impacts {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO incident_services (incident_id, service_id, impact) SELECT ?, id, ? FROM services WHERE id = ?`,
			incidentID, string(si.Impact), si.ServiceID); err != nil {
			return err
		}
	}
	return nil
}

func insertUpdate(ctx context.Context, tx *db.Tx, incidentID int64, upd *IncidentUpdate, now time.Time) error {
	var id int64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO incident_updates (incident_id, status, message, created_by, created_at) VALUES (?, ?, ?, ?, ?) RETURNING id`,
		incidentID, string(upd.Status), upd.Message, nullID(upd.CreatedBy), now.Unix()).Scan(&id)
	if err != nil {
		return err
	}
	upd.ID = id
	upd.IncidentID = incidentID
	upd.CreatedAt = now
	return nil
}

// CreateIncident inserts inc (ID, CreatedAt, UpdatedAt set on inc; StartedAt
// defaults to now when zero; ResolvedAt is set when Status is resolved) with
// its Services and a first update (first.IncidentID/ID/CreatedAt are set).
// first.Status should equal inc.Status (an empty first.Status is set to
// inc.Status). first may be nil to create the incident without an update.
// A ResolvedAt given for a resolved incident is kept (otherwise now); for
// other statuses ResolvedAt is cleared. Unknown service ids are ignored.
func (s *Store) CreateIncident(ctx context.Context, inc *Incident, first *IncidentUpdate) error {
	if strings.TrimSpace(inc.Title) == "" {
		return errors.New("create incident: the title must not be empty")
	}
	if !ValidIncidentStatus(inc.Status) {
		return fmt.Errorf("create incident: invalid status %q", inc.Status)
	}
	if first != nil && first.Status == "" {
		first.Status = inc.Status
	}
	if first != nil && !ValidIncidentStatus(first.Status) {
		return fmt.Errorf("create incident: invalid update status %q", first.Status)
	}
	impacts, err := validateImpacts(inc.Services)
	if err != nil {
		return fmt.Errorf("create incident: %w", err)
	}

	now := nowTime()
	started := now
	if !inc.StartedAt.IsZero() {
		started = truncate(inc.StartedAt)
	}
	var resolved *time.Time
	if inc.Status == IncidentResolved {
		r := now
		if inc.ResolvedAt != nil && !inc.ResolvedAt.IsZero() {
			r = truncate(*inc.ResolvedAt)
		}
		resolved = &r
	}

	var id int64
	err = s.db.InTx(ctx, func(tx *db.Tx) error {
		err := tx.QueryRowContext(ctx,
			`INSERT INTO incidents (title, status, started_at, resolved_at, created_by, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			inc.Title, string(inc.Status), started.Unix(), timeArg(resolved), nullID(inc.CreatedBy), now.Unix(), now.Unix()).Scan(&id)
		if err != nil {
			return err
		}
		if err := setIncidentServices(ctx, tx, id, impacts); err != nil {
			return err
		}
		if first != nil {
			return insertUpdate(ctx, tx, id, first, now)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("create incident: %w", err)
	}
	inc.ID = id
	inc.StartedAt = started
	inc.ResolvedAt = resolved
	inc.CreatedAt = now
	inc.UpdatedAt = now
	return nil
}

// AddIncidentUpdate appends an update and sets the incident status to
// upd.Status (resolved_at=now when it becomes resolved, NULL when it is
// reopened) and updated_at=now. If impacts is non-nil the affected services
// are replaced by impacts. upd.ID/IncidentID/CreatedAt are set. Returns
// ErrNotFound for an unknown incident.
func (s *Store) AddIncidentUpdate(ctx context.Context, incidentID int64, upd *IncidentUpdate, impacts []ServiceImpact) error {
	if !ValidIncidentStatus(upd.Status) {
		return fmt.Errorf("update incident: invalid status %q", upd.Status)
	}
	if impacts != nil {
		var err error
		if impacts, err = validateImpacts(impacts); err != nil {
			return fmt.Errorf("update incident: %w", err)
		}
	}
	now := nowTime()
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		var (
			current  string
			resolved sql.NullInt64
		)
		err := tx.QueryRowContext(ctx, `SELECT status, resolved_at FROM incidents WHERE id = ?`, incidentID).Scan(&current, &resolved)
		if err != nil {
			return notFound(err)
		}
		var resolvedAt any // NULL unless resolved
		if upd.Status == IncidentResolved {
			resolvedAt = now.Unix()
			if IncidentStatus(current) == IncidentResolved && resolved.Valid {
				resolvedAt = resolved.Int64 // already resolved: keep the time
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE incidents SET status = ?, resolved_at = ?, updated_at = ? WHERE id = ?`,
			string(upd.Status), resolvedAt, now.Unix(), incidentID); err != nil {
			return err
		}
		if impacts != nil {
			if err := setIncidentServices(ctx, tx, incidentID, impacts); err != nil {
				return err
			}
		}
		return insertUpdate(ctx, tx, incidentID, upd, now)
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("update incident %d: %w", incidentID, err)
	}
	return err
}

// UpdateIncidentTitle changes the title of an incident (and updated_at).
// Returns ErrNotFound for an unknown incident.
func (s *Store) UpdateIncidentTitle(ctx context.Context, id int64, title string) error {
	if strings.TrimSpace(title) == "" {
		return errors.New("update incident: the title must not be empty")
	}
	err := affected(s.db.ExecContext(ctx, `UPDATE incidents SET title = ?, updated_at = ? WHERE id = ?`, title, db.Now(), id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("update incident %d: %w", id, err)
	}
	return err
}

// GetIncident returns an incident with Services and all Updates (newest
// first, AuthorName filled) or ErrNotFound.
func (s *Store) GetIncident(ctx context.Context, id int64) (*Incident, error) {
	incs, err := s.listIncidents(ctx, false, `WHERE i.id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("get incident %d: %w", id, err)
	}
	if len(incs) == 0 {
		return nil, ErrNotFound
	}
	inc := &incs[0]
	inc.Updates, err = queryAll(ctx, s.db, scanUpdate,
		`SELECT `+updateCols+updateFrom+`WHERE iu.incident_id = ? ORDER BY iu.created_at DESC, iu.id DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("get incident %d: %w", id, err)
	}
	return inc, nil
}

// DeleteIncident deletes an incident with its updates. Returns ErrNotFound
// for an unknown incident.
func (s *Store) DeleteIncident(ctx context.Context, id int64) error {
	err := affected(s.db.ExecContext(ctx, `DELETE FROM incidents WHERE id = ?`, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("delete incident %d: %w", id, err)
	}
	return err
}

// ListOpenIncidents returns unresolved incidents, newest first, with Services
// and the latest update.
func (s *Store) ListOpenIncidents(ctx context.Context) ([]Incident, error) {
	incs, err := s.listIncidents(ctx, true, `WHERE i.status <> ?`, string(IncidentResolved))
	if err != nil {
		return nil, fmt.Errorf("list open incidents: %w", err)
	}
	return incs, nil
}

// ListIncidentsBetween returns incidents that overlap [from, to): started
// before to and not resolved before from. Newest first, with Services and the
// latest update.
func (s *Store) ListIncidentsBetween(ctx context.Context, from, to time.Time) ([]Incident, error) {
	incs, err := s.listIncidents(ctx, true,
		`WHERE i.started_at < ? AND (i.resolved_at IS NULL OR i.resolved_at >= ?)`, to.Unix(), from.Unix())
	if err != nil {
		return nil, fmt.Errorf("list incidents: %w", err)
	}
	return incs, nil
}

// listIncidents returns the incidents matching where (alias i), newest
// first, with Services and, if latest is true, the latest update.
func (s *Store) listIncidents(ctx context.Context, latest bool, where string, args ...any) ([]Incident, error) {
	incs, err := queryAll(ctx, s.db, scanIncident,
		`SELECT `+incidentCols+` FROM incidents i `+where+` ORDER BY i.started_at DESC, i.id DESC`, args...)
	if err != nil || len(incs) == 0 {
		return incs, err
	}
	ids := make([]int64, len(incs))
	pos := make(map[int64]int, len(incs))
	for i, inc := range incs {
		ids[i] = inc.ID
		pos[inc.ID] = i
	}
	in, inArgs := inList(ids)

	type impactRow struct {
		incident int64
		impact   ServiceImpact
	}
	impacts, err := queryAll(ctx, s.db, func(sc scanner) (impactRow, error) {
		var r impactRow
		var impact string
		err := sc.Scan(&r.incident, &r.impact.ServiceID, &r.impact.ServiceName, &impact)
		r.impact.Impact = Status(impact)
		return r, err
	}, `SELECT x.incident_id, sv.id, sv.name, x.impact
	    FROM incident_services x JOIN services sv ON sv.id = x.service_id
	    WHERE x.incident_id IN (`+in+`) ORDER BY `+serviceOrderSQL, inArgs...)
	if err != nil {
		return nil, err
	}
	for _, r := range impacts {
		p := pos[r.incident]
		incs[p].Services = append(incs[p].Services, r.impact)
	}

	if latest {
		updates, err := queryAll(ctx, s.db, scanUpdate,
			`SELECT `+updateCols+updateFrom+`WHERE iu.incident_id IN (`+in+`) AND iu.id = (
			   SELECT u2.id FROM incident_updates u2 WHERE u2.incident_id = iu.incident_id
			   ORDER BY u2.created_at DESC, u2.id DESC LIMIT 1)`, inArgs...)
		if err != nil {
			return nil, err
		}
		for _, u := range updates {
			p := pos[u.IncidentID]
			incs[p].Updates = []IncidentUpdate{u}
		}
	}
	return incs, nil
}
