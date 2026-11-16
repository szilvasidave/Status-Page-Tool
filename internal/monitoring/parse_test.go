package monitoring

import (
	"errors"
	"net/url"
	"reflect"
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

func kumaPayloadJSON(status int, msg string) string {
	return `{
  "heartbeat": {"monitorID": 7, "status": ` + string(rune('0'+status)) + `, "msg": "` + msg + `", "time": "2026-11-16 10:02:00.123",
                "timezone": "Europe/Berlin", "timezoneOffset": "+01:00", "localDateTime": "2026-11-16 11:02:00", "important": true,
                "duration": 60, "retries": 0, "ping": null},
  "monitor": {"id": 7, "name": "VPN gateway", "type": "http", "url": "https://vpn.example.com", "hostname": null, "port": null,
              "pathName": "Network / VPN gateway", "tags": [{"tag_id": 1, "monitor_id": 7, "value": "VPN", "name": "statuspage_service", "color": "#059669"}]},
  "msg": "[VPN gateway] [🔴 Down] ` + msg + `"
}`
}

func TestParseUptimeKuma(t *testing.T) {
	b := parse(t, ParseUptimeKuma, "application/json", kumaPayloadJSON(0, "connect ECONNREFUSED"))
	equal(t, "down", b.Alerts, []Alert{{
		Key: "monitor:7", Firing: true, Targets: []string{"VPN", "", "VPN gateway", "", "https://vpn.example.com"},
		Title: "VPN gateway is down", Message: "connect ECONNREFUSED",
	}})
	b = parse(t, ParseUptimeKuma, "application/json", kumaPayloadJSON(1, "200 - OK"))
	equal(t, "up", b.Alerts[0].Firing, false)
	equal(t, "up key", b.Alerts[0].Key, "monitor:7")
	b = parse(t, ParseUptimeKuma, "application/json", kumaPayloadJSON(2, "timeout"))
	equal(t, "pending", len(b.Alerts), 0)
	equal(t, "pending note", b.Notes, []string{"“VPN gateway” is pending: nothing to change."})

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

	parseError(t, ParseAzureMonitor, `{"schemaId": "Microsoft.Insights/activityLogs", "data": {}}`, "common alert schema")
	parseError(t, ParseAzureMonitor, `{"status": "Activated", "context": {}}`, "common alert schema")
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
			"$AGGREG_KEY": "9bd4ac313a4d1e8fae2482df7b77628", "$ALERT_TRANSITION": "Re-Triggered", "$TAGS[statuspage_service]": "",
			"$TAGS[service]": "vpn", "$HOSTNAME": "vpn-gw01", "$ALERT_TITLE": "[Triggered on {host:vpn-gw01}] Host is Down", "$TEXT_ONLY_MSG": "No data",
		}, Alert{Key: "9bd4ac313a4d1e8fae2482df7b77628", Firing: true, Targets: []string{"vpn", "vpn-gw01"},
			Title: "[Triggered on {host:vpn-gw01}] Host is Down", Message: "No data"}},
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

	// Nagios: what curl --data-urlencode sends.
	b = parse(t, Lookup("nagios").Parse, "application/x-www-form-urlencoded",
		"key=vpn-gw01%2FPING&status=CRITICAL&service=PING&service=vpn-gw01&title=PING+on+vpn-gw01&message=PING+CRITICAL+-+Packet+loss+%3D+100%25")
	equal(t, "nagios", b.Alerts, []Alert{{Key: "vpn-gw01/PING", Firing: true, Targets: []string{"PING", "vpn-gw01"},
		Title: "PING on vpn-gw01", Message: "PING CRITICAL - Packet loss = 100%", Severity: "critical"}})

	// The generic example.
	b = parse(t, ParseGeneric, "application/json", `{"service": "VPN", "status": "down", "title": "VPN gateway unreachable"}`)
	equal(t, "generic", b.Alerts, []Alert{{Key: "service:vpn", Firing: true, Targets: []string{"VPN"}, Title: "VPN gateway unreachable"}})
}
