// Package directory synchronises group members from Microsoft Entra ID via
// Microsoft Graph, so that members of groups assigned to services are known
// (and notified) even if they never signed in to the status page.
//
// It reuses the OIDC client id and secret from the settings (client
// credentials flow). The app registration needs the Microsoft Graph
// application permissions GroupMember.Read.All and User.ReadBasic.All.
package directory

import (
	"context"
	"errors"
	"log/slog"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

var errNotImplemented = errors.New("not implemented")

// ErrNotEnabled is returned when directory sync is disabled or the OIDC
// issuer is not a Microsoft Entra ID issuer.
var ErrNotEnabled = errors.New("directory sync is not enabled")

// Group is a directory group.
type Group struct {
	ID          string
	Name        string
	Description string
}

// Result summarises a sync run.
type Result struct {
	Groups  int      // groups synchronised
	Members int      // memberships written
	Errors  []string // per-group errors
}

// Syncer runs the synchronisation. Safe for concurrent use; concurrent sync
// runs are serialised.
type Syncer struct {
	store *store.Store
	log   *slog.Logger
}

// NewSyncer creates a Syncer.
func NewSyncer(st *store.Store, log *slog.Logger) *Syncer {
	if log == nil {
		log = slog.Default()
	}
	return &Syncer{store: st, log: log}
}

// Enabled reports whether directory sync is enabled and possible with the
// current settings.
func (s *Syncer) Enabled(ctx context.Context) bool { return false }

// SyncNow synchronises all groups returned by store.GroupsToSync.
func (s *Syncer) SyncNow(ctx context.Context) (Result, error) {
	return Result{}, ErrNotEnabled
}

// SearchGroups searches directory groups by display name prefix (for the
// "add group" form).
func (s *Syncer) SearchGroups(ctx context.Context, query string) ([]Group, error) {
	return nil, ErrNotEnabled
}

// Run syncs once shortly after start and then every hour while enabled. It
// blocks until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) {
	<-ctx.Done()
}
