package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/groupnames"
	"github.com/szilvasidave/status-page-tool/internal/monitoring"
	"github.com/szilvasidave/status-page-tool/internal/notify"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// monHook posts a webhook request without cookies (like a monitoring tool)
// and returns the status and the JSON answer.
func monHook(t *testing.T, e *testEnv, path, contentType, body string) (int, map[string]any) {
	t.Helper()
	status, out, _ := monHookHeader(t, e, path, contentType, body, nil)
	return status, out
}

// monHookHeader is monHook with request headers, returning the response
// headers too.
func monHookHeader(t *testing.T, e *testEnv, path, contentType, body string, header http.Header) (int, map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.Server.URL+path, strings.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: not JSON: %s", path, b)
	}
	return resp.StatusCode, out, resp.Header
}

// monSecondBrowser returns e with a client of its own (no cookies): another
// person using the same server.
func monSecondBrowser(e *testEnv) *testEnv {
	jar, _ := cookiejar.New(nil)
	c := *e
	c.Client = &http.Client{Jar: jar, CheckRedirect: e.Client.CheckRedirect}
	return &c
}

// monIntegration creates an integration directly in the store and returns
// its webhook path.
func monIntegration(t *testing.T, e *testEnv, provider string, notifyOn bool) (*store.Integration, string) {
	t.Helper()
	token, hash := monitoring.NewToken()
	in := &store.Integration{Name: "Prometheus", Provider: provider, Notify: notifyOn}
	if err := e.Store.CreateIntegration(context.Background(), in, hash); err != nil {
		t.Fatal(err)
	}
	return in, monitoring.Path + "/" + token
}

var monURLPattern = regexp.MustCompile(`<code class="code-box" id="webhook-url">([^<]+)</code>`)

func TestMonitoringAdminPages(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()

	r := e.get("/admin/monitoring")
	equal200(t, r)
	mustContain(t, r.Body, `<a href="/admin/monitoring" aria-current="page">Monitoring</a>`, "No integrations yet.")

	r = e.get("/admin/monitoring/new")
	equal200(t, r)
	mustContain(t, r.Body, `<option value="">Choose…</option>`, `<option value="alertmanager">Prometheus Alertmanager</option>`,
		`<option value="generic">Other tool or script</option>`, `By the alert's severity (recommended)`, `name="notify" aria-describedby="notify_help" checked`)
	if strings.Contains(r.Body, `name="paused"`) {
		t.Error("the new form offers the pause")
	}

	// validation
	r = e.post("/admin/monitoring", url.Values{"name": {""}, "provider": {"nonsense"}, "impact": {"maintenance"}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid: %d", r.Status)
	}
	mustContain(t, r.Body, "Enter a name", "Choose the monitoring tool.", "Choose an impact.")

	// create: the URL is shown once, to the administrator who created it
	r = e.post("/admin/monitoring", url.Values{"name": {"Prometheus prod"}, "provider": {"alertmanager"}, "impact": {""}, "notify": {"on"}})
	if r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Location, "/admin/monitoring/1?new-url=") || !strings.HasSuffix(r.Location, "#webhook") {
		t.Fatalf("create: %d %s", r.Status, r.Location)
	}
	page := e.get(strings.TrimSuffix(r.Location, "#webhook"))
	equal200(t, page)
	m := monURLPattern.FindStringSubmatch(page.Body)
	if m == nil {
		t.Fatalf("no webhook URL shown: %s", page.Body)
	}
	hookURL := m[1]
	if !strings.HasPrefix(hookURL, e.Server.URL+"/hooks/") {
		t.Fatalf("webhook URL %q", hookURL)
	}
	mustContain(t, page.Body, "Integration created.", "Set up Prometheus Alertmanager", "alertmanager.yml",
		"url: &#39;"+hookURL+"&#39;", "<details class=\"disclosure\" open>", "No alert of this integration is firing.", "No requests yet.")
	again := e.get(strings.TrimSuffix(r.Location, "#webhook"))
	if strings.Contains(again.Body, hookURL) {
		t.Error("the webhook URL is shown a second time")
	}
	mustContain(t, again.Body, "The webhook URL was generated", "url: &#39;"+e.Server.URL+"/hooks/&lt;token&gt;&#39;")

	// the URL works; the request is listed on the page
	status, body := monHook(t, e, strings.TrimPrefix(hookURL, e.Server.URL), "application/json", `{"alerts": []}`)
	if status != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("hook: %d %v", status, body)
	}
	r = e.get("/admin/monitoring/1")
	mustContain(t, r.Body, `<span class="badge event-unchanged">No change</span>`, "The request contained no alerts.",
		`<details class="disclosure">`, "{&#34;alerts&#34;: []}")
	r = e.get("/admin/monitoring")
	mustContain(t, r.Body, `<a href="/admin/monitoring/1">Prometheus prod</a>`, "<td>Prometheus Alertmanager</td>")

	// save: pause and another tool
	r = e.post("/admin/monitoring/1", url.Values{"name": {"Grafana"}, "provider": {"grafana"}, "impact": {"degraded"}, "paused": {"on"}})
	if r.Status != http.StatusSeeOther || r.Location != "/admin/monitoring/1" {
		t.Fatalf("save: %d %s", r.Status, r.Location)
	}
	r = e.get("/admin/monitoring/1")
	mustContain(t, r.Body, "The integration “Grafana” has been saved.", "This integration is paused", "Set up Grafana Alerting",
		`<option value="degraded" selected>Always: Degraded performance</option>`, `name="paused" aria-describedby="paused_help" checked`)
	in, err := e.Store.GetIntegration(context.Background(), 1)
	if err != nil || in.Provider != "grafana" || in.Impact != store.StatusDegraded || in.Notify || !in.Paused {
		t.Fatalf("saved: %+v %v", in, err)
	}
	r = e.post("/admin/monitoring/1", url.Values{"name": {""}, "provider": {"grafana"}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid save: %d", r.Status)
	}
	mustContain(t, r.Body, "<h1>Grafana</h1>", "Enter a name")

	// a new URL replaces the old one
	r = e.post("/admin/monitoring/1/url", nil)
	if r.Status != http.StatusSeeOther || !strings.Contains(r.Location, "?new-url=") {
		t.Fatalf("new URL: %d %s", r.Status, r.Location)
	}
	page = e.get(strings.TrimSuffix(r.Location, "#webhook"))
	mustContain(t, page.Body, "New webhook URL generated.", "The previous URL no longer works.")
	newURL := monURLPattern.FindStringSubmatch(page.Body)[1]
	if newURL == hookURL {
		t.Fatal("the URL did not change")
	}
	if status, _ := monHook(t, e, strings.TrimPrefix(hookURL, e.Server.URL), "application/json", `{}`); status != http.StatusUnauthorized {
		t.Fatalf("old URL: %d", status)
	}
	if status, _ := monHook(t, e, strings.TrimPrefix(newURL, e.Server.URL), "application/json", `{"alerts": []}`); status != http.StatusOK {
		t.Fatalf("new URL: %d", status)
	}

	// editors have no access
	editor := monSecondBrowser(e)
	editor.loginAs("editor@example.com", store.RoleEditor)
	if r := editor.get("/admin/monitoring/1"); r.Status != http.StatusForbidden {
		t.Fatalf("editor: %d", r.Status)
	}

	// delete
	r = e.post("/admin/monitoring/1/delete", nil)
	if r.Status != http.StatusSeeOther || r.Location != "/admin/monitoring" {
		t.Fatalf("delete: %d %s", r.Status, r.Location)
	}
	mustContain(t, e.get("/admin/monitoring").Body, "The integration “Grafana” has been deleted.")
	if r := e.get("/admin/monitoring/1"); r.Status != http.StatusNotFound {
		t.Fatalf("deleted: %d", r.Status)
	}
	if status, _ := monHook(t, e, strings.TrimPrefix(newURL, e.Server.URL), "application/json", `{}`); status != http.StatusUnauthorized {
		t.Fatalf("URL of a deleted integration: %d", status)
	}
}

func equal200(t *testing.T, r Response) {
	t.Helper()
	if r.Status != http.StatusOK {
		t.Fatalf("status %d: %s", r.Status, r.Body)
	}
}

// The URL slot of another administrator is not shown.
func TestMonitoringURLShownOnlyToItsCreator(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	r := e.post("/admin/monitoring", url.Values{"name": {"Kuma"}, "provider": {"uptimekuma"}})
	other := monSecondBrowser(e)
	other.loginAs("other-admin@example.com", store.RoleAdmin)
	page := other.get(strings.TrimSuffix(r.Location, "#webhook"))
	equal200(t, page)
	if monURLPattern.MatchString(page.Body) {
		t.Fatal("another administrator saw the new webhook URL")
	}
	if !monURLPattern.MatchString(e.get(strings.TrimSuffix(r.Location, "#webhook")).Body) {
		t.Fatal("the creator did not see it afterwards")
	}
}

func TestMonitoringEndToEnd(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()

	// A service with a name in monitoring tools, from the service form.
	r := e.post("/admin/services", url.Values{"name": {"VPN"}, "category": {""}, "sort_order": {"0"},
		"monitor_names": {"vpn01:9100\r\n  VPN01:9100 \r\n\r\ngw  2"}})
	if r.Status != http.StatusSeeOther {
		t.Fatalf("create service: %d %s", r.Status, r.Body)
	}
	svc, err := e.Store.GetService(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(svc.MonitorNames, "|") != "vpn01:9100|gw 2" {
		t.Fatalf("monitor names: %q", svc.MonitorNames)
	}
	mustContain(t, e.get("/admin/services/1").Body, ">vpn01:9100\ngw 2</textarea>")

	// Someone is notified about the service.
	u := e.loginAs("jane@example.com", store.RoleViewer)
	e.loginAdmin()
	g, err := e.Store.CreateGroup(ctx, "ops", "Operations")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SetUserGroups(ctx, u.ID, []store.GroupRef{{ExternalID: "ops"}}); err != nil {
		t.Fatal(err)
	}
	svc.GroupIDs = []int64{g.ID}
	if err := e.Store.UpdateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	st, _ := e.Store.GetSettings(ctx)
	st.MailMethod, st.SMTPHost, st.SMTPFrom = store.MailSMTP, "smtp.example.com", "status@example.com"
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	_, hook := monIntegration(t, e, "alertmanager", true)
	firing := `{"version": "4", "status": "firing", "alerts": [{"status": "firing",
		"labels": {"alertname": "InstanceDown", "instance": "vpn01:9100", "severity": "warning"},
		"annotations": {"summary": "VPN gateway slow", "description": "Latency above 2 s"},
		"startsAt": "2026-01-01T00:00:00Z", "fingerprint": "f1"}]}`
	status, body := monHook(t, e, hook, "application/json", firing)
	if status != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("firing: %d %v", status, body)
	}

	// The status page, the API, the incident and the editors see it.
	r = e.get("/api/status")
	var api publicAPIStatus
	if err := json.Unmarshal([]byte(r.Body), &api); err != nil {
		t.Fatal(err)
	}
	if api.Status != store.StatusDegraded || len(api.Incidents) != 1 || api.Incidents[0].Title != "VPN gateway slow" {
		t.Fatalf("api: %+v", api)
	}
	mustContain(t, e.get("/").Body, "VPN gateway slow")
	mustContain(t, e.get("/incidents/1").Body, "Detected by monitoring", "Monitoring reports a problem: “VPN gateway slow”.<br><br>Latency above 2 s",
		"by Prometheus (monitoring)")
	mustContain(t, e.get("/manage").Body, `title="Opened by monitoring">Monitoring</span>`)
	mustContain(t, e.get("/manage/incidents/1").Body, "This incident was opened by monitoring and is resolved automatically",
		"<li>VPN gateway slow – VPN, Degraded performance, firing since")
	mustContain(t, e.get("/admin/monitoring/1").Body, `<span class="badge event-changed">Incidents changed</span>`,
		"“VPN gateway slow” (firing): opened incident #1 of VPN.", `<a href="/manage/incidents/1">#1</a>`)

	// E-mail to the members of the service's groups.
	mails, err := e.Store.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mails) != 1 || mails[0].Email != "jane@example.com" || !strings.Contains(mails[0].Subject, "VPN gateway slow") {
		t.Fatalf("e-mails: %+v", mails)
	}

	// Resolved: operational again, with a second e-mail.
	resolved := strings.Replace(firing, `"status": "firing",
		"labels"`, `"status": "resolved",
		"labels"`, 1)
	if status, body := monHook(t, e, hook, "application/json", resolved); status != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("resolved: %d %v", status, body)
	}
	if err := json.Unmarshal([]byte(e.get("/api/status").Body), &api); err != nil {
		t.Fatal(err)
	}
	if api.Status != store.StatusOperational || len(api.Incidents) != 0 {
		t.Fatalf("api after resolution: %+v", api)
	}
	mails, _ = e.Store.ListNotifications(ctx, 10)
	if len(mails) != 2 {
		t.Fatalf("e-mails after resolution: %d", len(mails))
	}
	mustContain(t, e.get("/manage/incidents/1").Body, "This incident was opened by monitoring.")
}

func TestMonitoringServiceFormNames(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	mustContain(t, e.get("/admin/services/new").Body, `<textarea id="monitor_names" name="monitor_names"`)
	var names strings.Builder
	for i := range store.MaxMonitorNames + 1 {
		fmt.Fprintf(&names, "name %d\n", i)
	}
	r := e.post("/admin/services", url.Values{"name": {"VPN"}, "monitor_names": {names.String()}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("too many names: %d", r.Status)
	}
	mustContain(t, r.Body, "Enter at most 50 names.")
	r = e.post("/admin/services", url.Values{"name": {"VPN"}, "monitor_names": {strings.Repeat("x", store.MaxMonitorNameLength+1)}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("long name: %d", r.Status)
	}
	mustContain(t, r.Body, "A name is too long (at most 200 characters each).")
}

// The webhook passes the middleware without cookies or CSRF token, also
// before the setup is done and with a foreign origin, and its token never
// reaches the request log.
func TestMonitoringWebhookMiddleware(t *testing.T) {
	var logBuf monSyncBuffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	base := newTestEnvNoAdmin(t) // setup pending: no local administrator yet
	cfg := &config.Config{Listen: ":0", DBDriver: config.DriverSQLite, DBDSN: "statuspage.db", SecretKey: config.NewSecretKey()}
	h, err := NewHandler(Deps{Config: cfg, DataDir: base.DataDir, Store: base.Store, Auth: base.Auth,
		Notifier: notify.New(base.Store, log), GroupNames: groupnames.New(base.Store, log), Version: "test", Log: log, SetupToken: "x"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e := &testEnv{t: t, DataDir: base.DataDir, Store: base.Store, Auth: base.Auth, Server: srv, Client: base.Client}

	if r := e.get("/admin/monitoring"); r.Status != http.StatusSeeOther || r.Location != "/setup" {
		t.Fatalf("pages wait for the setup: %d %s", r.Status, r.Location)
	}
	if err := e.Store.CreateService(context.Background(), &store.Service{Name: "VPN"}); err != nil {
		t.Fatal(err)
	}
	_, hook := monIntegration(t, e, monitoring.Generic, false)
	status, body, header := monHookHeader(t, e, hook, "application/x-www-form-urlencoded", "service=VPN&status=down",
		http.Header{"Origin": {"https://evil.example"}})
	if status != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("hook: %d %v", status, body)
	}
	if c := header.Get("Set-Cookie"); c != "" {
		t.Fatalf("the webhook set a cookie: %s", c)
	}
	if status, body := monHook(t, e, "/hooks/unknown-token", "application/json", `{}`); status != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d %v", status, body)
	}

	// Paths that the routes do not take as a webhook do not log the token
	// either (the middleware refuses them as cross-site requests).
	token := strings.TrimPrefix(hook, monitoring.Path+"/")
	for _, odd := range []string{"//hooks/" + token, "/HOOKS/" + token, "/prefix/hooks/" + token} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+odd, strings.NewReader("service=VPN&status=down"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	logs := logBuf.String()
	if strings.Contains(logs, token) || strings.Contains(logs, "unknown-token") {
		t.Fatalf("a token was logged:\n%s", logs)
	}
	mustContain(t, logs, "path=/hooks/…", "monitoring alerts received")
}

// monSyncBuffer is a bytes.Buffer that is safe for concurrent writes.
type monSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *monSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *monSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
