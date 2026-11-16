package monitoring

// What the requests of particular monitoring tools do, through the
// handler: Grafana's alerts for rules it cannot evaluate, Azure's activity
// log alerts, PRTG's states and acknowledged alarms, Datadog's tests and
// renotifications, and the starts that Azure's Service Health, Uptime Kuma
// (1.23 and 2) and the generic format report.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// integrationCreated sets when the integration was created: incidents do
// not start before it.
func (e *hookEnv) integrationCreated(in *store.Integration, at time.Time) {
	e.t.Helper()
	if _, err := e.store.DB().ExecContext(ctx, `UPDATE integrations SET created_at = ? WHERE id = ?`, at.Unix(), in.ID); err != nil {
		e.t.Fatal(err)
	}
}

// editorResolves resolves an incident as an editor does.
func (e *hookEnv) editorResolves(id int64) {
	e.t.Helper()
	if err := e.store.AddIncidentUpdate(ctx, id, &store.IncidentUpdate{Status: store.IncidentResolved, Message: "Fixed."}, nil); err != nil {
		e.t.Fatal(err)
	}
}

// theOpenIncident returns the one open incident.
func (e *hookEnv) theOpenIncident(what string) *store.Incident {
	e.t.Helper()
	open := e.openIncidents()
	if len(open) != 1 {
		e.t.Fatalf("%s: %d open incidents, want 1", what, len(open))
	}
	return &open[0]
}

func TestToolsGrafanaNotEvaluated(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN")
		in, token := e.integration("grafana", nil)
		// The data source of a rule labelled for VPN fails: Grafana sends
		// DatasourceError with the rule's labels and annotations.
		body := `{"receiver": "status-page", "status": "firing", "orgId": 1, "alerts": [{"status": "firing",
			"labels": {"alertname": "DatasourceError", "rulename": "VPN gateway", "datasource_uid": "P1809F7CD0C75ACF3",
			"grafana_folder": "Network", "statuspage_service": "VPN", "severity": "critical"},
			"annotations": {"summary": "VPN gateway unreachable", "Error": "failed to execute query A: connection refused"},
			"startsAt": "2026-11-16T10:00:00Z", "endsAt": "0001-01-01T00:00:00Z", "fingerprint": "8a3d5c1f2b9e7d04"}]}`
		r := e.post(Path+"/"+token, "application/json", body)
		equal(t, "status", r.body.Status, "ok")
		equal(t, "summary", r.body.Summary, []string{"Alert 1: Grafana could not evaluate the rule “VPN gateway” (DatasourceError): ignored."})
		equal(t, "nothing opened", len(e.openIncidents()), 0)
		equal(t, "no e-mail", len(e.notifier.calls), 0)
		if outcome := e.events(in)[0].Outcome; outcome == store.EventChanged {
			t.Fatalf("outcome %q", outcome)
		}
		r = e.post(Path+"/"+token, "application/json", strings.ReplaceAll(body, "DatasourceError", "DatasourceNoData"))
		equal(t, "no data", r.body.Summary, []string{"Alert 1: Grafana received no data for the rule “VPN gateway” (DatasourceNoData): ignored."})
		equal(t, "still nothing opened", len(e.openIncidents()), 0)
	})
}

// newAlertID replaces the alert id of an Azure alert: each event of an
// activity log alert is an alert of its own.
func newAlertID(body string, n int) string {
	i := strings.Index(body, `"alertId": "`) + len(`"alertId": "`)
	end := i + strings.Index(body[i:], `"`)
	return body[:i] + fmt.Sprintf("%s-%d", body[i:end], n) + body[end:]
}

func TestToolsAzureServiceHealth(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("Azure SQL", "test-ServiceHealthAlertRule")
		in, token := e.integration("azuremonitor", nil)
		e.integrationCreated(in, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		url := Path + "/" + token

		// The test action of the action group: an incident in the stage
		// Resolved (sent as Fired) – nothing was firing.
		e.post(url, "application/json", azureServiceHealthTest)
		equal(t, "the test action opens nothing", len(e.openIncidents()), 0)

		// The incident is active: degraded performance (Sev4) since its
		// impact started.
		active := strings.Replace(strings.Replace(azureServiceHealthTest, `"stage": "Resolved"`, `"stage": "Active"`, 1), `"status": "Resolved"`, `"status": "Active"`, 1)
		e.post(url, "application/json", newAlertID(active, 1))
		inc := e.theOpenIncident("active")
		equal(t, "impact", inc.Impact(), store.StatusDegraded)
		equal(t, "start", inc.StartedAt.Unix(), time.Date(2025, 4, 15, 18, 20, 36, 0, time.UTC).Unix())
		equal(t, "e-mail", e.notifier.calls, []string{fmt.Sprintf("created %d", inc.ID)})

		// More notices of the incident (new alert ids, the same tracking
		// id) are repeats.
		e.post(url, "application/json", newAlertID(active, 2))
		equal(t, "a repeat", e.theOpenIncident("repeat").ID, inc.ID)
		equal(t, "repeat: firing alerts", len(e.firingAlerts(in)), 1)

		// Resolved: the incident is resolved; the root cause analysis later
		// changes nothing.
		e.post(url, "application/json", newAlertID(azureServiceHealthTest, 3))
		equal(t, "resolved", len(e.openIncidents()), 0)
		equal(t, "resolved: e-mails", e.notifier.calls, []string{fmt.Sprintf("created %d", inc.ID), fmt.Sprintf("updated %d", inc.ID)})
		e.post(url, "application/json", newAlertID(strings.Replace(azureServiceHealthTest, `"stage": "Resolved"`, `"stage": "RCA"`, 1), 4))
		equal(t, "after the RCA", len(e.openIncidents()), 0)

		// A planned maintenance is no incident.
		r := e.post(url, "application/json", azureActivity("ServiceHealth", "m1", azureServiceHealthContext))
		equal(t, "maintenance", r.body.Summary, []string{"“VPN health”: a Service Health notice of the type “Maintenance”, not an incident: ignored."})
		equal(t, "maintenance opens nothing", len(e.openIncidents()), 0)
	})
}

// Each notice of a Service Health incident is an alert of its own, fired
// when Azure sends it; its start is when the impact began, the same in
// every notice. So a later notice is a repeat: once an editor resolved the
// incident, it opens none.
func TestToolsAzureServiceHealthUpdates(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("Azure SQL", "test-ServiceHealthAlertRule")
		in, token := e.integration("azuremonitor", nil)
		e.integrationCreated(in, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		url := Path + "/" + token

		// The impact began at 18:00; Azure fired the first notice at 18:20.
		active := strings.Replace(strings.Replace(azureServiceHealthTest, `"stage": "Resolved"`, `"stage": "Active"`, 1), `"status": "Resolved"`, `"status": "Active"`, 1)
		active = strings.Replace(active, `"impactStartTime": "2025-04-15T18:20:36.256Z"`, `"impactStartTime": "2025-04-15T18:00:00Z"`, 1)
		e.post(url, "application/json", newAlertID(active, 1))
		inc := e.theOpenIncident("active")
		equal(t, "start", inc.StartedAt.Unix(), time.Date(2025, 4, 15, 18, 0, 0, 0, time.UTC).Unix())
		e.editorResolves(inc.ID)

		// An update of the incident, fired 40 minutes later.
		update := strings.Replace(active, `"firedDateTime": "2025-04-15T18:20:36.256Z"`, `"firedDateTime": "2025-04-15T19:00:36.256Z"`, 1)
		e.post(url, "application/json", newAlertID(update, 2))
		equal(t, "the editor's resolution stays", len(e.openIncidents()), 0)
		equal(t, "e-mails", e.notifier.calls, []string{fmt.Sprintf("created %d", inc.ID)})
		equal(t, "still firing", len(e.firingAlerts(in)), 1)
	})
}

func TestToolsAzureResourceHealth(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN", "test-VM")
		in, token := e.integration("azuremonitor", nil)
		e.integrationCreated(in, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
		url := Path + "/" + token

		// The test action: test-VM became Unavailable – a major outage since
		// the event.
		e.post(url, "application/json", azureResourceHealthTest)
		inc := e.theOpenIncident("unavailable")
		equal(t, "impact", inc.Impact(), store.StatusMajorOutage)
		equal(t, "start", inc.StartedAt.Unix(), time.Date(2025, 4, 15, 18, 19, 45, 0, time.UTC).Unix())

		// Degraded: the same incident, degraded performance.
		degraded := strings.Replace(strings.Replace(azureResourceHealthTest, `"currentHealthStatus": "Unavailable"`, `"currentHealthStatus": "Degraded"`, 1),
			`"previousHealthStatus": "Available"`, `"previousHealthStatus": "Unavailable"`, 1)
		e.post(url, "application/json", newAlertID(degraded, 1))
		inc2 := e.theOpenIncident("degraded")
		equal(t, "the same incident", inc2.ID, inc.ID)
		equal(t, "degraded impact", inc2.Impact(), store.StatusDegraded)

		// Unknown says nothing about the resource.
		r := e.post(url, "application/json", newAlertID(strings.Replace(degraded, `"currentHealthStatus": "Degraded"`, `"currentHealthStatus": "Unknown"`, 1), 2))
		equal(t, "unknown", r.body.Summary, []string{"“test-ResourceHealthAlertRule”: the health of the resource is “Unknown”: ignored."})
		equal(t, "still degraded", e.theOpenIncident("unknown").Impact(), store.StatusDegraded)

		// Available again: resolved.
		available := strings.Replace(strings.Replace(azureResourceHealthTest, `"currentHealthStatus": "Unavailable"`, `"currentHealthStatus": "Available"`, 1),
			`"previousHealthStatus": "Available"`, `"previousHealthStatus": "Degraded"`, 1)
		e.post(url, "application/json", newAlertID(available, 3))
		equal(t, "resolved", len(e.openIncidents()), 0)
		equal(t, "no firing alert", len(e.firingAlerts(in)), 0)
	})
}

func TestToolsAzureOtherActivityLog(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN", "test-activityLogAlertRule", "test-VM")
		in, token := e.integration("azuremonitor", nil)
		r := e.post(Path+"/"+token, "application/json", azureActivityLogTest)
		equal(t, "summary", r.body.Summary, []string{"“test-activityLogAlertRule” is an activity log alert (Activity Log - Administrative), which never resolves: ignored (of the activity log alerts, only Service Health and Resource Health alerts are read)."})
		equal(t, "nothing opened", len(e.openIncidents()), 0)
		equal(t, "no firing alert", len(e.firingAlerts(in)), 0)
	})
}

func TestToolsPRTG(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN", "vpn-gw01")
		in, token := e.integration("prtg", nil)
		url, form := Path+"/"+token, "application/x-www-form-urlencoded"

		// The test of the notification template sends the placeholders.
		r := e.post(url, form, "key=%sensorid&status=%laststatus&service=%device&service=%group&title=%device: %name&message=%message")
		equal(t, "test", r.body.Status, "test")
		equal(t, "test outcome", e.events(in)[0].Outcome, store.EventTest)

		// Down opens a major outage; the trigger's "no longer Down" with the
		// status Warning resolves it (Warning sends nothing more).
		e.post(url, form, prtgBody("Down"))
		equal(t, "down", e.theOpenIncident("down").Impact(), store.StatusMajorOutage)
		e.post(url, form, prtgBody("Warning"))
		equal(t, "no longer down", len(e.openIncidents()), 0)

		// In a failover cluster: Down (Partial) is a partial outage, Down a
		// major one; Up (as %status before 20.1.57) resolves it.
		e.post(url, form, prtgBody("Down (Partial)"))
		inc := e.theOpenIncident("partial")
		equal(t, "partial", inc.Impact(), store.StatusPartialOutage)
		e.post(url, form, prtgBody("Down ended (now: Down)"))
		equal(t, "down after partial", e.theOpenIncident("down").Impact(), store.StatusMajorOutage)
		e.post(url, form, prtgBody("Down ended (now: Up)"))
		equal(t, "up", len(e.openIncidents()), 0)

		// Paused or Unknown end it too.
		e.post(url, form, prtgBody("Down"))
		e.post(url, form, prtgBody("Paused (paused by user)"))
		equal(t, "paused", len(e.openIncidents()), 0)
	})
}

// PRTG: an acknowledged alarm (Down (Acknowledged)) stays an outage. The
// Down trigger sends nothing when it comes back up; the second trigger, for
// Up, does. Its notifications for sensors that come up from another status
// change nothing.
func TestToolsPRTGAcknowledged(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN", "vpn-gw01")
		in, token := e.integration("prtg", nil)
		url, form := Path+"/"+token, "application/x-www-form-urlencoded"

		e.post(url, form, prtgBody("Down"))
		inc := e.theOpenIncident("down")
		// The alarm is acknowledged: "no longer Down" with the status Down
		// (Acknowledged).
		e.post(url, form, prtgBody("Down (Acknowledged)"))
		equal(t, "acknowledged: still an outage", e.theOpenIncident("acknowledged").Impact(), store.StatusMajorOutage)
		// The sensor is up again: the Up trigger.
		e.post(url, form, prtgBody("Up"))
		equal(t, "resolved", len(e.openIncidents()), 0)
		equal(t, "e-mails", e.notifier.calls, []string{fmt.Sprintf("created %d", inc.ID), fmt.Sprintf("updated %d", inc.ID)})

		// The Up trigger for a sensor that comes up from Warning.
		e.post(url, form, prtgBody("Up"))
		equal(t, "up from warning", e.events(in)[0].Outcome, store.EventUnchanged)
		equal(t, "nothing opened", len(e.openIncidents()), 0)
	})
}

func TestToolsDatadog(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN")
		in, token := e.integration("datadog", nil)
		send := func(vars map[string]string) hookResponse {
			t.Helper()
			return e.post(Path+"/"+token, "application/json", datadogBody(vars))
		}

		// Test Notifications: a test, nothing opened, no e-mail.
		r := send(map[string]string{"$ALERT_TRANSITION": "Triggered", "$ALERT_TYPE": "error", "$ALERT_TITLE": "[TEST] [Triggered on {host:vpn-gw01}] Host is Down"})
		equal(t, "test", r.body.Status, "test")
		equal(t, "test outcome", e.events(in)[0].Outcome, store.EventTest)
		equal(t, "nothing opened", len(e.openIncidents()), 0)
		equal(t, "no e-mail", len(e.notifier.calls), 0)

		// Warn: degraded performance; its renotification stays degraded.
		send(map[string]string{"$ALERT_TRANSITION": "Warn", "$ALERT_TYPE": "warning"})
		inc := e.theOpenIncident("warn")
		equal(t, "warn", inc.Impact(), store.StatusDegraded)
		send(map[string]string{"$ALERT_TRANSITION": "Renotify", "$ALERT_TYPE": "warning"})
		equal(t, "renotify of a warning", e.theOpenIncident("renotify").Impact(), store.StatusDegraded)

		// A renotification of no data changes nothing.
		r = send(map[string]string{"$ALERT_TRANSITION": "Renotify", "$ALERT_TYPE": "info"})
		equal(t, "renotify of no data", r.body.Summary, []string{"Status “Renotify” of the alert type “info”, neither error nor warning (e.g. No Data): ignored."})
		equal(t, "still degraded", e.theOpenIncident("no data").Impact(), store.StatusDegraded)

		// Triggered: a major outage; recovered: resolved.
		send(map[string]string{"$ALERT_TRANSITION": "Triggered", "$ALERT_TYPE": "error"})
		equal(t, "triggered", e.theOpenIncident("triggered").Impact(), store.StatusMajorOutage)
		send(map[string]string{"$ALERT_TRANSITION": "Recovered", "$ALERT_TYPE": "success"})
		equal(t, "recovered", len(e.openIncidents()), 0)
	})
}

// kumaDownAt is a down notification of Uptime Kuma 2 at the time at;
// important is false for a resend.
func kumaDownAt(at time.Time, important bool) string {
	body := strings.Replace(kumaPayloadJSON(0, "timeout"), `"time": "2026-11-16 10:02:00.123"`, `"time": "`+at.UTC().Format("2006-01-02 15:04:05.000")+`"`, 1)
	if !important {
		body = strings.Replace(body, `"important": true`, `"important": false`, 1)
	}
	return body
}

// Uptime Kuma: the important down heartbeat starts the problem; its
// resends are repeats, and a later outage is a new problem even if the
// status page did not hear that the monitor came up in between.
func TestToolsUptimeKumaOccurrences(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN")
		in, token := e.integration("uptimekuma", nil)
		e.integrationCreated(in, time.Now().Add(-24*time.Hour))
		url := Path + "/" + token

		first := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
		e.post(url, "application/json", kumaDownAt(first, true))
		inc := e.theOpenIncident("down")
		equal(t, "it started when the monitor went down", inc.StartedAt.Unix(), first.Unix())

		// An editor resolves the incident while the monitor is still down;
		// the resends of the monitor open nothing.
		e.editorResolves(inc.ID)
		e.post(url, "application/json", kumaDownAt(first.Add(10*time.Minute), false))
		e.post(url, "application/json", kumaDownAt(first.Add(20*time.Minute), false))
		equal(t, "resends open nothing", len(e.openIncidents()), 0)

		// The monitor came up, but the status page missed it; an hour later
		// it goes down again: a new problem. (Uptime Kuma sends the same when
		// one of its maintenances ends with the monitor still down:
		// MAINTENANCE → DOWN is important.)
		second := first.Add(time.Hour)
		e.post(url, "application/json", kumaDownAt(second, true))
		inc2 := e.theOpenIncident("down again")
		equal(t, "a new incident", inc2.ID != inc.ID, true)
		equal(t, "its start", inc2.StartedAt.Unix(), second.Unix())
	})
}

// Uptime Kuma 1.23 counts a monitor's retries from 0 again when it restarts
// (or the monitor is saved or resumed): a monitor with retries that is
// still down is reported down again, as important. That is no new problem:
// an incident an editor resolved stays resolved. A monitor without retries
// is reported down only when it goes down, so there a later outage is a
// new one.
func TestToolsUptimeKuma123(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN")
		in, token := e.integration("uptimekuma", nil)
		e.integrationCreated(in, time.Now().Add(-24*time.Hour))
		url := Path + "/" + token
		down := func(at time.Time, important bool, maxretries int) {
			t.Helper()
			e.post(url, "application/json", kuma123(kumaDownAt(at, important), maxretries))
		}

		first := time.Now().Add(-5 * time.Hour).Truncate(time.Second)
		down(first, true, 2)
		inc := e.theOpenIncident("down")
		e.editorResolves(inc.ID)
		down(first.Add(10*time.Minute), false, 2) // a resend
		// Uptime Kuma restarts an hour later; after the monitor's retries it
		// is reported down again.
		down(first.Add(time.Hour), true, 2)
		equal(t, "the editor's resolution stays", len(e.openIncidents()), 0)
		equal(t, "e-mails", e.notifier.calls, []string{fmt.Sprintf("created %d", inc.ID)})

		// The monitor comes up. Without retries it goes down again later, and
		// once more after its up notification got lost: two new problems.
		e.post(url, "application/json", kuma123(kumaPayloadJSON(kumaUp, "200 - OK"), 0))
		equal(t, "up", len(e.firingAlerts(in)), 0)
		second := first.Add(2 * time.Hour)
		down(second, true, 0)
		inc2 := e.theOpenIncident("down without retries")
		equal(t, "its start", inc2.StartedAt.Unix(), second.Unix())
		e.editorResolves(inc2.ID)
		third := second.Add(time.Hour)
		down(third, true, 0)
		inc3 := e.theOpenIncident("down again")
		equal(t, "a new incident", inc3.ID != inc2.ID, true)
		equal(t, "its start", inc3.StartedAt.Unix(), third.Unix())
	})
}

// The generic format's started: a repeat with the same start, or without
// one, stays dismissed when an editor resolved its incident; a later start
// is a new problem.
func TestToolsGenericStarted(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN")
		in, token := e.integration(Generic, nil)
		e.integrationCreated(in, time.Now().Add(-24*time.Hour))
		url := Path + "/" + token
		down := func(started time.Time) string {
			return fmt.Sprintf(`{"service": "VPN", "status": "down", "title": "VPN gateway unreachable", "started": %d}`, started.Unix())
		}

		first := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
		e.post(url, "application/json", down(first))
		inc := e.theOpenIncident("down")
		equal(t, "start", inc.StartedAt.Unix(), first.Unix())
		e.editorResolves(inc.ID)

		e.post(url, "application/json", down(first))
		e.post(url, "application/json", `{"service": "VPN", "status": "down", "title": "VPN gateway unreachable"}`)
		equal(t, "repeats stay dismissed", len(e.openIncidents()), 0)

		later := first.Add(time.Hour)
		e.post(url, "application/json", down(later))
		inc2 := e.theOpenIncident("started again")
		equal(t, "a new incident", inc2.ID != inc.ID, true)
		equal(t, "its start", inc2.StartedAt.Unix(), later.Unix())

		// A start that is no time is noted and ignored.
		r := e.post(url, "application/json", `{"service": "VPN", "status": "down", "started": "yesterday"}`)
		if !strings.Contains(strings.Join(r.body.Summary, "\n"), "Start “yesterday” is not a time") {
			t.Fatalf("summary: %q", r.body.Summary)
		}
	})
}

// Values of a request that the summary quotes are one line and short.
func TestToolsEchoOneLine(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN")
		in, token := e.integration(Generic, nil)
		r := e.post(Path+"/"+token, "application/json", `{"service": "VPN", "status": "odd\nAlert 2: faked line `+strings.Repeat("x", 500)+`"}`)
		equal(t, "one line", len(r.body.Summary), 1)
		if l := r.body.Summary[0]; strings.Contains(l, "\n") || utf8.RuneCountInString(l) > 150 {
			t.Fatalf("summary line %q", l)
		}
		equal(t, "recorded as one line", strings.Count(e.events(in)[0].Summary, "\n"), 0)

		// An error that starts with a quote keeps it.
		r = e.post(Path+"/"+token, "application/json", `{"alerts": {"service": "VPN"}}`)
		equal(t, "400", r.status, http.StatusBadRequest)
		if !strings.HasPrefix(r.body.Error, "“alerts” must be a list of objects") {
			t.Fatalf("error %q", r.body.Error)
		}
	})
}

// firingAlerts returns the firing alerts of an integration.
func (e *hookEnv) firingAlerts(in *store.Integration) []store.IntegrationAlert {
	e.t.Helper()
	alerts, err := e.store.ListIntegrationAlerts(ctx, in.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return alerts
}
