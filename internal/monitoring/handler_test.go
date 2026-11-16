package monitoring

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

var ctx = context.Background()

// fakeNotifier records the e-mails it was asked for.
type fakeNotifier struct {
	mu    sync.Mutex
	calls []string
}

// IncidentUpdateNotice records "created <incident>" or "updated <incident>".
func (n *fakeNotifier) IncidentUpdateNotice(_ context.Context, id, _ int64, created bool) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	what := "updated "
	if created {
		what = "created "
	}
	n.calls = append(n.calls, what+strconv.FormatInt(id, 10))
	return 1, nil
}

// hookEnv is the handler on a local server, backed by SQLite (and
// PostgreSQL when STATUSPAGE_TEST_PG_DSN is set).
type hookEnv struct {
	t        *testing.T
	store    *store.Store
	handler  *Handler
	notifier *fakeNotifier
	srv      *httptest.Server
}

func forEachDB(t *testing.T, fn func(t *testing.T, e *hookEnv)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		d, err := db.Open(ctx, config.DriverSQLite, filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		fn(t, newHookEnv(t, d))
	})
	if dsn := os.Getenv("STATUSPAGE_TEST_PG_DSN"); dsn != "" {
		t.Run("postgres", func(t *testing.T) {
			d, err := db.Open(ctx, config.DriverPostgres, dsn)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.ExecContext(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
				t.Fatal(err)
			}
			fn(t, newHookEnv(t, d))
		})
	}
}

func newHookEnv(t *testing.T, d *db.DB) *hookEnv {
	t.Helper()
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	st := store.New(d, []byte("0123456789abcdef0123456789abcdef"))
	n := &fakeNotifier{}
	h := New(st, n, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	mux.Handle(Path, h)
	mux.Handle(Path+"/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &hookEnv{t: t, store: st, handler: h, notifier: n, srv: srv}
}

// integration creates an integration and returns it with its token.
func (e *hookEnv) integration(provider string, mod func(*store.Integration)) (*store.Integration, string) {
	e.t.Helper()
	token, hash := NewToken()
	in := &store.Integration{Name: "Monitoring " + provider, Provider: provider, Notify: true}
	if mod != nil {
		mod(in)
	}
	if err := e.store.CreateIntegration(ctx, in, hash); err != nil {
		e.t.Fatal(err)
	}
	return in, token
}

type hookResponse struct {
	status int
	header http.Header
	body   response
}

func (e *hookEnv) post(path, contentType, body string, header ...string) hookResponse {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	return e.do(req)
}

func (e *hookEnv) do(req *http.Request) hookResponse {
	e.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var r hookResponse
	r.status, r.header = resp.StatusCode, resp.Header
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &r.body); err != nil {
		e.t.Fatalf("response is not JSON: %s", b)
	}
	return r
}

func (e *hookEnv) service(name string, monitorNames ...string) *store.Service {
	e.t.Helper()
	svc := &store.Service{Name: name, MonitorNames: monitorNames}
	if err := e.store.CreateService(ctx, svc); err != nil {
		e.t.Fatal(err)
	}
	return svc
}

func (e *hookEnv) events(in *store.Integration) []store.IntegrationEvent {
	e.t.Helper()
	evs, err := e.store.ListIntegrationEvents(ctx, in.ID, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

func (e *hookEnv) openIncidents() []store.Incident {
	e.t.Helper()
	open, err := e.store.ListOpenIncidents(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return open
}

func TestHandlerAlertmanager(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN", "vpn01:9100")
		in, token := e.integration("alertmanager", nil)

		r := e.post(Path+"/"+token, "application/json", alertmanagerPayload)
		equal(t, "status", r.status, http.StatusOK)
		equal(t, "body", r.body, response{Status: "ok", Alerts: 2, Summary: []string{
			"“VPN gateway unreachable” (firing): opened incident #1 of VPN.",
			"“InstanceDown” (resolved): was not firing, nothing to do.",
			"Alert 3: status “pending” is neither firing nor resolved: ignored.",
		}})
		equal(t, "e-mail", e.notifier.calls, []string{"created 1"})
		open := e.openIncidents()
		equal(t, "incident", len(open), 1)
		equal(t, "impact (label severity critical)", open[0].Impact(), store.StatusMajorOutage)
		evs := e.events(in)
		equal(t, "recorded", evs[0].Outcome, store.EventChanged)
		equal(t, "payload recorded", evs[0].Payload, alertmanagerPayload)

		// The same group again, resolved; by bearer token.
		resolved := strings.Replace(alertmanagerPayload, `"status": "firing",
      "labels": {"alertname": "InstanceDown", "instance": "vpn01:9100"`, `"status": "resolved",
      "labels": {"alertname": "InstanceDown", "instance": "vpn01:9100"`, 1)
		r = e.post(Path, "application/json", resolved, "Authorization", "Bearer "+token)
		equal(t, "resolved", r.body.Summary[0], "“VPN gateway unreachable” (resolved): resolved incident #1 of VPN.")
		equal(t, "e-mails", e.notifier.calls, []string{"created 1", "updated 1"})
		equal(t, "no open incident", len(e.openIncidents()), 0)
	})
}

func TestHandlerOutcomes(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN")
		in, token := e.integration(Generic, func(in *store.Integration) { in.Notify = false; in.Impact = store.StatusDegraded })
		url := Path + "/" + token

		// unmatched: names no service
		r := e.post(url, "application/json", `{"service": ["vpn-gw01", "Network"], "status": "down", "title": "Ping"}`)
		equal(t, "unmatched", r.body.Summary, []string{"“Ping” (firing): no service is named “vpn-gw01”, “Network”. Add one of these names to a service's names in monitoring tools (Admin → Services)."})
		equal(t, "outcome", e.events(in)[0].Outcome, store.EventUnmatched)

		// the integration's fixed impact; no e-mails
		r = e.post(url, "application/x-www-form-urlencoded", "service=VPN&status=critical")
		equal(t, "opened", r.body.Summary, []string{"Alert service:vpn (firing): opened incident #1 of VPN."})
		open := e.openIncidents()
		equal(t, "fixed impact", open[0].Impact(), store.StatusDegraded)
		equal(t, "no e-mails", len(e.notifier.calls), 0)

		// a repeat changes nothing
		e.post(url, "", "service=VPN&status=critical")
		equal(t, "unchanged", e.events(in)[0].Outcome, store.EventUnchanged)

		// test message
		r = e.post(url, "application/json", `{"service": "VPN", "status": "test"}`)
		equal(t, "test", r.body.Status, "test")
		equal(t, "test outcome", e.events(in)[0].Outcome, store.EventTest)

		// a request that cannot be read: 400, recorded with its body
		r = e.post(url, "application/json", `{"service": "VPN"}`)
		equal(t, "400", r.status, http.StatusBadRequest)
		equal(t, "error", r.body.Error, "The status is missing: send firing (or problem, down, critical, warning, …) or resolved (or ok, up, recovered, …).")
		evs := e.events(in)
		equal(t, "invalid outcome", evs[0].Outcome, store.EventInvalid)
		equal(t, "invalid payload", evs[0].Payload, `{"service": "VPN"}`)

		// query parameters only: recorded as the payload
		r = e.post(url+"?service=VPN&status=ok", "", "")
		equal(t, "resolved by query", r.body.Summary, []string{"Alert service:vpn (resolved): resolved incident #1 of VPN."})
		equal(t, "query recorded", e.events(in)[0].Payload, "?service=VPN&status=ok")

		// paused: recorded, nothing changes
		in.Paused = true
		if err := e.store.UpdateIntegration(ctx, in); err != nil {
			t.Fatal(err)
		}
		r = e.post(url, "application/json", `{"service": "VPN", "status": "down"}`)
		equal(t, "paused", r.body, response{Status: "paused", Alerts: 1, Summary: []string{"The integration is paused: 1 alert received, nothing was changed."}})
		equal(t, "nothing opened", len(e.openIncidents()), 0)
		equal(t, "paused outcome", e.events(in)[0].Outcome, store.EventPaused)
	})
}

// Form fields can carry invalid UTF-8 and NUL characters, which PostgreSQL
// does not store; a JSON body may start with a byte order mark.
func TestHandlerOddInput(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN")
		in, token := e.integration(Generic, nil)
		r := e.post(Path+"/"+token, "application/x-www-form-urlencoded", "service=VPN&status=down&key=a%00b%FF&title=Gate%00way%FF&message=x%00y%FEz")
		equal(t, "status", r.status, http.StatusOK)
		open := e.openIncidents()
		equal(t, "incident", len(open), 1)
		equal(t, "title", open[0].Title, "VPN: Gate way\uFFFD")
		evs := e.events(in)
		if !strings.Contains(evs[0].Payload, "%00") {
			t.Fatalf("payload: %q", evs[0].Payload)
		}
		// the same key, cleaned the same way, resolves it
		r = e.post(Path+"/"+token, "application/x-www-form-urlencoded", "service=VPN&status=up&key=a%00b%FF")
		equal(t, "resolved", r.body.Summary, []string{"Alert a b\uFFFD (resolved): resolved incident #1 of VPN."})
		// an unknown status with a NUL character is recorded
		r = e.post(Path+"/"+token, "application/x-www-form-urlencoded", "service=VPN&status=no%00pe%FF")
		equal(t, "unknown status", r.status, http.StatusOK)
		if evs := e.events(in); !strings.Contains(evs[0].Summary, "neither a problem nor a recovery") {
			t.Fatalf("summary: %q", evs[0].Summary)
		}
		// byte order mark
		r = e.post(Path+"/"+token, "application/json", "\xef\xbb\xbf"+`{"service": "VPN", "status": "test"}`)
		equal(t, "BOM", r.body.Status, "test")
	})
}

func TestHandlerRejects(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		in, token := e.integration(Generic, nil)

		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+Path+"/"+token, nil)
		r := e.do(req)
		equal(t, "GET", r.status, http.StatusMethodNotAllowed)
		equal(t, "Allow", r.header.Get("Allow"), "POST")

		r = e.post(Path, "application/json", `{}`)
		equal(t, "no token", r.status, http.StatusUnauthorized)
		if !strings.HasPrefix(r.body.Error, "No token") || r.header.Get("WWW-Authenticate") == "" {
			t.Fatalf("no token: %+v", r)
		}
		r = e.post(Path+"/wrong", "application/json", `{}`)
		equal(t, "wrong token", r.status, http.StatusUnauthorized)
		r = e.post(Path+"/"+token+"/x", "application/json", `{}`)
		equal(t, "token with a path after it", r.status, http.StatusUnauthorized)
		r = e.post(Path, "application/json", `{}`, "Authorization", "Basic "+token)
		equal(t, "not a bearer token", r.status, http.StatusUnauthorized)

		big := `{"service": "VPN", "status": "down", "message": "` + strings.Repeat("x", MaxBodyBytes) + `"}`
		r = e.post(Path+"/"+token, "application/json", big)
		equal(t, "too large", r.status, http.StatusRequestEntityTooLarge)
		evs := e.events(in)
		equal(t, "recorded", evs[0].Outcome, store.EventInvalid)
		equal(t, "without payload", evs[0].Payload, "")

		// A replaced token stops working.
		_, hash := NewToken()
		if err := e.store.SetIntegrationToken(ctx, in.ID, hash); err != nil {
			t.Fatal(err)
		}
		r = e.post(Path+"/"+token, "application/json", `{"service": "VPN", "status": "test"}`)
		equal(t, "old token", r.status, http.StatusUnauthorized)
	})
}

func TestHandlerReleaseAfterMaintenance(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		vpn := e.service("VPN")
		in, token := e.integration("uptimekuma", nil)
		now := time.Now()
		m := &store.Maintenance{Title: "Firmware", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Services: []store.ServiceRef{{ID: vpn.ID}}}
		if err := e.store.CreateMaintenance(ctx, m); err != nil {
			t.Fatal(err)
		}
		body := strings.Replace(kumaPayloadJSON(0, "timeout"), `"value": "VPN"`, `"value": "vpn"`, 1)
		r := e.post(Path+"/"+token, "application/json", body)
		equal(t, "suppressed", r.body.Summary, []string{"“VPN gateway is down” (firing): VPN is in maintenance: if the alert still fires when the maintenance has ended, it opens an incident."})

		e.handler.release(ctx)
		equal(t, "still in maintenance", len(e.openIncidents()), 0)

		if _, err := e.store.DB().ExecContext(ctx, `UPDATE maintenances SET ends_at = ? WHERE id = ?`, now.Add(-time.Second).Unix(), m.ID); err != nil {
			t.Fatal(err)
		}
		e.handler.release(ctx)
		equal(t, "opened after the maintenance", len(e.openIncidents()), 1)
		equal(t, "e-mail", e.notifier.calls, []string{"created 1"})
		evs := e.events(in)
		equal(t, "recorded", evs[0].Summary, "“VPN gateway is down” (firing since a maintenance that has ended): opened incident #1 of VPN.")
		equal(t, "outcome", evs[0].Outcome, store.EventChanged)

		// The worker runs it on its own.
		e.handler.releaseInterval = 10 * time.Millisecond
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { e.handler.Run(runCtx); close(done) }()
		time.Sleep(50 * time.Millisecond)
		cancel()
		<-done
	})
}

func TestURLAndPaths(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://status.local:8080/admin/monitoring/1", nil)
	equal(t, "from the request", URL(r, "", "abc"), "http://status.local:8080/hooks/abc")
	equal(t, "base URL", URL(r, "https://status.example.com/", "abc"), "https://status.example.com/hooks/abc")
	equal(t, "without token", URL(r, "https://status.example.com", ""), "https://status.example.com/hooks")
	equal(t, "redacted", RedactPath("/hooks/secret-token"), "/hooks/…")
	equal(t, "any segment starting with hooks", RedactPath("/hooksx/a"), "/hooksx/…")
	equal(t, "not a hook", RedactPath("/admin/a"), "/admin/a")
	equal(t, "IsPath", []bool{IsPath("/hooks"), IsPath("/hooks/x"), IsPath("/hooksx"), IsPath("/admin")}, []bool{true, true, false, false})

	token, hash := NewToken()
	if len(token) < 40 || HashToken(token) != hash || len(hash) != 64 {
		t.Fatalf("token %q hash %q", token, hash)
	}
}
