package store

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMatchServices(t *testing.T) {
	services := []Service{
		{ID: 1, Name: "VPN", MonitorNames: []string{"vpn01:9100", "Data  Centre 1"}},
		{ID: 2, Name: "E-mail", MonitorNames: []string{"smtp", "data centre 1"}},
		{ID: 3, Name: "ERP"},
	}
	for _, tc := range []struct {
		targets []string
		ids     []int64
		matched string
	}{
		{[]string{"VPN"}, []int64{1}, "VPN"},
		{[]string{"  vpn "}, []int64{1}, "  vpn "},
		{[]string{"InstanceDown", "node", "VPN01:9100"}, []int64{1}, "VPN01:9100"}, // first target that names a service
		{[]string{"erp", "vpn"}, []int64{3}, "erp"},
		{[]string{"data centre   1"}, []int64{1, 2}, "data centre   1"}, // shared name, white space collapsed
		{[]string{"E-Mail"}, []int64{2}, "E-Mail"},
		{[]string{"", "  ", "unknown"}, nil, ""},
		{nil, nil, ""},
	} {
		ids, matched := MatchServices(services, tc.targets)
		equal(t, strings.Join(tc.targets, "|")+" ids", ids, tc.ids)
		equal(t, strings.Join(tc.targets, "|")+" matched", matched, tc.matched)
	}
}

func TestNormalizeMonitorNames(t *testing.T) {
	equal(t, "normalised", NormalizeMonitorNames([]string{" vpn01 ", "", "VPN01", "Data\tCentre\n 1", "data centre 1", "x"}),
		[]string{"vpn01", "Data Centre 1", "x"})
	equal(t, "nothing", NormalizeMonitorNames([]string{" ", ""}), []string(nil))
}

func TestAutomaticIncidentTitle(t *testing.T) {
	equal(t, "prefixed", AutomaticIncidentTitle("E-mail", "SMTP down"), "E-mail: SMTP down")
	equal(t, "names the service", AutomaticIncidentTitle("VPN", "vpn gateway  unreachable"), "vpn gateway unreachable")
	equal(t, "no title", AutomaticIncidentTitle("VPN", " "), "VPN: monitoring reports a problem")
	long := AutomaticIncidentTitle("VPN", strings.Repeat("x", 300))
	if n := len([]rune(long)); n != 200 || !strings.HasSuffix(long, "…") {
		t.Fatalf("long title: %d characters: %q", n, long)
	}
}

func TestServiceMonitorNames(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		svc := &Service{Name: "VPN", MonitorNames: []string{"vpn01", " VPN01 ", "", "gw  1"}}
		check(t, s.CreateService(ctx, svc))
		equal(t, "normalised on create", svc.MonitorNames, []string{"vpn01", "gw 1"})
		got, err := s.GetService(ctx, svc.ID)
		check(t, err)
		equal(t, "stored", got.MonitorNames, []string{"vpn01", "gw 1"})

		got.MonitorNames = []string{"vpn02"}
		check(t, s.UpdateService(ctx, got))
		all, err := s.ListServices(ctx)
		check(t, err)
		equal(t, "updated", all[0].MonitorNames, []string{"vpn02"})

		got.MonitorNames = nil
		check(t, s.UpdateService(ctx, got))
		got, err = s.GetService(ctx, svc.ID)
		check(t, err)
		equal(t, "cleared", got.MonitorNames, []string(nil))
	})
}

// ------------------------------------------------------------------ fixtures

func mustIntegration(t *testing.T, s *Store, name string, notify bool) *Integration {
	t.Helper()
	in := &Integration{Name: name, Provider: "alertmanager", Notify: notify}
	check(t, s.CreateIntegration(ctx, in, "hash-"+name))
	return in
}

func firing(key, target, title string, impact Status) AlertInput {
	return AlertInput{Key: key, Firing: true, Targets: []string{target}, Title: title, Impact: impact}
}

func resolved(key string) AlertInput { return AlertInput{Key: key} }

func apply(t *testing.T, s *Store, in *Integration, alerts ...AlertInput) *ApplyResult {
	t.Helper()
	res, err := s.ApplyAlerts(ctx, in, alerts)
	check(t, err)
	return res
}

// actions returns "<service>:<action>:<incident>" for every service of every alert.
func actions(res *ApplyResult) []string {
	var out []string
	for _, a := range res.Alerts {
		for _, sr := range a.Services {
			out = append(out, sr.ServiceName+":"+string(sr.Action)+":"+strconv.FormatInt(sr.IncidentID, 10))
		}
	}
	return out
}

func mustIncident(t *testing.T, s *Store, id int64) *Incident {
	t.Helper()
	inc, err := s.GetIncident(ctx, id)
	check(t, err)
	return inc
}

func currentStatus(t *testing.T, s *Store, serviceID int64) Status {
	t.Helper()
	services, err := s.ListServices(ctx)
	check(t, err)
	open, err := s.ListOpenIncidents(ctx)
	check(t, err)
	now := time.Now()
	maint, err := s.ListCurrentMaintenances(ctx, now)
	check(t, err)
	return ServiceStatuses(services, open, maint, now)[serviceID]
}

// ------------------------------------------------------------------ lifecycle

func TestApplyAlertsLifecycle(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		vpn := &Service{Name: "VPN", MonitorNames: []string{"vpn01"}}
		check(t, s.CreateService(ctx, vpn))
		mail := mustService(t, s, "E-mail", "", 0)
		in := mustIntegration(t, s, "Prometheus", true)

		// A firing alert opens an automatic incident.
		a := firing("a", "vpn01", "VPN gateway unreachable", StatusPartialOutage)
		a.Message = "No answer since 10:02."
		res := apply(t, s, in, a)
		equal(t, "opened", actions(res), []string{"VPN:opened:1"})
		equal(t, "matched", res.Alerts[0].Matched, "vpn01")
		equal(t, "notices", res.Notices, []IncidentNotice{{IncidentID: 1, Created: true}})
		inc := mustIncident(t, s, 1)
		equal(t, "automatic", inc.Automatic, true)
		equal(t, "title (names the service)", inc.Title, "VPN gateway unreachable")
		equal(t, "status", inc.Status, IncidentInvestigating)
		equal(t, "services", inc.Services, []ServiceImpact{{ServiceID: vpn.ID, ServiceName: "VPN", Impact: StatusPartialOutage}})
		equal(t, "one update", len(inc.Updates), 1)
		equal(t, "first update", inc.Updates[0].Message, "Monitoring reports a problem: “VPN gateway unreachable”.\n\nNo answer since 10:02.")
		equal(t, "source", inc.Updates[0].Source, "Prometheus")
		equal(t, "author", inc.Updates[0].AuthorName, "Prometheus (monitoring)")
		equal(t, "status of VPN", currentStatus(t, s, vpn.ID), StatusPartialOutage)
		equal(t, "status of E-mail", currentStatus(t, s, mail.ID), StatusOperational)

		// The same alert again (e.g. repeat_interval): nothing new.
		res = apply(t, s, in, a)
		equal(t, "repeated", actions(res), []string{"VPN:repeated:1"})
		equal(t, "no notices", len(res.Notices), 0)
		equal(t, "still one update", len(mustIncident(t, s, 1).Updates), 1)

		// A repeat without a title keeps the title.
		untitled := a
		untitled.Title = ""
		apply(t, s, in, untitled)
		alerts0, err := s.ListIntegrationAlerts(ctx, in.ID)
		check(t, err)
		equal(t, "title kept", alerts0[0].Title, "VPN gateway unreachable")

		// The same alert with a new severity: the impact follows.
		a.Impact = StatusMajorOutage
		apply(t, s, in, a)
		inc = mustIncident(t, s, 1)
		equal(t, "impact raised", inc.Impact(), StatusMajorOutage)
		equal(t, "impact update", inc.Updates[0].Message, "Impact on VPN: Major outage (was: Partial outage).")

		// Another alert of the service joins the incident.
		b := firing("b", "VPN", "VPN portal slow", StatusDegraded)
		res = apply(t, s, in, b)
		equal(t, "joined", actions(res), []string{"VPN:joined:1"})
		inc = mustIncident(t, s, 1)
		equal(t, "impact: worst alert", inc.Impact(), StatusMajorOutage)
		equal(t, "join update", inc.Updates[0].Message, "Monitoring reports another problem: “VPN portal slow”.")
		alerts, err := s.ListIncidentAlerts(ctx, 1)
		check(t, err)
		equal(t, "firing alerts of the incident", len(alerts), 2)
		equal(t, "alert fields", alerts[0].IntegrationName+"|"+alerts[0].ServiceName+"|"+string(alerts[0].Impact), "Prometheus|VPN|major_outage")

		// The worse alert clears: the impact goes down to the other one.
		res = apply(t, s, in, resolved("a"))
		equal(t, "cleared", actions(res), []string{"VPN:cleared:1"})
		inc = mustIncident(t, s, 1)
		equal(t, "still open", inc.Status, IncidentInvestigating)
		equal(t, "impact lowered", inc.Impact(), StatusDegraded)
		equal(t, "clear update", inc.Updates[0].Message,
			"Monitoring reports that “VPN gateway unreachable” is resolved; 1 alert is still firing.\n\nImpact on VPN: Degraded performance (was: Major outage).")

		// The last alert clears: the incident is resolved.
		res = apply(t, s, in, resolved("b"))
		equal(t, "resolved", actions(res), []string{"VPN:resolved:1"})
		equal(t, "resolve notice", res.Notices, []IncidentNotice{{IncidentID: 1}})
		inc = mustIncident(t, s, 1)
		equal(t, "resolved status", inc.Status, IncidentResolved)
		if inc.ResolvedAt == nil || !recent(*inc.ResolvedAt) {
			t.Fatalf("resolved_at: %v", inc.ResolvedAt)
		}
		equal(t, "resolve update", inc.Updates[0].Message, "Resolved: monitoring reports that “VPN portal slow” is resolved.")
		equal(t, "resolve update status", inc.Updates[0].Status, IncidentResolved)
		equal(t, "operational", currentStatus(t, s, vpn.ID), StatusOperational)

		// A resolved alert that is not firing changes nothing.
		res = apply(t, s, in, resolved("b"), resolved("unknown"))
		equal(t, "nothing to clear", actions(res), []string(nil))

		// The next occurrence opens a new incident.
		res = apply(t, s, in, a)
		equal(t, "new occurrence", actions(res), []string{"VPN:opened:2"})
		firingAlerts, err := s.ListIntegrationAlerts(ctx, in.ID)
		check(t, err)
		equal(t, "one firing alert", len(firingAlerts), 1)
		equal(t, "attached", firingAlerts[0].IncidentID, int64(2))
		equal(t, "open", firingAlerts[0].IncidentOpen, true)
		got, err := s.GetIntegration(ctx, in.ID)
		check(t, err)
		equal(t, "firing count", got.Firing, 1)
	})
}

func TestApplyAlertsBatch(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		mustService(t, s, "VPN", "", 0)
		in := mustIntegration(t, s, "Grafana", false)

		// Two alerts of one service in one request: one incident, one update.
		res := apply(t, s, in, firing("a", "VPN", "Gateway down", StatusMajorOutage), firing("b", "vpn", "Portal down", StatusDegraded),
			firing("c", "nothing", "Unrelated", StatusDegraded), AlertInput{Firing: true, Targets: []string{"VPN"}})
		equal(t, "actions", actions(res), []string{"VPN:opened:1", "VPN:joined:1"})
		equal(t, "unmatched", res.Alerts[2].Matched, "")
		equal(t, "no key: ignored", len(res.Alerts[3].Services), 0)
		equal(t, "notify off", len(res.Notices), 0)
		inc := mustIncident(t, s, 1)
		equal(t, "title", inc.Title, "VPN: Gateway down")
		equal(t, "updates", len(inc.Updates), 1)
		equal(t, "message", inc.Updates[0].Message, "Monitoring reports these problems:\n- “Gateway down”\n- “Portal down”")
		equal(t, "impact", inc.Impact(), StatusMajorOutage)

		// Both clear in one request: resolved with one update.
		res = apply(t, s, in, resolved("a"), resolved("b"))
		equal(t, "actions", actions(res), []string{"VPN:resolved:1", "VPN:resolved:1"})
		inc = mustIncident(t, s, 1)
		equal(t, "updates", len(inc.Updates), 2)
		equal(t, "message", inc.Updates[0].Message, "Resolved: monitoring reports that these are resolved:\n- “Gateway down”\n- “Portal down”")
	})
}

func TestApplyAlertsSharedName(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		vpn := &Service{Name: "VPN", MonitorNames: []string{"dc1"}}
		mail := &Service{Name: "E-mail", MonitorNames: []string{"DC1"}}
		check(t, s.CreateService(ctx, vpn))
		check(t, s.CreateService(ctx, mail))
		in := mustIntegration(t, s, "Zabbix", true)

		// One alert, two services: an incident for each.
		res := apply(t, s, in, firing("power", "dc1", "Power failure", StatusMajorOutage))
		equal(t, "actions", actions(res), []string{"E-mail:opened:1", "VPN:opened:2"})
		equal(t, "notices", len(res.Notices), 2)
		equal(t, "VPN", currentStatus(t, s, vpn.ID), StatusMajorOutage)
		equal(t, "E-mail", currentStatus(t, s, mail.ID), StatusMajorOutage)

		// The services are found again by the alert, even when the names
		// changed meanwhile.
		mail.MonitorNames = nil
		check(t, s.UpdateService(ctx, mail))
		res = apply(t, s, in, resolved("power"))
		equal(t, "both resolved", actions(res), []string{"VPN:resolved:2", "E-mail:resolved:1"})
		equal(t, "VPN", currentStatus(t, s, vpn.ID), StatusOperational)
		equal(t, "E-mail", currentStatus(t, s, mail.ID), StatusOperational)
	})
}

func TestApplyAlertsLeaveManualIncidents(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		vpn := mustService(t, s, "VPN", "", 0)
		in := mustIntegration(t, s, "Uptime Kuma", true)
		manual := &Incident{Title: "ISP problem", Status: IncidentIdentified, Services: []ServiceImpact{{ServiceID: vpn.ID, Impact: StatusDegraded}}}
		check(t, s.CreateIncident(ctx, manual, &IncidentUpdate{Message: "Our ISP has a problem."}))

		res := apply(t, s, in, firing("m1", "VPN", "VPN is down", StatusMajorOutage))
		equal(t, "own incident", actions(res), []string{"VPN:opened:2"})
		apply(t, s, in, resolved("m1"))
		got := mustIncident(t, s, manual.ID)
		equal(t, "manual incident untouched", got.Status, IncidentIdentified)
		equal(t, "manual updates", len(got.Updates), 1)
		equal(t, "manual impact", got.Impact(), StatusDegraded)
		equal(t, "VPN stays degraded", currentStatus(t, s, vpn.ID), StatusDegraded)
	})
}

func TestApplyAlertsEditorChanges(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		vpn := mustService(t, s, "VPN", "", 0)
		in := mustIntegration(t, s, "Prometheus", true)
		a := firing("a", "VPN", "VPN down", StatusMajorOutage)

		// An editor resolves the incident while the alert still fires: the
		// repeats do not reopen it.
		apply(t, s, in, a)
		check(t, s.AddIncidentUpdate(ctx, 1, &IncidentUpdate{Status: IncidentResolved, Message: "False alarm."}, nil))
		res := apply(t, s, in, a)
		equal(t, "dismissed", actions(res), []string{"VPN:dismissed:1"})
		equal(t, "operational", currentStatus(t, s, vpn.ID), StatusOperational)
		alerts, err := s.ListIntegrationAlerts(ctx, in.ID)
		check(t, err)
		equal(t, "listed as firing, incident closed", alerts[0].IncidentOpen, false)
		res = apply(t, s, in, resolved("a"))
		equal(t, "ended", actions(res), []string{"VPN:ended:1"})
		equal(t, "editor's resolution kept", len(mustIncident(t, s, 1).Updates), 2)

		// The next occurrence opens a new incident; an editor's update does
		// not stop the automatic resolution.
		res = apply(t, s, in, a)
		equal(t, "new incident", actions(res), []string{"VPN:opened:2"})
		check(t, s.AddIncidentUpdate(ctx, 2, &IncidentUpdate{Status: IncidentIdentified, Message: "A switch failed."}, nil))
		res = apply(t, s, in, resolved("a"))
		equal(t, "resolved", actions(res), []string{"VPN:resolved:2"})

		// An editor deletes the incident of a firing alert: the repeats do
		// not bring it back.
		apply(t, s, in, a)
		check(t, s.DeleteIncident(ctx, 3))
		res = apply(t, s, in, a)
		equal(t, "deleted incident", actions(res), []string{"VPN:dismissed:0"})
		open, err := s.ListOpenIncidents(ctx)
		check(t, err)
		equal(t, "no open incident", len(open), 0)
		res = apply(t, s, in, resolved("a"))
		equal(t, "ended without incident", actions(res), []string{"VPN:ended:0"})

		// An editor reopens a resolved automatic incident: alerts join it.
		check(t, s.AddIncidentUpdate(ctx, 2, &IncidentUpdate{Status: IncidentMonitoring, Message: "Watching it."}, nil))
		res = apply(t, s, in, firing("b", "VPN", "VPN slow", StatusDegraded))
		equal(t, "joined the reopened incident", actions(res), []string{"VPN:joined:2"})
		got := mustIncident(t, s, 2)
		equal(t, "status kept", got.Status, IncidentMonitoring)
		equal(t, "update with the incident's status", got.Updates[0].Status, IncidentMonitoring)
	})
}

func TestApplyAlertsMaintenance(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		vpn := mustService(t, s, "VPN", "", 0)
		mail := mustService(t, s, "E-mail", "", 0)
		in := mustIntegration(t, s, "PRTG", true)
		now := time.Now()
		m := &Maintenance{Title: "Firmware", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Services: []ServiceRef{{ID: vpn.ID}}}
		check(t, s.CreateMaintenance(ctx, m))

		// During the maintenance alerts of its services are suppressed.
		res := apply(t, s, in, firing("a", "VPN", "Gateway down", StatusMajorOutage), firing("b", "E-mail", "SMTP down", StatusPartialOutage))
		equal(t, "actions", actions(res), []string{"VPN:suppressed:0", "E-mail:opened:1"})
		equal(t, "VPN in maintenance", currentStatus(t, s, vpn.ID), StatusMaintenance)
		alerts, err := s.ListIntegrationAlerts(ctx, in.ID)
		check(t, err)
		equal(t, "suppressed listed", alerts[0].Suppressed || alerts[1].Suppressed, true)

		// Still in maintenance: nothing to release, repeats stay suppressed.
		rel, err := s.ReleaseSuppressedAlerts(ctx)
		check(t, err)
		equal(t, "nothing released", len(rel.Alerts), 0)
		res = apply(t, s, in, firing("a", "VPN", "Gateway down", StatusMajorOutage))
		equal(t, "still suppressed", actions(res), []string{"VPN:suppressed:0"})

		// The maintenance ends while the alert still fires: released.
		mustExec(t, s, `UPDATE maintenances SET ends_at = ? WHERE id = ?`, now.Add(-time.Minute).Unix(), m.ID)
		rel, err = s.ReleaseSuppressedAlerts(ctx)
		check(t, err)
		equal(t, "released", actions(rel), []string{"VPN:opened:2"})
		equal(t, "notice", rel.Notices, []IncidentNotice{{IncidentID: 2, Created: true}})
		inc := mustIncident(t, s, 2)
		if !recent(inc.StartedAt) {
			t.Fatalf("an incident opened after maintenance starts now, not %v", inc.StartedAt)
		}
		equal(t, "message", inc.Updates[0].Message,
			"Monitoring reports a problem: “Gateway down”.\n\nIt started during maintenance and has not cleared since.")
		rel, err = s.ReleaseSuppressedAlerts(ctx)
		check(t, err)
		equal(t, "released once", len(rel.Alerts), 0)

		// A suppressed alert that clears during maintenance changes nothing;
		// a repeat after the maintenance attaches a suppressed alert too.
		m2 := &Maintenance{Title: "Mail upgrade", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Services: []ServiceRef{{ID: mail.ID}}}
		check(t, s.CreateMaintenance(ctx, m2))
		res = apply(t, s, in, firing("c", "E-mail", "IMAP down", StatusDegraded), firing("d", "E-mail", "POP down", StatusDegraded))
		equal(t, "suppressed although the service has an open incident", actions(res), []string{"E-mail:suppressed:0", "E-mail:suppressed:0"})
		res = apply(t, s, in, resolved("c"))
		equal(t, "cleared while suppressed", actions(res), []string{"E-mail:ended:0"})
		mustExec(t, s, `UPDATE maintenances SET ends_at = ? WHERE id = ?`, now.Add(-time.Minute).Unix(), m2.ID)
		res = apply(t, s, in, firing("d", "E-mail", "POP down", StatusDegraded))
		equal(t, "attached by its repeat", actions(res), []string{"E-mail:joined:1"})

		// Paused integrations keep their alerts suppressed.
		in2 := mustIntegration(t, s, "Paused", false)
		m3 := &Maintenance{Title: "ERP", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Services: []ServiceRef{{ID: vpn.ID}}}
		check(t, s.CreateMaintenance(ctx, m3))
		apply(t, s, in2, firing("x", "VPN", "X", StatusDegraded))
		in2.Paused = true
		check(t, s.UpdateIntegration(ctx, in2))
		mustExec(t, s, `UPDATE maintenances SET ends_at = ? WHERE id = ?`, now.Add(-time.Minute).Unix(), m3.ID)
		rel, err = s.ReleaseSuppressedAlerts(ctx)
		check(t, err)
		equal(t, "paused: not released", len(rel.Alerts), 0)
	})
}

func TestApplyAlertsStartTime(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		mustService(t, s, "A", "", 0)
		mustService(t, s, "B", "", 0)
		mustService(t, s, "C", "", 0)
		in := mustIntegration(t, s, "Prometheus", false)
		in.CreatedAt = time.Now().Add(-2 * time.Hour).Truncate(time.Second).UTC()

		started := time.Now().Add(-30 * time.Minute).Truncate(time.Second).UTC()
		a := firing("a", "A", "A", StatusDegraded)
		a.StartedAt = started
		b := firing("b", "B", "B", StatusDegraded)
		b.StartedAt = time.Now().Add(-48 * time.Hour) // before the integration existed
		c := firing("c", "C", "C", StatusDegraded)
		c.StartedAt = time.Now().Add(time.Hour) // in the future
		apply(t, s, in, a, b, c)
		equal(t, "alert's start", mustIncident(t, s, 1).StartedAt, started)
		equal(t, "not before the integration", mustIncident(t, s, 2).StartedAt, in.CreatedAt)
		if st := mustIncident(t, s, 3).StartedAt; !recent(st) {
			t.Fatalf("a start in the future becomes now, not %v", st)
		}
	})
}
