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
//
// Microsoft Graph may not know a group created a moment ago (replication),
// so a group recorded less than newGroupWindow ago that it does not know is
// looked up again after retryDelay instead of being reported as missing.
// Lookups take turns per request of msgraph.MaxGroupIDs groups, so that an
// administrator's lookup waits for at most one request of the worker, and
// the worker stops when the lookup is turned off or the app registration
// changes.
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
	// requestTimeout is how long a request to Microsoft Graph (with the
	// access token request) may take, shorter than an administrator's
	// lookup may take (see internal/web), so that a hang is reported as a
	// failure.
	requestTimeout = 10 * time.Second
	// newGroupWindow and retryDelay: a group recorded less than
	// newGroupWindow ago that Microsoft Graph does not know is looked up
	// again after retryDelay. Groups recorded less than twice
	// newGroupWindow ago are looked up even without members, a role or
	// services (see store.Store.GroupsForNameLookup), so that the first
	// lookup after newGroupWindow reports such a group as missing.
	newGroupWindow = 15 * time.Minute
	retryDelay     = 2 * time.Minute
	// maxRun is how many groups one run of the worker looks up at most;
	// the next run follows after wakeDelay, so that the new groups of
	// sign-ins (looked up first) do not wait for a whole daily refresh.
	maxRun = 20 * msgraph.MaxGroupIDs
)

// ErrOff is returned by LookUpAll and LookUpGroup while the lookup is off
// (see store.Settings.GroupNamesActive), also when it was turned off during
// the lookup.
var ErrOff = errors.New("looking up group names in Microsoft Entra ID is turned off")

// errChanged ends a lookup when the app registration of the single sign-on
// settings changed during it; the worker goes on with the new one.
var errChanged = errors.New("the app registration of the single sign-on settings changed during the lookup; " +
	"the names are looked up with the new one in the background")

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

	// Clock, intervals and limits; tests replace them.
	now            func() time.Time
	checkInterval  time.Duration
	refreshAfter   time.Duration
	wakeDelay      time.Duration
	errorPause     time.Duration
	requestTimeout time.Duration
	newGroupWindow time.Duration
	retryDelay     time.Duration
	maxRun         int

	mu          sync.Mutex
	graph       *msgraph.Client // client for graphFor, reused for its access token
	graphFor    [3]string       // issuer, client ID and client secret of graph
	status      Status
	pausedUntil time.Time // the worker pauses after a failure …
	pausedFor   [3]string // … while the app registration stays the same
	// An interrupted LookUpAll leaves the rest to the worker: its next run
	// looks up the groups not looked up since refreshBefore with the app
	// registration refreshFor, also during a pause.
	refreshBefore time.Time
	refreshFor    [3]string
	retrying      bool // a Wake after retryDelay is scheduled
}

// New creates a Resolver.
func New(st *store.Store, log *slog.Logger) *Resolver {
	if log == nil {
		log = slog.Default()
	}
	return &Resolver{
		store:          st,
		log:            log,
		wake:           make(chan struct{}, 1),
		sem:            make(chan struct{}, 1),
		now:            time.Now,
		checkInterval:  checkInterval,
		refreshAfter:   refreshAfter,
		wakeDelay:      wakeDelay,
		errorPause:     errorPause,
		requestTimeout: requestTimeout,
		newGroupWindow: newGroupWindow,
		retryDelay:     retryDelay,
		maxRun:         maxRun,
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
// not since refreshAfter (or since the start of an interrupted LookUpAll),
// at most maxRun of them, unless the lookup is off or pauses after a
// failure.
func (r *Resolver) lookUpDue(ctx context.Context) {
	st, err := r.store.GetSettings(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("group names: could not load the settings", "err", err)
		}
		return
	}
	if !st.GroupNamesActive() {
		return
	}
	now := r.now()
	checkedBefore := now.Add(-r.refreshAfter)
	r.mu.Lock()
	refresh := r.refreshBefore
	if refresh.IsZero() || r.refreshFor != appKey(st) {
		refresh = time.Time{}
	} else if refresh.After(checkedBefore) {
		checkedBefore = refresh
	}
	paused := now.Before(r.pausedUntil) && r.pausedFor == appKey(st)
	r.mu.Unlock()
	if paused && refresh.IsZero() {
		return
	}
	groups, err := r.store.GroupsForNameLookup(ctx, checkedBefore, now.Add(-2*r.newGroupWindow))
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("group names: could not list the groups to look up", "err", err)
		}
		return
	}
	more := len(groups) > r.maxRun
	if more {
		groups = groups[:r.maxRun]
	}
	if len(groups) == 0 {
		r.refreshDone(refresh)
		return
	}
	res, err := r.lookUp(ctx, st, groups)
	switch {
	case ctx.Err() != nil:
		// Shutting down.
	case errors.Is(err, ErrOff):
		r.log.Info("looking up the names of groups in Microsoft Entra ID was turned off during the lookup",
			"groups", len(groups), "looked_up", res.Groups)
	case errors.Is(err, errChanged):
		r.log.Info("the app registration changed during the lookup of the names of groups in Microsoft Entra ID",
			"groups", len(groups), "looked_up", res.Groups)
		r.Wake()
	case err != nil:
		r.log.Warn("could not look up the names of groups in Microsoft Entra ID; the status page tries again later",
			"groups", len(groups), "looked_up", res.Groups, "err", err)
		r.refreshDone(refresh) // the pause applies again
	default:
		r.log.Info("looked up the names of groups in Microsoft Entra ID", "groups", res.Groups, "found", res.Found)
		if more {
			r.Wake()
		} else {
			r.refreshDone(refresh)
		}
	}
}

// refreshDone ends the refresh that an interrupted LookUpAll requested at
// t, unless a later one was requested meanwhile.
func (r *Resolver) refreshDone(t time.Time) {
	if t.IsZero() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.refreshBefore.After(t) {
		r.refreshBefore = time.Time{}
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
	start := r.now()
	groups, err := r.store.GroupsForNameLookup(ctx, time.Time{}, start.Add(-2*r.newGroupWindow))
	if err != nil {
		return Status{}, err
	}
	if len(groups) == 0 {
		return Status{At: start}, nil
	}
	// A new access token: e.g. with a permission granted since the last one.
	r.dropClient(nil)
	res, err := r.lookUp(ctx, st, groups)
	switch {
	case err != nil && ctx.Err() != nil:
		r.log.Info("looking up the names of groups in Microsoft Entra ID was interrupted; the rest follows in the background",
			"groups", len(groups), "looked_up", res.Groups)
		r.mu.Lock()
		if start.After(r.refreshBefore) {
			r.refreshBefore = start
		}
		r.refreshFor = appKey(st)
		r.mu.Unlock()
		r.Wake()
	case errors.Is(err, errChanged):
		r.Wake()
	case errors.Is(err, ErrOff):
	case err != nil:
		r.log.Warn("could not look up the names of groups in Microsoft Entra ID", "groups", len(groups), "looked_up", res.Groups, "err", err)
	default:
		r.log.Info("looked up the names of all groups in Microsoft Entra ID", "groups", res.Groups, "found", res.Found)
		r.refreshDone(start)
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
// found before it. Before each further request it reads the settings again
// and stops with ErrOff when the lookup was turned off, or with errChanged
// when the app registration changed. It records the outcome (see Status);
// after a failure, the worker pauses. When ctx ends first, it returns
// ctx.Err() and records only the names stored before (they show that the
// lookup works).
func (r *Resolver) lookUp(ctx context.Context, st *store.Settings, groups []store.Group) (Status, error) {
	var res Status
	var err error
	for chunk := range slices.Chunk(groups, msgraph.MaxGroupIDs) {
		if res.Groups > 0 {
			var cur *store.Settings
			if cur, err = r.store.GetSettings(ctx); err != nil {
				break
			}
			if !cur.GroupNamesActive() {
				return res, ErrOff
			}
			if appKey(cur) != appKey(st) {
				return res, errChanged
			}
		}
		var found int
		if found, err = r.lookUpChunk(ctx, st, chunk); err != nil {
			break
		}
		res.Groups += len(chunk)
		res.Found += found
	}
	res.At = r.now()
	if err != nil && ctx.Err() != nil {
		// Interrupted (shutdown, or the caller's time is up): not a
		// failure of the lookup.
		if res.Groups > 0 {
			r.record(st, res, nil)
		}
		return res, ctx.Err()
	}
	err = explain(err)
	r.record(st, res, err)
	return res, err
}

// lookUpChunk looks up the names of up to msgraph.MaxGroupIDs groups with
// one request and stores them, one lookup at a time. It returns how many of
// them Microsoft Entra ID knows. A group it does not know is stored as
// missing, unless it was never looked up and was recorded less than
// newGroupWindow ago: that one is looked up again after retryDelay.
func (r *Resolver) lookUpChunk(ctx context.Context, st *store.Settings, chunk []store.Group) (int, error) {
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	defer func() { <-r.sem }()
	c, err := r.client(st)
	if err != nil {
		return 0, err
	}
	ids := make([]string, len(chunk))
	for i, g := range chunk {
		ids[i] = g.ExternalID
	}
	rctx, cancel := context.WithTimeout(ctx, r.requestTimeout)
	names, err := c.GroupNames(rctx, ids)
	cancel()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			// The request took too long, not the caller: a failure, not an
			// interruption (the message says which server did not respond).
			err = fmt.Errorf("%s (within %s)", err.Error(), r.requestTimeout)
		}
		return 0, err
	}
	now := r.now()
	checked := make([]int64, 0, len(chunk))
	found := make(map[int64]string, len(names))
	retry := false
	for _, g := range chunk {
		if name, ok := names[strings.ToLower(g.ExternalID)]; ok {
			found[g.ID] = name
		} else if g.DirectoryCheckedAt == nil && now.Sub(g.CreatedAt) < r.newGroupWindow {
			retry = true
			continue
		}
		checked = append(checked, g.ID)
	}
	if err := r.store.SetGroupDirectoryNames(ctx, checked, found, now); err != nil {
		return 0, err
	}
	if retry {
		r.retryLater()
	}
	return len(found), nil
}

// retryLater wakes the worker after retryDelay, to look up the groups that
// Microsoft Graph did not know yet.
func (r *Resolver) retryLater() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retrying {
		return
	}
	r.retrying = true
	time.AfterFunc(r.retryDelay, func() {
		r.mu.Lock()
		r.retrying = false
		r.mu.Unlock()
		r.Wake()
	})
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

// dropClient makes the next lookup create a new client, with a new access
// token; with c not nil, only if that is still the client in use.
func (r *Resolver) dropClient(c *msgraph.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c == nil || r.graph == c {
		r.graph = nil
	}
}

// record stores the outcome of a lookup. After a failure the worker pauses
// for errorPause, or as long as Microsoft Graph asks (at most errorPause),
// while the app registration stays the same; a success ends the pause.
// After a failure other than throttling, the next lookup gets a new access
// token, e.g. one with a permission granted since.
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
	throttled := false
	if errors.As(err, &ge) {
		if ge.RetryAfter > 0 {
			pause = min(ge.RetryAfter, r.errorPause)
		}
		switch ge.StatusCode {
		case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			throttled = true
		}
	}
	if !throttled && r.graphFor == appKey(st) {
		r.graph = nil
	}
	r.pausedUntil, r.pausedFor = res.At.Add(pause), appKey(st)
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
