package monitoring

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

func equal[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got: %#v\nwant: %#v", what, got, want)
	}
}

func parse(t *testing.T, fn func(*Request) (*Batch, error), contentType, body string) *Batch {
	t.Helper()
	b, err := fn(&Request{Body: []byte(body), ContentType: contentType})
	if err != nil {
		t.Fatalf("parse %s: %v", body, err)
	}
	return b
}

func parseError(t *testing.T, fn func(*Request) (*Batch, error), body, contains string) {
	t.Helper()
	_, err := fn(&Request{Body: []byte(body), ContentType: "application/json"})
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("parse %s: want a ParseError, got %v", body, err)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("parse %s: error %q does not contain %q", body, err, contains)
	}
}

func TestParseStatus(t *testing.T) {
	for _, tc := range []struct {
		in       string
		state    state
		severity string
	}{
		{"firing", stateFiring, ""},
		{"Down", stateFiring, ""},
		{"DOWN", stateFiring, ""},
		{"Down (Partial)", stateFiring, "partial"},
		{"Down (Acknowledged)", stateFiring, ""},
		{"Down (was: Warning)", stateFiring, ""},
		{"Down ended (now: Up)", stateResolved, ""},
		{"Warning ended (now: Down)", stateFiring, ""},
		{"CRITICAL", stateFiring, "critical"},
		{"WARNING", stateFiring, "warning"},
		{"Warn", stateFiring, "warning"},
		{"Re-Warn", stateFiring, "warning"},
		{"Unusual", stateFiring, "warning"},
		{"Triggered", stateFiring, ""},
		{"Re-Triggered", stateFiring, ""},
		{"Renotify", stateFiring, ""},
		{"UNREACHABLE", stateFiring, ""},
		{"problem", stateFiring, ""},
		{"resolved", stateResolved, ""},
		{"Recovered", stateResolved, ""},
		{"OK", stateResolved, ""},
		{"UP", stateResolved, ""},
		{" up ", stateResolved, ""},
		{"No Data", stateUnknown, ""},
		{"Re-No Data", stateUnknown, ""},
		{"UNKNOWN", stateUnknown, ""},
		{"Paused", stateUnknown, ""},
		{"pending", stateUnknown, ""},
		{"1", stateUnknown, ""},
		{"", stateUnknown, ""},
		{"test", stateTest, ""},
	} {
		st, sev := parseStatus(tc.in)
		if st != tc.state || sev != tc.severity {
			t.Errorf("parseStatus(%q) = %v, %q; want %v, %q", tc.in, st, sev, tc.state, tc.severity)
		}
	}
}

func TestSeverityImpact(t *testing.T) {
	for in, want := range map[string]store.Status{
		"critical": store.StatusMajorOutage, "CRITICAL": store.StatusMajorOutage, "Disaster": store.StatusMajorOutage,
		"High": store.StatusMajorOutage, "Sev0": store.StatusMajorOutage, "P1": store.StatusMajorOutage,
		"error": store.StatusPartialOutage, "Average": store.StatusPartialOutage, "SEV-1": store.StatusPartialOutage,
		"partial": store.StatusPartialOutage, "p2": store.StatusPartialOutage,
		"warning": store.StatusDegraded, "Information": store.StatusDegraded, "Not classified": store.StatusDegraded,
		"Sev 3": store.StatusDegraded, "info": store.StatusDegraded, "P5": store.StatusDegraded,
	} {
		got, ok := SeverityImpact(in)
		if !ok || got != want {
			t.Errorf("SeverityImpact(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "sev9", "whatever"} {
		if got, ok := SeverityImpact(in); ok {
			t.Errorf("SeverityImpact(%q) = %q, want none", in, got)
		}
	}
}

// The severity table of the README lists exactly the severities of
// severityImpacts, with their impacts.
func TestREADMESeverityTable(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(readme), "\n| Severity ")
	if !ok {
		t.Fatal("README: no severity table")
	}
	table, _, _ = strings.Cut(table, "\n\n")
	impacts := map[string]store.Status{"major outage": store.StatusMajorOutage, "partial outage": store.StatusPartialOutage, "degraded performance": store.StatusDegraded}
	got := map[string]store.Status{}
	unknown := false
	for _, row := range strings.Split(table, "\n")[2:] {
		cells := strings.Split(row, "|")
		if len(cells) != 4 {
			t.Fatalf("README: severity row %q", row)
		}
		severities, impact := strings.TrimSpace(cells[1]), strings.TrimSpace(cells[2])
		if severities == "no severity, or an unknown one" {
			unknown = strings.HasPrefix(impact, "major outage")
			continue
		}
		severities, _, _ = strings.Cut(severities, ";") // the status words follow
		parts := strings.Split(severities, "`")         // the names are the odd parts
		for i := 1; i < len(parts); i += 2 {
			got[severityKey(parts[i])] = impacts[impact]
			if i+2 < len(parts) && parts[i+1] == "–" { // a range: `Sev2`–`Sev4`
				from, to := strings.TrimRight(parts[i], "0123456789"), strings.TrimRight(parts[i+2], "0123456789")
				first, _ := strconv.Atoi(parts[i][len(from):])
				last, _ := strconv.Atoi(parts[i+2][len(to):])
				for n := first; from == to && n < last; n++ {
					got[severityKey(from+strconv.Itoa(n))] = impacts[impact]
				}
			}
		}
	}
	equal(t, "README severity table", got, severityImpacts)
	equal(t, "README: no severity, or an unknown one, is a major outage", unknown, true)
}

func TestParseImpact(t *testing.T) {
	for in, want := range map[string]store.Status{
		"degraded": store.StatusDegraded, "Degraded performance": store.StatusDegraded,
		"partial_outage": store.StatusPartialOutage, "Partial outage": store.StatusPartialOutage, "partial-outage": store.StatusPartialOutage,
		"major_outage": store.StatusMajorOutage, "MAJOR": store.StatusMajorOutage,
	} {
		if got, ok := ParseImpact(in); !ok || got != want {
			t.Errorf("ParseImpact(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "operational", "maintenance", "critical"} {
		if got, ok := ParseImpact(in); ok {
			t.Errorf("ParseImpact(%q) = %q, want none", in, got)
		}
	}
}

func TestAlertInput(t *testing.T) {
	// impact: the alert's own, else the integration's, else the severity, else major
	a := &Alert{Key: "k", Firing: true, Severity: "warning", Impact: store.StatusPartialOutage}
	equal(t, "explicit", a.input(store.StatusMajorOutage).Impact, store.StatusPartialOutage)
	a.Impact = ""
	equal(t, "fixed by the integration", a.input(store.StatusMajorOutage).Impact, store.StatusMajorOutage)
	equal(t, "severity", a.input("").Impact, store.StatusDegraded)
	a.Severity = "unheard of"
	equal(t, "default", a.input("").Impact, store.StatusMajorOutage)
	equal(t, "resolved: no impact", (&Alert{Key: "k"}).input(store.StatusDegraded).Impact, store.Status(""))

	// limits and clean-up
	long := strings.Repeat("k", maxKeyLength+1)
	in := (&Alert{
		Key:     long,
		Firing:  true,
		Title:   "  VPN\ndown\x00 " + strings.Repeat("x", 300),
		Message: "line 1\r\nline 2\x07\n",
		Targets: []string{" VPN ", "vpn", "", strings.Repeat("y", maxTargetLength+1), "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"},
	}).input("")
	equal(t, "long key hashed", in.Key, HashToken(long))
	if r := []rune(in.Title); len(r) != maxTitleLength || !strings.HasPrefix(in.Title, "VPN down xxx") || !strings.HasSuffix(in.Title, "…") {
		t.Fatalf("title: %q", in.Title)
	}
	equal(t, "message", in.Message, "line 1\nline 2")
	equal(t, "targets", in.Targets, []string{"VPN", "a", "b", "c", "d", "e", "f", "g", "h", "i"})
}

func TestParseTime(t *testing.T) {
	at := time.Date(2026, 11, 16, 10, 2, 3, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"2026-11-16T10:02:03Z":          at,
		"2026-11-16T11:02:03+01:00":     at,
		"2026-11-16T10:02:03.250Z":      at.Add(250 * time.Millisecond),
		"2026-11-16T10:02:03":           at, // no zone: UTC
		"2026-11-16 10:02:03":           at,
		"2026-11-16 10:02:03.123":       at.Add(123 * time.Millisecond), // Uptime Kuma
		"2026-11-16 11:02:03+01:00":     at,
		" 2026-11-16 10:02:03 ":         at,
		"1794823323":                    at, // Unix seconds
		"1794823323.5":                  at.Add(500 * time.Millisecond),
		"1794823323000":                 at, // Unix milliseconds
		"2019-03-22T13:58:24.3713213Z":  time.Date(2019, 3, 22, 13, 58, 24, 371321300, time.UTC),
		"2017-07-20T21:41:00.0000000Z":  time.Date(2017, 7, 20, 21, 41, 0, 0, time.UTC),
		"2019-06-24T15:42:54.074+00:00": time.Date(2019, 6, 24, 15, 42, 54, 74e6, time.UTC),
	} {
		got, ok := readTime(in)
		if !ok || !got.Equal(want) {
			t.Errorf("readTime(%q) = %v, %v; want %v", in, got, ok, want)
		}
		if got := parseTime(in); !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v; want %v", in, got, want)
		}
	}
	// Times before 2001 ("never") are no start, but times.
	for _, in := range []string{"", "0", "0001-01-01T00:00:00Z", "978307199"} {
		if got, ok := readTime(in); !ok || !got.IsZero() {
			t.Errorf("readTime(%q) = %v, %v; want the zero time", in, got, ok)
		}
	}
	// Not times.
	for _, in := range []string{"yesterday", "16.11.2026 10:02", "11/16/2026 10:02:03 AM", "1.2.3", ".", "-1794823323", "1e9", "179482332300000000"} {
		if got, ok := readTime(in); ok || !got.IsZero() {
			t.Errorf("readTime(%q) = %v, %v; want no time", in, got, ok)
		}
	}
}

// ------------------------------------------------------------------ generic format

func TestParseGeneric(t *testing.T) {
	// one alert, keys in any case, a severity implied by the status
	b := parse(t, ParseGeneric, "application/json", `{"Service": "VPN", "STATUS": "warning", "title": "Slow", "description": "Latency 900 ms"}`)
	equal(t, "one", b.Alerts, []Alert{{Key: "service:vpn", Firing: true, Targets: []string{"VPN"}, Title: "Slow", Message: "Latency 900 ms", Severity: "warning"}})

	// a list, a service list, numbers, an explicit severity and impact
	b = parse(t, ParseGeneric, "text/plain", `[{"id": 42, "status": "CRITICAL", "service": ["db01", "ERP"], "severity": "Average", "impact": "partial outage"},
		{"key": "x", "status": "OK", "service": "ERP"}]`)
	equal(t, "list", b.Alerts, []Alert{
		{Key: "42", Firing: true, Targets: []string{"db01", "ERP"}, Severity: "Average", Impact: store.StatusPartialOutage},
		{Key: "x", Targets: []string{"ERP"}},
	})

	// {"alerts": […]}, unknown status and test alerts
	b = parse(t, ParseGeneric, "application/json", `{"alerts": [{"service": "VPN", "status": "No Data"}, {"service": "VPN", "status": "up"}]}`)
	equal(t, "alerts", len(b.Alerts), 1)
	equal(t, "note", b.Notes, []string{"Alert 1: status “No Data” is neither a problem nor a recovery: ignored."})
	b = parse(t, ParseGeneric, "application/json", `{"service": "VPN", "status": "test"}`)
	equal(t, "test", b.Test, true)

	// form fields (PRTG, curl --data-urlencode): lenient escaping, repeated fields, query parameters
	b, err := ParseGeneric(&Request{
		Body:        []byte("key=2041&status=Down%20ended%20(now%3A%20Up)&service=vpn-gw01&service=Network&title=vpn-gw01: Ping&message=Loss 100% (timeout)"),
		ContentType: "application/x-www-form-urlencoded",
		Query:       url.Values{"severity": {"warning"}, "status": {"down"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "form", b.Alerts, []Alert{{Key: "2041", Targets: []string{"vpn-gw01", "Network"}, Title: "vpn-gw01: Ping", Message: "Loss 100% (timeout)", Severity: "warning"}})
	b, err = ParseGeneric(&Request{Query: url.Values{"service": {"VPN"}, "status": {"down"}}})
	if err != nil || len(b.Alerts) != 1 || !b.Alerts[0].Firing {
		t.Fatalf("query parameters: %+v %v", b, err)
	}

	// a field's own name wins over its synonyms, whatever the order
	for range 20 {
		b = parse(t, ParseGeneric, "application/json", `{"id": "event-1", "key": "k", "summary": "S", "title": "T",
			"description": "D", "message": "M", "priority": "P1", "severity": "warning", "state": "ok", "status": "down",
			"services": ["B"], "service": "A"}`)
		equal(t, "synonyms (JSON)", b.Alerts, []Alert{{Key: "k", Firing: true, Targets: []string{"A", "B"}, Title: "T", Message: "M", Severity: "warning"}})
		b = parse(t, ParseGeneric, "application/x-www-form-urlencoded", "id=event-1&key=k&summary=S&title=T&state=ok&status=down&service=A")
		equal(t, "synonyms (form)", b.Alerts, []Alert{{Key: "k", Firing: true, Targets: []string{"A"}, Title: "T"}})
	}

	// started: when the problem began, in any of the formats of parseTime,
	// also as startsAt, started_at, startedAt or starts_at; the field's own
	// name wins
	began := time.Date(2026, 11, 16, 10, 2, 3, 0, time.UTC)
	for body, want := range map[string]time.Time{
		`{"service": "VPN", "status": "down", "started": "2026-11-16T11:02:03+01:00"}`:                      began,
		`{"service": "VPN", "status": "down", "startsAt": "2026-11-16 10:02:03"}`:                           began,
		`{"service": "VPN", "status": "down", "started_at": 1794823323}`:                                    began,
		`{"service": "VPN", "status": "down", "startedAt": 1794823323000}`:                                  began,
		`{"service": "VPN", "status": "down", "starts_at": "1794823323"}`:                                   began,
		`{"service": "VPN", "status": "down", "startsAt": "2001-01-01T00:00:00Z", "started": "1794823323"}`: began,
		`{"service": "VPN", "status": "down", "started": 0}`:                                                {}, // never: unknown
	} {
		b = parse(t, ParseGeneric, "application/json", body)
		equal(t, "started "+body, b.Alerts[0].StartsAt, want)
		equal(t, "no note "+body, len(b.Notes), 0)
	}
	b = parse(t, ParseGeneric, "application/x-www-form-urlencoded", "service=VPN&status=CRITICAL&started=1794823323")
	equal(t, "started as a form field", b.Alerts[0].StartsAt, began)
	b = parse(t, ParseGeneric, "application/json", `{"service": "VPN", "status": "down", "started": "16.11.2026 10:02"}`)
	equal(t, "a start that is no time", b.Alerts[0].StartsAt, time.Time{})
	equal(t, "its note", b.Notes, []string{"Start “16.11.2026 10:02” is not a time (RFC 3339, “YYYY-MM-DD HH:MM:SS” in UTC, or Unix seconds or milliseconds): ignored."})

	// values quoted in notes and errors: one line, clipped
	long := "no\npe " + strings.Repeat("x", 300)
	b = parse(t, ParseGeneric, "application/json", `{"service": "VPN", "status": "`+strings.ReplaceAll(long, "\n", `\n`)+`"}`)
	equal(t, "unknown status, clipped", b.Notes, []string{"Status “no pe " + strings.Repeat("x", 73) + "…” is neither a problem nor a recovery: ignored."})
	_, err = ParseGeneric(&Request{Body: []byte(`{"service": "VPN", "status": "down", "impact": "` + strings.ReplaceAll(long, "\n", `\n`) + `"}`)})
	equal(t, "unknown impact, clipped", err.Error(), "unknown impact “no pe "+strings.Repeat("x", 73)+"…”: send degraded, partial_outage or major_outage, or leave it out")

	// errors
	parseError(t, ParseGeneric, `{"service": "VPN"}`, "the status is missing")
	parseError(t, ParseGeneric, `{"status": "down"}`, "the service is missing")
	parseError(t, ParseGeneric, `[{"service": "a", "status": "down"}, {"service": "", "status": "down"}]`, "alert 2: the service is missing")
	parseError(t, ParseGeneric, `{"service": "VPN", "status": "down", "impact": "huge"}`, "unknown impact “huge”")
	parseError(t, ParseGeneric, `{"service": "VPN", "status": {"a": 1}}`, "must be text")
	parseError(t, ParseGeneric, `{"service": "VPN", "status": "down"`, "not valid JSON")
	parseError(t, ParseGeneric, `[]`, "contains no alert")
	parseError(t, ParseGeneric, ``, "contains no alert")
	parseError(t, ParseGeneric, `{"alerts": {"service": "VPN"}}`, "must be a list")
	_, err = ParseGeneric(&Request{Body: []byte("--x\r\n"), ContentType: "multipart/form-data"})
	if err == nil || !strings.Contains(err.Error(), "multipart") {
		t.Fatalf("multipart: %v", err)
	}
}

func TestCapitalize(t *testing.T) {
	for in, want := range map[string]string{
		"status “x”":                         "Status “x”",
		"“alerts” must be a list of objects": "“alerts” must be a list of objects",
		"élan":                               "Élan",
		"":                                   "",
		"\xff":                               "\xff",
	} {
		equal(t, in, capitalize(in), want)
	}
}

// ------------------------------------------------------------------ Alertmanager and Grafana

const alertmanagerPayload = `{
  "version": "4",
  "groupKey": "{}:{alertname=\"InstanceDown\"}",
  "truncatedAlerts": 0,
  "status": "firing",
  "receiver": "status-page",
  "groupLabels": {"alertname": "InstanceDown"},
  "commonLabels": {"alertname": "InstanceDown", "job": "node"},
  "commonAnnotations": {},
  "externalURL": "http://alertmanager:9093",
  "alerts": [
    {
      "status": "firing",
      "labels": {"alertname": "InstanceDown", "instance": "vpn01:9100", "job": "node", "Severity": "critical", "statuspage_service": "VPN"},
      "annotations": {"summary": "VPN gateway unreachable", "description": "vpn01:9100 has been down for more than 5 minutes."},
      "startsAt": "2026-11-16T10:00:00.123Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "http://prometheus:9090/graph?g0.expr=up+%3D%3D+0",
      "fingerprint": "c6e9b6e2a5b4d1f0"
    },
    {
      "status": "resolved",
      "labels": {"alertname": "InstanceDown", "instance": "mail01:9100", "job": "node", "statuspage_impact": "partial_outage"},
      "annotations": {},
      "startsAt": "2026-11-16T09:00:00Z",
      "endsAt": "2026-11-16T10:01:00Z",
      "fingerprint": "a1b2c3d4e5f60708"
    },
    {
      "status": "pending",
      "labels": {"alertname": "Odd"},
      "annotations": {},
      "fingerprint": "ffff"
    }
  ]
}`

func TestParseAlertmanager(t *testing.T) {
	b := parse(t, ParseAlertmanager, "application/json", alertmanagerPayload)
	equal(t, "alerts", b.Alerts, []Alert{
		{
			Key: "c6e9b6e2a5b4d1f0", Firing: true, Targets: []string{"VPN", "InstanceDown", "node", "vpn01:9100"},
			Title: "VPN gateway unreachable", Message: "vpn01:9100 has been down for more than 5 minutes.", Severity: "critical",
			StartsAt: time.Date(2026, 11, 16, 10, 0, 0, 123e6, time.UTC),
		},
		{
			Key: "a1b2c3d4e5f60708", Targets: []string{"InstanceDown", "node", "mail01:9100"}, Title: "InstanceDown",
			Impact: store.StatusPartialOutage, StartsAt: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC),
		},
	})
	equal(t, "notes", b.Notes, []string{"Alert 3: status “pending” is neither firing nor resolved: ignored."})

	// Grafana: the same alerts with more fields; its test alert is a test.
	b = parse(t, ParseAlertmanager, "application/json", `{"receiver": "status-page", "status": "firing", "orgId": 1, "state": "alerting",
		"title": "[FIRING:1] High latency", "message": "…", "version": "1",
		"alerts": [{"status": "firing", "labels": {"alertname": "High latency", "grafana_folder": "Network", "service": "VPN", "severity": "warning"},
		"annotations": {"description": "p95 above 2 s"}, "startsAt": "2026-11-16T10:00:00Z", "endsAt": "0001-01-01T00:00:00Z",
		"fingerprint": "57c6d9296de2ad39", "silenceURL": "http://grafana/alerting/silence/new", "dashboardURL": "", "panelURL": "",
		"values": {"B": 2.4, "C": 1}, "valueString": "[ var='B' value=2.4 ]"}]}`)
	equal(t, "grafana", b.Alerts, []Alert{{
		Key: "57c6d9296de2ad39", Firing: true, Targets: []string{"VPN", "High latency"}, Title: "High latency",
		Message: "p95 above 2 s", Severity: "warning", StartsAt: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC),
	}})
	b = parse(t, ParseAlertmanager, "application/json", `{"receiver": "test", "status": "firing", "alerts": [{"status": "firing",
		"labels": {"alertname": "TestAlert", "instance": "Grafana"}, "annotations": {"summary": "Notification test"}, "fingerprint": "x"}]}`)
	equal(t, "grafana test", b.Test, true)

	// Grafana's alerts for a rule it could not evaluate keep the rule's
	// labels and annotations, with the rule's name as rulename: ignored,
	// firing or resolved. An alert of such a name without rulename is not
	// one of them.
	b = parse(t, ParseAlertmanager, "application/json", `{"receiver": "status-page", "status": "firing", "orgId": 1, "alerts": [
		{"status": "firing", "labels": {"alertname": "DatasourceError", "rulename": "VPN gateway", "datasource_uid": "P1809F7CD0C75ACF3",
		 "grafana_folder": "Network", "statuspage_service": "VPN", "severity": "critical"},
		 "annotations": {"summary": "VPN gateway unreachable", "Error": "failed to execute query A: connection refused"},
		 "startsAt": "2026-11-16T10:00:00Z", "endsAt": "0001-01-01T00:00:00Z", "fingerprint": "8a3d5c1f2b9e7d04"},
		{"status": "resolved", "labels": {"alertname": "DatasourceNoData", "rulename": "Mail queue", "statuspage_service": "Mail"},
		 "annotations": {}, "startsAt": "2026-11-16T09:00:00Z", "endsAt": "2026-11-16T09:30:00Z", "fingerprint": "1b2c3d4e5f607182"},
		{"status": "firing", "labels": {"alertname": "DatasourceNoData", "rulename": "Disk\nfull", "statuspage_service": "Mail"}, "fingerprint": "aa"},
		{"status": "resolved", "labels": {"alertname": "DatasourceError", "rulename": "Disk"}, "fingerprint": "bb"},
		{"status": "firing", "labels": {"alertname": "DatasourceError", "service": "VPN"}, "fingerprint": "cc"}]}`)
	equal(t, "not evaluated", b.Notes, []string{
		"Alert 1: Grafana could not evaluate the rule “VPN gateway” (DatasourceError): ignored.",
		"Alert 2: Grafana receives data for the rule “Mail queue” again (DatasourceNoData resolved): ignored.",
		"Alert 3: Grafana received no data for the rule “Disk full” (DatasourceNoData): ignored.",
		"Alert 4: Grafana can evaluate the rule “Disk” again (DatasourceError resolved): ignored.",
	})
	equal(t, "without rulename", b.Alerts, []Alert{{Key: "cc", Firing: true, Targets: []string{"VPN", "DatasourceError"}, Title: "DatasourceError"}})

	// values quoted in notes: one line, clipped
	long := strings.Repeat("y", 100)
	b = parse(t, ParseAlertmanager, "application/json", `{"alerts": [{"status": "odd\n`+long+`", "fingerprint": "a"},
		{"status": "firing", "labels": {"service": "VPN", "statuspage_impact": "huge\n`+long+`"}, "fingerprint": "b"}]}`)
	equal(t, "clipped notes", b.Notes, []string{
		"Alert 1: status “odd " + strings.Repeat("y", 75) + "…” is neither firing nor resolved: ignored.",
		"Alert 2: unknown statuspage_impact “huge " + strings.Repeat("y", 74) + "…” (use degraded, partial_outage or major_outage): the severity decides.",
	})

	// an alert without fingerprint is identified by its labels
	b1 := parse(t, ParseAlertmanager, "", `{"alerts": [{"status": "firing", "labels": {"a": "1", "b": "2"}}]}`)
	b2 := parse(t, ParseAlertmanager, "", `{"alerts": [{"status": "resolved", "labels": {"b": "2", "a": "1"}}]}`)
	if k := b1.Alerts[0].Key; k == "" || k != b2.Alerts[0].Key {
		t.Fatalf("labels keys: %q %q", k, b2.Alerts[0].Key)
	}

	parseError(t, ParseAlertmanager, `{"service": "VPN", "status": "down"}`, "has no “alerts”")
	parseError(t, ParseAlertmanager, `{"alerts": [{"labels": {"a": 1}}]}`, "unexpected JSON value")
	parseError(t, ParseAlertmanager, `<xml/>`, "not valid JSON")
}

// ------------------------------------------------------------------ Uptime Kuma

// kumaPayloadJSON is an important notification of Uptime Kuma 2 (2.0.2: its
// heartbeat carries the retries) at 10:02:00.123 UTC, with the heartbeat
// status status, for the monitor VPN gateway, which has one retry.
func kumaPayloadJSON(status int, msg string) string {
	retries := "0"
	if status == kumaDown {
		retries = "2" // its retry (PENDING), then DOWN
	}
	return `{
  "heartbeat": {"monitorID": 7, "status": ` + string(rune('0'+status)) + `, "msg": "` + msg + `", "time": "2026-11-16 10:02:00.123",
                "timezone": "Europe/Berlin", "timezoneOffset": "+01:00", "localDateTime": "2026-11-16 11:02:00", "important": true,
                "duration": 60, "retries": ` + retries + `, "ping": null},
  "monitor": {"id": 7, "name": "VPN gateway", "type": "http", "url": "https://vpn.example.com", "hostname": null, "port": null, "maxretries": 1,
              "pathName": "Network / VPN gateway", "tags": [{"tag_id": 1, "monitor_id": 7, "value": "VPN", "name": "statuspage_service", "color": "#059669"}]},
  "msg": "[VPN gateway] [🔴 Down] ` + msg + `"
}`
}

// kuma123 turns a notification of kumaPayloadJSON into one of Uptime Kuma
// 1.23, whose heartbeats carry no retries, for a monitor with maxretries
// retries.
func kuma123(body string, maxretries int) string {
	body = strings.Replace(strings.Replace(body, `"retries": 0, `, ``, 1), `"retries": 2, `, ``, 1)
	return strings.Replace(body, `"maxretries": 1,`, `"maxretries": `+strconv.Itoa(maxretries)+`,`, 1)
}

// kuma123Down is a down notification as Uptime Kuma 1.23.16 sends it
// (heartbeat.js toJSON with the time zone of sendNotification, and
// Monitor.toJSON without sensitive data, shortened): the heartbeat carries
// no retries; the monitor has two.
const kuma123Down = `{
  "heartbeat": {"monitorID": 7, "status": 0, "time": "2026-11-16 10:02:00.123", "msg": "connect ECONNREFUSED 10.0.0.1:443",
                "ping": null, "important": true, "duration": 60,
                "timezone": "Europe/Berlin", "timezoneOffset": "+01:00", "localDateTime": "2026-11-16 11:02:00"},
  "monitor": {"id": 7, "name": "VPN gateway", "description": null, "pathName": "VPN gateway", "parent": null, "childrenIDs": [],
              "url": "https://vpn.example.com", "method": "GET", "hostname": null, "port": null, "maxretries": 2, "weight": 2000,
              "active": true, "forceInactive": false, "type": "http", "timeout": 48, "interval": 60, "retryInterval": 60,
              "resendInterval": 0, "keyword": null, "invertKeyword": false, "expiryNotification": false, "ignoreTls": false,
              "upsideDown": false, "packetSize": 56, "maxredirects": 10, "accepted_statuscodes": ["200-299"],
              "dns_resolve_type": "A", "dns_resolve_server": "1.1.1.1", "dns_last_result": null, "docker_container": "",
              "docker_host": null, "proxyId": null, "notificationIDList": {"1": true},
              "tags": [{"id": 1, "monitor_id": 7, "tag_id": 1, "value": "VPN", "name": "statuspage_service", "color": "#059669"}],
              "maintenance": false, "includeSensitiveData": false},
  "msg": "[VPN gateway] [🔴 Down] connect ECONNREFUSED 10.0.0.1:443"
}`

func TestParseUptimeKuma(t *testing.T) {
	// The heartbeat of the change to down is important: its time (UTC) is
	// when the problem began.
	b := parse(t, ParseUptimeKuma, "application/json", kumaPayloadJSON(0, "connect ECONNREFUSED"))
	equal(t, "down", b.Alerts, []Alert{{
		Key: "monitor:7", Firing: true, Targets: []string{"VPN", "", "VPN gateway", "", "https://vpn.example.com"},
		Title: "VPN gateway is down", Message: "connect ECONNREFUSED", StartsAt: time.Date(2026, 11, 16, 10, 2, 0, 123e6, time.UTC),
	}})
	// A resend while the monitor stays down is not important: no start.
	resend := strings.Replace(kumaPayloadJSON(0, "timeout"), `"important": true`, `"important": false`, 1)
	b = parse(t, ParseUptimeKuma, "application/json", resend)
	equal(t, "resend firing", b.Alerts[0].Firing, true)
	equal(t, "resend without a start", b.Alerts[0].StartsAt, time.Time{})
	b = parse(t, ParseUptimeKuma, "application/json", strings.Replace(kumaPayloadJSON(0, "timeout"), `"important": true`, `"important": 1`, 1))
	equal(t, "important as a number", b.Alerts[0].StartsAt, time.Date(2026, 11, 16, 10, 2, 0, 123e6, time.UTC))

	// Uptime Kuma 1.23 counts a monitor's retries from 0 again when it
	// restarts, so a monitor with retries that is still down is reported
	// down again as important: no start. Without retries it is reported
	// down only when it goes down.
	b = parse(t, ParseUptimeKuma, "application/json", kuma123Down)
	equal(t, "1.23 with retries", b.Alerts, []Alert{{
		Key: "monitor:7", Firing: true, Targets: []string{"VPN", "", "VPN gateway", "", "https://vpn.example.com"},
		Title: "VPN gateway is down", Message: "connect ECONNREFUSED 10.0.0.1:443",
	}})
	b = parse(t, ParseUptimeKuma, "application/json", strings.Replace(kuma123Down, `"maxretries": 2,`, `"maxretries": 0,`, 1))
	equal(t, "1.23 without retries", b.Alerts[0].StartsAt, time.Date(2026, 11, 16, 10, 2, 0, 123e6, time.UTC))
	b = parse(t, ParseUptimeKuma, "application/json", strings.Replace(kuma123Down, `"maxretries": 2,`, ``, 1))
	equal(t, "neither retries nor maxretries", b.Alerts[0].StartsAt, time.Time{})
	b = parse(t, ParseUptimeKuma, "application/json", strings.Replace(kumaPayloadJSON(0, "timeout"), `"retries": 2,`, `"retries": null,`, 1))
	equal(t, "retries null, the monitor with a retry", b.Alerts[0].StartsAt, time.Time{})
	b = parse(t, ParseUptimeKuma, "application/json", strings.Replace(kumaPayloadJSON(0, "timeout"), `"maxretries": 1,`, `"maxretries": 5,`, 1))
	equal(t, "Uptime Kuma 2 keeps the retries", b.Alerts[0].StartsAt, time.Date(2026, 11, 16, 10, 2, 0, 123e6, time.UTC))
	b = parse(t, ParseUptimeKuma, "application/json", strings.Replace(kuma123Down, `"important": true`, `"important": false`, 1))
	equal(t, "1.23 resend", b.Alerts[0].StartsAt, time.Time{})
	b = parse(t, ParseUptimeKuma, "application/json", kumaPayloadJSON(1, "200 - OK"))
	equal(t, "up", b.Alerts[0].Firing, false)
	equal(t, "up key", b.Alerts[0].Key, "monitor:7")
	equal(t, "up without a start", b.Alerts[0].StartsAt, time.Time{})
	b = parse(t, ParseUptimeKuma, "application/json", kumaPayloadJSON(2, "timeout"))
	equal(t, "pending", len(b.Alerts), 0)
	equal(t, "pending note", b.Notes, []string{"“VPN gateway” is pending: nothing to change."})
	b = parse(t, ParseUptimeKuma, "application/json", strings.Replace(kumaPayloadJSON(2, "timeout"), `"name": "VPN gateway"`, `"name": "VPN\ngateway`+strings.Repeat(" x", 100)+`"`, 1))
	equal(t, "a name in a note: one line, clipped", b.Notes, []string{"“VPN gateway" + strings.Repeat(" x", 34) + "…” is pending: nothing to change."})

	b = parse(t, ParseUptimeKuma, "application/json", `{"heartbeat": null, "monitor": null, "msg": "Status page Testing"}`)
	equal(t, "test", b.Test, true)
	b = parse(t, ParseUptimeKuma, "application/json", `{"heartbeat": null, "monitor": null, "msg": "[VPN gateway] Certificate will expire in 14 days"}`)
	equal(t, "reminder", b.Test, false)
	equal(t, "reminder alerts", len(b.Alerts), 0)

	parseError(t, ParseUptimeKuma, `{"service": "VPN"}`, "not a notification of Uptime Kuma")
	parseError(t, ParseUptimeKuma, `data={"x":1}`, "Preset – application/json")
}

// ------------------------------------------------------------------ Azure Monitor

// The sample of the Azure Monitor documentation (common alert schema).
const azureSample = `{
  "schemaId": "azureMonitorCommonAlertSchema",
  "data": {
    "essentials": {
      "alertId": "/subscriptions/<subscription ID>/providers/Microsoft.AlertsManagement/alerts/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e",
      "alertRule": "WCUS-R2-Gen2",
      "severity": "Sev3",
      "signalType": "Metric",
      "monitorCondition": "Resolved",
      "monitoringService": "Platform",
      "alertTargetIDs": ["/subscriptions/<subscription ID>/resourcegroups/pipelinealertrg/providers/microsoft.compute/virtualmachines/wcus-r2-gen2"],
      "configurationItems": ["wcus-r2-gen2"],
      "originAlertId": "3f2d4487-b0fc-4125-8bd5-7ad17384221e_PipeLineAlertRG_microsoft.insights_metricAlerts_WCUS-R2-Gen2_-117781227",
      "firedDateTime": "2019-03-22T13:58:24.3713213Z",
      "resolvedDateTime": "2019-03-22T14:03:16.2246313Z",
      "description": "",
      "essentialsVersion": "1.0",
      "alertContextVersion": "1.0"
    },
    "alertContext": {"properties": null, "conditionType": "SingleResourceMultipleMetricCriteria", "condition": {"windowSize": "PT5M", "allOf": []}},
    "customProperties": {"Key1": "Value1", "Key2": "Value2"}
  }
}`

func TestParseAzureMonitor(t *testing.T) {
	b := parse(t, ParseAzureMonitor, "application/json", azureSample)
	equal(t, "resolved", b.Alerts, []Alert{{
		Key:      "/subscriptions/<subscription ID>/providers/Microsoft.AlertsManagement/alerts/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e",
		Targets:  []string{"", "", "WCUS-R2-Gen2", "wcus-r2-gen2", "wcus-r2-gen2"},
		Title:    "WCUS-R2-Gen2",
		Severity: "Sev3",
		StartsAt: time.Date(2019, 3, 22, 13, 58, 24, 371321300, time.UTC),
	}})

	fired := strings.Replace(azureSample, `"Resolved"`, `"Fired"`, 1)
	fired = strings.Replace(fired, `{"Key1": "Value1", "Key2": "Value2"}`, `{"StatusPage_Service": "ERP", "statuspage_impact": "degraded"}`, 1)
	b = parse(t, ParseAzureMonitor, "application/json", fired)
	equal(t, "fired", b.Alerts[0].Firing, true)
	equal(t, "custom property names the service", b.Alerts[0].Targets[0], "ERP")
	equal(t, "impact", b.Alerts[0].Impact, store.StatusDegraded)
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(azureSample, `"customProperties": {"Key1": "Value1", "Key2": "Value2"}`, `"customProperties": null`, 1))
	equal(t, "no custom properties", len(b.Alerts), 1)

	// The custom property service comes after statuspage_service, before
	// the rule's name.
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(fired, `{"StatusPage_Service": "ERP", "statuspage_impact": "degraded"}`,
		`{"Service": "Networking", "Owner": "Team A"}`, 1))
	equal(t, "custom property service", b.Alerts[0].Targets[:3], []string{"", "Networking", "WCUS-R2-Gen2"})

	// values quoted in notes: one line, clipped
	long := strings.Repeat("z", 100)
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(azureSample, `"Resolved"`, `"Odd\n`+long+`"`, 1))
	equal(t, "clipped condition", b.Notes, []string{"Monitor condition “Odd " + strings.Repeat("z", 75) + "…” is neither Fired nor Resolved: ignored."})
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(fired, `"degraded"`, `"huge\n`+long+`"`, 1))
	equal(t, "clipped impact", b.Notes, []string{"Unknown custom property statuspage_impact “huge " + strings.Repeat("z", 74) + "…” (use degraded, partial_outage or major_outage): the severity decides."})

	parseError(t, ParseAzureMonitor, `{"schemaId": "Microsoft.Insights/activityLogs", "data": {}}`, "common alert schema")
	parseError(t, ParseAzureMonitor, `{"status": "Activated", "context": {}}`, "common alert schema")
}

// azureActivity returns an activity log alert: essentials like those of the
// sample test actions of Microsoft Learn, for monitoringService and with
// the alert id id, and the alert context context – as a JSON object, or as
// the samples give it, {"alertContext": {…}}.
func azureActivity(monitoringService, id, context string) string {
	var wrapped struct {
		AlertContext json.RawMessage `json:"alertContext"`
	}
	if json.Unmarshal([]byte(context), &wrapped) == nil && wrapped.AlertContext != nil {
		context = string(wrapped.AlertContext)
	}
	return `{"schemaId": "azureMonitorCommonAlertSchema", "data": {"essentials": {
		"alertId": "/subscriptions/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e/providers/Microsoft.AlertsManagement/alerts/` + id + `",
		"alertRule": "VPN health", "severity": "Sev4", "signalType": "Activity Log", "monitorCondition": "Fired",
		"monitoringService": "` + monitoringService + `",
		"alertTargetIDs": ["/subscriptions/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e/resourcegroups/net-rg/providers/microsoft.compute/virtualmachines/vpn-vm"],
		"configurationItems": ["vpn-vm"], "firedDateTime": "2026-11-16T10:05:00.000Z", "description": "VPN gateway VM",
		"essentialsVersion": "1.0", "alertContextVersion": "1.0"},
		"alertContext": ` + context + `, "customProperties": {"statuspage_service": "VPN"}}}`
}

// Activity log alerts are stateless (always Fired): Service Health and
// Resource Health alerts are read from their alert context, the others
// ignored.
func TestParseAzureActivityLog(t *testing.T) {
	// Service Health: the sample test action is an incident in the stage
	// Resolved (Azure sent it as Fired); the tracking id is the key.
	b := parse(t, ParseAzureMonitor, "application/json", azureServiceHealthTest)
	equal(t, "service health resolved", b.Alerts, []Alert{{
		Key: "servicehealth:TEST-TTT", Targets: []string{"", "", "test-ServiceHealthAlertRule", "aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e"},
		Title: "test-ServiceHealthAlertRule", Message: "TestActionGroup-TestServiceHealthAlert", Severity: "Sev4",
		StartsAt: time.Date(2025, 4, 15, 18, 20, 36, 256e6, time.UTC),
	}})
	equal(t, "no notes", len(b.Notes), 0)
	active := strings.Replace(strings.Replace(azureServiceHealthTest, `"stage": "Resolved"`, `"stage": "Active"`, 1), `"status": "Resolved"`, `"status": "Active"`, 1)
	b = parse(t, ParseAzureMonitor, "application/json", active)
	equal(t, "service health active", b.Alerts[0].Firing, true)
	equal(t, "same key", b.Alerts[0].Key, "servicehealth:TEST-TTT")
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(azureServiceHealthTest, `"stage": "Resolved"`, `"stage": "RCA"`, 1))
	equal(t, "root cause analysis: over", len(b.Alerts) == 1 && !b.Alerts[0].Firing, true)
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(active, `"stage": "Active",`, ``, 1))
	equal(t, "no stage: the status", b.Alerts[0].Firing, true)
	// The start is when the impact began, the same in every notice of the
	// incident, not when Azure fired the notice (in the sample both are the
	// same).
	update := strings.Replace(strings.Replace(active, `"impactStartTime": "2025-04-15T18:20:36.256Z"`, `"impactStartTime": "2025-04-15T18:00:00Z"`, 1),
		`"firedDateTime": "2025-04-15T18:20:36.256Z"`, `"firedDateTime": "2025-04-15T19:00:36.256Z"`, 1)
	b = parse(t, ParseAzureMonitor, "application/json", update)
	equal(t, "start: impactStartTime", b.Alerts[0].StartsAt, time.Date(2025, 4, 15, 18, 0, 0, 0, time.UTC))
	// Without the monitoring service, the operation tells the kind.
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(active, `"monitoringService": "ServiceHealth"`, `"monitoringService": ""`, 1))
	equal(t, "service health by its operation", len(b.Alerts) == 1 && b.Alerts[0].Key == "servicehealth:TEST-TTT", true)
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(active, `"stage": "Active"`, `"stage": "Odd"`, 1))
	equal(t, "unknown stage", b.Notes, []string{"“test-ServiceHealthAlertRule”: stage “Odd” of the Service Health incident TEST-TTT is neither Active nor Resolved: ignored."})
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(active, `"trackingId": "TEST-TTT",`, ``, 1))
	equal(t, "no tracking id", b.Notes, []string{"“test-ServiceHealthAlertRule”: a Service Health incident without a tracking id: ignored."})
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(active, `"incidentType": "Incident"`, `"incidentType": "Security"`, 1))
	equal(t, "security advisory", b.Notes, []string{"“test-ServiceHealthAlertRule”: a Service Health notice of the type “Security”, not an incident: ignored."})
	// A planned maintenance (the alert context of Microsoft Learn): ignored.
	b = parse(t, ParseAzureMonitor, "application/json", azureActivity("ServiceHealth", "m1", azureServiceHealthContext))
	equal(t, "maintenance", b.Notes, []string{"“VPN health”: a Service Health notice of the type “Maintenance”, not an incident: ignored."})
	equal(t, "maintenance alerts", len(b.Alerts), 0)

	// Resource Health: the sample test action, Unavailable after Available:
	// a major outage from the time of the event; the resource is the key.
	resourceKey := "resourcehealth:/subscriptions/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e/resourcegroups/test-rg/providers/microsoft.compute/virtualmachines/test-vm"
	b = parse(t, ParseAzureMonitor, "application/json", azureResourceHealthTest)
	equal(t, "resource unavailable", b.Alerts, []Alert{{
		Key: resourceKey, Firing: true, Targets: []string{"", "", "test-ResourceHealthAlertRule", "test-VM", "test-VM"},
		Title: "test-ResourceHealthAlertRule", Message: "Rebooted by user", Severity: "critical",
		StartsAt: time.Date(2025, 4, 15, 18, 19, 45, 304e6, time.UTC),
	}})
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(azureResourceHealthTest, `"monitoringService": "Resource Health"`, `"monitoringService": ""`, 1))
	equal(t, "resource health by its operation", len(b.Alerts) == 1 && b.Alerts[0].Key == resourceKey && b.Alerts[0].Firing, true)
	degraded := strings.Replace(strings.Replace(azureResourceHealthTest, `"currentHealthStatus": "Unavailable"`, `"currentHealthStatus": "Degraded"`, 1),
		`"previousHealthStatus": "Available"`, `"previousHealthStatus": "Unavailable"`, 1)
	b = parse(t, ParseAzureMonitor, "application/json", degraded)
	equal(t, "degraded", b.Alerts[0].Firing && b.Alerts[0].Severity == "warning" && b.Alerts[0].Key == resourceKey, true)
	equal(t, "still unhealthy: no start", b.Alerts[0].StartsAt, time.Time{})
	available := strings.Replace(strings.Replace(azureResourceHealthTest, `"currentHealthStatus": "Unavailable"`, `"currentHealthStatus": "Available"`, 1),
		`"previousHealthStatus": "Available"`, `"previousHealthStatus": "Unavailable"`, 1)
	b = parse(t, ParseAzureMonitor, "application/json", available)
	equal(t, "available again", len(b.Alerts) == 1 && !b.Alerts[0].Firing && b.Alerts[0].Key == resourceKey, true)
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(azureResourceHealthTest, `"currentHealthStatus": "Unavailable"`, `"currentHealthStatus": "Unknown"`, 1))
	equal(t, "unknown", b.Notes, []string{"“test-ResourceHealthAlertRule”: the health of the resource is “Unknown”: ignored."})
	noHealth := strings.Replace(azureResourceHealthTest, `"currentHealthStatus": "Unavailable"`, `"currentHealthStatus": null`, 1)
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(noHealth, `"status": "Active"`, `"status": "Updated"`, 1))
	equal(t, "updated without a health", b.Notes, []string{"“test-ResourceHealthAlertRule”: a Resource Health event without a health status: nothing to change."})
	b = parse(t, ParseAzureMonitor, "application/json", strings.Replace(noHealth, `"status": "Active"`, `"status": "Resolved"`, 1))
	equal(t, "resolved without a health", len(b.Alerts) == 1 && !b.Alerts[0].Firing, true)
	// The alert context of Microsoft Learn (monitoringService ResourceHealth,
	// without a space), with custom properties.
	b = parse(t, ParseAzureMonitor, "application/json", azureActivity("ResourceHealth", "r1", azureResourceHealthContext))
	equal(t, "resource health context", b.Alerts, []Alert{{
		Key:    "resourcehealth:/subscriptions/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e/resourcegroups/net-rg/providers/microsoft.compute/virtualmachines/vpn-vm",
		Firing: true, Targets: []string{"VPN", "", "VPN health", "vpn-vm", "vpn-vm"}, Title: "VPN health",
		Message:  "This virtual machine is stopping and deallocating as requested by an authorized user or process",
		Severity: "critical", StartsAt: time.Date(2019, 6, 24, 15, 42, 54, 74e6, time.UTC),
	}})

	// Other activity log alerts never resolve: ignored.
	b = parse(t, ParseAzureMonitor, "application/json", azureActivityLogTest)
	equal(t, "administrative", b.Notes, []string{"“test-activityLogAlertRule” is an activity log alert (Activity Log - Administrative), which never resolves: ignored (of the activity log alerts, only Service Health and Resource Health alerts are read)."})
	equal(t, "administrative alerts", len(b.Alerts), 0)
	b = parse(t, ParseAzureMonitor, "application/json", azureActivity("Activity Log - Security", "s1", `{"eventSource": "Security", "status": "Active", "properties": {"severity": "Low"}}`))
	equal(t, "security", len(b.Alerts) == 0 && len(b.Notes) == 1, true)
}

// ------------------------------------------------------------------ PRTG

// prtgBody is the payload of the instructions as PRTG sends it, with the
// status status.
func prtgBody(status string) string {
	return "key=2041&status=" + url.QueryEscape(status) + "&service=vpn-gw01&service=Network&title=vpn-gw01: Ping&message=Request timed out"
}

// The status is the sensor's current status: Down in any form is a
// problem, every other status of PRTG its end.
func TestParsePRTG(t *testing.T) {
	for status, want := range map[string]Alert{
		"Down":                      {Firing: true},
		"Down (Partial)":            {Firing: true, Severity: "partial"},
		"Down (Acknowledged)":       {Firing: true},
		"Down (was: Up)":            {Firing: true},
		"Warning ended (now: Down)": {Firing: true},
		"Up":                        {},
		"Warning":                   {},
		"Unusual":                   {},
		"Paused (paused by user)":   {},
		"Paused by parent":          {},
		"Unknown":                   {},
		"None":                      {},
		"Down ended (now: Warning)": {},
		"Down ended (now: Up)":      {},
		"Up (was: Down)":            {},
		// %status before 20.1.57: only the current status counts, not the
		// one it was
		"Down (was: Down (Partial))":     {Firing: true},
		"Down (Partial) (was: Down)":     {Firing: true, Severity: "partial"},
		"Down (was: Warning) ESCALATION": {Firing: true},
	} {
		b := parse(t, ParsePRTG, "application/x-www-form-urlencoded", prtgBody(status))
		want.Key, want.Targets, want.Title, want.Message = "2041", []string{"vpn-gw01", "Network"}, "vpn-gw01: Ping", "Request timed out"
		equal(t, status, b.Alerts, []Alert{want})
	}
	b := parse(t, ParsePRTG, "application/x-www-form-urlencoded", prtgBody("Down")+"&severity=warning")
	equal(t, "the field severity wins", b.Alerts[0].Severity, "warning")
	// The status test (e.g. sent with curl) tests the connection.
	b = parse(t, ParsePRTG, "application/x-www-form-urlencoded", prtgBody("test"))
	equal(t, "status test", b.Test, true)
	equal(t, "status test: no alert", len(b.Alerts), 0)

	// The test of a notification template sends the placeholders unfilled.
	b = parse(t, ParsePRTG, "application/x-www-form-urlencoded", "key=%sensorid&status=%laststatus&service=%device&service=%group&title=%device: %name&message=%message")
	equal(t, "test", b.Test, true)
	// A status PRTG did not fill in (%laststatus before 20.1.57) is no state.
	b = parse(t, ParsePRTG, "application/x-www-form-urlencoded", "key=2041&status=%laststatus&service=vpn-gw01")
	equal(t, "unfilled status", b.Notes, []string{"Status “%laststatus” is neither a problem nor a recovery: ignored."})
	equal(t, "unfilled status: no test", b.Test, false)
}

// ------------------------------------------------------------------ Datadog

// datadogBody is the payload of the instructions as Datadog sends it, with
// the variables vars filled in.
func datadogBody(vars map[string]string) string {
	body := Lookup("datadog").Example
	for k, v := range map[string]string{"$AGGREG_KEY": "9bd4ac313a4d1e8fae2482df7b77628", "$TAGS[statuspage_service]": "", "$TAGS[service]": "vpn",
		"$HOSTNAME": "vpn-gw01", "$ALERT_TITLE": "[Triggered on {host:vpn-gw01}] Host is Down", "$TEXT_ONLY_MSG": "No data"} {
		if _, ok := vars[k]; !ok {
			vars[k] = v
		}
	}
	for k, v := range vars {
		body = strings.ReplaceAll(body, k, v)
	}
	return body
}

// The transition says whether the monitor alerts, the alert type how
// badly; a Renotify counts only with the alert type error or warning.
func TestParseDatadog(t *testing.T) {
	for _, tc := range []struct {
		transition, alertType string
		firing                bool
		severity              string
	}{
		{"Triggered", "error", true, "critical"},
		{"Re-Triggered", "error", true, "critical"},
		{"Triggered", "", true, ""}, // the payload before the alert type: a major outage
		{"Warn", "warning", true, "warning"},
		{"Re-Warn", "warning", true, "warning"},
		{"Warn", "", true, "warning"},
		{"Renotify", "warning", true, "warning"},
		{"Renotify", "error", true, "critical"},
		{"Recovered", "success", false, ""},
	} {
		b := parse(t, ParseDatadog, "application/json", datadogBody(map[string]string{"$ALERT_TRANSITION": tc.transition, "$ALERT_TYPE": tc.alertType}))
		equal(t, tc.transition+"/"+tc.alertType, b.Alerts, []Alert{{Key: "9bd4ac313a4d1e8fae2482df7b77628", Firing: tc.firing,
			Targets: []string{"vpn", "vpn-gw01"}, Title: "[Triggered on {host:vpn-gw01}] Host is Down", Message: "No data", Severity: tc.severity}})
	}
	for _, tc := range []struct{ transition, alertType, note string }{
		{"No Data", "error", "Status “No Data” is neither a problem nor a recovery: ignored."},
		{"Re-No Data", "info", "Status “Re-No Data” is neither a problem nor a recovery: ignored."},
		{"Renotify", "info", "Status “Renotify” of the alert type “info”, neither error nor warning (e.g. No Data): ignored."},
		{"Renotify", "", "Status “Renotify” without the alert type: ignored (send \"severity\": \"$ALERT_TYPE\" as in the payload of the instructions)."},
	} {
		b := parse(t, ParseDatadog, "application/json", datadogBody(map[string]string{"$ALERT_TRANSITION": tc.transition, "$ALERT_TYPE": tc.alertType}))
		equal(t, tc.transition+"/"+tc.alertType+": no alert", len(b.Alerts), 0)
		equal(t, tc.transition+"/"+tc.alertType+": note", b.Notes, []string{tc.note})
	}

	// Test Notifications: [TEST] among the tags the title starts with.
	for title, test := range map[string]bool{
		"[TEST] [Triggered on {host:vpn-gw01}] Host is Down": true,
		"[P2] [Triggered] [TEST] Test Monitor":               true,
		"[test] Host is Down":                                true,
		"[Triggered on {host:vpn-gw01}] Host is Down":        false,
		"Host [TEST] is Down":                                false,
		"[Triggered on {host:vpn-gw01} Host is Down":         false,
	} {
		b := parse(t, ParseDatadog, "application/json", datadogBody(map[string]string{"$ALERT_TRANSITION": "Triggered", "$ALERT_TYPE": "error", "$ALERT_TITLE": title}))
		equal(t, title, b.Test, test)
		equal(t, title+": alerts", len(b.Alerts) == 0, test)
	}
}

// ------------------------------------------------------------------ providers

func TestProviders(t *testing.T) {
	seen := map[string]bool{}
	list := Providers()
	equal(t, "all", len(list), len(providers))
	equal(t, "generic last", list[len(list)-1].ID, Generic)
	for _, p := range list {
		if p.ID == "" || p.Name == "" || p.Parse == nil || p.Service == "" || len(p.Steps) == 0 || seen[p.ID] {
			t.Errorf("provider %q is incomplete or duplicate", p.ID)
		}
		seen[p.ID] = true
		if (p.Example == "") != (p.ExampleLabel == "") {
			t.Errorf("provider %q: example without label or label without example", p.ID)
		}
		if Lookup(p.ID) != p {
			t.Errorf("Lookup(%q)", p.ID)
		}
	}
	if Lookup("nonsense") != nil {
		t.Error("Lookup of an unknown id")
	}
	equal(t, "expand", Expand("url: '{url}'", "https://s/hooks/t"), "url: 'https://s/hooks/t'")
}

// The request bodies the setup instructions give for tools that send the
// generic format are understood, with the tools' variables filled in.
func TestProviderExamples(t *testing.T) {
	for _, tc := range []struct {
		id, contentType string
		vars            map[string]string
		want            Alert
	}{
		{"datadog", "application/json", map[string]string{
			"$AGGREG_KEY": "9bd4ac313a4d1e8fae2482df7b77628", "$ALERT_TRANSITION": "Re-Triggered", "$ALERT_TYPE": "error", "$TAGS[statuspage_service]": "",
			"$TAGS[service]": "vpn", "$HOSTNAME": "vpn-gw01", "$ALERT_TITLE": "[Triggered on {host:vpn-gw01}] Host is Down", "$TEXT_ONLY_MSG": "No data",
		}, Alert{Key: "9bd4ac313a4d1e8fae2482df7b77628", Firing: true, Targets: []string{"vpn", "vpn-gw01"},
			Title: "[Triggered on {host:vpn-gw01}] Host is Down", Message: "No data", Severity: "critical"}},
		{"datadog", "application/json", map[string]string{
			"$AGGREG_KEY": "9bd4ac313a4d1e8fae2482df7b77628", "$ALERT_TRANSITION": "Renotify", "$ALERT_TYPE": "warning", "$TAGS[statuspage_service]": "VPN",
			"$TAGS[service]": "vpn", "$HOSTNAME": "vpn-gw01", "$ALERT_TITLE": "[Warn on {host:vpn-gw01}] Packet loss", "$TEXT_ONLY_MSG": "",
		}, Alert{Key: "9bd4ac313a4d1e8fae2482df7b77628", Firing: true, Targets: []string{"VPN", "vpn", "vpn-gw01"},
			Title: "[Warn on {host:vpn-gw01}] Packet loss", Severity: "warning"}},
		{"uptimerobot", "application/json", map[string]string{
			"*monitorID*": "778899", "*alertTypeFriendlyName*": "Down", "*monitorFriendlyName*": "VPN", "*alertDetails*": "Connection Timeout",
		}, Alert{Key: "778899", Firing: true, Targets: []string{"VPN"}, Title: "VPN", Message: "Connection Timeout"}},
		{"prtg", "application/x-www-form-urlencoded", map[string]string{
			"%sensorid": "2041", "%laststatus": "Down", "%device": "vpn-gw01", "%group": "Network", "%name": "Ping", "%message": "Request%20timed%20out",
		}, Alert{Key: "2041", Firing: true, Targets: []string{"vpn-gw01", "Network"}, Title: "vpn-gw01: Ping", Message: "Request timed out"}},
	} {
		p := Lookup(tc.id)
		body := p.Example
		for k, v := range tc.vars {
			body = strings.ReplaceAll(body, k, v)
		}
		b := parse(t, p.Parse, tc.contentType, body)
		equal(t, tc.id, b.Alerts, []Alert{tc.want})
	}

	// Zabbix: the body its script sends.
	b := parse(t, Lookup("zabbix").Parse, "application/json",
		`{"key":"4711","status":"resolved","service":["{EVENT.TAGS.statuspage_service}","vpn-gw01"],"title":"Unavailable by ICMP ping","severity":"High"}`)
	equal(t, "zabbix", b.Alerts, []Alert{{Key: "4711", Targets: []string{"{EVENT.TAGS.statuspage_service}", "vpn-gw01"},
		Title: "Unavailable by ICMP ping", Severity: "High"}})

	// Nagios: the bodies curl sends for the commands of the example, with
	// the macros filled in; it tries again for a while within Nagios'
	// notification timeout (30 s): at most 15 s of retries, 10 s per try.
	nagios := Lookup("nagios").Example
	for _, want := range []string{"-m 10 ", "--retry 4 ", "--retry-connrefused ", "--retry-max-time 15 "} {
		if strings.Count(nagios, want) != 2 {
			t.Errorf("nagios: both commands should have %q", want)
		}
	}
	macros := strings.NewReplacer("$HOSTNAME$", "vpn-gw01", "$SERVICEDESC$", "PING", "$SERVICESTATE$", "CRITICAL", "$LASTSERVICEOK$", "1794823323",
		"$SERVICEOUTPUT$", "PING CRITICAL - Packet loss = 100%", "$HOSTSTATE$", "DOWN", "$LASTHOSTUP$", "0", "$HOSTOUTPUT$", "CRITICAL - 10.0.0.1: rta nan, lost 100%")
	commands := strings.Split(nagios, "define command")
	equal(t, "nagios commands", len(commands), 3)
	for i, want := range []Alert{
		{Key: "vpn-gw01/PING", Firing: true, Targets: []string{"PING", "vpn-gw01"}, Title: "PING on vpn-gw01",
			Message: "PING CRITICAL - Packet loss = 100%", Severity: "critical", StartsAt: time.Date(2026, 11, 16, 10, 2, 3, 0, time.UTC)},
		{Key: "vpn-gw01", Firing: true, Targets: []string{"vpn-gw01"}, Title: "vpn-gw01", Message: "CRITICAL - 10.0.0.1: rta nan, lost 100%"}, // never up: no start
	} {
		b = parse(t, Lookup("nagios").Parse, "application/x-www-form-urlencoded", curlFormBody(t, macros.Replace(commands[i+1])))
		equal(t, "nagios", b.Alerts, []Alert{want})
	}

	// Alertmanager: a webhook of an existing receiver; the routes stay.
	am := Lookup("alertmanager").Example
	for _, want := range []string{"receivers:\n", "    webhook_configs:\n      - url: '{url}'\n        send_resolved: true"} {
		if !strings.Contains(am, want) {
			t.Errorf("alertmanager: the example lacks %q", want)
		}
	}
	if strings.Contains(am, "routes:") || strings.Contains(am, "continue:") {
		t.Error("alertmanager: the example should not change the routes")
	}

	// The generic example; PowerShell sends UTF-8 only with the charset.
	b = parse(t, ParseGeneric, "application/json", `{"service": "VPN", "status": "down", "title": "VPN gateway unreachable"}`)
	equal(t, "generic", b.Alerts, []Alert{{Key: "service:vpn", Firing: true, Targets: []string{"VPN"}, Title: "VPN gateway unreachable"}})
	if !strings.Contains(Lookup(Generic).Example, "-ContentType 'application/json; charset=utf-8'") {
		t.Error("generic: PowerShell must send the charset (Windows PowerShell 5.1 encodes the body as ISO-8859-1 without it)")
	}
}

// Facts of the tools that the setup steps must state (each was missing
// once).
func TestProviderSteps(t *testing.T) {
	for id, wants := range map[string][]string{
		// the example's curl option needs curl 7.52
		"nagios": {"--retry-connrefused", "curl 7.52 or later"},
		// the Down trigger sends nothing when an acknowledged alarm comes up
		"prtg": {"“When sensor status is Up for at least … seconds, perform” the same template", "Down (Acknowledged)"},
		// the recovery of a resource is a Resolved event
		"azuremonitor": {"at least Active and Resolved", "Available among the current resource statuses"},
		// 1.23 restarts the retries; leaving a maintenance while down is sent
		"uptimekuma": {"Uptime Kuma 1.23", "Retries 0", "still down after the maintenance is reported down again"},
	} {
		steps := strings.Join(Lookup(id).Steps, "\n")
		for _, want := range wants {
			if !strings.Contains(steps, want) {
				t.Errorf("%s: the steps should contain %q", id, want)
			}
		}
	}
}

// curlFormBody returns the form body that a curl command line sends with
// its --data-urlencode 'name=content' arguments.
func curlFormBody(t *testing.T, command string) string {
	t.Helper()
	var fields []string
	for _, arg := range strings.Split(command, "--data-urlencode '")[1:] {
		arg, _, _ = strings.Cut(arg, "'")
		name, content, ok := strings.Cut(arg, "=")
		if !ok {
			t.Fatalf("curl argument %q", arg)
		}
		fields = append(fields, name+"="+url.QueryEscape(content))
	}
	return strings.Join(fields, "&")
}

func TestRedactPath(t *testing.T) {
	for in, want := range map[string]string{
		"/hooks/secret":        "/hooks/…",
		"/hooks":               "/hooks",
		"/hooks/":              "/hooks/…",
		"//hooks/secret":       "//hooks/…",
		"/HOOKS/secret":        "/HOOKS/…",
		"/prefix/hooks/secret": "/prefix/hooks/…",
		"/hooks;x/secret":      "/hooks;x/…",
		"/hooks/secret/more":   "/hooks/…",
		"/admin/monitoring/1":  "/admin/monitoring/1",
		"/":                    "/",
		"":                     "",
		"/static/app.js":       "/static/app.js",
	} {
		if got := RedactPath(in); got != want {
			t.Errorf("RedactPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapLines(t *testing.T) {
	lines := make([]string, maxSummaryLines+50)
	for i := range lines {
		lines[i] = "line"
	}
	got := capLines(lines)
	equal(t, "count", len(got), maxSummaryLines)
	equal(t, "last", got[len(got)-1], "… and 51 more lines.")
	equal(t, "short", capLines([]string{"a", "b"}), []string{"a", "b"})
	full := make([]string, maxSummaryLines)
	equal(t, "exactly the limit", len(capLines(full)), maxSummaryLines)
}
