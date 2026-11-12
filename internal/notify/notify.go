// Package notify turns incidents and maintenance windows into e-mails for the
// members of the identity provider groups assigned to the affected services.
// E-mails are written to the notifications outbox table and delivered by a
// background worker (Run) so that sending never blocks a web request and
// failed deliveries are retried.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

const (
	// batchSize is the number of e-mails delivered over one SMTP connection.
	batchSize = 50
	// keepNotifications is how long sent and failed e-mails are kept.
	keepNotifications = 90 * 24 * time.Hour
)

// retryDelays are the waits after the first, second, … failed delivery
// attempt. After len(retryDelays)+1 failed attempts an e-mail is given up
// (status failed).
var retryDelays = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// Notifier is safe for concurrent use.
type Notifier struct {
	store *store.Store
	log   *slog.Logger
	wake  chan struct{} // signals the outbox worker that new mail is queued

	// Clock and worker intervals; tests replace them.
	now                 func() time.Time
	outboxInterval      time.Duration // delivery of due e-mails (retries)
	maintenanceInterval time.Duration // "maintenance started/completed" check
	cleanupInterval     time.Duration // removal of old e-mails
}

// New creates a Notifier.
func New(st *store.Store, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{
		store:               st,
		log:                 log,
		wake:                make(chan struct{}, 1),
		now:                 time.Now,
		outboxInterval:      15 * time.Second,
		maintenanceInterval: 30 * time.Second,
		cleanupInterval:     24 * time.Hour,
	}
}

// The following methods are called by the web handlers after a successful
// change when the editor ticked "notify subscribers". They look up the
// recipients (members of groups assigned to the affected services), render
// the e-mail and enqueue one message per recipient. They return the number of
// queued e-mails. If SMTP is not configured they queue nothing and return 0.

// IncidentCreated notifies about a new incident.
func (n *Notifier) IncidentCreated(ctx context.Context, incidentID int64) (int, error) {
	return n.incident(ctx, incidentID, nil, true)
}

// IncidentUpdated notifies about the latest update of an incident. Members of
// groups for services that were affected before the update
// (previousServiceIDs) are notified as well.
func (n *Notifier) IncidentUpdated(ctx context.Context, incidentID int64, previousServiceIDs []int64) (int, error) {
	return n.incident(ctx, incidentID, previousServiceIDs, false)
}

// MaintenanceScheduled notifies about a newly scheduled maintenance.
func (n *Notifier) MaintenanceScheduled(ctx context.Context, maintenanceID int64) (int, error) {
	return n.maintenance(ctx, maintenanceID, MaintenanceEventScheduled, nil)
}

// MaintenanceUpdated notifies about a changed maintenance window.
func (n *Notifier) MaintenanceUpdated(ctx context.Context, maintenanceID int64, previousServiceIDs []int64) (int, error) {
	return n.maintenance(ctx, maintenanceID, MaintenanceEventUpdated, previousServiceIDs)
}

// MaintenanceCancelled notifies that a maintenance was cancelled.
func (n *Notifier) MaintenanceCancelled(ctx context.Context, maintenanceID int64) (int, error) {
	return n.maintenance(ctx, maintenanceID, MaintenanceEventCancelled, nil)
}

func (n *Notifier) incident(ctx context.Context, id int64, previousServiceIDs []int64, isNew bool) (int, error) {
	about := "incident " + strconv.FormatInt(id, 10)
	st, err := n.mailSettings(ctx, about)
	if st == nil {
		return 0, err
	}
	inc, err := n.store.GetIncident(ctx, id)
	if err != nil {
		return 0, err
	}
	e := RenderIncident(st, inc, nil, isNew) // nil: the latest update
	return n.queue(ctx, about, append(inc.ServiceIDs(), previousServiceIDs...), e)
}

func (n *Notifier) maintenance(ctx context.Context, id int64, ev MaintenanceEvent, previousServiceIDs []int64) (int, error) {
	about := fmt.Sprintf("maintenance %d (%s)", id, ev)
	st, err := n.mailSettings(ctx, about)
	if st == nil {
		return 0, err
	}
	m, err := n.store.GetMaintenance(ctx, id)
	if err != nil {
		return 0, err
	}
	return n.queue(ctx, about, append(m.ServiceIDs(), previousServiceIDs...), RenderMaintenance(st, m, ev))
}

// mailSettings returns the settings, or nil if e-mail is not configured.
func (n *Notifier) mailSettings(ctx context.Context, about string) (*store.Settings, error) {
	st, err := n.store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	if !st.SMTPConfigured() {
		n.log.Debug("e-mail is not configured, no notification sent", "about", about)
		return nil, nil
	}
	return st, nil
}

// queue enqueues e for every recipient of the services and wakes the outbox
// worker. It returns the number of queued e-mails.
func (n *Notifier) queue(ctx context.Context, about string, serviceIDs []int64, e Email) (int, error) {
	recipients, err := n.store.RecipientsForServices(ctx, serviceIDs)
	if err != nil {
		return 0, err
	}
	if len(recipients) == 0 {
		n.log.Info("no e-mail recipients: nobody with an e-mail address is in a group assigned to the affected services", "about", about)
		return 0, nil
	}
	msgs := make([]store.OutboundEmail, len(recipients))
	for i, r := range recipients {
		msgs[i] = store.OutboundEmail{UserID: r.UserID, Email: r.Email, Subject: e.Subject, BodyText: e.Text, BodyHTML: e.HTML}
	}
	if err := n.store.EnqueueEmails(ctx, msgs); err != nil {
		return 0, err
	}
	n.log.Info("e-mails queued", "about", about, "count", len(msgs))
	select {
	case n.wake <- struct{}{}:
	default: // the worker has already been woken
	}
	return len(msgs), nil
}

// SendTestEmail sends a test e-mail synchronously with the current SMTP
// settings and returns the delivery error, if any.
func (n *Notifier) SendTestEmail(ctx context.Context, to string) error {
	to = strings.TrimSpace(to)
	if to == "" {
		return errors.New("enter the address to send the test e-mail to")
	}
	st, err := n.store.GetSettings(ctx)
	if err != nil {
		return err
	}
	e := renderTestEmail(st)
	errs, err := Send(ctx, st, []Message{{To: to, Subject: e.Subject, Text: e.Text, HTML: e.HTML}})
	if err == nil {
		err = errs[0]
	}
	if err != nil {
		n.log.Warn("test e-mail failed", "to", to, "err", err)
		return err
	}
	n.log.Info("test e-mail sent", "to", to)
	return nil
}

// renderTestEmail renders the test e-mail sent from the settings page.
func renderTestEmail(st *store.Settings) Email {
	st = settingsOrDefault(st)
	server := strings.TrimSpace(st.SMTPHost)
	if st.SMTPPort != 0 {
		server = net.JoinHostPort(server, strconv.Itoa(st.SMTPPort))
	}
	security := map[string]string{"starttls": "STARTTLS", "tls": "TLS", "none": "None"}[strings.ToLower(st.SMTPSecurity)]
	if security == "" {
		security = st.SMTPSecurity
	}
	v := emailView{
		Subject:    subject(st, "Test e-mail"),
		SiteName:   st.SiteName,
		Badge:      "Test",
		BadgeColor: colorOperational,
		Title:      "Test e-mail",
		Intro:      "E-mail delivery works. Notifications will be sent with these settings.",
		Facts: []emailFact{
			{"SMTP server", server},
			{"Security", security},
			{"Sender", st.SMTPFrom},
		},
		Footer: "You receive this e-mail because an administrator sent a test e-mail from the settings page.",
	}
	if base := baseURL(st); base != "" {
		v.LinkURL = base + "/"
		v.LinkText = "Open the status page"
	}
	return render(v)
}

// Run processes the outbox (with retries), sends "maintenance started" and
// "maintenance completed" e-mails and removes old notifications. It blocks
// until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context) {
	outbox := time.NewTicker(n.outboxInterval)
	defer outbox.Stop()
	maintenance := time.NewTicker(n.maintenanceInterval)
	defer maintenance.Stop()
	cleanup := time.NewTicker(n.cleanupInterval)
	defer cleanup.Stop()

	n.cleanup(ctx)
	n.checkMaintenances(ctx)
	n.deliver(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.wake:
			n.deliver(ctx)
		case <-outbox.C:
			n.deliver(ctx)
		case <-maintenance.C:
			n.checkMaintenances(ctx) // wakes the delivery if e-mails were queued
		case <-cleanup.C:
			n.cleanup(ctx)
		}
	}
}

// deliver sends due e-mails in batches until none are left. It stops early
// when the SMTP server cannot be reached; the next run tries again.
func (n *Notifier) deliver(ctx context.Context) {
	for ctx.Err() == nil {
		st, err := n.store.GetSettings(ctx)
		if err != nil {
			n.logError(ctx, "deliver e-mails", err)
			return
		}
		if !st.SMTPConfigured() {
			return // queued e-mails wait until e-mail is configured again
		}
		due, err := n.store.DueNotifications(ctx, n.now(), batchSize)
		if err != nil {
			n.logError(ctx, "deliver e-mails", err)
			return
		}
		if len(due) == 0 || !n.sendBatch(ctx, st, due) || len(due) < batchSize {
			return
		}
	}
}

// sendBatch delivers the notifications over one SMTP connection and records
// the results. It reports whether the delivery may go on with the next batch.
func (n *Notifier) sendBatch(ctx context.Context, st *store.Settings, due []store.Notification) bool {
	msgs := make([]Message, len(due))
	for i, d := range due {
		msgs[i] = Message{To: d.Email, Subject: d.Subject, Text: d.BodyText, HTML: d.BodyHTML}
	}
	errs, connErr := Send(ctx, st, msgs)
	if connErr != nil && ctx.Err() == nil {
		n.log.Warn("e-mails not delivered, will retry", "count", len(msgs), "err", connErr)
	}

	// Record the results even if ctx has been cancelled meanwhile, so that
	// delivered e-mails are not sent again after a restart.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	now := n.now()
	ok := connErr == nil
	sent, failed := 0, 0
	for i, d := range due {
		sendErr := errs[i]
		if sendErr != nil && ctx.Err() != nil &&
			(errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded)) {
			continue // interrupted by shutdown: not an attempt, retried after the restart
		}
		var err error
		if sendErr == nil {
			sent++
			err = n.store.MarkNotificationSent(rctx, d.ID)
		} else {
			failed++
			retryAt := nextAttempt(now, d.Attempts+1)
			switch {
			case retryAt == nil:
				n.log.Warn("giving up on e-mail", "id", d.ID, "to", d.Email, "attempts", d.Attempts+1, "err", sendErr)
			case connErr == nil: // connection errors are logged once above
				n.log.Warn("e-mail not delivered, will retry", "id", d.ID, "to", d.Email, "retry_at", retryAt.Format(time.RFC3339), "err", sendErr)
			}
			err = n.store.MarkNotificationAttemptFailed(rctx, d.ID, sendErr.Error(), retryAt)
		}
		if err != nil {
			n.log.Error("record e-mail delivery", "id", d.ID, "err", err)
			ok = false
		}
	}
	if sent > 0 {
		n.log.Info("e-mails sent", "count", sent, "failed", failed)
	}
	return ok && ctx.Err() == nil
}

// nextAttempt returns when to retry after the given number of failed
// attempts, or nil to give up.
func nextAttempt(now time.Time, attempts int) *time.Time {
	if attempts < 1 || attempts > len(retryDelays) {
		return nil
	}
	t := now.Add(retryDelays[attempts-1])
	return &t
}

// checkMaintenances queues the "maintenance started" and "maintenance
// completed" e-mails for maintenance windows with notifications enabled. A
// window that has already ended only gets the "completed" e-mail. The
// windows are marked as notified even if e-mail is not configured or nobody
// is subscribed, so that no outdated e-mails are sent later.
func (n *Notifier) checkMaintenances(ctx context.Context) {
	now := n.now()
	ms, err := n.store.MaintenancesToNotify(ctx, now)
	if err != nil || len(ms) == 0 {
		n.logError(ctx, "check maintenance windows", err)
		return
	}
	st, err := n.store.GetSettings(ctx)
	if err != nil {
		n.logError(ctx, "check maintenance windows", err)
		return
	}
	for i := range ms {
		m := &ms[i]
		ended := !m.EndsAt.After(now)
		ev := MaintenanceEventStarted
		if ended {
			ev = MaintenanceEventCompleted
		}
		// An ended window whose "completed" e-mail went out only needs its
		// start flag set.
		if !ended || !m.NotifiedEnd {
			about := fmt.Sprintf("maintenance %d (%s)", m.ID, ev)
			if !st.SMTPConfigured() {
				n.log.Debug("e-mail is not configured, no notification sent", "about", about)
			} else if _, err := n.queue(ctx, about, m.ServiceIDs(), RenderMaintenance(st, m, ev)); err != nil {
				n.logError(ctx, "queue maintenance e-mails", err)
				continue // try again next time
			}
		}
		if !m.NotifiedStart {
			if err := n.store.MarkMaintenanceNotified(ctx, m.ID, true); err != nil {
				n.logError(ctx, "check maintenance windows", err)
			}
		}
		if ended && !m.NotifiedEnd {
			if err := n.store.MarkMaintenanceNotified(ctx, m.ID, false); err != nil {
				n.logError(ctx, "check maintenance windows", err)
			}
		}
	}
}

// cleanup removes sent and failed e-mails older than keepNotifications.
func (n *Notifier) cleanup(ctx context.Context) {
	err := n.store.DeleteNotificationsBefore(ctx, n.now().Add(-keepNotifications))
	n.logError(ctx, "remove old e-mails", err)
}

// logError logs err unless it is nil or caused by shutting down.
func (n *Notifier) logError(ctx context.Context, action string, err error) {
	if err != nil && ctx.Err() == nil {
		n.log.Error(action, "err", err)
	}
}
