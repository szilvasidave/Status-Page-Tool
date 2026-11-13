package notify

// Delivery with Microsoft Graph against the fake server of
// internal/testutil/fakegraph. The fixtures (notifyEnv) are in
// notify_test.go.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/msgraph"
	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakegraph"
)

// configureGraph stores settings that send with the fake Microsoft Graph
// server.
func (e *notifyEnv) configureGraph(fg *fakegraph.Server) {
	e.t.Helper()
	st := store.DefaultSettings()
	st.SiteName = "Acme IT"
	st.BaseURL = "https://status.example.com"
	st.MailMethod = store.MailGraph
	st.GraphSender = fg.Sender
	st.OIDCIssuer, st.OIDCClientID, st.OIDCClientSecret = fg.Issuer(), fg.ClientID, fg.ClientSecret
	if err := e.store.SaveSettings(context.Background(), &st); err != nil {
		e.t.Fatal(err)
	}
}

// changeSettings applies change to the stored settings.
func (e *notifyEnv) changeSettings(change func(st *store.Settings)) {
	e.t.Helper()
	st, err := e.store.GetSettings(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	change(st)
	if err := e.store.SaveSettings(context.Background(), st); err != nil {
		e.t.Fatal(err)
	}
}

// enqueue queues n e-mails to user0@example.com, user1@example.com, …
func (e *notifyEnv) enqueue(n int) {
	e.t.Helper()
	var msgs []store.OutboundEmail
	for i := range n {
		msgs = append(msgs, store.OutboundEmail{Email: fmt.Sprintf("user%d@example.com", i), Subject: "Hello", BodyText: "Hi", BodyHTML: "<p>Hi</p>"})
	}
	if err := e.store.EnqueueEmails(context.Background(), msgs); err != nil {
		e.t.Fatal(err)
	}
}

// graphRecipients returns the recipients of the accepted messages.
func graphRecipients(msgs []fakegraph.Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.To)
	}
	return out
}

func TestGraphDeliver(t *testing.T) {
	fg := fakegraph.Start(t)
	e := newNotifyEnv(t)
	e.configureGraph(fg)
	ctx := context.Background()

	id := e.incident("VPN down", store.ServiceImpact{ServiceID: e.vpn, Impact: store.StatusMajorOutage})
	if _, err := e.n.IncidentCreated(ctx, id); err != nil {
		t.Fatal(err)
	}
	e.n.deliver(ctx)

	got := fg.Messages()
	if rcpts := graphRecipients(got); !slices.Equal(rcpts, []string{"alice@example.com", "bob@example.com"}) {
		t.Fatalf("recipients = %v", rcpts)
	}
	for _, m := range got {
		if m.Sender != fg.Sender || m.Subject != "[Acme IT] Investigating: VPN down" || m.ContentType != "HTML" {
			t.Errorf("message = %+v", m)
		}
		assertContains(t, "html", m.Content, "<!DOCTYPE html>", "VPN down", "Major outage", "We are investigating.",
			"https://status.example.com/incidents/")
	}
	for _, n := range e.outbox() {
		if n.Status != store.NotificationSent || n.Attempts != 1 || n.SentAt == nil || n.LastError != "" {
			t.Errorf("notification after delivery = %+v", n)
		}
	}

	// Several batches use the same access token; nothing is sent twice.
	e.enqueue(2*batchSize + 7)
	e.n.deliver(ctx)
	e.n.deliver(ctx)
	if n := len(fg.Messages()); n != 2+2*batchSize+7 {
		t.Errorf("%d e-mails delivered, want %d", n, 2+2*batchSize+7)
	}
	if n := fg.TokenRequests(); n != 1 {
		t.Errorf("%d token requests, want 1", n)
	}
}

// TestGraphThrottled: when Microsoft Graph throttles the delivery, it
// pauses as long as Retry-After says (1 minute without it, at most 10
// minutes); the throttled e-mail and the rest keep their attempts.
func TestGraphThrottled(t *testing.T) {
	tests := []struct {
		name    string
		failure fakegraph.Failure
		pause   time.Duration
	}{
		{"429 with Retry-After", fakegraph.Throttled("120"), 2 * time.Minute},
		{"429 without Retry-After", fakegraph.Throttled(""), time.Minute},
		{"429 with a long Retry-After", fakegraph.Throttled("7200"), 10 * time.Minute},
		{"503", fakegraph.Unavailable(503, "30"), 30 * time.Second},
		{"504", fakegraph.Unavailable(504, ""), time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fg := fakegraph.Start(t)
			e := newNotifyEnv(t)
			e.configureGraph(fg)
			ctx := context.Background()
			e.enqueue(7)

			fg.SetLimit(3, tc.failure)
			e.n.deliver(ctx)
			if n, r := len(fg.Messages()), fg.Requests(); n != 3 || r != 4 {
				t.Fatalf("%d e-mails accepted after %d requests, want 3 after 4", n, r)
			}
			for i, n := range e.outbox() {
				switch {
				case i < 3 && (n.Status != store.NotificationSent || n.Attempts != 1):
					t.Errorf("e-mail %d = %+v, want sent", i, n)
				case i >= 3 && (n.Status != store.NotificationPending || n.Attempts != 0 || n.LastError != ""):
					t.Errorf("e-mail %d = %+v, want pending without a failed attempt", i, n)
				}
			}

			// Paused: no request until the pause is over.
			e.clock = e.clock.Add(tc.pause - time.Second)
			e.n.deliver(ctx)
			if r := fg.Requests(); r != 4 {
				t.Fatalf("%d requests during the pause, want none after the first 4", r-4)
			}
			e.clock = e.clock.Add(time.Second)
			fg.SetLimit(-1, fakegraph.Failure{})
			e.n.deliver(ctx)
			if n := len(fg.Messages()); n != 7 {
				t.Errorf("%d e-mails delivered after the pause, want 7", n)
			}
			for _, n := range e.outbox() {
				if n.Status != store.NotificationSent || n.Attempts != 1 {
					t.Errorf("%s: %+v, want sent at the first counted attempt", n.Email, n)
				}
			}
			// Throttling keeps the access token.
			if n := fg.TokenRequests(); n != 1 {
				t.Errorf("%d token requests, want 1", n)
			}
		})
	}
}

// TestGraphBatchFailure: a problem that concerns every e-mail (no access
// token, missing permission, unknown sender mailbox, server error, …) stops
// the batch and counts a failed attempt for every e-mail of it, with a
// readable error.
func TestGraphBatchFailure(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(fg *fakegraph.Server, e *notifyEnv)
		want    []string // parts of the recorded error
	}{
		{"access denied", func(fg *fakegraph.Server, e *notifyEnv) { fg.Respond(always(fakegraph.AccessDenied())) },
			[]string{"HTTP 403 (ErrorAccessDenied): Access is denied.", "application permission Mail.Send with admin consent", "status@contoso.example"}},
		{"no Exchange Online mailbox", func(fg *fakegraph.Server, e *notifyEnv) { fg.Respond(always(fakegraph.NoMailbox())) },
			[]string{"HTTP 404 (MailboxNotEnabledForRESTAPI)", "check the sender mailbox status@contoso.example"}},
		{"unknown sender", func(fg *fakegraph.Server, e *notifyEnv) {
			e.changeSettings(func(st *store.Settings) { st.GraphSender = "ghost@contoso.example" })
		}, []string{"HTTP 404 (ErrorInvalidUser): The requested user 'ghost@contoso.example' is invalid.", "must exist and have an Exchange Online mailbox"}},
		{"server error", func(fg *fakegraph.Server, e *notifyEnv) { fg.Respond(always(fakegraph.ServerError(500))) },
			[]string{"Microsoft Graph returned HTTP 500"}},
		{"bad gateway", func(fg *fakegraph.Server, e *notifyEnv) { fg.Respond(always(fakegraph.ServerError(502))) },
			[]string{"Microsoft Graph returned HTTP 502"}},
		{"wrong client secret", func(fg *fakegraph.Server, e *notifyEnv) {
			e.changeSettings(func(st *store.Settings) { st.OIDCClientSecret = "wrong" })
		}, []string{"could not get an access token from Microsoft Entra ID", "AADSTS7000215"}},
		{"unreachable", func(fg *fakegraph.Server, e *notifyEnv) {
			dead := httptest.NewServer(nil)
			dead.Close()
			msgraph.DefaultEndpoints = msgraph.Endpoints{Login: dead.URL, Graph: dead.URL} // restored by fakegraph
		}, []string{"could not reach Microsoft Entra ID to get an access token"}},
		{"not an Entra ID issuer", func(fg *fakegraph.Server, e *notifyEnv) {
			e.changeSettings(func(st *store.Settings) { st.OIDCIssuer = "https://keycloak.example.com/realms/it" })
		}, []string{"Microsoft Graph uses the app registration of the single sign-on settings: the issuer is not a Microsoft Entra ID issuer"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fg := fakegraph.Start(t)
			e := newNotifyEnv(t)
			e.configureGraph(fg)
			tc.prepare(fg, e)
			e.enqueue(3)
			e.n.deliver(context.Background())
			if r := fg.Requests(); r > 1 {
				t.Errorf("%d sendMail requests, want at most 1: the batch stops at the first failure", r)
			}
			for _, n := range e.outbox() {
				if n.Status != store.NotificationPending || n.Attempts != 1 || n.NextAttemptAt.Unix() != e.clock.Add(time.Minute).Unix() {
					t.Errorf("%s: %+v, want pending with one failed attempt, retried in a minute", n.Email, n)
				}
				assertContains(t, "error of "+n.Email, n.LastError, tc.want...)
			}
		})
	}
}

// always returns a Respond function that fails every request with f.
func always(f fakegraph.Failure) func(fakegraph.Message) *fakegraph.Failure {
	return func(fakegraph.Message) *fakegraph.Failure { return &f }
}

// TestGraphRetryAndGiveUp: like an unreachable SMTP server, a lasting
// failure marks the e-mails as failed after the last retry (about 80
// minutes), and they are not sent later.
func TestGraphRetryAndGiveUp(t *testing.T) {
	fg := fakegraph.Start(t)
	e := newNotifyEnv(t)
	e.configureGraph(fg)
	ctx := context.Background()
	e.enqueue(2)
	fg.Respond(always(fakegraph.AccessDenied()))

	for attempt, delay := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 0} {
		e.n.deliver(ctx)
		for _, n := range e.outbox() {
			if n.Attempts != attempt+1 {
				t.Fatalf("attempt %d: %+v", attempt+1, n)
			}
			if delay == 0 && n.Status != store.NotificationFailed {
				t.Errorf("after the last attempt: %+v, want failed", n)
			}
		}
		e.clock = e.clock.Add(delay)
	}
	// One request per round; a new access token after every failure.
	if r, tok := fg.Requests(), fg.TokenRequests(); r != 5 || tok != 5 {
		t.Errorf("%d requests and %d token requests, want 5 each", r, tok)
	}
	fg.Respond(nil)
	e.clock = e.clock.Add(24 * time.Hour)
	e.n.deliver(ctx)
	if n := len(fg.Messages()); n != 0 {
		t.Errorf("%d failed e-mails delivered", n)
	}
}

// TestGraphMessageFailure: an e-mail Microsoft Graph rejects (an invalid
// recipient) fails alone; the others are delivered.
func TestGraphMessageFailure(t *testing.T) {
	fg := fakegraph.Start(t)
	e := newNotifyEnv(t)
	e.configureGraph(fg)
	ctx := context.Background()

	fg.FailRecipient("bob@example.com", fakegraph.InvalidRecipient("bob@example.com"))
	id := e.incident("Outage",
		store.ServiceImpact{ServiceID: e.mail, Impact: store.StatusMajorOutage},
		store.ServiceImpact{ServiceID: e.vpn, Impact: store.StatusDegraded})
	if _, err := e.n.IncidentCreated(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.store.EnqueueEmails(ctx, []store.OutboundEmail{{Email: "not an address", Subject: "x", BodyText: "x"}}); err != nil {
		t.Fatal(err)
	}
	e.n.deliver(ctx)

	if rcpts := graphRecipients(fg.Messages()); !slices.Equal(rcpts, []string{"alice@example.com", "erin@example.com"}) {
		t.Errorf("delivered to %v", rcpts)
	}
	for _, n := range e.outbox() {
		switch n.Email {
		case "bob@example.com", "not an address":
			if n.Status != store.NotificationPending || n.Attempts != 1 {
				t.Errorf("%s: %+v, want one failed attempt", n.Email, n)
			}
			assertContains(t, "error of "+n.Email, n.LastError, "HTTP 400 (ErrorInvalidRecipients)", "Recipient '"+n.Email+"' is not resolved")
		default:
			if n.Status != store.NotificationSent || n.Attempts != 1 {
				t.Errorf("%s: %+v, want sent", n.Email, n)
			}
		}
	}
	if n := fg.TokenRequests(); n != 1 {
		t.Errorf("%d token requests, want 1 (a rejected e-mail keeps the token)", n)
	}
}

// TestGraphFailureMidBatch: e-mails accepted before a server error are
// recorded as sent; the failing one and the rest count a failed attempt.
func TestGraphFailureMidBatch(t *testing.T) {
	fg := fakegraph.Start(t)
	e := newNotifyEnv(t)
	e.configureGraph(fg)
	e.enqueue(4)
	fg.SetLimit(2, fakegraph.ServerError(500))
	e.n.deliver(context.Background())
	for i, n := range e.outbox() {
		if i < 2 && (n.Status != store.NotificationSent || n.Attempts != 1) {
			t.Errorf("e-mail %d = %+v, want sent", i, n)
		}
		if i >= 2 && (n.Status != store.NotificationPending || n.Attempts != 1 || !strings.Contains(n.LastError, "HTTP 500")) {
			t.Errorf("e-mail %d = %+v, want a failed attempt", i, n)
		}
	}
	if r := fg.Requests(); r != 3 {
		t.Errorf("%d requests, want 3", r)
	}
}

// TestGraphNewTokenAfterFailure: after a delivery failed (e.g. because the
// Mail.Send permission was missing), the next attempt gets a new access
// token, which contains a permission granted meanwhile.
func TestGraphNewTokenAfterFailure(t *testing.T) {
	fg := fakegraph.Start(t)
	e := newNotifyEnv(t)
	e.configureGraph(fg)
	ctx := context.Background()
	e.enqueue(2)

	fg.FailNext(fakegraph.AccessDenied())
	e.n.deliver(ctx)
	e.clock = e.clock.Add(time.Minute)
	e.n.deliver(ctx)
	if n, tok := len(fg.Messages()), fg.TokenRequests(); n != 2 || tok != 2 {
		t.Errorf("%d e-mails delivered with %d token requests, want 2 and 2", n, tok)
	}

	// A revoked access token: HTTP 401, then a new token.
	fg.RevokeTokens()
	e.enqueue(1)
	e.n.deliver(ctx)
	if n := e.outbox()[2]; n.Attempts != 1 || !strings.Contains(n.LastError, "HTTP 401") || !strings.Contains(n.LastError, "did not accept the access token") {
		t.Errorf("e-mail after the token was revoked: %+v", n)
	}
	e.clock = e.clock.Add(time.Minute)
	e.n.deliver(ctx)
	if n, tok := len(fg.Messages()), fg.TokenRequests(); n != 3 || tok != 3 {
		t.Errorf("%d e-mails delivered with %d token requests, want 3 and 3", n, tok)
	}

	// New credentials in the settings: a new client.
	fg.ClientSecret = "new-secret"
	e.changeSettings(func(st *store.Settings) { st.OIDCClientSecret = "new-secret" })
	e.enqueue(1)
	e.n.deliver(ctx)
	if n, tok := len(fg.Messages()), fg.TokenRequests(); n != 4 || tok != 4 {
		t.Errorf("%d e-mails delivered with %d token requests, want 4 and 4", n, tok)
	}
}

// TestGraphRecordsEachResult: a delivered e-mail is recorded right away,
// not after the whole batch.
func TestGraphRecordsEachResult(t *testing.T) {
	fg := fakegraph.Start(t)
	e := newNotifyEnv(t)
	e.configureGraph(fg)
	e.enqueue(2)
	hold := make(chan struct{})
	fg.Respond(func(m fakegraph.Message) *fakegraph.Failure {
		if m.To == "user1@example.com" {
			<-hold
		}
		return nil
	})
	done := make(chan struct{})
	go func() {
		e.n.deliver(context.Background())
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for e.outbox()[0].Status != store.NotificationSent {
		if time.Now().After(deadline) {
			t.Fatal("the first e-mail was not recorded as sent while the second one was being delivered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := e.outbox()[1]; n.Status != store.NotificationPending || n.Attempts != 0 {
		t.Errorf("second e-mail = %+v", n)
	}
	close(hold)
	<-done
	if n := e.outbox()[1]; n.Status != store.NotificationSent {
		t.Errorf("second e-mail after delivery = %+v", n)
	}
}

// TestGraphAndSMTP: each batch uses the method chosen in the settings.
func TestGraphAndSMTP(t *testing.T) {
	fg := fakegraph.Start(t)
	e := newNotifyEnv(t)
	ctx := context.Background()
	e.enqueue(1)
	e.n.deliver(ctx) // e-mail is not configured: the e-mail waits
	if n := e.outbox()[0]; n.Status != store.NotificationPending || n.Attempts != 0 {
		t.Fatalf("e-mail = %+v", n)
	}
	e.configureGraph(fg)
	e.n.deliver(ctx)
	if n := len(fg.Messages()); n != 1 {
		t.Fatalf("%d e-mails sent with Microsoft Graph, want 1", n)
	}

	srv := e.startSMTP(nil) // switches to SMTP
	if err := e.store.EnqueueEmails(ctx, []store.OutboundEmail{{Email: "smtp@example.com", Subject: "x", BodyText: "x"}}); err != nil {
		t.Fatal(err)
	}
	e.n.deliver(ctx)
	if got := srv.received(); len(got) != 1 || got[0].To[0] != "smtp@example.com" || len(fg.Messages()) != 1 {
		t.Errorf("SMTP received %+v, Microsoft Graph %d e-mails", got, len(fg.Messages()))
	}
}

func TestSendTestEmailGraph(t *testing.T) {
	fg := fakegraph.Start(t)
	e := newNotifyEnv(t)
	ctx := context.Background()

	if err := e.n.SendTestEmail(ctx, "admin@example.com"); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("without settings: error = %v", err)
	}
	e.configureGraph(fg)
	if err := e.n.SendTestEmail(ctx, " admin@example.com "); err != nil {
		t.Fatalf("SendTestEmail: %v", err)
	}
	got := fg.Messages()
	if len(got) != 1 || got[0].To != "admin@example.com" || got[0].Sender != fg.Sender || got[0].Subject != "[Acme IT] Test e-mail" {
		t.Fatalf("messages = %+v", got)
	}
	assertContains(t, "html", got[0].Content, "E-mail delivery works.", "Sent with", "Microsoft 365 (Microsoft Graph)", fg.Sender,
		"https://status.example.com/")
	if n := len(e.outbox()); n != 0 {
		t.Errorf("test e-mail written to the outbox (%d rows)", n)
	}

	fg.FailNext(fakegraph.AccessDenied())
	err := e.n.SendTestEmail(ctx, "admin@example.com")
	if err == nil {
		t.Fatal("access denied: no error")
	}
	assertContains(t, "access denied error", err.Error(), "HTTP 403", "Mail.Send")

	fg.FailNext(fakegraph.Throttled("30"))
	err = e.n.SendTestEmail(ctx, "admin@example.com")
	if err == nil || err.Error() != "Microsoft Graph returned HTTP 429 (ApplicationThrottled): Application is over its MailboxConcurrency limit. (retry after 30s)" {
		t.Errorf("throttled: error = %v", err)
	}

	// Every test e-mail gets a new access token: a permission granted
	// meanwhile applies at once.
	if n := fg.TokenRequests(); n != 3 {
		t.Errorf("%d token requests for 3 test e-mails, want 3", n)
	}

	e.changeSettings(func(st *store.Settings) { st.OIDCClientSecret = "wrong" })
	if err := e.n.SendTestEmail(ctx, "admin@example.com"); err == nil || !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Errorf("wrong client secret: error = %v", err)
	}
}
