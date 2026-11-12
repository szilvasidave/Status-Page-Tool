// Package notify turns incidents and maintenance windows into e-mails for the
// members of the identity provider groups assigned to the affected services.
// E-mails are written to the notifications outbox table and delivered by a
// background worker (Run) so that sending never blocks a web request and
// failed deliveries are retried.
package notify

import (
	"context"
	"errors"
	"log/slog"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

var errNotImplemented = errors.New("not implemented")

// Notifier is safe for concurrent use.
type Notifier struct {
	store *store.Store
	log   *slog.Logger
	wake  chan struct{} // signals the outbox worker that new mail is queued
}

// New creates a Notifier.
func New(st *store.Store, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{store: st, log: log, wake: make(chan struct{}, 1)}
}

// The following methods are called by the web handlers after a successful
// change when the editor ticked "notify subscribers". They look up the
// recipients (members of groups assigned to the affected services), render
// the e-mail and enqueue one message per recipient. They return the number of
// queued e-mails. If SMTP is not configured they queue nothing and return 0.

// IncidentCreated notifies about a new incident.
func (n *Notifier) IncidentCreated(ctx context.Context, incidentID int64) (int, error) {
	return 0, errNotImplemented
}

// IncidentUpdated notifies about the latest update of an incident. Members of
// groups for services that were affected before the update
// (previousServiceIDs) are notified as well.
func (n *Notifier) IncidentUpdated(ctx context.Context, incidentID int64, previousServiceIDs []int64) (int, error) {
	return 0, errNotImplemented
}

// MaintenanceScheduled notifies about a newly scheduled maintenance.
func (n *Notifier) MaintenanceScheduled(ctx context.Context, maintenanceID int64) (int, error) {
	return 0, errNotImplemented
}

// MaintenanceUpdated notifies about a changed maintenance window.
func (n *Notifier) MaintenanceUpdated(ctx context.Context, maintenanceID int64, previousServiceIDs []int64) (int, error) {
	return 0, errNotImplemented
}

// MaintenanceCancelled notifies that a maintenance was cancelled.
func (n *Notifier) MaintenanceCancelled(ctx context.Context, maintenanceID int64) (int, error) {
	return 0, errNotImplemented
}

// SendTestEmail sends a test e-mail synchronously with the current SMTP
// settings and returns the delivery error, if any.
func (n *Notifier) SendTestEmail(ctx context.Context, to string) error {
	return errNotImplemented
}

// Run processes the outbox (with retries), sends "maintenance started" and
// "maintenance completed" e-mails and removes old notifications. It blocks
// until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context) {
	<-ctx.Done()
}
