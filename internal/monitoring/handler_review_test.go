package monitoring

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// holdMonitoring makes the transactions of monitoring wait, like a long
// request of another instance (PostgreSQL) or any writer (SQLite), until
// the returned function is called.
func (e *hookEnv) holdMonitoring() (release func()) {
	e.t.Helper()
	d := e.store.DB()
	tx, err := d.DB.BeginTx(ctx, nil) // SQLite: BEGIN IMMEDIATE
	if err != nil {
		e.t.Fatal(err)
	}
	if d.IsPostgres() {
		if _, err := tx.ExecContext(ctx, d.Rebind(`SELECT pg_advisory_xact_lock(?)`), 7395001); err != nil {
			e.t.Fatal(err)
		}
	}
	return func() { _ = tx.Rollback() }
}

// waitFor polls cond for up to 10 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *hookEnv) setLastEvent(in *store.Integration, at time.Time) {
	e.t.Helper()
	d := e.store.DB()
	if _, err := d.ExecContext(ctx, `UPDATE integrations SET last_event_at = ? WHERE id = ?`, at.Unix(), in.ID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *hookEnv) lastEvent(in *store.Integration) time.Time {
	e.t.Helper()
	got, err := e.store.GetIntegration(ctx, in.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	if got.LastEventAt == nil {
		return time.Time{}
	}
	return *got.LastEventAt
}

// A tool that hangs up while its request waits behind other alerts: what it
// sent is applied and recorded anyway (tools like Uptime Kuma do not send it
// again).
func TestHandlerToolHangsUp(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN")
		in, token := e.integration(Generic, nil)
		release := e.holdMonitoring()
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/hooks/"+token, strings.NewReader(`{"service": "VPN", "status": "down", "title": "VPN down"}`))
		req.Header.Set("Content-Type", "application/json")
		if _, err := (&http.Client{Timeout: 300 * time.Millisecond}).Do(req); err == nil {
			t.Fatal("the request did not wait")
		}
		release()
		waitFor(t, "the incident", func() bool { return len(e.openIncidents()) == 1 })
		waitFor(t, "the record", func() bool {
			evs := e.events(in)
			return len(evs) == 1 && evs[0].Outcome == store.EventChanged
		})
	})
}

// A request that cannot get its turn in time is answered 503 (the tool may
// retry) and recorded as not applied.
func TestHandlerBusy(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN")
		in, token := e.integration(Generic, nil)
		e.handler.applyTimeout = 300 * time.Millisecond
		release := e.holdMonitoring()
		timer := time.AfterFunc(time.Second, release)
		defer timer.Stop()
		r := e.post("/hooks/"+token, "application/json", `{"service": "VPN", "status": "down"}`)
		if r.status != http.StatusServiceUnavailable || r.header.Get("Retry-After") == "" || r.body.Status != "error" {
			t.Fatalf("got %d %+v (Retry-After %q)", r.status, r.body, r.header.Get("Retry-After"))
		}
		evs := e.events(in)
		if len(evs) != 1 || evs[0].Outcome != store.EventFailed || !strings.Contains(evs[0].Summary, "busy") || evs[0].Payload == "" {
			t.Fatalf("request log: %+v", evs)
		}
		time.Sleep(1200 * time.Millisecond)
		if n := len(e.openIncidents()); n != 0 {
			t.Fatalf("%d incidents opened by a request answered 503", n)
		}
	})
}

// The answer reaches the tool even after the server's WriteTimeout (a
// minute in internal/app), which a request that waits for its turn can
// outlast: the busy answer (503 with Retry-After) and a late success, over
// HTTP/1.1 and HTTP/2.
func TestHandlerAnswersAfterWriteTimeout(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "http1", true: "http2"}[h2], func(t *testing.T) {
			forEachDB(t, func(t *testing.T, e *hookEnv) {
				e.service("VPN")
				_, token := e.integration(Generic, nil)
				mux := http.NewServeMux()
				mux.Handle(Path+"/", e.handler)
				srv := httptest.NewUnstartedServer(mux)
				srv.Config.ReadTimeout, srv.Config.WriteTimeout = 200*time.Millisecond, 200*time.Millisecond
				srv.EnableHTTP2 = h2
				srv.StartTLS()
				defer srv.Close()
				post := func(body string, hold time.Duration) (*http.Response, []byte) {
					t.Helper()
					release := e.holdMonitoring()
					timer := time.AfterFunc(hold, release)
					defer func() { timer.Stop(); release() }() // PostgreSQL answers 503 before hold
					resp, err := srv.Client().Post(srv.URL+Path+"/"+token, "application/json", strings.NewReader(body))
					if err != nil {
						t.Fatalf("no answer: %v", err)
					}
					defer resp.Body.Close()
					b, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatalf("answer cut off: %v", err)
					}
					if (resp.ProtoMajor == 2) != h2 {
						t.Fatalf("protocol %s", resp.Proto)
					}
					return resp, b
				}

				e.handler.applyTimeout = 500 * time.Millisecond
				resp, b := post(`{"key": "a", "service": "VPN", "status": "down"}`, 1200*time.Millisecond)
				if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
					t.Fatalf("busy: got %d %s (Retry-After %q)", resp.StatusCode, b, resp.Header.Get("Retry-After"))
				}

				e.handler.applyTimeout = 10 * time.Second
				resp, b = post(`{"key": "a", "service": "VPN", "status": "down"}`, 800*time.Millisecond)
				if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "opened incident #1 of VPN") {
					t.Fatalf("late: got %d %s", resp.StatusCode, b)
				}
			})
		})
	}
}

// Alerts beyond MaxAlerts are ignored, and the answer and the request log
// say so first, before the lines of the alerts read (which are cut off).
func TestHandlerTooManyAlerts(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN")
		in, token := e.integration(Generic, nil)
		var b strings.Builder
		b.WriteString("[")
		for i := range MaxAlerts {
			fmt.Fprintf(&b, `{"key": "h%d", "service": "Host %d", "status": "up"},`, i, i)
		}
		b.WriteString(`{"service": "VPN", "status": "down"}]`)
		want := fmt.Sprintf("Only the first %d of %d alerts were read: the others were ignored (send at most %d per request).", MaxAlerts, MaxAlerts+1, MaxAlerts)
		r := e.post("/hooks/"+token, "application/json", b.String())
		if r.status != http.StatusOK || r.body.Alerts != MaxAlerts || len(r.body.Summary) == 0 || r.body.Summary[0] != want {
			t.Fatalf("got %d, %d alerts, %q", r.status, r.body.Alerts, r.body.Summary[:min(2, len(r.body.Summary))])
		}
		if ev := e.events(in)[0]; !strings.HasPrefix(ev.Summary, want+"\n") {
			t.Fatalf("request log: %.200s", ev.Summary)
		}
		if n := len(e.openIncidents()); n != 0 {
			t.Fatalf("%d incidents opened by an alert that was not read", n)
		}

		// Alertmanager (and Grafana), paused: also first.
		am, amToken := e.integration("alertmanager", func(in *store.Integration) { in.Paused = true })
		b.Reset()
		b.WriteString(`{"version": "4", "status": "firing", "alerts": [`)
		for i := range MaxAlerts + 2 {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"status": "resolved", "labels": {"alertname": "Down", "instance": "h%d"}}`, i)
		}
		b.WriteString(`]}`)
		want = fmt.Sprintf("Only the first %d of %d alerts were read: the others were ignored (send at most %d per request).", MaxAlerts, MaxAlerts+2, MaxAlerts)
		r = e.post("/hooks/"+amToken, "application/json", b.String())
		if r.status != http.StatusOK || len(r.body.Summary) < 2 || r.body.Summary[1] != want ||
			!strings.HasPrefix(r.body.Summary[0], "The integration is paused") {
			t.Fatalf("got %d, %q", r.status, r.body.Summary[:min(2, len(r.body.Summary))])
		}
		if ev := e.events(am)[0]; !strings.Contains(ev.Summary, "\n"+want+"\n") {
			t.Fatalf("request log: %.300s", ev.Summary)
		}
	})
}

// A paused integration still applies resolved alerts, so that a recovery
// received while it was paused does not leave an incident open or release
// a held-back alert after the maintenance.
func TestHandlerPausedRecoveries(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		vpn := e.service("VPN")
		e.service("Mail")
		in, token := e.integration(Generic, nil)
		pause := func(p bool) {
			t.Helper()
			in.Paused = p
			if err := e.store.UpdateIntegration(ctx, in); err != nil {
				t.Fatal(err)
			}
		}
		post := func(body string) hookResponse {
			t.Helper()
			return e.post("/hooks/"+token, "application/json", body)
		}

		// An open incident, its recovery while paused: resolved.
		post(`{"key": "gw", "service": "Mail", "status": "down", "title": "SMTP down"}`)
		pause(true)
		r := post(`[{"key": "gw", "service": "Mail", "status": "up"}, {"key": "x", "service": "Mail", "status": "down"}]`)
		if r.status != http.StatusOK || r.body.Status != "paused" {
			t.Fatalf("got %d %+v", r.status, r.body)
		}
		if n := len(e.openIncidents()); n != 0 {
			t.Fatalf("%d open incidents after the recovery", n)
		}
		ev := e.events(in)[0]
		if ev.Outcome != store.EventChanged || !strings.HasPrefix(ev.Summary, "The integration is paused: 1 firing alert ignored; resolved alerts still clear.\n") ||
			!strings.Contains(ev.Summary, "resolved incident #1 of Mail") {
			t.Fatalf("record: %+v", ev)
		}

		// A held-back alert, its recovery while paused: not released later.
		pause(false)
		now := time.Now()
		m := &store.Maintenance{Title: "Upgrade", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Services: []store.ServiceRef{{ID: vpn.ID}}}
		if err := e.store.CreateMaintenance(ctx, m); err != nil {
			t.Fatal(err)
		}
		post(`{"key": "vpn", "service": "VPN", "status": "down"}`)
		pause(true)
		r = post(`{"key": "vpn", "service": "VPN", "status": "up"}`)
		if ev := e.events(in)[0]; ev.Outcome != store.EventPaused || !strings.HasPrefix(ev.Summary, "The integration is paused: resolved alerts still clear.\n") {
			t.Fatalf("record: %+v", ev)
		}
		if _, err := e.store.DB().ExecContext(ctx, `UPDATE maintenances SET ends_at = ? WHERE id = ?`, now.Add(-time.Minute).Unix(), m.ID); err != nil {
			t.Fatal(err)
		}
		pause(false)
		e.handler.release(ctx)
		if n := len(e.openIncidents()); n != 0 {
			t.Fatalf("%d incidents opened for an alert that cleared while paused", n)
		}
	})
}

// Bodies that are not UTF-8 (Windows PowerShell 5.1 sends Windows-1252
// without saying so) are decoded.
func TestHandlerCharsets(t *testing.T) {
	for _, tc := range []struct {
		body    []byte
		charset string
		want    string
	}{
		{[]byte("Könyvelés"), "", "Könyvelés"},
		{[]byte{'K', 0xF6, 'n', 'y', 'v', 'e', 'l', 0xE9, 's', ' ', 0x96, ' ', 0x80}, "", "Könyvelés – €"},
		{[]byte{0xC3, 0xA9}, "ISO-8859-1", "Ã©"},
		{[]byte{0xE9, 0x81}, "windows-1252", "é\u0081"},
		{[]byte{0xE9}, "utf-8", "\xe9"}, // left alone: storable replaces it
	} {
		if got := string(toUTF8(tc.body, tc.charset)); got != tc.want {
			t.Errorf("%q (%s): got %q, want %q", tc.body, tc.charset, got, tc.want)
		}
	}

	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("Könyvelés")
		_, token := e.integration(Generic, nil)
		latin := string([]byte(`{"service": "K`)) + "\xf6nyvel\xe9s" + `", "status": "down", "title": "K` + "\xf6nyvel\xe9s \x96 le\xe1ll\xe1s" + `"}`
		r := e.post("/hooks/"+token, "application/json", latin)
		if r.status != http.StatusOK || len(r.body.Summary) != 1 || !strings.Contains(r.body.Summary[0], "opened incident #1 of Könyvelés") {
			t.Fatalf("got %d %+v", r.status, r.body)
		}
		if open := e.openIncidents(); len(open) != 1 || open[0].Title != "Könyvelés – leállás" {
			t.Fatalf("incidents: %+v", open)
		}
	})
}

// The worker's records of alerts released after maintenance name alerts
// without a title by their key and do not count as requests of the tool.
func TestHandlerReleaseRecord(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		vpn := e.service("VPN")
		in, token := e.integration(Generic, nil)
		now := time.Now()
		m := &store.Maintenance{Title: "Upgrade", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Services: []store.ServiceRef{{ID: vpn.ID}}}
		if err := e.store.CreateMaintenance(ctx, m); err != nil {
			t.Fatal(err)
		}
		e.post("/hooks/"+token, "application/json", `{"service": "VPN", "status": "down"}`)
		before := now.Add(-2 * time.Hour).Truncate(time.Second)
		e.setLastEvent(in, before)
		if _, err := e.store.DB().ExecContext(ctx, `UPDATE maintenances SET ends_at = ? WHERE id = ?`, now.Add(-time.Minute).Unix(), m.ID); err != nil {
			t.Fatal(err)
		}
		e.handler.release(ctx)
		if got := e.events(in)[0].Summary; got != "Alert service:vpn (firing since a maintenance that has ended): opened incident #1 of VPN." {
			t.Fatalf("record: %q", got)
		}
		if got := e.lastEvent(in); !got.Equal(before) {
			t.Fatalf("last request %v, want %v (the tool sent nothing)", got, before)
		}
	})
}

// An administrator clears an alert whose recovery the tool did not send.
func TestHandlerClear(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *hookEnv) {
		e.service("VPN")
		in, token := e.integration(Generic, nil)
		e.post("/hooks/"+token, "application/json", `{"key": "gw", "service": "VPN", "status": "down", "title": "VPN gateway down"}`)
		before := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
		e.setLastEvent(in, before)

		lines, err := e.handler.Clear(ctx, in, "gw", "Ada Admin")
		if err != nil {
			t.Fatal(err)
		}
		want := "“VPN gateway down” (cleared by Ada Admin): resolved incident #1 of VPN."
		if len(lines) != 1 || lines[0] != want {
			t.Fatalf("lines: %q", lines)
		}
		if n := len(e.openIncidents()); n != 0 {
			t.Fatalf("%d open incidents", n)
		}
		if got := strings.Join(e.notifier.calls, ","); got != "created 1,updated 1" {
			t.Fatalf("e-mails: %s", got)
		}
		if ev := e.events(in)[0]; ev.Summary != want || ev.Payload != "" || ev.Outcome != store.EventChanged {
			t.Fatalf("record: %+v", ev)
		}
		if got := e.lastEvent(in); !got.Equal(before) {
			t.Fatalf("last request %v, want %v", got, before)
		}
		if _, err := e.handler.Clear(ctx, in, "gw", "Ada Admin"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("clearing again: %v", err)
		}

		// An alert whose incident an editor resolved: no incident changes.
		e.post("/hooks/"+token, "application/json", `{"key": "gw", "service": "VPN", "status": "down", "title": "VPN gateway down"}`)
		if err := e.store.AddIncidentUpdate(ctx, 2, &store.IncidentUpdate{Status: store.IncidentResolved, Message: "ok"}, nil); err != nil {
			t.Fatal(err)
		}
		lines, err = e.handler.Clear(ctx, in, "gw", "Ada Admin")
		if err != nil {
			t.Fatal(err)
		}
		want = "“VPN gateway down” (cleared by Ada Admin): cleared for VPN (no open incident)."
		if ev := e.events(in)[0]; len(lines) != 1 || lines[0] != want || ev.Summary != want || ev.Outcome != store.EventUnchanged {
			t.Fatalf("lines %q, record %+v", lines, ev)
		}
	})
}

// In setup mode the webhook URLs answer 503, which tools retry, instead of
// a redirect to a page that they would take for a delivery.
func TestServeUnavailable(t *testing.T) {
	w := httptest.NewRecorder()
	ServeUnavailable(w, httptest.NewRequest(http.MethodPost, "/hooks/x", strings.NewReader("{}")))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" ||
		!strings.Contains(w.Body.String(), `"status":"error"`) || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %v %s", w.Code, w.Header(), w.Body)
	}
}
