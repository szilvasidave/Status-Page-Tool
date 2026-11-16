package store

// What the alerts of monitoring tools change (package monitoring reads them
// from the tools' requests).
//
// An alert affects the services it names (MatchServices). Each firing alert
// has a row in integration_alerts per service it affects, with the incident
// it is attached to; the row is deleted when the alert clears. The incidents
// are brought in line with these rows at the end of every request (finish):
//
//   - A firing alert without a row (a new occurrence) is attached to the
//     newest open automatic incident that affects the service, or opens a
//     new automatic incident for it. While maintenance of the service is in
//     progress it is suppressed instead: ReleaseSuppressedAlerts attaches it
//     once the maintenance has ended, if it still fires.
//   - The impact of an automatic incident on a service is the worst impact
//     of the alerts that fire for it there.
//   - When the last firing alert of an incident clears, the incident is
//     resolved.
//   - A firing alert that has a row (a repeat, e.g. after Alertmanager's
//     repeat_interval) updates its title, impact and reported start. If an
//     editor resolved or deleted its incident meanwhile, it stays detached
//     ("dismissed") until it clears – unless the tool reports that it
//     started again later (newOccurrence), e.g. Alertmanager without
//     send_resolved: then it is a new occurrence.
//   - An administrator can clear a firing alert whose recovery the tool did
//     not send (ClearIntegrationAlert): as if the tool had reported it.
//
// Automation never touches incidents that people opened. It changes the
// automatic ones by posting updates (source: the integration's name),
// setting the impact of services with firing alerts and resolving them;
// editors can post updates, rename and resolve them like any incident.
//
// The transactions that change alerts (and the deletions of incidents,
// services and integrations, whose cascades change them too) run one at a
// time (monitoringTurn, lockMonitoring). That keeps two requests from both
// opening an incident for a service, and their locks in one order.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

// ErrBusy is returned (wrapped) by the changes of monitoring that waited for
// their turn until their context ended, e.g. behind a long batch of alerts:
// nothing was changed.
var ErrBusy = errors.New("monitoring is busy")

// monitoringLockID identifies the PostgreSQL advisory lock taken by
// lockMonitoring.
const monitoringLockID = 7395001

// monitoringTurn waits until no other transaction of this process that
// changes alerts runs, and returns the function that ends the turn. They
// run one at a time anyway (lockMonitoring); waiting here, before the
// transaction, holds no database connection, so that requests queued
// behind a long batch of alerts cannot take the connections the pages
// need.
func (s *Store) monitoringTurn(ctx context.Context) (func(), error) {
	select {
	case s.monitoring <- struct{}{}:
		return func() { <-s.monitoring }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrBusy, ctx.Err())
	}
}

// lockMonitoring serialises the transactions that change alerts across
// processes (several instances on one PostgreSQL database), so that two
// requests cannot both decide that a service has no automatic incident
// yet, and the locks of a request and of a deletion are taken in one order.
// SQLite runs writing transactions one at a time anyway (BEGIN IMMEDIATE).
// Call it first in the transaction.
func (s *Store) lockMonitoring(ctx context.Context, tx *db.Tx) error {
	if !s.db.IsPostgres() {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, monitoringLockID)
	return err
}

// inMonitoringTx runs fn in a transaction in the monitoring turn, with the
// monitoring lock.
func (s *Store) inMonitoringTx(ctx context.Context, fn func(tx *db.Tx) error) error {
	done, err := s.monitoringTurn(ctx)
	if err != nil {
		return err
	}
	defer done()
	return s.db.InTx(ctx, func(tx *db.Tx) error {
		if err := s.lockMonitoring(ctx, tx); err != nil {
			return err
		}
		return fn(tx)
	})
}

// matchKey is the form in which names of services are compared: lower case,
// every run of white space as one space, no surrounding spaces.
func matchKey(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// MatchServices returns the services that the first target naming any
// service names: by the service's name or one of its MonitorNames, compared
// without regard to case and white space. matched is that target; ids is
// nil and matched "" if no target names a service. Several services can
// share a name (e.g. a monitor name "datacenter-1" on all services in that
// data centre): all of them are returned, in the order of services.
func MatchServices(services []Service, targets []string) (ids []int64, matched string) {
	byName := make(map[string][]int64, len(services))
	for _, svc := range services {
		names := append([]string{svc.Name}, svc.MonitorNames...)
		for i, n := range names {
			k := matchKey(n)
			if k == "" || slices.ContainsFunc(names[:i], func(o string) bool { return matchKey(o) == k }) {
				continue
			}
			byName[k] = append(byName[k], svc.ID)
		}
	}
	for _, t := range targets {
		if k := matchKey(t); k != "" && len(byName[k]) > 0 {
			return byName[k], t
		}
	}
	return nil, ""
}

// AutomaticIncidentTitle is the title of an incident that monitoring opens
// for a service because of an alert with the title alert: the alert's title
// if it names the service (as whole words), else "<service>: <alert>", at
// most 200 characters.
func AutomaticIncidentTitle(service, alert string) string {
	alert = strings.Join(strings.Fields(alert), " ")
	var t string
	switch {
	case alert == "":
		t = service + ": monitoring reports a problem"
	case namesWords(alert, service):
		t = alert
	default:
		t = service + ": " + alert
	}
	return truncateRunes(t, 200)
}

// namesWords reports whether text contains name as whole words, without
// regard to case and white space: "VPN" is in "VPN gateway down" but not
// "AD" in "Load average high".
func namesWords(text, name string) bool {
	text, name = matchKey(text), matchKey(name)
	if name == "" {
		return false
	}
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	for i := 0; i <= len(text)-len(name); {
		j := strings.Index(text[i:], name)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(name)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		first, _ := utf8.DecodeRuneInString(name)
		last, _ := utf8.DecodeLastRuneInString(name)
		// A boundary is needed only where the name itself starts or ends
		// with a letter or digit ("C++" in "C++ build" counts).
		if (start == 0 || !isWord(first) || !isWord(before)) && (end == len(text) || !isWord(last) || !isWord(after)) {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[start:])
		i = start + size
	}
	return false
}

// truncateRunes shortens s to at most n characters, the last one "…".
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// ApplyAlerts applies the alerts of one request of the integration in (see
// the comment at the top of this file) in one transaction and returns what
// it did, with the e-mails to send. Firing alerts without a key, or that
// name no service, change nothing; resolved alerts that were not firing
// change nothing. It waits for its turn (see monitoringTurn) until ctx ends
// (ErrBusy).
func (s *Store) ApplyAlerts(ctx context.Context, in *Integration, alerts []AlertInput) (*ApplyResult, error) {
	var res *ApplyResult
	err := s.inMonitoringTx(ctx, func(tx *db.Tx) error {
		r, err := s.newAlertRun(ctx, tx)
		if err != nil {
			return err
		}
		for i := range alerts {
			a := &alerts[i]
			ar := AlertResult{IntegrationID: in.ID, Alert: a}
			switch {
			case a.Key == "":
			case a.Firing:
				ids, matched := MatchServices(r.services, a.Targets)
				ar.Matched = matched
				if len(ids) > 0 {
					if ar.Services, err = r.fire(in, a, ids); err != nil {
						return err
					}
				}
			default:
				if ar.Services, err = r.clear(in, a); err != nil {
					return err
				}
			}
			r.result.Alerts = append(r.result.Alerts, ar)
		}
		if err := r.finish(); err != nil {
			return err
		}
		res = r.result
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("apply alerts of integration %d: %w", in.ID, err)
	}
	return res, nil
}

// ClearIntegrationAlert treats a firing alert of an integration as resolved,
// as if the tool had reported its recovery: the remedy for a recovery that
// the tool did not send or the status page did not receive. Its incidents
// say that an administrator cleared it and are resolved when it was their
// last firing alert. Returns ErrNotFound if the alert is not firing.
func (s *Store) ClearIntegrationAlert(ctx context.Context, in *Integration, key string) (*ApplyResult, error) {
	var res *ApplyResult
	err := s.inMonitoringTx(ctx, func(tx *db.Tx) error {
		r, err := s.newAlertRun(ctx, tx)
		if err != nil {
			return err
		}
		r.manual = true
		a := &AlertInput{Key: key}
		services, err := r.clear(in, a)
		if err != nil {
			return err
		}
		if len(services) == 0 {
			return ErrNotFound
		}
		r.result.Alerts = append(r.result.Alerts, AlertResult{IntegrationID: in.ID, Alert: a, Services: services})
		if err := r.finish(); err != nil {
			return err
		}
		res = r.result
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("clear alert of integration %d: %w", in.ID, err)
	}
	return res, nil
}

// ReleaseSuppressedAlerts attaches the alerts that fired while maintenance
// of their service was in progress and still fire, once no maintenance of
// the service is in progress any more (alerts of paused integrations wait).
// The incidents they open start now. The monitoring worker calls it every
// minute.
func (s *Store) ReleaseSuppressedAlerts(ctx context.Context) (*ApplyResult, error) {
	// Most of the time nothing waits: check that without a writing
	// transaction.
	var waiting int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM integration_alerts WHERE suppressed = ?`,
		true).Scan(&waiting); err != nil {
		return nil, fmt.Errorf("release alerts after maintenance: %w", err)
	}
	if waiting == 0 {
		return &ApplyResult{}, nil
	}
	var res *ApplyResult
	err := s.inMonitoringTx(ctx, func(tx *db.Tx) error {
		r, err := s.newAlertRun(ctx, tx)
		if err != nil {
			return err
		}
		type held struct {
			integrationID, serviceID int64
			key, title, name         string
			impact                   Status
			notify                   bool
		}
		rows, err := queryAll(ctx, tx, func(sc scanner) (held, error) {
			var (
				h      held
				impact string
			)
			err := sc.Scan(&h.integrationID, &h.key, &h.serviceID, &h.title, &impact, &h.name, &h.notify)
			h.impact = Status(impact)
			return h, err
		}, `SELECT a.integration_id, a.alert_key, a.service_id, a.title, a.impact, ig.name, ig.notify
		    FROM integration_alerts a JOIN integrations ig ON ig.id = a.integration_id
		    WHERE a.suppressed = ? AND ig.paused = ?
		    ORDER BY a.started_at, a.integration_id, a.alert_key, a.service_id`, true, false)
		if err != nil {
			return err
		}
		for _, h := range rows {
			busy, err := r.inMaintenance(h.serviceID)
			if err != nil {
				return err
			}
			if busy {
				continue
			}
			in := &Integration{ID: h.integrationID, Name: h.name, Notify: h.notify}
			a := &AlertInput{Key: h.key, Firing: true, Title: h.title, Impact: h.impact,
				Message: "It started during maintenance and has not cleared since."}
			sr := AlertServiceResult{ServiceID: h.serviceID, ServiceName: r.names[h.serviceID]}
			if sr.Action, sr.IncidentID, err = r.attach(in, a, h.serviceID, r.now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE integration_alerts SET incident_id = ?, suppressed = ? WHERE integration_id = ? AND alert_key = ? AND service_id = ?`,
				nullID(sr.IncidentID), sr.Action == AlertSuppressed, in.ID, a.Key, h.serviceID); err != nil {
				return err
			}
			r.result.Alerts = append(r.result.Alerts, AlertResult{IntegrationID: in.ID, Alert: a, Services: []AlertServiceResult{sr}})
		}
		if err := r.finish(); err != nil {
			return err
		}
		res = r.result
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("release alerts after maintenance: %w", err)
	}
	return res, nil
}

// alertRun is the work of one ApplyAlerts, ClearIntegrationAlert or
// ReleaseSuppressedAlerts call, inside its transaction.
type alertRun struct {
	ctx      context.Context
	tx       *db.Tx
	now      time.Time
	manual   bool             // ClearIntegrationAlert: an administrator cleared the alert
	services []Service        // id, name and monitor names, for MatchServices
	names    map[int64]string // service names by id
	changes  map[int64]*incidentChange
	order    []int64        // incidents in the order they were first changed
	resolved map[int64]bool // incidents resolved by finish

	// What the run found out, so that a batch of many alerts asks once
	// (the monitoring lock keeps other runs out; what editors change
	// meanwhile is seen through the incident locks).
	maintenance map[int64]bool  // service → maintenance in progress
	openOf      map[int64]int64 // service → its open automatic incident, locked by this run
	locked      map[int64]bool  // incidents locked (or opened) by this run: open until finish
	open        map[int64]bool  // incidents read (not locked) by this run → open

	result *ApplyResult
}

// incidentChange collects what happened to an incident during a run.
type incidentChange struct {
	created  bool
	notify   bool   // created by an integration that sends e-mails
	manual   bool   // the alerts were cleared by an administrator
	source   string // name of the integration that changed it first
	problems []alertLine
	cleared  []alertLine
	services map[int64]bool // services whose impact may have to change
}

// alertLine is an alert as mentioned in an update.
type alertLine struct{ key, title, message string }

// addLine appends l unless an alert with its key is listed already (an alert
// that names two services of one incident is mentioned once).
func addLine(lines []alertLine, l alertLine) []alertLine {
	if slices.ContainsFunc(lines, func(o alertLine) bool { return o.key == l.key }) {
		return lines
	}
	return append(lines, l)
}

// newAlertRun loads the services (the caller holds the monitoring lock).
func (s *Store) newAlertRun(ctx context.Context, tx *db.Tx) (*alertRun, error) {
	services, err := queryAll(ctx, tx, func(sc scanner) (Service, error) {
		var (
			svc   Service
			names string
		)
		err := sc.Scan(&svc.ID, &svc.Name, &names)
		svc.MonitorNames = decodeMonitorNames(names)
		return svc, err
	}, `SELECT sv.id, sv.name, sv.monitor_names FROM services sv ORDER BY `+serviceOrderSQL)
	if err != nil {
		return nil, err
	}
	r := &alertRun{
		ctx: ctx, tx: tx, now: nowTime(), services: services,
		names: make(map[int64]string, len(services)), changes: map[int64]*incidentChange{},
		resolved: map[int64]bool{}, maintenance: map[int64]bool{}, openOf: map[int64]int64{},
		locked: map[int64]bool{}, open: map[int64]bool{}, result: &ApplyResult{},
	}
	for _, svc := range services {
		r.names[svc.ID] = svc.Name
	}
	return r, nil
}

// touch returns the change record of an incident, creating it for in.
func (r *alertRun) touch(incidentID int64, in *Integration) *incidentChange {
	ch := r.changes[incidentID]
	if ch == nil {
		ch = &incidentChange{source: in.Name, manual: r.manual, services: map[int64]bool{}}
		r.changes[incidentID] = ch
		r.order = append(r.order, incidentID)
	}
	return ch
}

// alertRow is a row of integration_alerts.
type alertRow struct {
	incidentID    int64 // 0: NULL
	suppressed    bool
	title         string
	reportedStart sql.NullInt64
}

// alertRows returns the rows of an alert by service.
func (r *alertRun) alertRows(integrationID int64, key string) (map[int64]alertRow, error) {
	type keyed struct {
		serviceID int64
		row       alertRow
	}
	rows, err := queryAll(r.ctx, r.tx, func(sc scanner) (keyed, error) {
		var (
			k        keyed
			incident sql.NullInt64
		)
		err := sc.Scan(&k.serviceID, &incident, &k.row.suppressed, &k.row.title, &k.row.reportedStart)
		k.row.incidentID = incident.Int64
		return k, err
	}, `SELECT service_id, incident_id, suppressed, title, reported_start FROM integration_alerts WHERE integration_id = ? AND alert_key = ?`,
		integrationID, key)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]alertRow, len(rows))
	for _, k := range rows {
		out[k.serviceID] = k.row
	}
	return out, nil
}

// startTime is when the incident of a new alert starts: when the tool says
// the problem began, but not in the future and not before the integration
// was created (the history before it stays as it was).
func (r *alertRun) startTime(in *Integration, a *AlertInput) time.Time {
	t := a.StartedAt
	if t.IsZero() || t.After(r.now) {
		return r.now
	}
	if t.Before(in.CreatedAt) {
		t = in.CreatedAt
	}
	return truncate(t)
}

// reportedStart is the start of an alert as the tool reported it, for
// integration_alerts.reported_start (NULL if unknown).
func reportedStart(a *AlertInput) any {
	if a.StartedAt.IsZero() {
		return nil
	}
	return a.StartedAt.Unix()
}

// newOccurrence reports whether a firing alert whose incident an editor
// resolved or deleted started again: the tool reports a start more than a
// minute after the stored one, which is the last start reported before the
// dismissal (repeats keep it up to date). Tools that report starts keep the
// start of an alert while it fires.
func newOccurrence(row alertRow, a *AlertInput) bool {
	return row.reportedStart.Valid && !a.StartedAt.IsZero() && a.StartedAt.Unix() > row.reportedStart.Int64+60
}

// fire applies a firing alert to the services it names.
func (r *alertRun) fire(in *Integration, a *AlertInput, serviceIDs []int64) ([]AlertServiceResult, error) {
	rows, err := r.alertRows(in.ID, a.Key)
	if err != nil {
		return nil, err
	}
	impact := a.Impact
	if !ValidImpact(impact) {
		impact = StatusMajorOutage
	}
	w := &alertWrites{}
	out := make([]AlertServiceResult, 0, len(serviceIDs))
	for _, sid := range serviceIDs {
		res := AlertServiceResult{ServiceID: sid, ServiceName: r.names[sid]}
		if row, ok := rows[sid]; ok {
			res.Action, res.IncidentID, err = r.repeat(in, a, sid, impact, row, w)
		} else {
			res.Action, res.IncidentID, err = r.occur(in, a, sid, impact, w)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, r.write(in, a, impact, w)
}

// alertWrites collects the row changes of one firing alert that are alike
// for all its services, so that an alert of many services (a shared name)
// takes few statements.
type alertWrites struct {
	inserts   []any   // the values of new rows, len(alertInsertCols) each
	repeats   []int64 // services whose row is a repeat of an attached alert
	dismissed []int64 // services whose row is a repeat of a dismissed alert
}

const alertInsertCols = `integration_id, alert_key, service_id, incident_id, suppressed, impact, title, reported_start, started_at, updated_at`

// write runs the collected statements: new rows, then the repeats (a repeat
// without a title keeps the title; the reported start is kept up to date,
// so that a dismissal compares with the occurrence the incident showed; a
// dismissed alert keeps it).
func (r *alertRun) write(in *Integration, a *AlertInput, impact Status, w *alertWrites) error {
	const perInsert = 50 // rows per statement
	const n = 10         // columns
	for rows := w.inserts; len(rows) > 0; {
		k := min(len(rows), perInsert*n)
		values := strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?), ", k/n), ", ")
		if _, err := r.tx.ExecContext(r.ctx, `INSERT INTO integration_alerts (`+alertInsertCols+`) VALUES `+values, rows[:k]...); err != nil {
			return err
		}
		rows = rows[k:]
	}
	for _, u := range []struct {
		set      string
		args     []any
		services []int64
	}{
		{`impact = ?, title = COALESCE(NULLIF(?, ''), title), updated_at = ?, reported_start = COALESCE(?, reported_start)`,
			[]any{string(impact), a.Title, r.now.Unix(), reportedStart(a)}, w.repeats},
		{`impact = ?, title = COALESCE(NULLIF(?, ''), title), updated_at = ?`,
			[]any{string(impact), a.Title, r.now.Unix()}, w.dismissed},
	} {
		for ids := u.services; len(ids) > 0; {
			k := min(len(ids), 500)
			args := append(slices.Clone(u.args), in.ID, a.Key)
			for _, id := range ids[:k] {
				args = append(args, id)
			}
			if _, err := r.tx.ExecContext(r.ctx, `UPDATE integration_alerts SET `+u.set+
				` WHERE integration_id = ? AND alert_key = ? AND service_id IN (`+strings.TrimSuffix(strings.Repeat("?, ", k), ", ")+`)`,
				args...); err != nil {
				return err
			}
			ids = ids[k:]
		}
	}
	return nil
}

// occur applies a new occurrence of a firing alert to one service.
func (r *alertRun) occur(in *Integration, a *AlertInput, serviceID int64, impact Status, w *alertWrites) (AlertAction, int64, error) {
	started := r.startTime(in, a)
	action, incidentID, err := r.attach(in, a, serviceID, started)
	if err != nil {
		return "", 0, err
	}
	w.inserts = append(w.inserts, in.ID, a.Key, serviceID, nullID(incidentID), action == AlertSuppressed, string(impact), a.Title,
		reportedStart(a), started.Unix(), r.now.Unix())
	return action, incidentID, nil
}

// repeat applies a firing alert to a service for which it fires already.
func (r *alertRun) repeat(in *Integration, a *AlertInput, serviceID int64, impact Status, row alertRow, w *alertWrites) (AlertAction, int64, error) {
	title := a.Title
	if title == "" {
		title = row.title
	}
	const where = ` WHERE integration_id = ? AND alert_key = ? AND service_id = ?`
	switch {
	case row.suppressed:
		// Fired during maintenance: attach it now if the maintenance has
		// ended. The incident starts now, not during the maintenance.
		action, incidentID, err := r.attach(in, a, serviceID, r.now)
		if err != nil {
			return "", 0, err
		}
		_, err = r.tx.ExecContext(r.ctx,
			`UPDATE integration_alerts SET impact = ?, title = ?, updated_at = ?, reported_start = COALESCE(?, reported_start),
			 incident_id = ?, suppressed = ?`+where,
			string(impact), title, r.now.Unix(), reportedStart(a), nullID(incidentID), action == AlertSuppressed,
			in.ID, a.Key, serviceID)
		return action, incidentID, err
	case row.incidentID != 0:
		open, err := r.incidentOpen(row.incidentID)
		if err != nil {
			return "", 0, err
		}
		if open {
			w.repeats = append(w.repeats, serviceID)
			r.touch(row.incidentID, in).services[serviceID] = true // the impact may have changed
			return AlertRepeated, row.incidentID, nil
		}
	}
	// Its incident was resolved by an editor, or deleted: dismissed, unless
	// the alert started again.
	if !newOccurrence(row, a) {
		w.dismissed = append(w.dismissed, serviceID)
		return AlertDismissed, row.incidentID, nil
	}
	started := r.startTime(in, a)
	action, incidentID, err := r.attach(in, a, serviceID, started)
	if err != nil {
		return "", 0, err
	}
	_, err = r.tx.ExecContext(r.ctx,
		`UPDATE integration_alerts SET impact = ?, title = ?, updated_at = ?, reported_start = ?, started_at = ?,
		 incident_id = ?, suppressed = ?`+where,
		string(impact), title, r.now.Unix(), reportedStart(a), started.Unix(), nullID(incidentID), action == AlertSuppressed,
		in.ID, a.Key, serviceID)
	return action, incidentID, err
}

// attach finds the incident for a firing alert of a service: the open
// automatic incident of the service, or a new one that starts at started;
// while maintenance of the service is in progress the alert is suppressed
// instead (no incident). It returns what it did and the incident; the
// caller writes the alert's row.
func (r *alertRun) attach(in *Integration, a *AlertInput, serviceID int64, started time.Time) (AlertAction, int64, error) {
	busy, err := r.inMaintenance(serviceID)
	if err != nil {
		return "", 0, err
	}
	if busy {
		return AlertSuppressed, 0, nil
	}
	action := AlertJoined
	incidentID, err := r.openIncidentOf(serviceID)
	if err != nil {
		return "", 0, err
	}
	if incidentID == 0 {
		action = AlertOpened
		impact := a.Impact
		if !ValidImpact(impact) {
			impact = StatusMajorOutage
		}
		err = r.tx.QueryRowContext(r.ctx,
			`INSERT INTO incidents (title, status, started_at, resolved_at, created_by, created_at, updated_at, automatic, automatic_notify)
			 VALUES (?, ?, ?, NULL, NULL, ?, ?, ?, ?) RETURNING id`,
			AutomaticIncidentTitle(r.names[serviceID], a.Title), string(IncidentInvestigating), started.Unix(),
			r.now.Unix(), r.now.Unix(), true, in.Notify).Scan(&incidentID)
		if err == nil {
			_, err = r.tx.ExecContext(r.ctx, `INSERT INTO incident_services (incident_id, service_id, impact) VALUES (?, ?, ?)`,
				incidentID, serviceID, string(impact))
		}
		if err != nil {
			return "", 0, err
		}
		ch := r.touch(incidentID, in)
		ch.created, ch.notify = true, in.Notify
		r.locked[incidentID] = true
		r.openOf[serviceID] = incidentID
	}
	ch := r.touch(incidentID, in)
	ch.problems = addLine(ch.problems, alertLine{key: a.Key, title: a.Title, message: a.Message})
	ch.services[serviceID] = true
	return action, incidentID, nil
}

// openIncidentOf returns the newest open automatic incident that affects the
// service (0 if there is none), locked for this transaction. On PostgreSQL
// the lock waits for an editor who is resolving that incident at this
// moment; it is then no longer open and the next one is looked for, so that
// an alert never joins an incident that is being resolved (it would be
// dismissed with it).
func (r *alertRun) openIncidentOf(serviceID int64) (int64, error) {
	if id, ok := r.openOf[serviceID]; ok {
		return id, nil
	}
	const tries = 3
	for range tries {
		var id int64
		err := r.tx.QueryRowContext(r.ctx,
			`SELECT i.id FROM incidents i JOIN incident_services x ON x.incident_id = i.id
			 WHERE x.service_id = ? AND i.automatic = ? AND i.status <> ? ORDER BY i.id DESC LIMIT 1`,
			serviceID, true, string(IncidentResolved)).Scan(&id)
		if err = notFound(err); errors.Is(err, ErrNotFound) {
			return 0, nil
		} else if err != nil {
			return 0, err
		}
		if r.locked[id] {
			// Locked by this run before: nobody changed it since.
			r.openOf[serviceID] = id
			return id, nil
		}
		// The lock; then check again what the editor may have changed
		// before it was granted (status, affected services).
		res, err := r.tx.ExecContext(r.ctx, `UPDATE incidents SET updated_at = updated_at WHERE id = ? AND status <> ?`,
			id, string(IncidentResolved))
		if err != nil {
			return 0, err
		}
		if k, err := res.RowsAffected(); err != nil {
			return 0, err
		} else if k == 0 {
			r.open[id] = false
			continue
		}
		r.locked[id] = true
		var n int
		if err := r.tx.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM incident_services WHERE incident_id = ? AND service_id = ?`,
			id, serviceID).Scan(&n); err != nil {
			return 0, err
		}
		if n > 0 {
			r.openOf[serviceID] = id
			return id, nil
		}
	}
	return 0, nil // the candidates kept changing: open a new incident
}

// clear applies a resolved alert: it deletes the rows of the alert (every
// service it affects) and notes the change of their open incidents. An
// alert without a title takes the title of its rows.
func (r *alertRun) clear(in *Integration, a *AlertInput) ([]AlertServiceResult, error) {
	type firingRow struct {
		serviceID int64
		row       alertRow
	}
	rows, err := queryAll(r.ctx, r.tx, func(sc scanner) (firingRow, error) {
		var (
			f        firingRow
			incident sql.NullInt64
		)
		err := sc.Scan(&f.serviceID, &incident, &f.row.title)
		f.row.incidentID = incident.Int64
		return f, err
	}, `SELECT service_id, incident_id, title FROM integration_alerts WHERE integration_id = ? AND alert_key = ?
	    ORDER BY service_id`, in.ID, a.Key)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	if a.Title == "" && r.manual {
		a.Title = rows[0].row.title // for the request log
	}
	if _, err := r.tx.ExecContext(r.ctx, `DELETE FROM integration_alerts WHERE integration_id = ? AND alert_key = ?`, in.ID, a.Key); err != nil {
		return nil, err
	}
	var out []AlertServiceResult
	for _, f := range rows {
		res := AlertServiceResult{ServiceID: f.serviceID, ServiceName: r.names[f.serviceID], Action: AlertEnded, IncidentID: f.row.incidentID}
		if f.row.incidentID != 0 {
			open, err := r.incidentOpen(f.row.incidentID)
			if err != nil {
				return nil, err
			}
			if open {
				title := f.row.title
				if title == "" {
					title = a.Title
				}
				ch := r.touch(f.row.incidentID, in)
				ch.cleared = addLine(ch.cleared, alertLine{key: a.Key, title: title})
				ch.services[f.serviceID] = true
				res.Action = AlertCleared
			}
		}
		out = append(out, res)
	}
	return out, nil
}

// inMaintenance reports whether maintenance of the service is in progress.
func (r *alertRun) inMaintenance(serviceID int64) (bool, error) {
	if busy, ok := r.maintenance[serviceID]; ok {
		return busy, nil
	}
	var n int
	err := r.tx.QueryRowContext(r.ctx,
		`SELECT COUNT(*) FROM maintenances m JOIN maintenance_services ms ON ms.maintenance_id = m.id
		 WHERE ms.service_id = ? AND m.cancelled = ? AND m.starts_at <= ? AND m.ends_at > ?`,
		serviceID, false, r.now.Unix(), r.now.Unix()).Scan(&n)
	if err != nil {
		return false, err
	}
	r.maintenance[serviceID] = n > 0
	return n > 0, nil
}

// incidentOpen reports whether the incident exists and is not resolved.
func (r *alertRun) incidentOpen(id int64) (bool, error) {
	if r.locked[id] {
		return true, nil
	}
	if open, ok := r.open[id]; ok {
		return open, nil
	}
	var status string
	err := r.tx.QueryRowContext(r.ctx, `SELECT status FROM incidents WHERE id = ?`, id).Scan(&status)
	if err = notFound(err); errors.Is(err, ErrNotFound) {
		r.open[id] = false
		return false, nil
	} else if err != nil {
		return false, err
	}
	r.open[id] = IncidentStatus(status) != IncidentResolved
	return r.open[id], nil
}

// finish brings the changed incidents in line with their alerts: impacts,
// one update per incident with what happened, and resolution when no alert
// fires any more.
func (r *alertRun) finish() error {
	for _, id := range r.order {
		ch := r.changes[id]
		if !r.locked[id] {
			// Take the row lock (PostgreSQL), so that an editor who
			// resolves the incident at the same time is seen, and skip
			// incidents that are no longer open.
			n, err := r.tx.ExecContext(r.ctx, `UPDATE incidents SET updated_at = updated_at WHERE id = ? AND status <> ?`,
				id, string(IncidentResolved))
			if err != nil {
				return err
			}
			if k, err := n.RowsAffected(); err != nil {
				return err
			} else if k == 0 {
				continue
			}
			r.locked[id] = true
		}
		var (
			status string
			notify bool
		)
		if err := r.tx.QueryRowContext(r.ctx, `SELECT status, automatic_notify FROM incidents WHERE id = ?`, id).Scan(&status, &notify); err != nil {
			return err
		}
		impactLines, changed, err := r.reconcileImpacts(id, ch)
		if err != nil {
			return err
		}
		var firing int
		if err := r.tx.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM integration_alerts WHERE incident_id = ?`,
			id).Scan(&firing); err != nil {
			return err
		}
		var opened, update, resolution *IncidentUpdate
		if ch.created {
			opened = &IncidentUpdate{Status: IncidentStatus(status), Message: ch.openMessage(), Source: ch.source}
		} else if msg := ch.updateMessage(impactLines, firing); msg != "" && firing > 0 {
			update = &IncidentUpdate{Status: IncidentStatus(status), Message: msg, Source: ch.source}
		}
		if firing == 0 {
			resolution = &IncidentUpdate{Status: IncidentResolved, Message: ch.resolveMessage(), Source: ch.source}
			r.resolved[id] = true
		}
		for _, upd := range []*IncidentUpdate{opened, update, resolution} {
			if upd == nil {
				continue
			}
			if err := insertUpdate(r.ctx, r.tx, id, upd, r.now); err != nil {
				return err
			}
		}
		// E-mails: the integration that opened the incident decides
		// (automatic_notify), also for a resolution that another
		// integration's alert brings. Opened and resolved in one run: one
		// e-mail, about the resolution.
		switch {
		case !notify:
		case resolution != nil:
			r.result.Notices = append(r.result.Notices, IncidentNotice{IncidentID: id, UpdateID: resolution.ID})
		case opened != nil:
			r.result.Notices = append(r.result.Notices, IncidentNotice{IncidentID: id, UpdateID: opened.ID, Created: true})
		}
		switch {
		case resolution != nil:
			_, err = r.tx.ExecContext(r.ctx, `UPDATE incidents SET status = ?, resolved_at = ?, updated_at = ? WHERE id = ?`,
				string(IncidentResolved), r.now.Unix(), r.now.Unix(), id)
		case changed || opened != nil || update != nil:
			_, err = r.tx.ExecContext(r.ctx, `UPDATE incidents SET updated_at = ? WHERE id = ?`, r.now.Unix(), id)
		}
		if err != nil {
			return err
		}
	}
	for i := range r.result.Alerts {
		for j := range r.result.Alerts[i].Services {
			if sr := &r.result.Alerts[i].Services[j]; sr.Action == AlertCleared && r.resolved[sr.IncidentID] {
				sr.Action = AlertResolved
			}
		}
	}
	return nil
}

// reconcileImpacts sets the impact of the incident on every changed service
// that has firing alerts to their worst impact, and returns a line for each
// change of an incident that existed before, and whether it changed an
// impact. A service without firing alerts keeps its impact, and one an
// editor removed from the incident is not added again.
func (r *alertRun) reconcileImpacts(incidentID int64, ch *incidentChange) (lines []string, changed bool, err error) {
	ids := make([]int64, 0, len(ch.services))
	for sid := range ch.services {
		ids = append(ids, sid)
	}
	slices.SortFunc(ids, func(a, b int64) int { return strings.Compare(r.serviceSortKey(a), r.serviceSortKey(b)) })
	for _, sid := range ids {
		impacts, err := queryAll(r.ctx, r.tx, func(sc scanner) (Status, error) {
			var v string
			err := sc.Scan(&v)
			return Status(v), err
		}, `SELECT impact FROM integration_alerts WHERE incident_id = ? AND service_id = ?`, incidentID, sid)
		if err != nil {
			return nil, false, err
		}
		if len(impacts) == 0 {
			continue
		}
		worst := StatusDegraded
		for _, v := range impacts {
			if ValidImpact(v) {
				worst = Worst(worst, v)
			}
		}
		var cur string
		err = r.tx.QueryRowContext(r.ctx, `SELECT impact FROM incident_services WHERE incident_id = ? AND service_id = ?`,
			incidentID, sid).Scan(&cur)
		if err = notFound(err); errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return nil, false, err
		}
		if Status(cur) == worst {
			continue
		}
		if _, err := r.tx.ExecContext(r.ctx, `UPDATE incident_services SET impact = ? WHERE incident_id = ? AND service_id = ?`,
			string(worst), incidentID, sid); err != nil {
			return nil, false, err
		}
		changed = true
		if !ch.created {
			lines = append(lines, fmt.Sprintf("Impact on %s: %s (was: %s).", r.names[sid], worst.Label(), Status(cur).Label()))
		}
	}
	return lines, changed, nil
}

// serviceSortKey orders services by name for the updates.
func (r *alertRun) serviceSortKey(id int64) string {
	return strings.ToLower(r.names[id]) + "\x00" + strconv.FormatInt(id, 10)
}

// ---------------------------------------------------------------- the texts of the updates

// maxListedTitles is the number of lines an update lists alerts in; the
// alerts beyond are counted.
const maxListedTitles = 10

// alertGroup is the alerts of an update that have one title.
type alertGroup struct {
	title   string // quoted, see alertTitle
	message string // the message of the first alert
	n       int
}

// alertTitle is the title of an alert as quoted in updates.
func alertTitle(l alertLine) string {
	if l.title == "" {
		return "an alert without a title"
	}
	return "“" + l.title + "”"
}

// groupAlerts groups alerts by title, in the order of their first alert.
func groupAlerts(lines []alertLine) []alertGroup {
	var groups []alertGroup
	at := map[string]int{}
	for _, l := range lines {
		t := alertTitle(l)
		if i, ok := at[t]; ok {
			groups[i].n++
			continue
		}
		at[t] = len(groups)
		groups = append(groups, alertGroup{title: t, message: l.message, n: 1})
	}
	return groups
}

// phrase names a group: “X”, or “X” (3 alerts).
func (g alertGroup) phrase() string {
	if g.n == 1 {
		return g.title
	}
	return fmt.Sprintf("%s (%d alerts)", g.title, g.n)
}

// verb returns singular or plural for the group.
func (g alertGroup) verb(singular, plural string) string {
	if g.n == 1 {
		return singular
	}
	return plural
}

// bulletList lists groups, one line each, at most maxListedTitles lines.
func bulletList(groups []alertGroup) string {
	var b strings.Builder
	for i, g := range groups {
		if i == maxListedTitles-1 && len(groups) > maxListedTitles {
			rest := 0
			for _, g := range groups[i:] {
				rest += g.n
			}
			fmt.Fprintf(&b, "\n- and %d more alerts", rest)
			break
		}
		b.WriteString("\n- ")
		b.WriteString(g.phrase())
	}
	return b.String()
}

// openMessage is the first update of an incident that monitoring opened.
func (ch *incidentChange) openMessage() string {
	groups := groupAlerts(ch.problems)
	switch len(groups) {
	case 0:
		return "Monitoring reports a problem."
	case 1:
		g := groups[0]
		msg := "Monitoring reports " + g.verb("a problem", "problems") + ": " + g.phrase() + "."
		if g.n == 1 && g.message != "" {
			msg += "\n\n" + g.message
		}
		return msg
	}
	return "Monitoring reports these problems:" + bulletList(groups)
}

// updateMessage is the update of an open incident that monitoring changed
// ("" if nothing worth an update happened, e.g. a repeated alert).
func (ch *incidentChange) updateMessage(impactLines []string, firing int) string {
	var parts []string
	problems := groupAlerts(ch.problems)
	switch len(problems) {
	case 0:
	case 1:
		g := problems[0]
		part := "Monitoring reports " + g.verb("another problem", "more problems") + ": " + g.phrase() + "."
		if g.n == 1 && g.message != "" {
			part += "\n\n" + g.message
		}
		parts = append(parts, part)
	default:
		parts = append(parts, "Monitoring reports more problems:"+bulletList(problems))
	}
	if cleared := groupAlerts(ch.cleared); len(cleared) > 0 {
		still := "1 alert is still firing"
		if firing != 1 {
			still = strconv.Itoa(firing) + " alerts are still firing"
		}
		switch {
		case len(cleared) == 1 && ch.manual:
			parts = append(parts, "An administrator cleared "+cleared[0].phrase()+" (the monitoring tool did not report its end); "+still+".")
		case len(cleared) == 1:
			g := cleared[0]
			parts = append(parts, "Monitoring reports that "+g.phrase()+" "+g.verb("is", "are")+" resolved; "+still+".")
		default:
			parts = append(parts, "Monitoring reports that these are resolved:"+bulletList(cleared)+"\n"+strings.ToUpper(still[:1])+still[1:]+".")
		}
	}
	parts = append(parts, impactLines...)
	return strings.Join(parts, "\n\n")
}

// resolveMessage is the update that resolves an incident.
func (ch *incidentChange) resolveMessage() string {
	cleared := groupAlerts(ch.cleared)
	switch {
	case len(cleared) == 0:
		return "Resolved: monitoring reports no more problems."
	case len(cleared) == 1 && ch.manual:
		return "Resolved: an administrator cleared " + cleared[0].phrase() + " (the monitoring tool did not report its end)."
	case len(cleared) == 1:
		g := cleared[0]
		return "Resolved: monitoring reports that " + g.phrase() + " " + g.verb("is", "are") + " resolved."
	}
	return "Resolved: monitoring reports that these are resolved:" + bulletList(cleared)
}
