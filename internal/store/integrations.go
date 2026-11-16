package store

// Monitoring integrations: the monitoring tools that send their alerts to a
// webhook URL of the status page (package monitoring), with the latest
// requests they sent (integration events). What alerts change is in
// alerts.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

// KeepIntegrationEvents is the number of requests kept per integration
// (AddIntegrationEvent deletes older ones).
const KeepIntegrationEvents = 50

// MaxEventPayload is the number of bytes of a request body that
// AddIntegrationEvent stores.
const MaxEventPayload = 8 << 10

// maxEventSummary is the number of bytes of a summary that
// AddIntegrationEvent stores (package monitoring shortens it to fewer lines
// first).
const maxEventSummary = 32 << 10

const integrationCols = `ig.id, ig.name, ig.provider, ig.impact, ig.notify, ig.paused, ig.token_created_at, ig.last_event_at, ig.created_at,
	(SELECT COUNT(*) FROM integration_alerts fa WHERE fa.integration_id = ig.id)`

func scanIntegration(sc scanner) (Integration, error) {
	var (
		in                    Integration
		impact                string
		tokenCreated, created int64
		lastEvent             sql.NullInt64
	)
	err := sc.Scan(&in.ID, &in.Name, &in.Provider, &impact, &in.Notify, &in.Paused, &tokenCreated, &lastEvent, &created, &in.Firing)
	in.Impact = Status(impact)
	in.TokenCreatedAt = fromUnix(tokenCreated)
	in.LastEventAt = fromNullUnix(lastEvent)
	in.CreatedAt = fromUnix(created)
	return in, err
}

// validateIntegration checks the fields that are written to the database.
func validateIntegration(in *Integration) error {
	switch {
	case strings.TrimSpace(in.Name) == "":
		return errors.New("the name must not be empty")
	case strings.TrimSpace(in.Provider) == "":
		return errors.New("the monitoring tool must be set")
	case in.Impact != "" && !ValidImpact(in.Impact):
		return fmt.Errorf("invalid impact %q", in.Impact)
	}
	return nil
}

// CreateIntegration inserts in with the hash of its webhook token (see
// package monitoring); ID, CreatedAt and TokenCreatedAt are set on in.
func (s *Store) CreateIntegration(ctx context.Context, in *Integration, tokenHash string) error {
	if err := validateIntegration(in); err != nil {
		return fmt.Errorf("create integration: %w", err)
	}
	if tokenHash == "" {
		return errors.New("create integration: no token")
	}
	now := nowTime()
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO integrations (name, provider, token_hash, token_created_at, impact, notify, paused, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		in.Name, in.Provider, tokenHash, now.Unix(), string(in.Impact), in.Notify, in.Paused, now.Unix()).Scan(&id)
	if err != nil {
		return fmt.Errorf("create integration %q: %w", in.Name, err)
	}
	in.ID = id
	in.CreatedAt = now
	in.TokenCreatedAt = now
	return nil
}

// UpdateIntegration changes the name, tool, impact, e-mails and pause of an
// integration (not its token, see SetIntegrationToken). Returns ErrNotFound
// for an unknown integration.
func (s *Store) UpdateIntegration(ctx context.Context, in *Integration) error {
	if err := validateIntegration(in); err != nil {
		return fmt.Errorf("update integration: %w", err)
	}
	err := affected(s.db.ExecContext(ctx,
		`UPDATE integrations SET name = ?, provider = ?, impact = ?, notify = ?, paused = ? WHERE id = ?`,
		in.Name, in.Provider, string(in.Impact), in.Notify, in.Paused, in.ID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("update integration %d: %w", in.ID, err)
	}
	return err
}

// SetIntegrationToken replaces the webhook token of an integration (by its
// hash): the previous URL stops working at once. Returns ErrNotFound for an
// unknown integration.
func (s *Store) SetIntegrationToken(ctx context.Context, id int64, tokenHash string) error {
	if tokenHash == "" {
		return errors.New("set integration token: no token")
	}
	err := affected(s.db.ExecContext(ctx,
		`UPDATE integrations SET token_hash = ?, token_created_at = ? WHERE id = ?`, tokenHash, db.Now(), id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("set token of integration %d: %w", id, err)
	}
	return err
}

// DeleteIntegration deletes an integration with its alerts and events. The
// incidents it opened stay; open ones are no longer resolved by its alerts
// (an editor resolves them, or the alerts of other integrations attached to
// them when they clear). Returns ErrNotFound for an unknown integration.
func (s *Store) DeleteIntegration(ctx context.Context, id int64) error {
	// In the monitoring turn: the cascade deletes alerts (see DeleteIncident).
	err := s.inMonitoringTx(ctx, func(tx *db.Tx) error {
		return affected(tx.ExecContext(ctx, `DELETE FROM integrations WHERE id = ?`, id))
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("delete integration %d: %w", id, err)
	}
	return err
}

// GetIntegration returns an integration (Firing filled) or ErrNotFound.
func (s *Store) GetIntegration(ctx context.Context, id int64) (*Integration, error) {
	in, err := scanIntegration(s.db.QueryRowContext(ctx,
		`SELECT `+integrationCols+` FROM integrations ig WHERE ig.id = ?`, id))
	if err != nil {
		if err = notFound(err); errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("get integration %d: %w", id, err)
	}
	return &in, nil
}

// IntegrationByTokenHash returns the integration whose webhook token has
// this hash (Firing filled), or ErrNotFound.
func (s *Store) IntegrationByTokenHash(ctx context.Context, tokenHash string) (*Integration, error) {
	if tokenHash == "" {
		return nil, ErrNotFound
	}
	in, err := scanIntegration(s.db.QueryRowContext(ctx,
		`SELECT `+integrationCols+` FROM integrations ig WHERE ig.token_hash = ?`, tokenHash))
	if err != nil {
		if err = notFound(err); errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("find integration: %w", err)
	}
	return &in, nil
}

// ListIntegrations returns all integrations by name (Firing filled).
func (s *Store) ListIntegrations(ctx context.Context) ([]Integration, error) {
	ins, err := queryAll(ctx, s.db, scanIntegration,
		`SELECT `+integrationCols+` FROM integrations ig ORDER BY LOWER(ig.name), ig.id`)
	if err != nil {
		return nil, fmt.Errorf("list integrations: %w", err)
	}
	return ins, nil
}

// AddIntegrationEvent records a request of an integration (ID set on ev; a
// zero ReceivedAt is now), stores at most MaxEventPayload bytes of its
// payload (saying so if it was longer), sets the integration's last request
// time (unless ev.Internal) and deletes all but its latest
// KeepIntegrationEvents events.
func (s *Store) AddIntegrationEvent(ctx context.Context, ev *IntegrationEvent) error {
	if ev.ReceivedAt.IsZero() {
		ev.ReceivedAt = nowTime()
	}
	ev.ReceivedAt = truncate(ev.ReceivedAt)
	if n := len(ev.Payload); n > MaxEventPayload {
		ev.Payload = truncateUTF8(ev.Payload, MaxEventPayload) +
			fmt.Sprintf("\n… (shortened: the first %d KB of %d bytes are kept)", MaxEventPayload>>10, n)
	}
	ev.Summary = truncateUTF8(ev.Summary, maxEventSummary)
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		err := tx.QueryRowContext(ctx,
			`INSERT INTO integration_events (integration_id, received_at, outcome, summary, payload) VALUES (?, ?, ?, ?, ?) RETURNING id`,
			ev.IntegrationID, ev.ReceivedAt.Unix(), ev.Outcome, ev.Summary, ev.Payload).Scan(&ev.ID)
		if err != nil {
			return err
		}
		if !ev.Internal {
			if _, err := tx.ExecContext(ctx, `UPDATE integrations SET last_event_at = ? WHERE id = ?`, ev.ReceivedAt.Unix(), ev.IntegrationID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx,
			`DELETE FROM integration_events WHERE integration_id = ? AND id NOT IN (
			   SELECT id FROM integration_events WHERE integration_id = ? ORDER BY id DESC LIMIT ?)`,
			ev.IntegrationID, ev.IntegrationID, KeepIntegrationEvents)
		return err
	})
	if err != nil {
		return fmt.Errorf("record request of integration %d: %w", ev.IntegrationID, err)
	}
	return nil
}

// ListIntegrationEvents returns the latest events of an integration, newest
// first.
func (s *Store) ListIntegrationEvents(ctx context.Context, integrationID int64, limit int) ([]IntegrationEvent, error) {
	evs, err := queryAll(ctx, s.db, func(sc scanner) (IntegrationEvent, error) {
		var (
			ev       IntegrationEvent
			received int64
		)
		err := sc.Scan(&ev.ID, &ev.IntegrationID, &received, &ev.Outcome, &ev.Summary, &ev.Payload)
		ev.ReceivedAt = fromUnix(received)
		return ev, err
	}, `SELECT id, integration_id, received_at, outcome, summary, payload FROM integration_events
	    WHERE integration_id = ? ORDER BY id DESC LIMIT ?`, integrationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list requests of integration %d: %w", integrationID, err)
	}
	return evs, nil
}

// integrationAlertCols are read by scanIntegrationAlert (aliases: alert a,
// integration ig, service sv, incident i).
const integrationAlertCols = `a.integration_id, ig.name, a.alert_key, a.service_id, sv.name, a.incident_id, i.status,
	a.suppressed, a.impact, a.title, a.started_at, a.updated_at`

const integrationAlertFrom = ` FROM integration_alerts a
	JOIN integrations ig ON ig.id = a.integration_id
	JOIN services sv ON sv.id = a.service_id
	LEFT JOIN incidents i ON i.id = a.incident_id `

func scanIntegrationAlert(sc scanner) (IntegrationAlert, error) {
	var (
		a                IntegrationAlert
		incident         sql.NullInt64
		incidentStatus   sql.NullString
		impact           string
		started, updated int64
	)
	err := sc.Scan(&a.IntegrationID, &a.IntegrationName, &a.Key, &a.ServiceID, &a.ServiceName, &incident, &incidentStatus,
		&a.Suppressed, &impact, &a.Title, &started, &updated)
	a.IncidentID = incident.Int64
	a.IncidentOpen = incident.Valid && incidentStatus.Valid && IncidentStatus(incidentStatus.String) != IncidentResolved
	a.Impact = Status(impact)
	a.StartedAt = fromUnix(started)
	a.UpdatedAt = fromUnix(updated)
	return a, err
}

// ListIntegrationAlerts returns the firing alerts of an integration, oldest
// first.
func (s *Store) ListIntegrationAlerts(ctx context.Context, integrationID int64) ([]IntegrationAlert, error) {
	as, err := queryAll(ctx, s.db, scanIntegrationAlert,
		`SELECT `+integrationAlertCols+integrationAlertFrom+`WHERE a.integration_id = ?
		 ORDER BY a.started_at, LOWER(sv.name), a.alert_key`, integrationID)
	if err != nil {
		return nil, fmt.Errorf("list alerts of integration %d: %w", integrationID, err)
	}
	return as, nil
}

// ListIncidentAlerts returns the firing alerts attached to an incident,
// oldest first.
func (s *Store) ListIncidentAlerts(ctx context.Context, incidentID int64) ([]IntegrationAlert, error) {
	as, err := queryAll(ctx, s.db, scanIntegrationAlert,
		`SELECT `+integrationAlertCols+integrationAlertFrom+`WHERE a.incident_id = ?
		 ORDER BY a.started_at, LOWER(ig.name), a.alert_key`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("list alerts of incident %d: %w", incidentID, err)
	}
	return as, nil
}

// truncateUTF8 shortens s to at most n bytes, cut at a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
