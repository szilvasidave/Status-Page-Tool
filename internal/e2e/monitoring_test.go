package e2e

import (
	"context"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakesmtp"
)

// TestMonitoringJourney: a monitoring tool keeps the status page up to date
// in the running application. The administrator gives the service its name
// in Prometheus, adds an integration and copies its webhook URL; an alert
// of Alertmanager opens an incident (with e-mails to the service's groups),
// and its resolution resolves it.
func TestMonitoringJourney(t *testing.T) {
	smtp := fakesmtp.Start(t)
	logs := &logBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("application log:\n%s", logs)
		}
	})
	dataDir, addr := t.TempDir(), freeAddr(t)
	erp := graphMailPrepare(t, dataDir, "http://"+addr, func(st *store.Store) {
		ctx := context.Background()
		s, err := st.GetSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		s.MailMethod, s.SMTPHost, s.SMTPPort, s.SMTPSecurity, s.SMTPFrom = store.MailSMTP, smtp.Host(), smtp.Port(), "none", "IT Status <"+mailFrom+">"
		if err := st.SaveSettings(ctx, s); err != nil {
			t.Fatal(err)
		}
	})
	base := startApp(t, dataDir, addr, logs).URL

	admin := newBrowser(t, "admin")
	p := admin.get(base+"/login?local=1&next=%2Fadmin%2Fmonitoring").expectStatus(200).
		form("/login").set("username", "admin").set("password", adminPW).post()
	p.expectRedirect("/admin/monitoring")
	p.follow().expectStatus(200).expectText("No integrations yet.")

	// ERP's database server is called erp-db01:9100 in Prometheus.
	admin.get(base+"/admin/services/"+erp).expectStatus(200).form("/admin/services/"+erp).
		set("monitor_names", "erp-db01:9100").post().expectRedirect("/admin/services")

	// The integration, with its webhook URL shown once.
	p = admin.get(base+"/admin/monitoring/new").expectStatus(200).form("/admin/monitoring").
		set("name", "Prometheus").choose("provider", "alertmanager").post()
	p = p.follow().expectStatus(200).expectText("Integration created.", "Set up Prometheus Alertmanager")
	m := regexp.MustCompile(`id="webhook-url">([^<]+)<`).FindStringSubmatch(p.Body)
	if m == nil {
		t.Fatalf("no webhook URL on the page:\n%s", truncate(p.text(), 2000))
	}
	hook := html.UnescapeString(m[1])
	if !strings.HasPrefix(hook, base+"/hooks/") {
		t.Fatalf("webhook URL %q", hook)
	}

	// Alertmanager reports the database down.
	firing := `{"version": "4", "status": "firing", "receiver": "status-page", "alerts": [{"status": "firing",
		"labels": {"alertname": "MySQLDown", "instance": "erp-db01:9100", "job": "mysql", "severity": "critical"},
		"annotations": {"summary": "ERP database unreachable", "description": "The ERP database does not answer."},
		"startsAt": "2026-11-16T10:00:00Z", "endsAt": "0001-01-01T00:00:00Z", "fingerprint": "6e1f0c5d2b3a4978"}]}`
	sendAlert(t, hook, firing)

	anon := newBrowser(t, "anonymous")
	var status struct {
		Status    string `json:"status"`
		Services  []struct{ Name, Status string }
		Incidents []struct{ Title string }
	}
	anon.get(base + "/api/status").expectStatus(200).json(&status)
	if status.Status != "major_outage" || len(status.Incidents) != 1 || status.Incidents[0].Title != "ERP database unreachable" {
		t.Fatalf("status after the alert: %+v", status)
	}
	msgs := smtp.WaitFor(2, mailWait)
	to := map[string]bool{}
	for _, msg := range msgs {
		for _, rcpt := range msg.To {
			to[rcpt] = true
		}
		if !strings.Contains(msg.Subject, "ERP database unreachable") {
			t.Errorf("subject %q", msg.Subject)
		}
	}
	if !to["alice@contoso.example"] || !to["bob@contoso.example"] {
		t.Errorf("e-mails to %v", to)
	}
	smtp.Reset()

	// The resolution resolves the incident.
	sendAlert(t, hook, strings.Replace(firing, `[{"status": "firing",`, `[{"status": "resolved",`, 1))
	anon.get(base + "/api/status").expectStatus(200).json(&status)
	if status.Status != "operational" || len(status.Incidents) != 0 {
		t.Fatalf("status after the resolution: %+v", status)
	}
	smtp.WaitFor(2, mailWait)
	anon.get(base+"/incidents/1").expectStatus(200).expectText("Resolved", "Detected by monitoring",
		"Resolved: monitoring reports that “ERP database unreachable” is resolved.")
	admin.get(base+"/admin/monitoring/1").expectStatus(200).expectText("Incidents changed",
		"“ERP database unreachable” (firing): opened incident #1 of ERP.", "“ERP database unreachable” (resolved): resolved incident #1 of ERP.")
	if strings.Contains(logs.String(), strings.TrimPrefix(hook, base+"/hooks/")) {
		t.Error("the token of the webhook URL is in the log")
	}
}

func sendAlert(t *testing.T, hook, body string) {
	t.Helper()
	resp, err := http.Post(hook, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook: HTTP %d: %s", resp.StatusCode, b)
	}
}
