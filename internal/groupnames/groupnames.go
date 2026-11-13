// Package groupnames looks up the names of groups in Microsoft Entra ID.
//
// The groups claim of Microsoft Entra ID holds the object IDs of the user's
// groups, not their names, so a group that a sign-in records is only known
// by its object ID. With "Look up group names in Microsoft Entra ID" turned
// on (store.Settings.EntraGroupNames), a Resolver reads the display names of
// these groups with Microsoft Graph, with the app registration of the single
// sign-on settings, which needs the application permission
// GroupMember.Read.All. The names are stored as the groups' directory names
// (store.Group.DirectoryName); a name given by an administrator takes
// precedence. Provisioned groups are skipped: provisioning sends their names.
//
// The worker (Run) looks up the names of new groups shortly after the
// sign-in that recorded them (Wake), and every name again once a day, so that
// renamed groups follow. Administrators can look up all names at once
// (LookUpAll), and the name of a group they add by its object ID
// (LookUpGroup).
package groupnames

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/msgraph"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

const (
	// checkInterval is how often the worker looks for groups to look up.
	checkInterval = time.Hour
	// refreshAfter is how long a name is kept before it is looked up again.
	refreshAfter = 24 * time.Hour
	// wakeDelay is how long the worker waits after a Wake, so that the new
	// groups of sign-ins in quick succession are looked up together.
	wakeDelay = 2 * time.Second
	// errorPause is how long the worker pauses after a failure while the
	// app registration stays the same; when Microsoft Graph throttles, its
	// Retry-After (at most errorPause) is used instead.
	errorPause = time.Hour
)

// ErrOff is returned by LookUpAll and LookUpGroup while the lookup is off
// (see store.Settings.GroupNamesActive).
var ErrOff = errors.New("looking up group names in Microsoft Entra ID is turned off")

// Status is the outcome of the latest lookup, by the worker or an
// administrator.
type Status struct {
	At     time.Time // when it ended; zero: no lookup since the start
	Err    string    // why it failed; "" if it succeeded
	Groups int       // groups looked up (before a failure)
	Found  int       // of these, the groups Microsoft Entra ID knows
}

// Resolver looks up group names. It is safe for concurrent use; its methods
// may be called on a nil *Resolver, which does nothing.
type Resolver struct {
	store *store.Store
	log   *slog.Logger
	wake  chan struct{} // signals the worker that new groups were recorded
	sem   chan struct{} // held during a lookup: one at a time

	// Clock and intervals; tests replace them.
	now           func() time.Time
	checkInterval time.Duration
	refreshAfter  time.Duration
	wakeDelay     time.Duration
	errorPause    time.Duration

	mu          sync.Mutex
	graph       *msgraph.Client // client for graphFor, reused for its access token
	graphFor    [3]string       // issuer, client ID and client secret of graph
	status      Status
	pausedUntil time.Time // the worker pauses after a failure …
	pausedFor   [3]string // … while the app registration stays the same
}

// New creates a Resolver.
func New(st *store.Store, log *slog.Logger) *Resolver {
	if log == nil {
		log = slog.Default()
	}
	return &Resolver{
		store:         st,
		log:           log,
		wake:          make(chan struct{}, 1),
		sem:           make(chan struct{}, 1),
		now:           time.Now,
		checkInterval: checkInterval,
		refreshAfter:  refreshAfter,
		wakeDelay:     wakeDelay,
		errorPause:    errorPause,
	}
}

// Wake makes the worker look up the names of the groups that were never
// looked up, e.g. after a sign-in that may have recorded new groups. It does
// not block.
func (r *Resolver) Wake() {
	if r == nil {
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Status returns the outcome of the latest lookup.
func (r *Resolver) Status() Status {
	if r == nil {
		return Status{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Run looks up names until ctx is cancelled: at the start, shortly after a
// Wake and every hour, the names of the groups that were never looked up or
// not for a day (see lookUpDue).
func (r *Resolver) Run(ctx context.Context) {
	t := time.NewTicker(r.checkInterval)
	defer t.Stop()
	for {
		r.lookUpDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.wake:
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.wakeDelay):
			}
		}
	}
}

// lookUpDue looks up the names of the groups that were never looked up or
// not since refreshAfter, unless the lookup is off or pauses after a
// failure.
func (r *Resolver) lookUpDue(ctx context.Context) {
	st, err := r.store.GetSettings(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("group names: could not load the settings", "err", err)
		}
		return
	}
	if !st.GroupNamesActive() || r.paused(st) {
		return
	}
	groups, err := r.store.GroupsForNameLookup(ctx, r.now().Add(-r.refreshAfter))
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("group names: could not list the groups to look up", "err", err)
		}
		return
	}
	if len(groups) == 0 {
		return
	}
	res, err := r.lookUp(ctx, st, groups)
	switch {
	case ctx.Err() != nil:
		// Shutting down.
	case err != nil:
		r.log.Warn("could not look up the names of groups in Microsoft Entra ID; the status page tries again later",
			"groups", len(groups), "looked_up", res.Groups, "err", err)
	default:
		r.log.Info("looked up the names of groups in Microsoft Entra ID", "groups", res.Groups, "found", res.Found)
	}
}

// LookUpAll looks up the names of all groups whose names Microsoft Entra ID
// may know (see store.Store.GroupsForNameLookup) at once, whether they are
// due or not (Admin → Groups). It returns ErrOff while the lookup is off.
func (r *Resolver) LookUpAll(ctx context.Context) (Status, error) {
	if r == nil {
		return Status{}, ErrOff
	}
	st, err := r.store.GetSettings(ctx)
	if err != nil {
		return Status{}, err
	}
	if !st.GroupNamesActive() {
		return Status{}, ErrOff
	}
	groups, err := r.store.GroupsForNameLookup(ctx, time.Time{})
	if err != nil {
		return Status{}, err
	}
	if len(groups) == 0 {
		return Status{At: r.now()}, nil
	}
	res, err := r.lookUp(ctx, st, groups)
	switch {
	case err != nil && ctx.Err() != nil:
		r.log.Info("looking up the names of groups in Microsoft Entra ID was interrupted; the rest follows in the background",
			"groups", len(groups), "looked_up", res.Groups)
		r.Wake()
	case err != nil:
		r.log.Warn("could not look up the names of groups in Microsoft Entra ID", "groups", len(groups), "looked_up", res.Groups, "err", err)
	default:
		r.log.Info("looked up the names of all groups in Microsoft Entra ID", "groups", res.Groups, "found", res.Found)
	}
	return res, err
}

// LookUpGroup looks up the name of the group g, e.g. one an administrator
// just added by its object ID, and returns the group as stored afterwards;
// found reports whether Microsoft Entra ID knows it. Groups whose external
// id is not an object ID are not looked up (found is false). It returns
// ErrOff while the lookup is off.
func (r *Resolver) LookUpGroup(ctx context.Context, g *store.Group) (updated *store.Group, found bool, err error) {
	if r == nil {
		return nil, false, ErrOff
	}
	st, err := r.store.GetSettings(ctx)
	if err != nil {
		return nil, false, err
	}
	if !st.GroupNamesActive() {
		return nil, false, ErrOff
	}
	if !store.IsGUID(g.ExternalID) {
		return g, false, nil
	}
	res, err := r.lookUp(ctx, st, []store.Group{*g})
	if err != nil {
		r.log.Warn("could not look up the name of a group in Microsoft Entra ID", "group", g.ExternalID, "err", err)
		return nil, false, err
	}
	updated, err = r.store.GetGroup(ctx, g.ID)
	if err != nil {
		return nil, false, err
	}
	return updated, res.Found == 1, nil
}

// lookUp looks up the names of groups with Microsoft Graph and stores them,
// msgraph.MaxGroupIDs groups at a time, so that a failure keeps the names
// found before it. It records the outcome (see Status); after a failure,
// the worker pauses. When ctx ends first, it returns ctx.Err() and records
// nothing.
func (r *Resolver) lookUp(ctx context.Context, st *store.Settings, groups []store.Group) (Status, error) {
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return Status{}, ctx.Err()
	}
	defer func() { <-r.sem }()

	var res Status
	c, err := r.client(st)
	if err == nil {
		for chunk := range slices.Chunk(groups, msgraph.MaxGroupIDs) {
			ids := make([]string, len(chunk))
			checked := make([]int64, len(chunk))
			for i, g := range chunk {
				ids[i], checked[i] = g.ExternalID, g.ID
			}
			var names map[string]string
			if names, err = c.GroupNames(ctx, ids); err != nil {
				break
			}
			found := make(map[int64]string, len(names))
			for _, g := range chunk {
				if name, ok := names[strings.ToLower(g.ExternalID)]; ok {
					found[g.ID] = name
				}
			}
			if err = r.store.SetGroupDirectoryNames(ctx, checked, found, r.now()); err != nil {
				break
			}
			res.Groups += len(chunk)
			res.Found += len(found)
		}
	}
	res.At = r.now()
	if err != nil && ctx.Err() != nil {
		// Interrupted (shutdown, or the caller's time is up): not a
		// failure of the lookup; the worker looks up the rest.
		return res, ctx.Err()
	}
	err = explain(err)
	r.record(st, res, err)
	return res, err
}

// appKey identifies the app registration Microsoft Graph uses.
func appKey(st *store.Settings) [3]string {
	return [3]string{st.OIDCIssuer, st.OIDCClientID, st.OIDCClientSecret}
}

// client returns the Microsoft Graph client for the app registration of st,
// creating it if the settings changed.
func (r *Resolver) client(st *store.Settings) (*msgraph.Client, error) {
	key := appKey(st)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.graph != nil && r.graphFor == key {
		return r.graph, nil
	}
	c, err := msgraph.New(st.OIDCIssuer, st.OIDCClientID, st.OIDCClientSecret)
	if err != nil {
		return nil, fmt.Errorf("Microsoft Graph uses the app registration of the single sign-on settings: %w", err)
	}
	r.graph, r.graphFor = c, key
	return c, nil
}

// record stores the outcome of a lookup. After a failure the worker pauses
// for errorPause, or as long as Microsoft Graph asks (at most errorPause),
// while the app registration stays the same; a success ends the pause.
func (r *Resolver) record(st *store.Settings, res Status, err error) {
	if err != nil {
		res.Err = err.Error()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = res
	if err == nil {
		r.pausedUntil = time.Time{}
		return
	}
	pause := r.errorPause
	var ge *msgraph.Error
	if errors.As(err, &ge) && ge.RetryAfter > 0 {
		pause = min(ge.RetryAfter, r.errorPause)
	}
	r.pausedUntil, r.pausedFor = res.At.Add(pause), appKey(st)
}

// paused reports whether the worker pauses after a failure with the app
// registration of st.
func (r *Resolver) paused(st *store.Settings) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now().Before(r.pausedUntil) && r.pausedFor == appKey(st)
}

// explain adds a hint for administrators to an error response of Microsoft
// Graph.
func explain(err error) error {
	var ge *msgraph.Error
	if !errors.As(err, &ge) {
		return err
	}
	switch ge.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w – Microsoft Graph did not accept the access token of the app registration", err)
	case http.StatusForbidden:
		return fmt.Errorf("%w – the app registration needs the Microsoft Graph application permission GroupMember.Read.All with admin consent", err)
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if ge.RetryAfter > 0 {
			return fmt.Errorf("%w (retry after %s)", err, ge.RetryAfter)
		}
	}
	return err
}
