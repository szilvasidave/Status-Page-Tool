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
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// ErrNotEnabled is returned when directory sync is disabled or the OIDC
// issuer is not a Microsoft Entra ID issuer.
var ErrNotEnabled = errors.New("directory sync is not enabled")

// searchLimit is the maximum number of groups returned by SearchGroups.
const searchLimit = 20

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

	syncMu sync.Mutex // serialises sync runs

	clientMu  sync.Mutex
	client    *GraphClient // cached so that its access token is reused
	clientFor [3]string    // issuer, client id and secret of client

	// Replaced in tests.
	newClient  func(issuer, clientID, clientSecret string) (*GraphClient, error)
	firstDelay time.Duration // first sync after start
	interval   time.Duration // between syncs
}

// NewSyncer creates a Syncer.
func NewSyncer(st *store.Store, log *slog.Logger) *Syncer {
	if log == nil {
		log = slog.Default()
	}
	return &Syncer{
		store:      st,
		log:        log,
		newClient:  NewGraphClient,
		firstDelay: 30 * time.Second,
		interval:   time.Hour,
	}
}

// Enabled reports whether directory sync is enabled and possible with the
// current settings.
func (s *Syncer) Enabled(ctx context.Context) bool {
	_, err := s.graphClient(ctx)
	return err == nil
}

// graphClient returns the Graph client for the current settings or an error
// wrapping ErrNotEnabled.
func (s *Syncer) graphClient(ctx context.Context) (*GraphClient, error) {
	st, err := s.store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	if !st.DirectorySyncEnabled {
		return nil, ErrNotEnabled
	}
	key := [3]string{strings.TrimSpace(st.OIDCIssuer), strings.TrimSpace(st.OIDCClientID), st.OIDCClientSecret}
	if key[0] == "" || key[1] == "" || key[2] == "" {
		return nil, fmt.Errorf("%w: the OIDC issuer, client ID and client secret are required for directory sync", ErrNotEnabled)
	}

	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if s.client == nil || s.clientFor != key {
		c, err := s.newClient(key[0], key[1], key[2])
		if err != nil {
			return nil, err
		}
		s.client, s.clientFor = c, key
	}
	return s.client, nil
}

// SyncNow synchronises all groups returned by store.GroupsToSync.
//
// Problems with single groups are reported in Result.Errors and do not stop
// the run. An error is returned if the sync is not enabled, or if it had to
// stop because Microsoft Graph cannot be used at all (e.g. a wrong client
// secret).
func (s *Syncer) SyncNow(ctx context.Context) (Result, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	var res Result
	c, err := s.graphClient(ctx)
	if err != nil {
		return res, err
	}
	groups, err := s.store.GroupsToSync(ctx)
	if err != nil {
		return res, err
	}
	start := time.Now()
	for i := range groups {
		g := &groups[i]
		if !IsGUID(g.ExternalID) {
			res.Errors = append(res.Errors, fmt.Sprintf("group %q skipped: not a directory object id", g.ExternalID))
			continue
		}
		members, err := s.syncGroup(ctx, c, g)
		if err != nil {
			if stopsSync(ctx, err) {
				return res, err
			}
			res.Errors = append(res.Errors, groupProblem(g, err))
			continue
		}
		res.Groups++
		res.Members += members
	}

	s.log.Info("directory sync finished", "groups", res.Groups, "members", res.Members,
		"problems", len(res.Errors), "duration", time.Since(start).Round(time.Millisecond))
	for _, p := range res.Errors {
		s.log.Warn("directory sync", "problem", p)
	}
	return res, nil
}

// syncGroup reads the name and the members of g from the directory and
// stores them. It returns the number of members.
func (s *Syncer) syncGroup(ctx context.Context, c *GraphClient, g *store.Group) (int, error) {
	dg, err := c.Group(ctx, g.ExternalID)
	if err != nil {
		return 0, err
	}
	members, err := c.GroupMembers(ctx, g.ExternalID)
	if err != nil {
		return 0, err
	}
	if err := s.store.SyncGroupMembers(ctx, g.ID, dg.Name, members); err != nil {
		return 0, err
	}
	return len(members), nil
}

// stopsSync reports whether err affects every group, so that a sync run
// should stop instead of trying the other groups.
func stopsSync(ctx context.Context, err error) bool {
	var ce *connectionError
	var ge *graphError
	return ctx.Err() != nil || errors.As(err, &ce) ||
		errors.As(err, &ge) && ge.StatusCode == http.StatusUnauthorized
}

// groupProblem describes a per-group sync error.
func groupProblem(g *store.Group, err error) string {
	label := g.ExternalID
	if g.Name != "" && g.Name != g.ExternalID {
		label = fmt.Sprintf("%q (%s)", g.Name, g.ExternalID)
	}
	if errors.Is(err, ErrGroupNotFound) {
		return "group " + label + " no longer exists in the directory"
	}
	return "group " + label + ": " + err.Error()
}

// SearchGroups searches directory groups by display name prefix (for the
// "add group" form).
func (s *Syncer) SearchGroups(ctx context.Context, query string) ([]Group, error) {
	c, err := s.graphClient(ctx)
	if err != nil {
		return nil, err
	}
	return c.SearchGroups(ctx, query, searchLimit)
}

// Run syncs once shortly after start and then every hour while enabled. It
// blocks until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) {
	t := time.NewTimer(s.firstDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.SyncNow(ctx); errors.Is(err, ErrNotEnabled) {
			s.log.Debug("directory sync skipped", "reason", err)
		} else if err != nil && ctx.Err() == nil {
			s.log.Error("directory sync failed", "err", err)
		}
		t.Reset(s.interval)
	}
}
