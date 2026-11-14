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
	e.r.retryDelay = time.Hour // tests that want the retry set it shorter
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

// group adds a group with a role, so that it counts as used (see
// store.Store.GroupsForNameLookup) however far the clock moves.
func (e *env) group(externalID string) *store.Group {
	e.t.Helper()
	g, err := e.st.CreateGroup(ctx, externalID, "")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.UpdateGroup(ctx, g.ID, "", store.RoleEditor); err != nil {
		e.t.Fatal(err)
	}
	return e.get(g)
}

// retrying reports whether a retry of new groups unknown to Microsoft Graph
// is scheduled.
func (e *env) retrying() bool {
	e.r.mu.Lock()
	defer e.r.mu.Unlock()
	return e.r.retrying
}

// woken reports whether a Wake is pending, and takes it.
func (e *env) woken() bool {
	select {
	case <-e.r.wake:
		return true
	default:
		return false
	}
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
	// Unknown to Entra ID, but recorded a moment ago: Microsoft Graph may
	// not know a new group yet, so it is not reported missing but looked up
	// again shortly.
	if g := e.get(gone); g.DirectoryCheckedAt != nil || g.DirectoryMissing() || g.DisplayName() != objectID(3) {
		t.Errorf("new group unknown to Entra ID = %+v", g)
	}
	if !e.retrying() {
		t.Error("no retry scheduled for the new group unknown to Entra ID")
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
	// Still unknown after newGroupWindow: missing.
	e.advance(e.r.newGroupWindow + time.Minute)
	e.r.lookUpDue(ctx)
	if n, ids := e.fg.GroupRequests(); n != 2 || ids != 4 {
		t.Fatalf("%d requests for %d IDs, want the new group looked up again", n, ids)
	}
	if g := e.get(gone); g.DirectoryCheckedAt == nil || !g.DirectoryMissing() || g.DisplayName() != objectID(3) {
		t.Errorf("group unknown to Entra ID = %+v", g)
	}

	// Nothing is due within a day.
	e.advance(23 * time.Hour)
	e.r.lookUpDue(ctx)
	if e.requests() != 2 {
		t.Errorf("%d requests, want no new one within a day", e.requests())
	}

	// A day later the names are looked up again: renames follow, and a
	// group deleted since keeps its last name.
	e.fg.SetGroup(objectID(1), "IT Operations")
	e.fg.RemoveGroup(objectID(2))
	e.advance(2 * time.Hour)
	e.r.lookUpDue(ctx)
	if e.requests() != 3 {
		t.Fatalf("%d requests, want 3", e.requests())
	}
	if g := e.get(ops); g.DisplayName() != "IT Operations" {
		t.Errorf("renamed group shown as %q", g.DisplayName())
	}
	if g := e.get(staff); g.DisplayName() != "All Staff" || !g.DirectoryMissing() {
		t.Errorf("deleted group = %+v, want its last name, missing", g)
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
	if e.requests() != 3 {
		t.Errorf("%d requests, want none while the lookup is off", e.requests())
	}
}

// After a failure the worker pauses (an hour, or as long as Microsoft Graph
// asks, at most an hour) unless the app registration changes; the names are
// looked up later.
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

	e.advance(59 * time.Minute)
	e.r.lookUpDue(ctx)
	if e.requests() != 1 {
		t.Errorf("%d requests, want none during the pause", e.requests())
	}

	// The pause ends after an hour with the same app registration; a
	// success clears the error.
	e.advance(2 * time.Minute)
	e.r.lookUpDue(ctx)
	if e.requests() != 2 {
		t.Fatalf("%d requests, want 2 after the pause", e.requests())
	}
	if got := e.get(g); got.DirectoryName != "IT Ops" {
		t.Errorf("after the pause: %+v", got)
	}
	if s := e.r.Status(); s.Err != "" || s.Found != 1 {
		t.Errorf("status after success = %+v", s)
	}

	// Another app registration is tried at once, also during a pause, and
	// its pause does not hold back the first one.
	e.group(objectID(2))
	e.fg.SetGroup(objectID(2), "All Staff")
	e.fg.FailGroupsNext(fakegraph.GroupsDenied())
	e.r.lookUpDue(ctx)
	e.settings(func(s *store.Settings) { s.OIDCClientSecret = "new-secret" })
	e.r.lookUpDue(ctx)
	if s := e.r.Status(); !strings.Contains(s.Err, "AADSTS7000215") {
		t.Errorf("status with another secret = %+v", s)
	}
	e.settings(func(s *store.Settings) {})
	e.r.lookUpDue(ctx)
	if s := e.r.Status(); s.Err != "" || s.Found != 1 {
		t.Errorf("status with the first app registration again = %+v", s)
	}

	// Throttled: paused as long as Microsoft Graph asks …
	e.group(objectID(3))
	e.fg.SetGroup(objectID(3), "Sales")
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

	// … but at most an hour.
	e.group(objectID(4))
	e.fg.SetGroup(objectID(4), "Finance")
	e.fg.FailGroupsNext(fakegraph.Throttled("7200"))
	e.r.lookUpDue(ctx)
	if s := e.r.Status(); !strings.Contains(s.Err, "retry after 2h0m0s") {
		t.Errorf("status when throttled for two hours = %+v", s)
	}
	before = e.requests()
	e.advance(59 * time.Minute)
	e.r.lookUpDue(ctx)
	if e.requests() != before {
		t.Error("looked up again during the pause")
	}
	e.advance(2 * time.Minute)
	e.r.lookUpDue(ctx)
	if e.requests() != before+1 {
		t.Error("the pause after Retry-After: 7200 lasted longer than an hour")
	}
}

// An access token keeps the permissions it was issued with: after a failure
// other than throttling the next lookup gets a new token, so that a
// permission granted since applies at once; "Look up names now" always gets
// a new one.
func TestLookUpNewTokenAfterFailure(t *testing.T) {
	e := newEnv(t)
	e.fg.SetGroup(objectID(1), "IT Ops")
	e.fg.SetGroup(objectID(2), "All Staff")
	e.fg.SetGroup(objectID(3), "Sales")
	g1 := e.group(objectID(1))

	// The lookup is turned on before admin consent is granted.
	e.fg.SetGroupsPermission(false)
	e.r.lookUpDue(ctx)
	if s := e.r.Status(); !strings.Contains(s.Err, "GroupMember.Read.All") {
		t.Fatalf("status without the permission = %+v", s)
	}
	e.fg.SetGroupsPermission(true)
	// The worker, after its pause.
	e.advance(61 * time.Minute)
	e.r.lookUpDue(ctx)
	if got := e.get(g1); got.DirectoryName != "IT Ops" {
		t.Errorf("the worker after the permission was granted: %+v; status %+v", got, e.r.Status())
	}

	// Adding a group by its object ID: the token in use fails, the next
	// one is issued without the permission, then it is granted.
	e.fg.SetGroupsPermission(false)
	g2 := e.group(objectID(2))
	e.fg.FailGroupsNext(fakegraph.GroupsDenied())
	if _, _, err := e.r.LookUpGroup(ctx, g2); err == nil {
		t.Error("LookUpGroup did not fail")
	}
	if _, _, err := e.r.LookUpGroup(ctx, g2); err == nil || !strings.Contains(err.Error(), "GroupMember.Read.All") {
		t.Errorf("LookUpGroup with a new token without the permission = %v", err)
	}
	e.fg.SetGroupsPermission(true)
	if g, found, err := e.r.LookUpGroup(ctx, g2); err != nil || !found || g.DisplayName() != "All Staff" {
		t.Errorf("LookUpGroup after the permission was granted = %+v, %v, %v", g, found, err)
	}

	// "Look up names now" always gets a new token.
	tokens := e.fg.TokenRequests()
	if _, err := e.r.LookUpAll(ctx); err != nil {
		t.Fatal(err)
	}
	if e.fg.TokenRequests() != tokens+1 {
		t.Errorf("LookUpAll got %d new access tokens, want 1", e.fg.TokenRequests()-tokens)
	}
	// Throttling keeps the token.
	tokens = e.fg.TokenRequests()
	e.group(objectID(3))
	e.fg.FailGroupsNext(fakegraph.Throttled("1"))
	e.r.lookUpDue(ctx)
	e.advance(2 * time.Second)
	e.r.lookUpDue(ctx)
	if got := e.fg.TokenRequests(); got != tokens {
		t.Errorf("%d new access tokens after throttling, want none", got-tokens)
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
	// Unknown: not reported missing while it may be new (see TestLookUpDue).
	g, found, err = e.r.LookUpGroup(ctx, e.group(objectID(2)))
	if err != nil || found || g.DirectoryCheckedAt != nil || !e.retrying() {
		t.Errorf("LookUpGroup(unknown) = %+v, %v, %v; retrying %v", g, found, err, e.retrying())
	}
	e.advance(e.r.newGroupWindow)
	g, found, err = e.r.LookUpGroup(ctx, g)
	if err != nil || found || !g.DirectoryMissing() {
		t.Errorf("LookUpGroup(unknown) later = %+v, %v, %v", g, found, err)
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

// An interrupted LookUpAll leaves the rest to the worker: the names stored
// before end the pause and the error of an earlier failure, and the worker
// looks up the groups not looked up since the LookUpAll began – also those
// it would only look up a day later, and also during a pause.
func TestLookUpAllInterrupted(t *testing.T) {
	e := newEnv(t)
	var groups []*store.Group
	for i := 1; i <= 20; i++ {
		groups = append(groups, e.group(objectID(i)))
		e.fg.SetGroup(objectID(i), fmt.Sprint("Group ", i))
	}
	e.r.lookUpDue(ctx)
	for i := 1; i <= 20; i++ {
		e.fg.SetGroup(objectID(i), fmt.Sprint("Renamed ", i))
	}
	e.advance(time.Hour)
	e.fg.FailGroupsNext(fakegraph.GroupsDenied())
	if _, err := e.r.LookUpAll(ctx); err == nil {
		t.Fatal("LookUpAll did not fail")
	}

	// The second request is cut off.
	interrupted, cancel := context.WithCancel(ctx)
	defer cancel()
	first, _ := e.fg.GroupRequests()
	e.fg.OnGroupsRequest(func(n int) {
		if n == first+2 {
			cancel()
		}
	})
	res, err := e.r.LookUpAll(interrupted)
	e.fg.OnGroupsRequest(nil)
	if !errors.Is(err, context.Canceled) || res.Groups != 15 {
		t.Fatalf("LookUpAll = %+v, %v", res, err)
	}
	if s := e.r.Status(); s.Err != "" || s.Groups != 15 {
		t.Errorf("status after the interruption = %+v", s)
	}
	if !e.r.pausedUntil.IsZero() {
		t.Error("the pause after the earlier failure still holds")
	}
	if !e.woken() {
		t.Error("the worker was not woken")
	}
	if g := e.get(groups[15]); g.DisplayName() != "Group 16" {
		t.Fatalf("16th group = %+v", g)
	}

	// The worker looks up the other 5 at once.
	n, ids := e.fg.GroupRequests()
	e.r.lookUpDue(ctx)
	if n2, ids2 := e.fg.GroupRequests(); n2 != n+1 || ids2 != ids+5 {
		t.Errorf("the worker made %d requests for %d IDs, want 1 for 5", n2-n, ids2-ids)
	}
	for _, i := range []int{0, 14, 15, 19} {
		if g := e.get(groups[i]); g.DisplayName() != fmt.Sprint("Renamed ", i+1) {
			t.Errorf("group %d shown as %q", i+1, g.DisplayName())
		}
	}
	e.r.lookUpDue(ctx)
	if n3, _ := e.fg.GroupRequests(); n3 != n+1 {
		t.Error("the worker looked up again after the refresh")
	}

	// Interrupted before any name was stored (here: another lookup holds
	// the lock) after a failure: the status stays, the worker looks up all
	// groups anyway, ignoring the pause once.
	e.fg.FailGroupsNext(fakegraph.GroupsDenied())
	if _, err := e.r.LookUpAll(ctx); err == nil {
		t.Fatal("LookUpAll did not fail")
	}
	failed := e.r.Status()
	e.advance(time.Minute)
	e.r.sem <- struct{}{}
	short, cancelShort := context.WithTimeout(ctx, 20*time.Millisecond)
	res, err = e.r.LookUpAll(short)
	cancelShort()
	<-e.r.sem
	if !errors.Is(err, context.DeadlineExceeded) || res.Groups != 0 {
		t.Fatalf("LookUpAll while another lookup holds the lock = %+v, %v", res, err)
	}
	if e.r.Status() != failed || !e.woken() {
		t.Errorf("status %+v (want it unchanged), woken %v", e.r.Status(), e.woken())
	}
	n, ids = e.fg.GroupRequests()
	e.r.lookUpDue(ctx)
	if n2, ids2 := e.fg.GroupRequests(); n2 != n+2 || ids2 != ids+20 {
		t.Errorf("the worker made %d requests for %d IDs, want 2 for 20", n2-n, ids2-ids)
	}
	if s := e.r.Status(); s.Err != "" || s.Groups != 20 {
		t.Errorf("status after the worker's refresh = %+v", s)
	}
}

// A request that hangs is reported as a failure, not as an interruption.
func TestLookUpHang(t *testing.T) {
	e := newEnv(t)
	e.group(objectID(1))
	e.r.requestTimeout = 50 * time.Millisecond
	e.fg.OnGroupsRequest(func(int) { time.Sleep(300 * time.Millisecond) })
	_, err := e.r.LookUpAll(ctx)
	e.fg.OnGroupsRequest(nil)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "Microsoft Graph did not respond in time") {
		t.Errorf("LookUpAll = %v", err)
	}
	if s := e.r.Status(); !strings.Contains(s.Err, "did not respond") {
		t.Errorf("status = %+v", s)
	}
}

// One run of the worker looks up at most maxRun groups, the groups never
// looked up first, and wakes the worker for the rest.
func TestLookUpRunLimit(t *testing.T) {
	e := newEnv(t)
	e.r.maxRun = 15
	for i := 1; i <= 20; i++ {
		e.group(objectID(i))
		e.fg.SetGroup(objectID(i), fmt.Sprint("Group ", i))
	}
	e.r.lookUpDue(ctx)
	if n, ids := e.fg.GroupRequests(); n != 1 || ids != 15 || !e.woken() {
		t.Fatalf("first run: %d requests for %d IDs; woken %v", n, ids, e.woken())
	}
	e.r.lookUpDue(ctx)
	if n, ids := e.fg.GroupRequests(); n != 2 || ids != 20 || e.woken() {
		t.Fatalf("second run: %d requests for %d IDs; woken %v", n, ids, e.woken())
	}

	// A day later: a group a sign-in just recorded comes first.
	e.advance(25 * time.Hour)
	newest := e.group(objectID(21))
	e.fg.SetGroup(objectID(21), "Newest")
	e.r.lookUpDue(ctx)
	if g := e.get(newest); g.DisplayName() != "Newest" {
		t.Errorf("the new group waits for the daily refresh: %+v", g)
	}
}

// The worker stops when the lookup is turned off or the app registration
// changes during a run.
func TestLookUpSettingsChange(t *testing.T) {
	e := newEnv(t)
	for i := 1; i <= 20; i++ {
		e.group(objectID(i))
		e.fg.SetGroup(objectID(i), fmt.Sprint("Group ", i))
	}
	change := func(f func(*store.Settings)) {
		e.fg.OnGroupsRequest(func(n int) {
			if n != 1 {
				return
			}
			st, err := e.st.GetSettings(ctx)
			if err == nil {
				f(st)
				err = e.st.SaveSettings(ctx, st)
			}
			if err != nil {
				t.Error(err)
			}
		})
	}
	change(func(s *store.Settings) { s.EntraGroupNames = false })
	e.r.lookUpDue(ctx)
	e.fg.OnGroupsRequest(nil)
	if n, _ := e.fg.GroupRequests(); n != 1 {
		t.Errorf("%d requests after the lookup was turned off, want 1", n)
	}
	if log := e.log.String(); !strings.Contains(log, "turned off during the lookup") {
		t.Errorf("not logged:\n%s", log)
	}

	e = newEnv(t)
	for i := 1; i <= 20; i++ {
		e.group(objectID(i))
	}
	change(func(s *store.Settings) { s.OIDCClientID = "other-app" })
	e.r.lookUpDue(ctx)
	e.fg.OnGroupsRequest(nil)
	if n, _ := e.fg.GroupRequests(); n != 1 || !e.woken() {
		t.Errorf("%d requests after the app registration changed, want 1; woken %v", n, e.woken())
	}
	if s := e.r.Status(); s.Err != "" {
		t.Errorf("the change was recorded as a failure: %+v", s)
	}
}

// The worker looks up the names at its start, and those of new groups
// shortly after a Wake.
func TestRunWake(t *testing.T) {
	e := newEnv(t)
	e.r.wakeDelay = 10 * time.Millisecond
	e.r.retryDelay = 20 * time.Millisecond
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

	// A group that Microsoft Graph does not know yet is looked up again
	// shortly.
	before := e.requests()
	third := e.group(objectID(3))
	e.r.Wake()
	for deadline := time.Now().Add(10 * time.Second); e.requests() == before; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the new group was not looked up")
		}
	}
	e.fg.SetGroup(objectID(3), "Sales")
	waitFor(third, "Sales")

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
