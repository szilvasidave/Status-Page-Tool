package directory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

const (
	syncIssuer = "https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0"
	syncGroupA = "aaaaaaaa-0000-0000-0000-000000000001"
	syncGroupB = "bbbbbbbb-0000-0000-0000-000000000002"
	syncGroupC = "cccccccc-0000-0000-0000-000000000003" // not in the directory
	syncGroupD = "dddddddd-0000-0000-0000-000000000004" // no role, no service
)

// syncDirectory is the content of the fake directory.
type syncDirectory struct {
	mu       sync.Mutex
	groups   map[string]string           // id → display name
	members  map[string][]map[string]any // id → Graph user objects
	inFlight atomic.Int32                // concurrent member requests
	overlap  atomic.Bool                 // two member requests ran at the same time
}

func (d *syncDirectory) set(id, name string, members ...map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.groups[id] = name
	d.members[id] = members
}

func syncUser(id, name, mail string) map[string]any {
	return map[string]any{"id": id, "displayName": name, "mail": mail, "userPrincipalName": id + "@contoso.onmicrosoft.com", "accountEnabled": true}
}

// syncEnv is a Syncer on a fresh SQLite database talking to a fake Graph.
type syncEnv struct {
	t       *testing.T
	store   *store.Store
	syncer  *Syncer
	graph   *fakeGraph
	dir     *syncDirectory
	clients atomic.Int32 // Graph clients created
}

func newSyncEnv(t *testing.T) *syncEnv {
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

	e := &syncEnv{
		t: t, store: st, graph: newFakeGraph(t),
		dir: &syncDirectory{groups: map[string]string{}, members: map[string][]map[string]any{}},
	}
	e.graph.handle("GET /v1.0/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		e.dir.mu.Lock()
		name, ok := e.dir.groups[r.PathValue("id")]
		e.dir.mu.Unlock()
		if !ok {
			writeJSON(w, 404, graphErrorBody("Request_ResourceNotFound", "Resource does not exist."))
			return
		}
		writeJSON(w, 200, map[string]any{"id": r.PathValue("id"), "displayName": name})
	})
	e.graph.handle("GET /v1.0/groups/{id}/transitiveMembers/microsoft.graph.user", func(w http.ResponseWriter, r *http.Request) {
		if e.dir.inFlight.Add(1) > 1 {
			e.dir.overlap.Store(true)
		}
		defer e.dir.inFlight.Add(-1)
		time.Sleep(5 * time.Millisecond)
		e.dir.mu.Lock()
		members, ok := e.dir.members[r.PathValue("id")]
		e.dir.mu.Unlock()
		if !ok {
			writeJSON(w, 404, graphErrorBody("Request_ResourceNotFound", "Resource does not exist."))
			return
		}
		writeJSON(w, 200, map[string]any{"value": members})
	})
	e.graph.handle("GET /v1.0/groups", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$top"); got != "20" {
			t.Errorf("search $top = %q, want 20", got)
		}
		writeJSON(w, 200, map[string]any{"value": []map[string]any{{"id": syncGroupA, "displayName": "Service Owners"}}})
	})

	e.syncer = NewSyncer(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.syncer.newClient = func(issuer, clientID, clientSecret string) (*GraphClient, error) {
		// Same checks as in production, but talk to the fake server.
		if _, err := NewGraphClient(issuer, clientID, clientSecret); err != nil {
			return nil, err
		}
		e.clients.Add(1)
		return e.graph.client(), nil
	}
	return e
}

// settings stores directory sync settings; change modifies the defaults
// (sync enabled with an Entra ID issuer and complete credentials).
func (e *syncEnv) settings(change func(st *store.Settings)) {
	e.t.Helper()
	st := store.DefaultSettings()
	st.OIDCEnabled = true
	st.OIDCIssuer = syncIssuer
	st.OIDCClientID = "client-id"
	st.OIDCClientSecret = testSecret
	st.DirectorySyncEnabled = true
	if change != nil {
		change(&st)
	}
	if err := e.store.SaveSettings(context.Background(), &st); err != nil {
		e.t.Fatal(err)
	}
}

func (e *syncEnv) group(externalID, name string, role store.Role, withService bool) *store.Group {
	e.t.Helper()
	ctx := context.Background()
	g, err := e.store.CreateGroup(ctx, externalID, name)
	if err != nil {
		e.t.Fatal(err)
	}
	if role != store.RoleNone {
		if err := e.store.UpdateGroup(ctx, g.ID, g.Name, role); err != nil {
			e.t.Fatal(err)
		}
	}
	if withService {
		svc := &store.Service{Name: "Service for " + externalID, GroupIDs: []int64{g.ID}}
		if err := e.store.CreateService(ctx, svc); err != nil {
			e.t.Fatal(err)
		}
	}
	return g
}

func (e *syncEnv) getGroup(id int64) *store.Group {
	e.t.Helper()
	g, err := e.store.GetGroup(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return g
}

// members returns "email|name|directory id" of the group's members, sorted.
func (e *syncEnv) members(groupID int64) []string {
	e.t.Helper()
	users, err := e.store.GroupMembers(context.Background(), groupID)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, u := range users {
		out = append(out, u.Email+"|"+u.Name+"|"+u.DirectoryID)
	}
	slices.Sort(out)
	return out
}

func (e *syncEnv) userIDs(groupID int64) map[string]int64 {
	e.t.Helper()
	users, err := e.store.GroupMembers(context.Background(), groupID)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]int64{}
	for _, u := range users {
		out[u.DirectoryID] = u.ID
	}
	return out
}

func TestSyncNow(t *testing.T) {
	e := newSyncEnv(t)
	e.settings(nil)
	ctx := context.Background()

	gA := e.group(syncGroupA, "", store.RoleNone, true)            // service assigned, unnamed
	gB := e.group(syncGroupB, "Old name", store.RoleEditor, false) // role, renamed in the directory
	gC := e.group(syncGroupC, "Gone", store.RoleNone, true)        // deleted in the directory
	gD := e.group(syncGroupD, "Unused", store.RoleNone, false)     // not relevant
	e.group("IT-Admins", "", store.RoleAdmin, false)               // a name, not an object id

	// Users who signed in before: alice without a directory id (matched by
	// e-mail), bob with a different e-mail (matched by directory id).
	alice, err := e.store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "sub-alice", Email: "alice@contoso.com", Name: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := e.store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "sub-bob", Email: "robert@contoso.com", Name: "Bob", DirectoryID: "u-bob"})
	if err != nil {
		t.Fatal(err)
	}

	disabled := syncUser("u-old", "Disabled", "old@contoso.com")
	disabled["accountEnabled"] = false
	guest := syncUser("u-guest", "Guest", "")
	guest["userPrincipalName"] = "guest_example.com#EXT#@contoso.onmicrosoft.com"
	e.dir.set(syncGroupA, "Service Owners",
		syncUser("u-alice", "Alice Example", "Alice@Contoso.com"),
		syncUser("u-bob", "Bob Example", "bob@contoso.com"),
		guest, disabled)
	e.dir.set(syncGroupB, "Editors", syncUser("u-alice", "Alice Example", "alice@contoso.com"))
	e.dir.set(syncGroupD, "Unused", syncUser("u-zed", "Zed", "zed@contoso.com"))

	res, err := e.syncer.SyncNow(ctx)
	if err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if res.Groups != 2 || res.Members != 4 {
		t.Errorf("result = %+v, want 2 groups and 4 members", res)
	}
	wantErrors := []string{
		`group "Gone" (` + syncGroupC + `) no longer exists in the directory`,
		`group "IT-Admins" skipped: not a directory object id`,
	}
	if !slices.Equal(res.Errors, wantErrors) {
		t.Errorf("errors =\n%q\nwant\n%q", res.Errors, wantErrors)
	}

	// Existing users are reused; their names and e-mail addresses from the
	// sign-in are kept, missing values are filled in.
	wantA := []string{
		"alice@contoso.com|Alice|u-alice",
		"robert@contoso.com|Bob|u-bob",
		"|Guest|u-guest", // no usable e-mail address
	}
	if got := e.members(gA.ID); !slices.Equal(got, wantA) {
		t.Errorf("members of A =\n%q\nwant\n%q", got, wantA)
	}
	ids := e.userIDs(gA.ID)
	if ids["u-alice"] != alice.ID || ids["u-bob"] != bob.ID {
		t.Errorf("users not matched: %v, alice=%d bob=%d", ids, alice.ID, bob.ID)
	}
	if got := e.members(gB.ID); !slices.Equal(got, []string{"alice@contoso.com|Alice|u-alice"}) {
		t.Errorf("members of B = %q", got)
	}

	if g := e.getGroup(gA.ID); g.Name != "Service Owners" || g.LastSyncedAt == nil {
		t.Errorf("group A = %+v", g)
	}
	if g := e.getGroup(gB.ID); g.Name != "Editors" || g.Role != store.RoleEditor {
		t.Errorf("group B = %+v", g)
	}
	if g := e.getGroup(gC.ID); g.Name != "Gone" || g.LastSyncedAt != nil {
		t.Errorf("missing group C changed: %+v", g)
	}
	if g := e.getGroup(gD.ID); g.LastSyncedAt != nil || g.MemberCount != 0 {
		t.Errorf("unused group D synchronised: %+v", g)
	}

	// Members leave and join; the group is renamed.
	e.dir.set(syncGroupA, "Service Owners EU",
		syncUser("u-alice", "Alice Example", "alice@contoso.com"),
		syncUser("u-dave", "Dave", "dave@contoso.com"))
	if res, err = e.syncer.SyncNow(ctx); err != nil || res.Groups != 2 || res.Members != 3 {
		t.Fatalf("second sync: %+v, %v", res, err)
	}
	wantA = []string{"alice@contoso.com|Alice|u-alice", "dave@contoso.com|Dave|u-dave"}
	if got := e.members(gA.ID); !slices.Equal(got, wantA) {
		t.Errorf("members of A after the second sync =\n%q\nwant\n%q", got, wantA)
	}
	if g := e.getGroup(gA.ID); g.Name != "Service Owners EU" {
		t.Errorf("group A name = %q", g.Name)
	}

	// The synced members are notified.
	svc, err := e.store.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var serviceIDs []int64
	for _, s := range svc {
		serviceIDs = append(serviceIDs, s.ID)
	}
	recipients, err := e.store.RecipientsForServices(ctx, serviceIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(recipients) != 2 || recipients[0].Email != "alice@contoso.com" || recipients[1].Email != "dave@contoso.com" {
		t.Errorf("recipients = %+v", recipients)
	}

	// The Graph client (and its access token) is reused.
	if n := e.clients.Load(); n != 1 {
		t.Errorf("%d Graph clients created, want 1", n)
	}
	e.settings(func(st *store.Settings) { st.OIDCClientSecret = "new-secret" })
	if _, err := e.syncer.SyncNow(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.clients.Load(); n != 2 {
		t.Errorf("%d Graph clients created after changing the secret, want 2", n)
	}
}

func TestSyncNotEnabled(t *testing.T) {
	e := newSyncEnv(t)
	ctx := context.Background()

	tests := []struct {
		name   string
		change func(st *store.Settings)
	}{
		{"defaults", func(st *store.Settings) { *st = store.DefaultSettings() }},
		{"sync off", func(st *store.Settings) { st.DirectorySyncEnabled = false }},
		{"no issuer", func(st *store.Settings) { st.OIDCIssuer = "" }},
		{"no client id", func(st *store.Settings) { st.OIDCClientID = " " }},
		{"no secret", func(st *store.Settings) { st.OIDCClientSecret = "" }},
		{"not Entra ID", func(st *store.Settings) { st.OIDCIssuer = "https://keycloak.example.com/realms/it" }},
	}
	for _, tc := range tests {
		e.settings(tc.change)
		if e.syncer.Enabled(ctx) {
			t.Errorf("%s: Enabled = true", tc.name)
		}
		if _, err := e.syncer.SyncNow(ctx); !errors.Is(err, ErrNotEnabled) {
			t.Errorf("%s: SyncNow error = %v, want ErrNotEnabled", tc.name, err)
		}
		if _, err := e.syncer.SearchGroups(ctx, "IT"); !errors.Is(err, ErrNotEnabled) {
			t.Errorf("%s: SearchGroups error = %v, want ErrNotEnabled", tc.name, err)
		}
	}

	e.settings(nil)
	if !e.syncer.Enabled(ctx) {
		t.Error("complete settings: Enabled = false")
	}
}

func TestSyncStopsWithoutAccessToken(t *testing.T) {
	e := newSyncEnv(t)
	e.settings(nil)
	e.group(syncGroupA, "", store.RoleNone, true)
	e.group(syncGroupB, "", store.RoleEditor, false)
	e.graph.tokenStatus = 401
	e.graph.tokenBody = `{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided."}`

	res, err := e.syncer.SyncNow(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not get an access token") || !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Errorf("error = %v", err)
	}
	if res.Groups != 0 || len(res.Errors) != 0 {
		t.Errorf("result = %+v", res)
	}
	e.graph.mu.Lock()
	defer e.graph.mu.Unlock()
	if e.graph.tokenCalls != 1 {
		t.Errorf("token requested %d times, want 1 (the run stops at the first group)", e.graph.tokenCalls)
	}
}

func TestSyncPerGroupGraphError(t *testing.T) {
	e := newSyncEnv(t)
	e.settings(nil)
	gA := e.group(syncGroupA, "Service Owners", store.RoleNone, true)
	gB := e.group(syncGroupB, "", store.RoleEditor, false)
	e.dir.set(syncGroupA, "Service Owners", syncUser("u-alice", "Alice", "alice@contoso.com"))
	e.dir.set(syncGroupB, "Editors", syncUser("u-bob", "Bob", "bob@contoso.com"))
	// Reading the members of A is forbidden (e.g. hidden membership).
	e.graph.handle("GET /v1.0/groups/"+syncGroupA+"/transitiveMembers/microsoft.graph.user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, graphErrorBody("Authorization_RequestDenied", "Insufficient privileges to complete the operation."))
	})

	res, err := e.syncer.SyncNow(context.Background())
	if err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if res.Groups != 1 || res.Members != 1 || len(res.Errors) != 1 ||
		!strings.HasPrefix(res.Errors[0], `group "Service Owners" (`+syncGroupA+`): `) ||
		!strings.Contains(res.Errors[0], "Insufficient privileges") {
		t.Errorf("result = %+v", res)
	}
	if len(e.members(gA.ID)) != 0 || len(e.members(gB.ID)) != 1 {
		t.Errorf("members: A=%q B=%q", e.members(gA.ID), e.members(gB.ID))
	}
}

func TestSyncSerialised(t *testing.T) {
	e := newSyncEnv(t)
	e.settings(nil)
	for i, id := range []string{syncGroupA, syncGroupB} {
		e.group(id, "", store.RoleNone, true)
		e.dir.set(id, "Group", syncUser("u"+string(rune('0'+i)), "User", ""))
	}

	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := e.syncer.SyncNow(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if e.dir.overlap.Load() {
		t.Error("sync runs overlapped")
	}
}

func TestSyncerSearchGroups(t *testing.T) {
	e := newSyncEnv(t)
	e.settings(nil)
	got, err := e.syncer.SearchGroups(context.Background(), "Serv")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (Group{ID: syncGroupA, Name: "Service Owners"}) {
		t.Errorf("SearchGroups = %+v", got)
	}
}

func TestSyncerRun(t *testing.T) {
	e := newSyncEnv(t)
	e.settings(nil)
	g := e.group(syncGroupA, "", store.RoleNone, true)
	e.dir.set(syncGroupA, "Service Owners", syncUser("u-alice", "Alice", "alice@contoso.com"))
	e.syncer.firstDelay = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.syncer.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for e.getGroup(g.ID).LastSyncedAt == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.getGroup(g.ID); got.LastSyncedAt == nil || got.MemberCount != 1 {
		t.Errorf("group after the first run = %+v", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after the context was cancelled")
	}
}
