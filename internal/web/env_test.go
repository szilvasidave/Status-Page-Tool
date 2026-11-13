package web

// Shared full-stack test harness for the web package. Use it from all handler
// tests; do not redefine these helpers (add your own with a distinct prefix).

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/groupnames"
	"github.com/szilvasidave/status-page-tool/internal/notify"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

const (
	testAdminUser = "admin"
	testAdminPass = "admin-password"
)

// testEnv is a running server (normal mode) backed by a fresh SQLite
// database, with an HTTP client that keeps cookies and does not follow
// redirects.
type testEnv struct {
	t       *testing.T
	DataDir string
	Store   *store.Store
	Auth    *auth.Manager
	Server  *httptest.Server
	Client  *http.Client
}

// newTestEnv starts a server whose setup is complete (a local admin exists).
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnvNoAdmin(t)
	h, err := auth.HashPassword(testAdminPass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.CreateLocalAdmin(context.Background(), testAdminUser, "Administrator", h); err != nil {
		t.Fatal(err)
	}
	return e
}

// newTestEnvNoAdmin starts a server without a local admin (setup pending).
func newTestEnvNoAdmin(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	cfg := &config.Config{Listen: ":0", DBDriver: config.DriverSQLite, DBDSN: "statuspage.db", SecretKey: config.NewSecretKey()}
	d, err := db.Open(ctx, config.DriverSQLite, filepath.Join(dir, "statuspage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := cfg.Key()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(d, key)
	am := auth.New(st, key, log)
	h, err := NewHandler(Deps{
		Config: cfg, DataDir: dir, Store: st, Auth: am, Notifier: notify.New(st, log), GroupNames: groupnames.New(st, log),
		Version: "test", Log: log, SetupToken: "test-setup-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &testEnv{t: t, DataDir: dir, Store: st, Auth: am, Server: srv, Client: client}
}

// Response is a fully read HTTP response.
type Response struct {
	Status   int
	Location string
	Body     string
	Header   http.Header
}

func (e *testEnv) do(req *http.Request) Response {
	e.t.Helper()
	resp, err := e.Client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return Response{Status: resp.StatusCode, Location: resp.Header.Get("Location"), Body: string(b), Header: resp.Header}
}

// get performs a GET request.
func (e *testEnv) get(path string) Response {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.Server.URL+path, nil)
	return e.do(req)
}

// csrf returns the CSRF token for the client's cookie jar (fetching a page
// first if the client has no CSRF cookie yet).
func (e *testEnv) csrf() string {
	e.t.Helper()
	u, _ := url.Parse(e.Server.URL)
	for _, c := range e.Client.Jar.Cookies(u) {
		if c.Name == auth.CSRFCookie {
			return c.Value
		}
	}
	e.get("/login")
	for _, c := range e.Client.Jar.Cookies(u) {
		if c.Name == auth.CSRFCookie {
			return c.Value
		}
	}
	e.t.Fatal("no CSRF cookie set")
	return ""
}

// post submits a form with a valid CSRF token and same-origin Origin header.
// Multi-value fields can be passed via url.Values.
func (e *testEnv) post(path string, form url.Values) Response {
	e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if form.Get(auth.CSRFField) == "" {
		form.Set(auth.CSRFField, e.csrf())
	}
	req, _ := http.NewRequest(http.MethodPost, e.Server.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.Server.URL)
	return e.do(req)
}

// loginAdmin signs the client in as the local administrator.
func (e *testEnv) loginAdmin() {
	e.t.Helper()
	r := e.post("/login", url.Values{"username": {testAdminUser}, "password": {testAdminPass}, "next": {"/"}})
	if r.Status != http.StatusSeeOther {
		e.t.Fatalf("admin login failed: %d %s", r.Status, r.Body)
	}
}

// loginAs creates (or reuses) an SSO user that is a member of a group with
// the given role (store.RoleViewer = group without a role) and signs the
// client in as that user. Returns the user.
func (e *testEnv) loginAs(email string, role store.Role) *store.User {
	e.t.Helper()
	ctx := context.Background()
	u, err := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "sub-" + email, Email: email, Name: email})
	if err != nil {
		e.t.Fatal(err)
	}
	groupID := "test-group-" + role.String()
	g, err := e.Store.CreateGroup(ctx, groupID, groupID)
	if err != nil {
		e.t.Fatal(err)
	}
	if role > store.RoleViewer {
		if err := e.Store.UpdateGroup(ctx, g.ID, g.Name, role); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := e.Store.SetUserGroups(ctx, u.ID, []store.GroupRef{{ExternalID: groupID}}); err != nil {
		e.t.Fatal(err)
	}
	e.loginUserID(u.ID)
	return u
}

// loginUserID starts a session for an existing user and stores the cookie
// in the client's jar.
func (e *testEnv) loginUserID(userID int64) {
	e.t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, e.Server.URL+"/", nil)
	if err := e.Auth.StartSession(context.Background(), rec, req, userID); err != nil {
		e.t.Fatal(err)
	}
	u, _ := url.Parse(e.Server.URL)
	e.Client.Jar.SetCookies(u, rec.Result().Cookies())
}

// logout drops all cookies of the client.
func (e *testEnv) logout() {
	jar, _ := cookiejar.New(nil)
	e.Client.Jar = jar
}

// mustContain fails the test if body does not contain all substrings.
func mustContain(t *testing.T, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(body, s) {
			t.Errorf("response does not contain %q", s)
		}
	}
}
