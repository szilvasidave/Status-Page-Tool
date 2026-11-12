package notify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// ---------------------------------------------------------------- fixtures

// notifyEnv is a Notifier on a fresh SQLite database with a controllable
// clock and some subscribers:
//
//	group A: alice, carol (no e-mail), erin  → service Mail
//	group B: alice, bob                      → service VPN
//	group C: dave                            → service Printers
//	service Wiki has no groups
type notifyEnv struct {
	t     *testing.T
	store *store.Store
	n     *Notifier
	clock time.Time

	mail, vpn, wiki, printers int64 // service ids
}

func newNotifyEnv(t *testing.T) *notifyEnv {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, config.DriverSQLite, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	st := store.New(d, []byte("0123456789abcdef0123456789abcdef"))
	// The store stamps queued e-mails with the real time (in seconds), so the
	// test clock starts a little ahead of it.
	e := &notifyEnv{t: t, store: st, n: New(st, slog.New(slog.NewTextHandler(io.Discard, nil))), clock: time.Now().Add(time.Minute)}
	e.n.now = func() time.Time { return e.clock }

	groups := map[string]int64{}
	for _, ext := range []string{"grp-a", "grp-b", "grp-c"} {
		g, err := st.CreateGroup(ctx, ext, "")
		if err != nil {
			t.Fatal(err)
		}
		groups[ext] = g.ID
	}
	users := []struct {
		subject, email string
		groups         []string
	}{
		{"alice", "alice@example.com", []string{"grp-a", "grp-b"}},
		{"bob", "bob@example.com", []string{"grp-b"}},
		{"carol", "", []string{"grp-a"}},
		{"dave", "dave@example.com", []string{"grp-c"}},
		{"erin", "erin@example.com", []string{"grp-a"}},
	}
	for _, u := range users {
		user, err := st.UpsertOIDCUser(ctx, store.OIDCUser{Subject: u.subject, Email: u.email, Name: u.subject})
		if err != nil {
			t.Fatal(err)
		}
		var refs []store.GroupRef
		for _, g := range u.groups {
			refs = append(refs, store.GroupRef{ExternalID: g})
		}
		if err := st.SetUserGroups(ctx, user.ID, refs); err != nil {
			t.Fatal(err)
		}
	}
	e.mail = e.service("Mail", groups["grp-a"])
	e.vpn = e.service("VPN", groups["grp-b"])
	e.wiki = e.service("Wiki")
	e.printers = e.service("Printers", groups["grp-c"])
	return e
}

func (e *notifyEnv) service(name string, groupIDs ...int64) int64 {
	e.t.Helper()
	svc := &store.Service{Name: name, GroupIDs: groupIDs}
	if err := e.store.CreateService(context.Background(), svc); err != nil {
		e.t.Fatal(err)
	}
	return svc.ID
}

// configureSMTP stores SMTP settings for the given server.
func (e *notifyEnv) configureSMTP(host string, port int) {
	e.t.Helper()
	st := smtpSettings(host, port, "none")
	st.SiteName = "Acme IT"
	st.BaseURL = "https://status.example.com"
	if err := e.store.SaveSettings(context.Background(), st); err != nil {
		e.t.Fatal(err)
	}
}

// startSMTP starts a fake SMTP server and configures it.
func (e *notifyEnv) startSMTP(srv *fakeSMTP) *fakeSMTP {
	e.t.Helper()
	if srv == nil {
		srv = &fakeSMTP{}
	}
	host, port := srv.start(e.t)
	e.configureSMTP(host, port)
	return srv
}

// configureDeadSMTP configures an SMTP server that refuses connections.
func (e *notifyEnv) configureDeadSMTP() {
	e.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	e.configureSMTP("127.0.0.1", port)
}

func (e *notifyEnv) incident(title string, impacts ...store.ServiceImpact) int64 {
	e.t.Helper()
	inc := &store.Incident{Title: title, Status: store.IncidentInvestigating, Services: impacts}
	if err := e.store.CreateIncident(context.Background(), inc, &store.IncidentUpdate{Message: "We are investigating."}); err != nil {
		e.t.Fatal(err)
	}
	return inc.ID
}

// maintenance creates a maintenance window relative to the real time (the
// store decides with the real time whether a window is already past).
func (e *notifyEnv) maintenance(title string, start, end time.Duration, notify bool, serviceIDs ...int64) int64 {
	e.t.Helper()
	now := time.Now()
	m := &store.Maintenance{Title: title, StartsAt: now.Add(start), EndsAt: now.Add(end), Notify: notify}
	for _, id := range serviceIDs {
		m.Services = append(m.Services, store.ServiceRef{ID: id})
	}
	if err := e.store.CreateMaintenance(context.Background(), m); err != nil {
		e.t.Fatal(err)
	}
	return m.ID
}

func (e *notifyEnv) getMaintenance(id int64) *store.Maintenance {
	e.t.Helper()
	m, err := e.store.GetMaintenance(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

// outbox returns all notifications, oldest first.
func (e *notifyEnv) outbox() []store.Notification {
	e.t.Helper()
	ns, err := e.store.ListNotifications(context.Background(), 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	slices.Reverse(ns)
	return ns
}

// clearOutbox removes all notifications.
func (e *notifyEnv) clearOutbox() {
	e.t.Helper()
	if _, err := e.store.DB().ExecContext(context.Background(), `DELETE FROM notifications`); err != nil {
		e.t.Fatal(err)
	}
}

// notifyAddresses returns the recipient addresses of ns.
func notifyAddresses(ns []store.Notification) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.Email)
	}
	return out
}

// notifySubjects counts the notifications per subject.
func notifySubjects(ns []store.Notification) map[string]int {
	out := map[string]int{}
	for _, n := range ns {
		out[n.Subject]++
	}
	return out
}

func notifyExpect(t *testing.T, what string, got int, err error, want int) {
	t.Helper()
	if err != nil || got != want {
		t.Errorf("%s = %d, %v; want %d", what, got, err, want)
	}
}

// drainWake empties the wake channel and reports whether it was signalled.
func drainWake(n *Notifier) bool {
	select {
	case <-n.wake:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------- queueing

func TestNotifierIncidentRecipients(t *testing.T) {
	e := newNotifyEnv(t)
	e.startSMTP(nil)
	ctx := context.Background()

	id := e.incident("Mail is slow",
		store.ServiceImpact{ServiceID: e.mail, Impact: store.StatusMajorOutage},
		store.ServiceImpact{ServiceID: e.vpn, Impact: store.StatusDegraded})
	got, err := e.n.IncidentCreated(ctx, id)
	notifyExpect(t, "IncidentCreated", got, err, 3)
	if !drainWake(e.n) {
		t.Error("the outbox worker was not woken")
	}

	// alice (A and B) once, bob (B), erin (A); carol has no e-mail, dave's
	// group is not assigned to Mail or VPN.
	out := e.outbox()
	if addrs := notifyAddresses(out); !slices.Equal(addrs, []string{"alice@example.com", "bob@example.com", "erin@example.com"}) {
		t.Fatalf("recipients = %v", addrs)
	}
	for _, n := range out {
		if n.Subject != "[Acme IT] Investigating: Mail is slow" || n.Status != store.NotificationPending || n.UserID == 0 || n.Attempts != 0 {
			t.Errorf("notification = %+v", n)
		}
		assertContains(t, "text", n.BodyText, "A new incident has been reported.", "We are investigating.",
			"- Mail: Major outage", "https://status.example.com/incidents/")
		assertContains(t, "html", n.BodyHTML, "<!DOCTYPE html>", "Mail is slow")
	}

	// The update moves the incident from Mail+VPN to Wiki (no groups): the
	// subscribers of the previous services are still told.
	e.clearOutbox()
	upd := &store.IncidentUpdate{Status: store.IncidentMonitoring, Message: "Fix deployed."}
	if err := e.store.AddIncidentUpdate(ctx, id, upd, []store.ServiceImpact{{ServiceID: e.wiki, Impact: store.StatusDegraded}}); err != nil {
		t.Fatal(err)
	}
	got, err = e.n.IncidentUpdated(ctx, id, []int64{e.mail, e.printers})
	notifyExpect(t, "IncidentUpdated", got, err, 3)
	out = e.outbox()
	if addrs := notifyAddresses(out); !slices.Equal(addrs, []string{"alice@example.com", "dave@example.com", "erin@example.com"}) {
		t.Errorf("recipients = %v", addrs)
	}
	if out[0].Subject != "[Acme IT] Update – Monitoring: Mail is slow" || !strings.Contains(out[0].BodyText, "Fix deployed.") {
		t.Errorf("update e-mail = %q\n%s", out[0].Subject, out[0].BodyText)
	}

	// Without previous services only the Wiki subscribers (nobody) are told.
	e.clearOutbox()
	got, err = e.n.IncidentUpdated(ctx, id, nil)
	notifyExpect(t, "IncidentUpdated without subscribers", got, err, 0)
	if n := len(e.outbox()); n != 0 {
		t.Errorf("%d e-mails queued", n)
	}

	if _, err := e.n.IncidentCreated(ctx, 999); err == nil {
		t.Error("unknown incident: expected an error")
	}
}

func TestNotifierMaintenanceMails(t *testing.T) {
	e := newNotifyEnv(t)
	e.startSMTP(nil)
	ctx := context.Background()

	id := e.maintenance("Firewall upgrade", time.Hour, 2*time.Hour, true, e.vpn)
	got, err := e.n.MaintenanceScheduled(ctx, id)
	notifyExpect(t, "MaintenanceScheduled", got, err, 2) // alice, bob
	got, err = e.n.MaintenanceUpdated(ctx, id, []int64{e.printers, e.vpn})
	notifyExpect(t, "MaintenanceUpdated", got, err, 3) // + dave
	if err := e.store.CancelMaintenance(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err = e.n.MaintenanceCancelled(ctx, id)
	notifyExpect(t, "MaintenanceCancelled", got, err, 2)

	want := map[string]int{
		"[Acme IT] Scheduled maintenance: Firewall upgrade": 2,
		"[Acme IT] Maintenance updated: Firewall upgrade":   3,
		"[Acme IT] Maintenance cancelled: Firewall upgrade": 2,
	}
	if got := notifySubjects(e.outbox()); !maps.Equal(got, want) {
		t.Errorf("queued = %v, want %v", got, want)
	}
	if _, err := e.n.MaintenanceScheduled(ctx, 999); err == nil {
		t.Error("unknown maintenance: expected an error")
	}
}

func TestNotifierWithoutSMTP(t *testing.T) {
	e := newNotifyEnv(t)
	ctx := context.Background()

	inc := e.incident("Outage", store.ServiceImpact{ServiceID: e.mail, Impact: store.StatusMajorOutage})
	m := e.maintenance("Patching", time.Hour, 2*time.Hour, true, e.mail)
	calls := map[string]func() (int, error){
		"IncidentCreated":      func() (int, error) { return e.n.IncidentCreated(ctx, inc) },
		"IncidentUpdated":      func() (int, error) { return e.n.IncidentUpdated(ctx, inc, []int64{e.vpn}) },
		"MaintenanceScheduled": func() (int, error) { return e.n.MaintenanceScheduled(ctx, m) },
		"MaintenanceUpdated":   func() (int, error) { return e.n.MaintenanceUpdated(ctx, m, nil) },
		"MaintenanceCancelled": func() (int, error) { return e.n.MaintenanceCancelled(ctx, m) },
	}
	for name, call := range calls {
		got, err := call()
		notifyExpect(t, name, got, err, 0)
	}
	if n := len(e.outbox()); n != 0 {
		t.Errorf("%d e-mails queued without SMTP settings", n)
	}
	if drainWake(e.n) {
		t.Error("worker woken without queued e-mails")
	}
}

// ---------------------------------------------------------------- delivery

func TestNotifierDeliver(t *testing.T) {
	e := newNotifyEnv(t)
	srv := e.startSMTP(nil)
	ctx := context.Background()

	id := e.incident("VPN down", store.ServiceImpact{ServiceID: e.vpn, Impact: store.StatusMajorOutage})
	if _, err := e.n.IncidentCreated(ctx, id); err != nil {
		t.Fatal(err)
	}
	e.n.deliver(ctx)

	got := srv.received()
	if len(got) != 2 {
		t.Fatalf("delivered %d e-mails, want 2", len(got))
	}
	for i, to := range []string{"alice@example.com", "bob@example.com"} {
		if len(got[i].To) != 1 || got[i].To[0] != to {
			t.Errorf("mail %d to %v, want %s", i, got[i].To, to)
		}
		m := parseMail(t, got[i].Data)
		if m.Subject != "[Acme IT] Investigating: VPN down" {
			t.Errorf("subject = %q", m.Subject)
		}
		assertContains(t, "text", m.Text, "We are investigating.", "- VPN: Major outage")
		assertContains(t, "html", m.HTML, "VPN down", "Major outage")
	}
	for _, n := range e.outbox() {
		if n.Status != store.NotificationSent || n.Attempts != 1 || n.SentAt == nil || n.LastError != "" {
			t.Errorf("notification after delivery = %+v", n)
		}
	}

	// Nothing is sent twice.
	e.n.deliver(ctx)
	if n := len(srv.received()); n != 2 {
		t.Errorf("delivered %d e-mails in total, want 2", n)
	}
}

func TestNotifierDeliverManyBatches(t *testing.T) {
	e := newNotifyEnv(t)
	srv := e.startSMTP(nil)
	ctx := context.Background()

	var msgs []store.OutboundEmail
	for i := range 2*batchSize + 7 {
		msgs = append(msgs, store.OutboundEmail{Email: fmt.Sprintf("user%d@example.com", i), Subject: "Hello", BodyText: "Hi"})
	}
	if err := e.store.EnqueueEmails(ctx, msgs); err != nil {
		t.Fatal(err)
	}
	e.n.deliver(ctx)
	if n := len(srv.received()); n != len(msgs) {
		t.Errorf("delivered %d e-mails, want %d", n, len(msgs))
	}
}

func TestNotifierRetryAndGiveUp(t *testing.T) {
	e := newNotifyEnv(t)
	e.configureDeadSMTP()
	ctx := context.Background()

	id := e.incident("VPN down", store.ServiceImpact{ServiceID: e.vpn, Impact: store.StatusMajorOutage})
	if _, err := e.n.IncidentCreated(ctx, id); err != nil {
		t.Fatal(err)
	}

	// A connection error is a failed attempt for every e-mail. The delays
	// after attempts 1-4 are 1 min, 5 min, 15 min and 1 h; the fifth failed
	// attempt is the last one.
	for attempt, delay := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 0} {
		e.n.deliver(ctx)
		for _, n := range e.outbox() {
			if n.Attempts != attempt+1 || !strings.Contains(n.LastError, "could not connect to SMTP server") {
				t.Fatalf("attempt %d: notification = %+v", attempt+1, n)
			}
			if delay == 0 {
				if n.Status != store.NotificationFailed {
					t.Errorf("after the last attempt: status = %s, want failed", n.Status)
				}
				continue
			}
			if n.Status != store.NotificationPending || n.NextAttemptAt.Unix() != e.clock.Add(delay).Unix() {
				t.Errorf("attempt %d: status %s, next attempt %s; want pending at %s", attempt+1, n.Status, n.NextAttemptAt, e.clock.Add(delay).UTC())
			}
		}
		// Not due yet: no further attempt.
		e.clock = e.clock.Add(delay - time.Second)
		e.n.deliver(ctx)
		for _, n := range e.outbox() {
			if n.Attempts != attempt+1 {
				t.Fatalf("attempt %d: e-mail retried before it was due (%d attempts)", attempt+1, n.Attempts)
			}
		}
		e.clock = e.clock.Add(time.Second)
	}

	// Failed e-mails stay failed, even when the server works again.
	srv := e.startSMTP(nil)
	e.clock = e.clock.Add(24 * time.Hour)
	e.n.deliver(ctx)
	if n := len(srv.received()); n != 0 {
		t.Errorf("%d failed e-mails delivered", n)
	}
}

func TestNotifierRetrySucceeds(t *testing.T) {
	e := newNotifyEnv(t)
	srv := e.startSMTP(&fakeSMTP{rejectRcpt: "bob@example.com"})
	ctx := context.Background()

	id := e.incident("VPN down", store.ServiceImpact{ServiceID: e.vpn, Impact: store.StatusMajorOutage})
	if _, err := e.n.IncidentCreated(ctx, id); err != nil {
		t.Fatal(err)
	}
	e.n.deliver(ctx)

	out := e.outbox()
	if out[0].Email != "alice@example.com" || out[0].Status != store.NotificationSent {
		t.Errorf("alice: %+v", out[0])
	}
	bob := out[1]
	if bob.Email != "bob@example.com" || bob.Status != store.NotificationPending || bob.Attempts != 1 || !strings.Contains(bob.LastError, "550") {
		t.Errorf("bob: %+v", bob)
	}

	// Not due yet.
	srv = e.startSMTP(nil) // accepts bob
	e.n.deliver(ctx)
	if n := len(srv.received()); n != 0 {
		t.Fatalf("retried before it was due: %d e-mails", n)
	}
	// The retry is delivered once it is due.
	e.clock = e.clock.Add(time.Minute)
	e.n.deliver(ctx)
	got := srv.received()
	if len(got) != 1 || got[0].To[0] != "bob@example.com" {
		t.Fatalf("received %+v", got)
	}
	if bob := e.outbox()[1]; bob.Status != store.NotificationSent || bob.Attempts != 2 {
		t.Errorf("bob after retry: %+v", bob)
	}
}

func TestNotifierDeliverWaitsForSMTPSettings(t *testing.T) {
	e := newNotifyEnv(t)
	ctx := context.Background()
	if err := e.store.EnqueueEmails(ctx, []store.OutboundEmail{{Email: "a@example.com", Subject: "Hi", BodyText: "x"}}); err != nil {
		t.Fatal(err)
	}
	e.n.deliver(ctx) // e-mail not configured: nothing happens
	if n := e.outbox()[0]; n.Status != store.NotificationPending || n.Attempts != 0 {
		t.Errorf("notification = %+v", n)
	}
	srv := e.startSMTP(nil)
	e.n.deliver(ctx)
	if len(srv.received()) != 1 || e.outbox()[0].Status != store.NotificationSent {
		t.Error("queued e-mail not delivered after configuring SMTP")
	}
}

// ---------------------------------------------------------------- maintenance watcher

func TestNotifierMaintenanceStartedCompleted(t *testing.T) {
	e := newNotifyEnv(t)
	e.startSMTP(nil)
	start := e.clock

	m1 := e.maintenance("M1", time.Hour, 2*time.Hour, true, e.mail)                    // started, then completed
	m2 := e.maintenance("M2", time.Hour, 2*time.Hour, false, e.mail)                   // notifications off
	m3 := e.maintenance("M3", -3*time.Hour, -2*time.Hour, true, e.mail)                // in the past: no flood
	m4 := e.maintenance("M4", 130*time.Minute, 150*time.Minute, true, e.mail)          // only "completed"
	m5 := e.maintenance("M5", time.Hour, 2*time.Hour, true, e.mail)                    // cancelled
	m6 := e.maintenance("M6", time.Hour, 2*time.Hour, true, e.wiki)                    // nobody subscribed
	m7 := e.maintenance("M7", -time.Hour, 150*time.Minute, true, e.mail, e.vpn, e.vpn) // already running at creation
	if err := e.store.CancelMaintenance(context.Background(), m5); err != nil {
		t.Fatal(err)
	}

	e.clock = start.Add(90 * time.Minute)
	e.n.checkMaintenances(context.Background())
	e.n.checkMaintenances(context.Background())                    // idempotent
	want := map[string]int{"[Acme IT] Maintenance started: M1": 2} // alice, erin
	if got := notifySubjects(e.outbox()); !maps.Equal(got, want) {
		t.Errorf("after start: queued %v, want %v", got, want)
	}
	if !drainWake(e.n) {
		t.Error("the outbox worker was not woken")
	}
	if m := e.getMaintenance(m1); !m.NotifiedStart || m.NotifiedEnd {
		t.Errorf("M1 flags after start: start=%v end=%v", m.NotifiedStart, m.NotifiedEnd)
	}
	if m := e.getMaintenance(m6); !m.NotifiedStart {
		t.Error("M6 (no recipients) not marked as started")
	}

	e.clock = start.Add(3 * time.Hour)
	e.n.checkMaintenances(context.Background())
	e.n.checkMaintenances(context.Background())
	want["[Acme IT] Maintenance completed: M1"] = 2
	want["[Acme IT] Maintenance completed: M4"] = 2
	want["[Acme IT] Maintenance completed: M7"] = 3 // alice, bob, erin
	if got := notifySubjects(e.outbox()); !maps.Equal(got, want) {
		t.Errorf("after end: queued %v, want %v", got, want)
	}
	for _, id := range []int64{m1, m3, m4, m6, m7} {
		if m := e.getMaintenance(id); !m.NotifiedStart || !m.NotifiedEnd {
			t.Errorf("%s flags: start=%v end=%v, want both set", m.Title, m.NotifiedStart, m.NotifiedEnd)
		}
	}
	for _, id := range []int64{m2, m5} {
		if m := e.getMaintenance(id); m.NotifiedStart || m.NotifiedEnd {
			t.Errorf("%s flags changed: start=%v end=%v", m.Title, m.NotifiedStart, m.NotifiedEnd)
		}
	}
}

func TestNotifierMaintenanceWithoutSMTP(t *testing.T) {
	e := newNotifyEnv(t)
	id := e.maintenance("Patching", time.Hour, 2*time.Hour, true, e.mail)

	// Without SMTP the windows are marked anyway, so that configuring e-mail
	// later does not send outdated e-mails.
	e.clock = e.clock.Add(3 * time.Hour)
	e.n.checkMaintenances(context.Background())
	if m := e.getMaintenance(id); !m.NotifiedStart || !m.NotifiedEnd {
		t.Errorf("flags: start=%v end=%v, want both set", m.NotifiedStart, m.NotifiedEnd)
	}
	e.startSMTP(nil)
	e.n.checkMaintenances(context.Background())
	if n := len(e.outbox()); n != 0 {
		t.Errorf("%d e-mails queued", n)
	}
}

// ---------------------------------------------------------------- Run, cleanup, test e-mail

func TestNotifierRun(t *testing.T) {
	e := newNotifyEnv(t)
	srv := e.startSMTP(nil)
	e.n.outboxInterval = time.Hour // delivery only when woken
	e.n.maintenanceInterval = 10 * time.Millisecond
	e.n.now = time.Now

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.n.Run(ctx)
		close(done)
	}()

	id := e.incident("Mail down", store.ServiceImpact{ServiceID: e.mail, Impact: store.StatusMajorOutage})
	if _, err := e.n.IncidentCreated(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// A maintenance that starts right away is announced by the watcher.
	e.maintenance("Reboot", 2*time.Second, time.Hour, true, e.vpn)

	deadline := time.Now().Add(10 * time.Second)
	for len(srv.received()) < 4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	got := map[string]int{}
	for _, m := range srv.received() {
		got[parseMail(t, m.Data).Subject]++
	}
	want := map[string]int{"[Acme IT] Investigating: Mail down": 2, "[Acme IT] Maintenance started: Reboot": 2}
	if !maps.Equal(got, want) {
		t.Errorf("delivered %v, want %v", got, want)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after the context was cancelled")
	}
}

func TestNotifierCleanup(t *testing.T) {
	e := newNotifyEnv(t)
	srv := e.startSMTP(&fakeSMTP{rejectRcpt: "failed@example.com"})
	ctx := context.Background()
	if err := e.store.EnqueueEmails(ctx, []store.OutboundEmail{
		{Email: "sent@example.com", Subject: "1", BodyText: "x"},
		{Email: "failed@example.com", Subject: "2", BodyText: "x"},
	}); err != nil {
		t.Fatal(err)
	}
	e.n.deliver(ctx)
	if len(srv.received()) != 1 {
		t.Fatal("e-mail not delivered")
	}
	failed := e.outbox()[1]
	if err := e.store.MarkNotificationAttemptFailed(ctx, failed.ID, "gave up", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.store.EnqueueEmails(ctx, []store.OutboundEmail{{Email: "pending@example.com", Subject: "3", BodyText: "x"}}); err != nil {
		t.Fatal(err)
	}

	e.clock = e.clock.Add(89 * 24 * time.Hour)
	e.n.cleanup(ctx)
	if n := len(e.outbox()); n != 3 {
		t.Fatalf("%d e-mails left after 89 days, want 3", n)
	}
	e.clock = e.clock.Add(2 * 24 * time.Hour)
	e.n.cleanup(ctx)
	if left := notifyAddresses(e.outbox()); !slices.Equal(left, []string{"pending@example.com"}) {
		t.Errorf("left after 91 days: %v, want only the pending e-mail", left)
	}
}

func TestSendTestEmail(t *testing.T) {
	e := newNotifyEnv(t)
	ctx := context.Background()

	err := e.n.SendTestEmail(ctx, "admin@example.com")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("without SMTP settings: error = %v", err)
	}

	srv := e.startSMTP(&fakeSMTP{rejectRcpt: "nobody@example.com"})
	if err := e.n.SendTestEmail(ctx, " admin@example.com "); err != nil {
		t.Fatalf("SendTestEmail: %v", err)
	}
	got := srv.received()
	if len(got) != 1 || got[0].To[0] != "admin@example.com" {
		t.Fatalf("received %+v", got)
	}
	m := parseMail(t, got[0].Data)
	if m.Subject != "[Acme IT] Test e-mail" {
		t.Errorf("subject = %q", m.Subject)
	}
	assertContains(t, "text", m.Text, "E-mail delivery works.", "SMTP server: 127.0.0.1:", "Security:    None",
		"Sender:      IT Status <status@example.com>", "https://status.example.com/")
	assertContains(t, "html", m.HTML, "Test e-mail", "E-mail delivery works.")
	if n := len(e.outbox()); n != 0 {
		t.Errorf("test e-mail written to the outbox (%d rows)", n)
	}

	if err := e.n.SendTestEmail(ctx, "nobody@example.com"); err == nil || !strings.Contains(err.Error(), "550") {
		t.Errorf("rejected recipient: error = %v", err)
	}
	if err := e.n.SendTestEmail(ctx, ""); err == nil {
		t.Error("empty address: expected an error")
	}
	e.configureDeadSMTP()
	if err := e.n.SendTestEmail(ctx, "admin@example.com"); err == nil || !strings.Contains(err.Error(), "could not connect") {
		t.Errorf("dead server: error = %v", err)
	}
}
