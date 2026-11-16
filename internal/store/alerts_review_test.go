package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// A repeat keeps the reported start up to date: when an editor resolves
// the incident, a later repeat of the occurrence the incident showed stays
// dismissed (Alertmanager re-sends an alert that flapped unseen with its
// new start).
func TestApplyAlertsStartKeptUpToDate(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		mustService(t, s, "VPN", "", 0)
		mail := mustService(t, s, "Mail", "", 0)
		in := mustIntegration(t, s, "Prometheus", false)
		in.CreatedAt = time.Now().Add(-24 * time.Hour)
		t0 := time.Now().Add(-3 * time.Hour).Truncate(time.Second)

		a := firing("a", "VPN", "Gateway down", StatusMajorOutage)
		a.StartedAt = t0
		equal(t, "opened", actions(apply(t, s, in, a)), []string{"VPN:opened:1"})
		a.StartedAt = t0.Add(30 * time.Minute) // resolved and fired again, unseen
		equal(t, "repeat", actions(apply(t, s, in, a)), []string{"VPN:repeated:1"})
		check(t, s.AddIncidentUpdate(ctx, 1, &IncidentUpdate{Status: IncidentResolved, Message: "Fixed."}, nil))
		equal(t, "the occurrence the incident showed", actions(apply(t, s, in, a)), []string{"VPN:dismissed:1"})
		a.StartedAt = t0.Add(2 * time.Hour)
		equal(t, "started again", actions(apply(t, s, in, a)), []string{"VPN:opened:2"})

		// The same while the alert waits for the end of a maintenance.
		now := time.Now()
		m := &Maintenance{Title: "Upgrade", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Services: []ServiceRef{{ID: mail.ID}}}
		check(t, s.CreateMaintenance(ctx, m))
		b := firing("b", "Mail", "SMTP down", StatusMajorOutage)
		b.StartedAt = t0
		equal(t, "suppressed", actions(apply(t, s, in, b)), []string{"Mail:suppressed:0"})
		b.StartedAt = t0.Add(time.Hour)
		equal(t, "still suppressed", actions(apply(t, s, in, b)), []string{"Mail:suppressed:0"})
		mustExec(t, s, `UPDATE maintenances SET ends_at = ? WHERE id = ?`, now.Add(-time.Minute).Unix(), m.ID)
		rel, err := s.ReleaseSuppressedAlerts(ctx)
		check(t, err)
		equal(t, "released", actions(rel), []string{"Mail:opened:3"})
		check(t, s.AddIncidentUpdate(ctx, 3, &IncidentUpdate{Status: IncidentResolved, Message: "Fixed."}, nil))
		equal(t, "after the release", actions(apply(t, s, in, b)), []string{"Mail:dismissed:3"})
	})
}

// The integration that opens an incident decides about its e-mails: the
// resolution is e-mailed when the opening was, whichever integration's
// alert clears last.
func TestApplyAlertsNotifyByOpener(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		mustService(t, s, "VPN", "", 0)
		mustService(t, s, "Mail", "", 0)
		loud := mustIntegration(t, s, "Prometheus", true)
		quiet := mustIntegration(t, s, "Uptime Kuma", false)

		res := apply(t, s, loud, firing("a", "VPN", "Ping", StatusMajorOutage))
		equal(t, "opened", res.Notices, []IncidentNotice{wantNotice(t, s, 1, true)})
		res = apply(t, s, quiet, firing("b", "VPN", "HTTP", StatusMajorOutage))
		equal(t, "joined", actions(res), []string{"VPN:joined:1"})
		equal(t, "no e-mail for a join", len(res.Notices), 0)
		equal(t, "no e-mail for a clear", len(apply(t, s, loud, resolved("a")).Notices), 0)
		res = apply(t, s, quiet, resolved("b"))
		equal(t, "resolved by the quiet one", actions(res), []string{"VPN:resolved:1"})
		equal(t, "resolution e-mailed", res.Notices, []IncidentNotice{wantNotice(t, s, 1, false)})

		// Opened without e-mails: no e-mail about its resolution either.
		res = apply(t, s, quiet, firing("c", "Mail", "SMTP", StatusMajorOutage))
		equal(t, "opened quietly", len(res.Notices), 0)
		apply(t, s, loud, firing("d", "Mail", "IMAP", StatusMajorOutage))
		apply(t, s, quiet, resolved("c"))
		res = apply(t, s, loud, resolved("d"))
		equal(t, "resolved by the loud one", actions(res), []string{"Mail:resolved:2"})
		equal(t, "no resolution e-mail", len(res.Notices), 0)
	})
}

// An incident opened and resolved by one request gets one e-mail, about its
// resolution.
func TestApplyAlertsOpenedAndResolvedAtOnce(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		mustService(t, s, "VPN", "", 0)
		in := mustIntegration(t, s, "Script", true)
		res := apply(t, s, in, firing("k", "VPN", "VPN down", StatusMajorOutage), resolved("k"))
		equal(t, "actions", actions(res), []string{"VPN:opened:1", "VPN:resolved:1"})
		inc := mustIncident(t, s, 1)
		equal(t, "resolved", inc.Status, IncidentResolved)
		equal(t, "updates", len(inc.Updates), 2)
		equal(t, "one e-mail, about the resolution", res.Notices, []IncidentNotice{wantNotice(t, s, 1, false)})
	})
}

// The updates name alerts that share a title once, with their number, and
// list at most maxListedTitles lines.
func TestApplyAlertsUpdateTexts(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		mustService(t, s, "VPN", "", 0)
		in := mustIntegration(t, s, "Prometheus", false)
		var same []AlertInput
		for i := range 4 {
			same = append(same, firing(fmt.Sprintf("node%d", i), "VPN", "InstanceDown", StatusMajorOutage))
		}
		apply(t, s, in, same...)
		equal(t, "opened", mustIncident(t, s, 1).Updates[0].Message, "Monitoring reports problems: “InstanceDown” (4 alerts).")

		var many []AlertInput
		for i := range 12 {
			many = append(many, firing(fmt.Sprintf("m%d", i), "VPN", fmt.Sprintf("Check %d", i), StatusDegraded))
		}
		many = append(many, firing("m12", "VPN", "Check 0", StatusDegraded))
		apply(t, s, in, many...)
		equal(t, "listed and counted", mustIncident(t, s, 1).Updates[0].Message,
			"Monitoring reports more problems:\n- “Check 0” (2 alerts)\n- “Check 1”\n- “Check 2”\n- “Check 3”\n- “Check 4”\n"+
				"- “Check 5”\n- “Check 6”\n- “Check 7”\n- “Check 8”\n- and 3 more alerts")

		var clear []AlertInput
		for _, a := range same {
			clear = append(clear, resolved(a.Key))
		}
		apply(t, s, in, clear...)
		equal(t, "cleared", mustIncident(t, s, 1).Updates[0].Message,
			"Monitoring reports that “InstanceDown” (4 alerts) are resolved; 13 alerts are still firing.\n\n"+
				"Impact on VPN: Degraded performance (was: Major outage).")
	})
}

func TestAutomaticIncidentTitleWords(t *testing.T) {
	for _, tc := range []struct{ service, alert, want string }{
		{"AD", "Load average is too high", "AD: Load average is too high"},
		{"SAP", "Disk usage on sapdb01", "SAP: Disk usage on sapdb01"},
		{"Mail", "Mailbox quota exceeded", "Mail: Mailbox quota exceeded"},
		{"VPN", "VPN gateway unreachable", "VPN gateway unreachable"},
		{"VPN", "vpn-gw01 down", "vpn-gw01 down"},
		{"VPN", "Check of the VPN", "Check of the VPN"},
		{"Data Centre 1", "Power failure in data  centre 1", "Power failure in data centre 1"},
		{"C++ build", "C++ build failed", "C++ build failed"},
		{"Ärzteportal", "Ärzteportal langsam", "Ärzteportal langsam"},
		{"ERP", "ERP2 down", "ERP: ERP2 down"},
	} {
		equal(t, tc.service+" / "+tc.alert, AutomaticIncidentTitle(tc.service, tc.alert), tc.want)
	}
}

// An administrator clears a firing alert whose recovery the tool did not
// send: as if the tool had reported it.
func TestClearIntegrationAlert(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		mustService(t, s, "VPN", "", 0)
		mustService(t, s, "Mail", "", 0)
		in := mustIntegration(t, s, "Uptime Kuma", true)
		apply(t, s, in, firing("a", "VPN", "Gateway down", StatusMajorOutage), firing("b", "VPN", "Portal slow", StatusDegraded))

		res, err := s.ClearIntegrationAlert(ctx, in, "a")
		check(t, err)
		equal(t, "cleared", actions(res), []string{"VPN:cleared:1"})
		equal(t, "title for the log", res.Alerts[0].Alert.Title, "Gateway down")
		equal(t, "no e-mail", len(res.Notices), 0)
		inc := mustIncident(t, s, 1)
		equal(t, "update", inc.Updates[0].Message,
			"An administrator cleared “Gateway down” (the monitoring tool did not report its end); 1 alert is still firing.\n\n"+
				"Impact on VPN: Degraded performance (was: Major outage).")

		res, err = s.ClearIntegrationAlert(ctx, in, "b")
		check(t, err)
		equal(t, "resolved", actions(res), []string{"VPN:resolved:1"})
		equal(t, "resolution e-mailed", res.Notices, []IncidentNotice{wantNotice(t, s, 1, false)})
		equal(t, "resolution", mustIncident(t, s, 1).Updates[0].Message,
			"Resolved: an administrator cleared “Portal slow” (the monitoring tool did not report its end).")
		if _, err := s.ClearIntegrationAlert(ctx, in, "b"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("clearing an alert that is not firing: %v", err)
		}

		// A dismissed alert of a tool without start times: once cleared, it
		// opens an incident again when it fires.
		c := firing("c", "Mail", "SMTP down", StatusMajorOutage)
		apply(t, s, in, c)
		check(t, s.AddIncidentUpdate(ctx, 2, &IncidentUpdate{Status: IncidentResolved, Message: "Fixed."}, nil))
		equal(t, "dismissed", actions(apply(t, s, in, c)), []string{"Mail:dismissed:2"})
		res, err = s.ClearIntegrationAlert(ctx, in, "c")
		check(t, err)
		equal(t, "cleared without incident", actions(res), []string{"Mail:ended:2"})
		equal(t, "fires again", actions(apply(t, s, in, c)), []string{"Mail:opened:3"})
	})
}

// The changes of monitoring wait for their turn without a database
// connection, and give up with ErrBusy when their context ends.
func TestMonitoringTurn(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		mustService(t, s, "VPN", "", 0)
		in := mustIntegration(t, s, "Prometheus", false)
		s.monitoring <- struct{}{} // another change runs
		waiting, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := s.ApplyAlerts(waiting, in, []AlertInput{firing("a", "VPN", "Down", StatusMajorOutage)})
			done <- err
		}()
		time.Sleep(100 * time.Millisecond)
		equal(t, "connections in use while waiting", s.db.Stats().InUse, 0)
		if err := <-done; !errors.Is(err, ErrBusy) {
			t.Fatalf("got %v, want ErrBusy", err)
		}
		if err := s.DeleteIncident(waiting, 1); !errors.Is(err, ErrBusy) {
			t.Fatalf("delete: got %v, want ErrBusy", err)
		}
		<-s.monitoring
		equal(t, "after the turn", actions(apply(t, s, in, firing("a", "VPN", "Down", StatusMajorOutage))), []string{"VPN:opened:1"})
	})
}

// Deleting an incident, a service or an integration while a request of
// another instance (on PostgreSQL) changes alerts of it waits for the
// request, instead of taking its locks in the opposite order (deadlock).
func TestDeletesWaitForMonitoring(t *testing.T) {
	dsn := os.Getenv("STATUSPAGE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("STATUSPAGE_TEST_PG_DSN not set (SQLite runs one writing transaction at a time)")
	}
	for _, tc := range []struct {
		name string
		del  func(s *Store, in *Integration, serviceID int64) error
	}{
		{"incident", func(s *Store, _ *Integration, _ int64) error { return s.DeleteIncident(ctx, 1) }},
		{"service", func(s *Store, _ *Integration, sid int64) error { return s.DeleteService(ctx, sid) }},
		{"integration", func(s *Store, in *Integration, _ int64) error { return s.DeleteIntegration(ctx, in.ID) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openPostgres(t, dsn)
			vpn := mustService(t, s, "VPN", "", 0)
			in := mustIntegration(t, s, "Prometheus", false)
			apply(t, s, in, firing("a", "VPN", "Down", StatusMajorOutage))

			// A request of another instance, halfway: it holds the monitoring
			// lock and the row of the repeated alert a.
			req, err := s.db.DB.BeginTx(ctx, nil)
			check(t, err)
			defer req.Rollback()
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := req.ExecContext(ctx, s.db.Rebind(q), args...); err != nil {
					t.Fatalf("request: %v\n%s", err, q)
				}
			}
			exec(`SELECT pg_advisory_xact_lock(?)`, monitoringLockID)
			exec(`UPDATE integration_alerts SET updated_at = updated_at WHERE integration_id = ? AND alert_key = ?`, in.ID, "a")

			done := make(chan error, 1)
			go func() { done <- tc.del(s, in, vpn.ID) }()
			waitForLockWaiter(t, s)

			// The request goes on: a new alert of the service, and the
			// incident's lock in finish.
			now := time.Now().Unix()
			exec(`INSERT INTO integration_alerts (integration_id, alert_key, service_id, incident_id, suppressed, impact, title, started_at, updated_at)
			      VALUES (?, ?, ?, NULL, ?, ?, '', ?, ?)`, in.ID, "b", vpn.ID, false, string(StatusMajorOutage), now, now)
			exec(`UPDATE incidents SET updated_at = updated_at WHERE id = ?`, 1)
			check(t, req.Commit())
			select {
			case err := <-done:
				check(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("the deletion did not finish")
			}
		})
	}
}

// waitForLockWaiter waits until a session of the test database waits for a
// lock.
func waitForLockWaiter(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		check(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&n))
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("nobody waits for a lock")
}

func init() {
	// The texts above depend on it.
	if maxListedTitles != 10 || !strings.Contains(bulletList([]alertGroup{{title: "“x”", n: 2}}), "(2 alerts)") {
		panic("update the tests")
	}
}
