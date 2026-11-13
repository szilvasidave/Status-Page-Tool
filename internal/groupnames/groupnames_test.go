package groupnames

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakegraph"
)

var ctx = context.Background()

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env is a resolver with a SQLite store, the fake Microsoft Graph and a
// clock the test moves.
type env struct {
	t   *testing.T
	st  *store.Store
	fg  *fakegraph.Server
	r   *Resolver
	log *syncBuffer

	mu  sync.Mutex
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(ctx, config.DriverSQLite, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, err := (&config.Config{SecretKey: config.NewSecretKey()}).Key()
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: store.New(d, key), fg: fakegraph.Start(t), log: &syncBuffer{}, now: time.Now().UTC().Truncate(time.Second)}
	e.r = New(e.st, slog.New(slog.NewTextHandler(e.log, nil)))
	e.r.now = func() time.Time {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.now
	}
	e.settings(func(s *store.Settings) {})
	return e
}

// settings stores settings with the lookup on and the fake's app
// registration, changed by f.
func (e *env) settings(f func(*store.Settings)) {
	e.t.Helper()
	s := store.DefaultSettings()
	s.OIDCEnabled = true
	s.OIDCIssuer = e.fg.Issuer()
	s.OIDCClientID = e.fg.ClientID
	s.OIDCClientSecret = e.fg.ClientSecret
	s.EntraGroupNames = true
	f(&s)
	if err := e.st.SaveSettings(ctx, &s); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

func (e *env) group(externalID string) *store.Group {
	e.t.Helper()
	g, err := e.st.CreateGroup(ctx, externalID, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return g
}

func (e *env) get(g *store.Group) *store.Group {
	e.t.Helper()
	got, err := e.st.GetGroup(ctx, g.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// requests returns the number of groups requests the fake answered so far.
func (e *env) requests() int {
	n, _ := e.fg.GroupRequests()
	return n
}

func objectID(i int) string { return fmt.Sprintf("%08x-1111-4000-8000-000000000000", i) }

func TestLookUpDue(t *testing.T) {
	e := newEnv(t)
	ops := e.group(strings.ToUpper(objectID(1)))
	staff := e.group(objectID(2))
	gone := e.group(objectID(3)) // deleted in Entra ID
	keycloak := e.group("it-admins")
	prov, _, err := e.st.CreateSCIMGroup(ctx, store.SCIMGroupData{DisplayName: "Provisioned", ExternalID: objectID(4)})
	if err != nil {
		t.Fatal(err)
	}
	e.fg.SetGroup(objectID(1), "IT Ops")
	e.fg.SetGroup(objectID(2), "All Staff")
	e.fg.SetGroup(objectID(4), "Must not be looked up")

	e.r.lookUpDue(ctx)
	if n, ids := e.fg.GroupRequests(); n != 1 || ids != 3 {
		t.Fatalf("%d requests for %d IDs, want 1 for 3", n, ids)
	}
	if g := e.get(ops); g.DirectoryName != "IT Ops" || g.DisplayName() != "IT Ops" || g.DirectoryCheckedAt == nil || !g.DirectoryCheckedAt.Equal(e.r.now()) {
		t.Errorf("group 1 = %+v", g)
	}
	if g := e.get(staff); g.DisplayName() != "All Staff" {
		t.Errorf("group 2 shown as %q", g.DisplayName())
	}
	if g := e.get(gone); g.DirectoryName != "" || g.DisplayName() != objectID(3) || g.DirectoryCheckedAt == nil {
		t.Errorf("group unknown to Entra ID = %+v", g)
	}
	if g := e.get(keycloak); g.DirectoryCheckedAt != nil {
		t.Errorf("a group without object ID was looked up: %+v", g)
	}
	if g := e.get(prov); g.DirectoryCheckedAt != nil || g.DisplayName() != "Provisioned" {
		t.Errorf("a provisioned group was looked up: %+v", g)
	}
	if s := e.r.Status(); s.Err != "" || s.Groups != 3 || s.Found != 2 || s.At.IsZero() {
		t.Errorf("status = %+v", s)
	}

	// Nothing is due within a day.
	e.advance(23 * time.Hour)
	e.r.lookUpDue(ctx)
	if e.requests() != 1 {
		t.Errorf("%d requests, want no new one within a day", e.requests())
	}

	// A day later the names are looked up again: renames follow, and a
	// group deleted since keeps its last name.
	e.fg.SetGroup(objectID(1), "IT Operations")
	e.fg.RemoveGroup(objectID(2))
	e.advance(2 * time.Hour)
	e.r.lookUpDue(ctx)
	if e.requests() != 2 {
		t.Fatalf("%d requests, want 2", e.requests())
	}
	if g := e.get(ops); g.DisplayName() != "IT Operations" {
		t.Errorf("renamed group shown as %q", g.DisplayName())
	}
	if g := e.get(staff); g.DisplayName() != "All Staff" {
		t.Errorf("deleted group shown as %q, want its last name", g.DisplayName())
	}

	// A name given by an administrator takes precedence.
	if err := e.st.UpdateGroup(ctx, ops.ID, "Operations team", store.RoleEditor); err != nil {
		t.Fatal(err)
	}
	if g := e.get(ops); g.DisplayName() != "Operations team" || g.DirectoryName != "IT Operations" {
		t.Errorf("group with a name = %+v", g)
	}

	// Turned off: no lookups.
	e.settings(func(s *store.Settings) { s.EntraGroupNames = false })
	e.advance(25 * time.Hour)
	e.r.lookUpDue(ctx)
	// Single sign-on off: no lookups either.
	e.settings(func(s *store.Settings) { s.OIDCEnabled = false })
	e.r.lookUpDue(ctx)
	if e.requests() != 2 {
		t.Errorf("%d requests, want none while the lookup is off", e.requests())
	}
}

// After a failure the worker pauses (an hour, or as long as Microsoft Graph
// asks) unless the app registration changes; the names are looked up later.
func TestLookUpFailure(t *testing.T) {
	e := newEnv(t)
	g := e.group(objectID(1))
	e.fg.SetGroup(objectID(1), "IT Ops")

	e.fg.FailGroupsNext(fakegraph.GroupsDenied())
	e.r.lookUpDue(ctx)
	s := e.r.Status()
	if !strings.Contains(s.Err, "Authorization_RequestDenied") || !strings.Contains(s.Err, "GroupMember.Read.All") || s.Groups != 0 {
		t.Errorf("status = %+v", s)
	}
	if got := e.get(g); got.DirectoryCheckedAt != nil {
		t.Errorf("failed lookup recorded: %+v", got)
	}
	if log := e.log.String(); !strings.Contains(log, "could not look up the names of groups") {
		t.Errorf("failure not logged:\n%s", log)
	}

	e.advance(30 * time.Minute)
	e.r.lookUpDue(ctx)
	if e.requests() != 1 {
		t.Errorf("%d requests, want none during the pause", e.requests())
	}

	// Another app registration (here: a wrong secret) is tried at once.
	e.settings(func(s *store.Settings) { s.OIDCClientSecret = "new-secret" })
	e.r.lookUpDue(ctx)
	if s := e.r.Status(); !strings.Contains(s.Err, "AADSTS7000215") {
		t.Errorf("status with another secret = %+v", s)
	}

	// After the pause the names are looked up; a success clears the error.
	e.settings(func(s *store.Settings) {})
	e.advance(31 * time.Minute)
	e.r.lookUpDue(ctx)
	if got := e.get(g); got.DirectoryName != "IT Ops" {
		t.Errorf("after the pause: %+v", got)
	}
	if s := e.r.Status(); s.Err != "" || s.Found != 1 {
		t.Errorf("status after success = %+v", s)
	}

	// Throttled: paused as long as Microsoft Graph asks.
	e.group(objectID(2))
	e.fg.FailGroupsNext(fakegraph.Throttled("120"))
	e.r.lookUpDue(ctx)
	if s := e.r.Status(); !strings.Contains(s.Err, "retry after 2m0s") {
		t.Errorf("status when throttled = %+v", s)
	}
	before := e.requests()
	e.advance(time.Minute)
	e.r.lookUpDue(ctx)
	if e.requests() != before {
		t.Error("looked up again before Retry-After")
	}
	e.advance(90 * time.Second)
	e.r.lookUpDue(ctx)
	if e.requests() != before+1 {
		t.Error("not looked up again after Retry-After")
	}
}

// A failure after the first request keeps the names found before it.
func TestLookUpPartialFailure(t *testing.T) {
	e := newEnv(t)
	var groups []*store.Group
	for i := 1; i <= 20; i++ {
		groups = append(groups, e.group(objectID(i)))
		e.fg.SetGroup(objectID(i), fmt.Sprint("Group ", i))
	}
	e.fg.FailGroupsNext(fakegraph.Failure{}, fakegraph.GroupsDenied())
	e.r.lookUpDue(ctx)
	if s := e.r.Status(); s.Groups != 15 || s.Found != 15 || s.Err == "" {
		t.Errorf("status = %+v", s)
	}
	if g := e.get(groups[14]); g.DirectoryName != "Group 15" {
		t.Errorf("15th group = %+v", g)
	}
	if g := e.get(groups[15]); g.DirectoryCheckedAt != nil {
		t.Errorf("16th group = %+v", g)
	}
}

func TestLookUpAll(t *testing.T) {
	e := newEnv(t)
	g := e.group(objectID(1))
	e.fg.SetGroup(objectID(1), "IT Ops")
	e.r.lookUpDue(ctx)

	// Renamed: the worker would wait a day, LookUpAll looks up at once.
	e.fg.SetGroup(objectID(1), "IT Operations")
	e.group("it-admins")
	s, err := e.r.LookUpAll(ctx)
	if err != nil || s.Groups != 1 || s.Found != 1 {
		t.Errorf("LookUpAll = %+v, %v", s, err)
	}
	if got := e.get(g); got.DisplayName() != "IT Operations" {
		t.Errorf("after LookUpAll: %q", got.DisplayName())
	}

	// It ignores the pause after a failure.
	e.fg.FailGroupsNext(fakegraph.GroupsDenied())
	if _, err := e.r.LookUpAll(ctx); err == nil || !strings.Contains(err.Error(), "GroupMember.Read.All") {
		t.Errorf("LookUpAll error = %v", err)
	}
	if s, err := e.r.LookUpAll(ctx); err != nil || s.Found != 1 || e.r.Status().Err != "" {
		t.Errorf("LookUpAll after a failure = %+v, %v; status %+v", s, err, e.r.Status())
	}

	// Interrupted: nothing recorded, the worker is woken for the rest.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	before := e.r.Status()
	if _, err := e.r.LookUpAll(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("LookUpAll(cancelled) = %v", err)
	}
	if e.r.Status() != before {
		t.Errorf("an interrupted lookup changed the status: %+v", e.r.Status())
	}

	e.settings(func(s *store.Settings) { s.EntraGroupNames = false })
	if _, err := e.r.LookUpAll(ctx); !errors.Is(err, ErrOff) {
		t.Errorf("LookUpAll while off = %v", err)
	}
}

func TestLookUpGroup(t *testing.T) {
	e := newEnv(t)
	e.fg.SetGroup(objectID(1), "IT Ops")
	g, found, err := e.r.LookUpGroup(ctx, e.group(objectID(1)))
	if err != nil || !found || g.DisplayName() != "IT Ops" {
		t.Errorf("LookUpGroup = %+v, %v, %v", g, found, err)
	}
	g, found, err = e.r.LookUpGroup(ctx, e.group(objectID(2)))
	if err != nil || found || g.DirectoryCheckedAt == nil {
		t.Errorf("LookUpGroup(unknown) = %+v, %v, %v", g, found, err)
	}
	before := e.requests()
	if _, found, err := e.r.LookUpGroup(ctx, e.group("it-admins")); err != nil || found || e.requests() != before {
		t.Errorf("LookUpGroup(no object ID) = %v, %v; %d requests", found, err, e.requests()-before)
	}
	e.settings(func(s *store.Settings) { s.EntraGroupNames = false })
	if _, _, err := e.r.LookUpGroup(ctx, e.group(objectID(3))); !errors.Is(err, ErrOff) {
		t.Errorf("LookUpGroup while off = %v", err)
	}
}

// The worker looks up the names at its start, and those of new groups
// shortly after a Wake.
func TestRunWake(t *testing.T) {
	e := newEnv(t)
	e.r.wakeDelay = 10 * time.Millisecond
	waitFor := func(g *store.Group, name string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for e.get(g).DisplayName() != name {
			if time.Now().After(deadline) {
				t.Fatalf("group %s not named %q in time", g.ExternalID, name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	e.fg.SetGroup(objectID(1), "IT Ops")
	first := e.group(objectID(1))

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		e.r.Run(runCtx)
		close(done)
	}()
	waitFor(first, "IT Ops") // looked up at the start

	e.fg.SetGroup(objectID(2), "All Staff")
	second := e.group(objectID(2))
	e.r.Wake()
	waitFor(second, "All Staff")

	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestNilResolver(t *testing.T) {
	var r *Resolver
	r.Wake()
	if r.Status() != (Status{}) {
		t.Error("status of a nil resolver")
	}
	if _, err := r.LookUpAll(ctx); !errors.Is(err, ErrOff) {
		t.Errorf("LookUpAll = %v", err)
	}
	if _, _, err := r.LookUpGroup(ctx, &store.Group{}); !errors.Is(err, ErrOff) {
		t.Errorf("LookUpGroup = %v", err)
	}
}
