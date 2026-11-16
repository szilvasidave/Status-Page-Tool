package store

// What the alerts of monitoring tools change (package monitoring reads them
// from the tools' requests).
//
// An alert affects the services it names (MatchServices). Each alert and
// service has a row in integration_alerts: the alert's state as the tool
// last reported it, and the incident it is attached to. The incidents are
// brought in line with these rows at the end of every request (finish):
//
//   - A firing alert that was not firing before is attached to the newest
//     open automatic incident that affects the service, or opens a new
//     automatic incident for it. While maintenance of the service is in
//     progress it is suppressed instead: ReleaseSuppressedAlerts attaches it
//     once the maintenance has ended, if it still fires.
//   - The impact of an automatic incident on a service is the worst impact
//     of the alerts that fire for it there.
//   - When the last firing alert of an incident resolves, the incident is
//     resolved.
//   - A firing alert that was firing before (a repeat, e.g. after
//     Alertmanager's repeat_interval) only updates its title and impact.
//     If an editor resolved or deleted its incident meanwhile, it stays
//     detached until it resolves; its next occurrence opens a new incident.
//
// Automation never touches incidents that people opened. It changes the
// automatic ones by posting updates (source: the integration's name),
// setting the impact of services with firing alerts and resolving them;
// editors can post updates, rename and resolve them like any incident.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

// monitoringLockID identifies the PostgreSQL advisory lock taken by
// lockMonitoring.
const monitoringLockID = 7395001

// lockMonitoring serialises the transactions of ApplyAlerts and
// ReleaseSuppressedAlerts, so that two requests cannot both decide that a
// service has no automatic incident yet. PostgreSQL needs a lock for that;
// SQLite runs writing transactions one at a time anyway (BEGIN IMMEDIATE).
// Call it first in the transaction.
func (s *Store) lockMonitoring(ctx context.Context, tx *db.Tx) error {
	if !s.db.IsPostgres() {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, monitoringLockID)
	return err
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
// if it names the service, else "<service>: <alert>", at most 200
// characters.
func AutomaticIncidentTitle(service, alert string) string {
	alert = strings.Join(strings.Fields(alert), " ")
	var t string
	switch {
	case alert == "":
		t = service + ": monitoring reports a problem"
	case strings.Contains(strings.ToLower(alert), strings.ToLower(service)):
		t = alert
	default:
		t = service + ": " + alert
	}
	return truncateRunes(t, 200)
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
// it did, with the incidents to send e-mails about (if in.Notify). Firing
// alerts without a key, or that name no service, change nothing; resolved
// alerts that were not firing change nothing.
func (s *Store) ApplyAlerts(ctx context.Context, in *Integration, alerts []AlertInput) (*ApplyResult, error) {
	var res *ApplyResult
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
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
				for _, sid := range ids {
					sr, err := r.fire(in, a, sid)
					if err != nil {
						return err
					}
					ar.Services = append(ar.Services, sr)
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

// ReleaseSuppressedAlerts attaches the alerts that fired while maintenance
// of their service was in progress and still fire, once no maintenance of
// the service is in progress any more (alerts of paused integrations wait).
// The incidents they open start now. The monitoring worker calls it every
// minute.
func (s *Store) ReleaseSuppressedAlerts(ctx context.Context) (*ApplyResult, error) {
	// Most of the time nothing waits: check that without a writing
	// transaction.
	var waiting int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM integration_alerts WHERE firing = ? AND suppressed = ?`,
		true, true).Scan(&waiting); err != nil {
		return nil, fmt.Errorf("release alerts after maintenance: %w", err)
	}
	if waiting == 0 {
		return &ApplyResult{}, nil
	}
	var res *ApplyResult
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
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
		    WHERE a.firing = ? AND a.suppressed = ? AND ig.paused = ?
		    ORDER BY a.started_at, a.integration_id, a.alert_key, a.service_id`, true, true, false)
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

// alertRun is the work of one ApplyAlerts or ReleaseSuppressedAlerts call,
// inside its transaction.
type alertRun struct {
	ctx      context.Context
	tx       *db.Tx
	now      time.Time
	services []Service        // id, name and monitor names, for MatchServices
	names    map[int64]string // service names by id
	changes  map[int64]*incidentChange
	order    []int64        // incidents in the order they were first changed
	resolved map[int64]bool // incidents resolved by finish
	result   *ApplyResult
}

// incidentChange collects what happened to an incident during a run.
type incidentChange struct {
	created  bool
	source   string // name of the integration that changed it first
	notify   bool   // that integration sends e-mails
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

// newAlertRun takes the lock and loads the services.
func (s *Store) newAlertRun(ctx context.Context, tx *db.Tx) (*alertRun, error) {
	if err := s.lockMonitoring(ctx, tx); err != nil {
		return nil, err
	}
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
		resolved: map[int64]bool{}, result: &ApplyResult{},
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
		ch = &incidentChange{source: in.Name, notify: in.Notify, services: map[int64]bool{}}
		r.changes[incidentID] = ch
		r.order = append(r.order, incidentID)
	}
	return ch
}

// alertRow is a row of integration_alerts.
type alertRow struct {
	incidentID int64 // 0: NULL
	firing     bool
	suppressed bool
	title      string
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

// fire applies a firing alert to one service it names.
func (r *alertRun) fire(in *Integration, a *AlertInput, serviceID int64) (AlertServiceResult, error) {
	res := AlertServiceResult{ServiceID: serviceID, ServiceName: r.names[serviceID]}
	impact := a.Impact
	if !ValidImpact(impact) {
		impact = StatusMajorOutage
	}
	var row alertRow
	var incident sql.NullInt64
	err := r.tx.QueryRowContext(r.ctx,
		`SELECT incident_id, firing, suppressed, title FROM integration_alerts WHERE integration_id = ? AND alert_key = ? AND service_id = ?`,
		in.ID, a.Key, serviceID).Scan(&incident, &row.firing, &row.suppressed, &row.title)
	if err = notFound(err); err != nil && !errors.Is(err, ErrNotFound) {
		return res, err
	}
	row.incidentID = incident.Int64

	if errors.Is(err, ErrNotFound) || !row.firing {
		// A new occurrence of the alert.
		started := r.startTime(in, a)
		if _, err := r.tx.ExecContext(r.ctx,
			`INSERT INTO integration_alerts (integration_id, alert_key, service_id, incident_id, firing, suppressed, impact, title, started_at, updated_at)
			 VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (integration_id, alert_key, service_id) DO UPDATE SET
			   incident_id = NULL, firing = excluded.firing, suppressed = excluded.suppressed, impact = excluded.impact,
			   title = excluded.title, started_at = excluded.started_at, updated_at = excluded.updated_at`,
			in.ID, a.Key, serviceID, true, false, string(impact), a.Title, started.Unix(), r.now.Unix()); err != nil {
			return res, err
		}
		res.Action, res.IncidentID, err = r.attach(in, a, serviceID, started)
		return res, err
	}

	// A repeat: the alert was firing already. A repeat without a title keeps
	// the title.
	title := a.Title
	if title == "" {
		title = row.title
	}
	if _, err := r.tx.ExecContext(r.ctx,
		`UPDATE integration_alerts SET impact = ?, title = ?, updated_at = ? WHERE integration_id = ? AND alert_key = ? AND service_id = ?`,
		string(impact), title, r.now.Unix(), in.ID, a.Key, serviceID); err != nil {
		return res, err
	}
	switch {
	case row.incidentID != 0:
		res.IncidentID = row.incidentID
		open, err := r.incidentOpen(row.incidentID)
		if err != nil {
			return res, err
		}
		if !open {
			res.Action = AlertDismissed // resolved by an editor
			return res, nil
		}
		r.touch(row.incidentID, in).services[serviceID] = true // the impact may have changed
		res.Action = AlertRepeated
	case row.suppressed:
		// Fired during maintenance: attach it now if the maintenance has
		// ended. The incident starts now, not during the maintenance.
		res.Action, res.IncidentID, err = r.attach(in, a, serviceID, r.now)
		return res, err
	default:
		res.Action = AlertDismissed // its incident was deleted
	}
	return res, nil
}

// attach attaches a firing alert (its row exists) of a service to the open
// automatic incident of the service, or opens one that starts at started;
// while maintenance of the service is in progress it suppresses the alert
// instead. It returns what it did and the incident.
func (r *alertRun) attach(in *Integration, a *AlertInput, serviceID int64, started time.Time) (AlertAction, int64, error) {
	busy, err := r.inMaintenance(serviceID)
	if err != nil {
		return "", 0, err
	}
	if busy {
		_, err := r.tx.ExecContext(r.ctx,
			`UPDATE integration_alerts SET suppressed = ?, incident_id = NULL WHERE integration_id = ? AND alert_key = ? AND service_id = ?`,
			true, in.ID, a.Key, serviceID)
		return AlertSuppressed, 0, err
	}
	action := AlertJoined
	var incidentID int64
	err = r.tx.QueryRowContext(r.ctx,
		`SELECT i.id FROM incidents i JOIN incident_services x ON x.incident_id = i.id
		 WHERE x.service_id = ? AND i.automatic = ? AND i.status <> ? ORDER BY i.id DESC LIMIT 1`,
		serviceID, true, string(IncidentResolved)).Scan(&incidentID)
	if err = notFound(err); errors.Is(err, ErrNotFound) {
		action = AlertOpened
		impact := a.Impact
		if !ValidImpact(impact) {
			impact = StatusMajorOutage
		}
		err = r.tx.QueryRowContext(r.ctx,
			`INSERT INTO incidents (title, status, started_at, resolved_at, created_by, created_at, updated_at, automatic)
			 VALUES (?, ?, ?, NULL, NULL, ?, ?, ?) RETURNING id`,
			AutomaticIncidentTitle(r.names[serviceID], a.Title), string(IncidentInvestigating), started.Unix(),
			r.now.Unix(), r.now.Unix(), true).Scan(&incidentID)
		if err == nil {
			_, err = r.tx.ExecContext(r.ctx, `INSERT INTO incident_services (incident_id, service_id, impact) VALUES (?, ?, ?)`,
				incidentID, serviceID, string(impact))
		}
		if err == nil {
			r.touch(incidentID, in).created = true
		}
	}
	if err != nil {
		return "", 0, err
	}
	ch := r.touch(incidentID, in)
	ch.problems = addLine(ch.problems, alertLine{key: a.Key, title: a.Title, message: a.Message})
	ch.services[serviceID] = true
	_, err = r.tx.ExecContext(r.ctx,
		`UPDATE integration_alerts SET incident_id = ?, suppressed = ? WHERE integration_id = ? AND alert_key = ? AND service_id = ?`,
		incidentID, false, in.ID, a.Key, serviceID)
	return action, incidentID, err
}

// clear applies a resolved alert: every service its firing alert affects.
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
	}, `SELECT service_id, incident_id, title FROM integration_alerts WHERE integration_id = ? AND alert_key = ? AND firing = ?
	    ORDER BY service_id`, in.ID, a.Key, true)
	if err != nil {
		return nil, err
	}
	var out []AlertServiceResult
	for _, f := range rows {
		if _, err := r.tx.ExecContext(r.ctx,
			`UPDATE integration_alerts SET firing = ?, suppressed = ?, updated_at = ? WHERE integration_id = ? AND alert_key = ? AND service_id = ?`,
			false, false, r.now.Unix(), in.ID, a.Key, f.serviceID); err != nil {
			return nil, err
		}
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
	var n int
	err := r.tx.QueryRowContext(r.ctx,
		`SELECT COUNT(*) FROM maintenances m JOIN maintenance_services ms ON ms.maintenance_id = m.id
		 WHERE ms.service_id = ? AND m.cancelled = ? AND m.starts_at <= ? AND m.ends_at > ?`,
		serviceID, false, r.now.Unix(), r.now.Unix()).Scan(&n)
	return n > 0, err
}

// incidentOpen reports whether the incident exists and is not resolved.
func (r *alertRun) incidentOpen(id int64) (bool, error) {
	var status string
	err := r.tx.QueryRowContext(r.ctx, `SELECT status FROM incidents WHERE id = ?`, id).Scan(&status)
	if err = notFound(err); errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil && IncidentStatus(status) != IncidentResolved, err
}

// finish brings the changed incidents in line with their alerts: impacts,
// one update per incident with what happened, and resolution when no alert
// fires any more.
func (r *alertRun) finish() error {
	for _, id := range r.order {
		ch := r.changes[id]
		// Take the row lock first (PostgreSQL), so that an editor who
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
		var status string
		if err := r.tx.QueryRowContext(r.ctx, `SELECT status FROM incidents WHERE id = ?`, id).Scan(&status); err != nil {
			return err
		}
		impactLines, changed, err := r.reconcileImpacts(id, ch)
		if err != nil {
			return err
		}
		var firing int
		if err := r.tx.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM integration_alerts WHERE incident_id = ? AND firing = ?`,
			id, true).Scan(&firing); err != nil {
			return err
		}
		var updates []*IncidentUpdate
		if ch.created {
			updates = append(updates, &IncidentUpdate{Status: IncidentStatus(status), Message: ch.openMessage(), Source: ch.source})
			if ch.notify {
				r.result.Notices = append(r.result.Notices, IncidentNotice{IncidentID: id, Created: true})
			}
		} else if msg := ch.updateMessage(impactLines, firing); msg != "" && firing > 0 {
			updates = append(updates, &IncidentUpdate{Status: IncidentStatus(status), Message: msg, Source: ch.source})
		}
		resolve := firing == 0
		if resolve {
			updates = append(updates, &IncidentUpdate{Status: IncidentResolved, Message: ch.resolveMessage(), Source: ch.source})
			r.resolved[id] = true
			if ch.notify {
				r.result.Notices = append(r.result.Notices, IncidentNotice{IncidentID: id})
			}
		}
		for _, upd := range updates {
			if err := insertUpdate(r.ctx, r.tx, id, upd, r.now); err != nil {
				return err
			}
		}
		switch {
		case resolve:
			_, err = r.tx.ExecContext(r.ctx, `UPDATE incidents SET status = ?, resolved_at = ?, updated_at = ? WHERE id = ?`,
				string(IncidentResolved), r.now.Unix(), r.now.Unix(), id)
		case changed || len(updates) > 0:
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
		}, `SELECT impact FROM integration_alerts WHERE incident_id = ? AND service_id = ? AND firing = ?`, incidentID, sid, true)
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

// alertTitle is the title of an alert as quoted in updates.
func alertTitle(l alertLine) string {
	if l.title == "" {
		return "an alert without a title"
	}
	return "“" + l.title + "”"
}

// openMessage is the first update of an incident that monitoring opened.
func (ch *incidentChange) openMessage() string {
	switch len(ch.problems) {
	case 0:
		return "Monitoring reports a problem."
	case 1:
		msg := "Monitoring reports a problem: " + alertTitle(ch.problems[0]) + "."
		if m := ch.problems[0].message; m != "" {
			msg += "\n\n" + m
		}
		return msg
	}
	return "Monitoring reports these problems:" + bulletList(ch.problems)
}

// updateMessage is the update of an open incident that monitoring changed
// ("" if nothing worth an update happened, e.g. a repeated alert).
func (ch *incidentChange) updateMessage(impactLines []string, firing int) string {
	var parts []string
	switch len(ch.problems) {
	case 0:
	case 1:
		part := "Monitoring reports another problem: " + alertTitle(ch.problems[0]) + "."
		if m := ch.problems[0].message; m != "" {
			part += "\n\n" + m
		}
		parts = append(parts, part)
	default:
		parts = append(parts, "Monitoring reports more problems:"+bulletList(ch.problems))
	}
	if len(ch.cleared) > 0 {
		still := "1 alert is still firing"
		if firing != 1 {
			still = strconv.Itoa(firing) + " alerts are still firing"
		}
		if len(ch.cleared) == 1 {
			parts = append(parts, "Monitoring reports that "+alertTitle(ch.cleared[0])+" is resolved; "+still+".")
		} else {
			parts = append(parts, "Monitoring reports that these are resolved:"+bulletList(ch.cleared)+"\n"+strings.ToUpper(still[:1])+still[1:]+".")
		}
	}
	parts = append(parts, impactLines...)
	return strings.Join(parts, "\n\n")
}

// resolveMessage is the update that resolves an incident.
func (ch *incidentChange) resolveMessage() string {
	switch len(ch.cleared) {
	case 0:
		return "Resolved: monitoring reports no more problems."
	case 1:
		return "Resolved: monitoring reports that " + alertTitle(ch.cleared[0]) + " is resolved."
	}
	return "Resolved: monitoring reports that these are resolved:" + bulletList(ch.cleared)
}

func bulletList(lines []alertLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("\n- ")
		b.WriteString(alertTitle(l))
	}
	return b.String()
}
