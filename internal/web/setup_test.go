package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
)

const setupTestToken = "test-setup-token" // matches newTestEnvNoAdmin

// setupPostHeaders submits a form like e.post with additional headers.
func setupPostHeaders(e *testEnv, path string, form url.Values, headers map[string]string) Response {
	e.t.Helper()
	if form.Get(auth.CSRFField) == "" {
		form.Set(auth.CSRFField, e.csrf())
	}
	req, _ := http.NewRequest(http.MethodPost, e.Server.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.Server.URL)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return e.do(req)
}

func setupAdminForm() url.Values {
	return url.Values{
		"username": {"Root"}, "name": {"Rita Root"},
		"password": {"correct-horse-1"}, "password_confirm": {"correct-horse-1"},
		"site_name": {"ACME Status"}, "base_url": {"https://status.example.com/"},
		"timezone": {"Europe/Vienna"}, "public_access": {"on"},
	}
}

func setupHasAdmin(t *testing.T, e *testEnv) bool {
	t.Helper()
	ok, err := e.Store.HasLocalAdmin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// setupFakeHostname makes the setup wizard see name as the computer name.
func setupFakeHostname(t *testing.T, name string) {
	t.Helper()
	orig := setupHostname
	setupHostname = func() (string, error) { return name, nil }
	t.Cleanup(func() { setupHostname = orig })
}

func TestSetupSuggestBaseURL(t *testing.T) {
	setupFakeHostname(t, "SRV-STATUS")
	for _, c := range []struct {
		host string
		tls  bool
		want string
	}{
		{"localhost:8080", false, "http://srv-status:8080"},
		{"127.0.0.1:8080", false, "http://srv-status:8080"},
		{"[::1]:8080", false, "http://srv-status:8080"},
		{"localhost", true, "https://srv-status"},
		{"status.example.com:8080", false, "http://status.example.com:8080"},
		{"status.example.com", true, "https://status.example.com"},
		{"10.0.0.5:8080", false, "http://10.0.0.5:8080"},
		{"[fd00::5]:8080", false, "http://[fd00::5]:8080"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/setup", nil)
		r.Host = c.host
		if c.tls {
			r.Header.Set("X-Forwarded-Proto", "https")
		}
		if got := setupSuggestBaseURL(r); got != c.want {
			t.Errorf("Host %q: got %q, want %q", c.host, got, c.want)
		}
	}
	// No usable computer name: no suggestion rather than localhost.
	setupHostname = func() (string, error) { return "", errors.New("no name") }
	r := httptest.NewRequest(http.MethodGet, "/setup", nil)
	r.Host = "localhost:8080"
	if got := setupSuggestBaseURL(r); got != "" {
		t.Errorf("without a computer name: got %q, want empty", got)
	}
}

func TestSetupAdminStep(t *testing.T) {
	e := newTestEnvNoAdmin(t)
	tokenFile := filepath.Join(e.DataDir, config.SetupTokenFile)
	if err := os.WriteFile(tokenFile, []byte(setupTestToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if r := e.get("/"); r.Status != http.StatusSeeOther || r.Location != "/setup" {
		t.Fatalf("GET / before setup: %d %q", r.Status, r.Location)
	}
	setupFakeHostname(t, "Status-SRV")
	r := e.get("/setup")
	if r.Status != http.StatusOK {
		t.Fatalf("GET /setup: %d", r.Status)
	}
	// The page is opened from the server itself (127.0.0.1): the suggested
	// base URL uses the computer name, which other computers can reach.
	port := e.Server.URL[strings.LastIndex(e.Server.URL, ":"):]
	mustContain(t, r.Body, "Create the administrator", `name="username" type="text" value=""`, `placeholder="e.g. it-admin"`,
		`value="http://status-srv`+port+`"`, `value="UTC"`, `aria-current="step"`, "Administrator", `name="csrf_token"`)
	if strings.Contains(r.Body, `name="setup_token"`) {
		t.Error("the setup token is asked for a local request")
	}

	// Validation errors keep the entered values and create nothing.
	tests := []struct {
		field, value, want string
	}{
		{"password", "short", "at least 10 characters"},
		{"password_confirm", "something-else", "The passwords do not match."},
		{"username", "", "Enter a user name."},
		{"username", "has space", "no spaces"},
		{"site_name", "", "Enter a name for the status page."},
		{"base_url", "ftp://example.com", "starting with http:// or https://"},
		{"timezone", "Nowhere/City", "Unknown time zone"},
	}
	for _, tt := range tests {
		f := setupAdminForm()
		f.Set(tt.field, tt.value)
		r := e.post("/setup", f)
		if r.Status != http.StatusUnprocessableEntity {
			t.Fatalf("%s=%q: got %d, want 422", tt.field, tt.value, r.Status)
		}
		mustContain(t, r.Body, tt.want, `value="Rita Root"`)
	}
	if setupHasAdmin(t, e) {
		t.Fatal("an administrator was created from invalid input")
	}

	// Without a CSRF token the request is rejected by the auth middleware.
	f := setupAdminForm()
	f.Set(auth.CSRFField, "wrong")
	if r := e.post("/setup", f); r.Status != http.StatusForbidden {
		t.Fatalf("POST with a wrong CSRF token: %d", r.Status)
	}

	// Success: signed in, settings saved, token file deleted.
	r = e.post("/setup", setupAdminForm())
	if r.Status != http.StatusSeeOther || r.Location != "/admin/settings" {
		t.Fatalf("setup: %d %q %s", r.Status, r.Location, r.Body)
	}
	r = e.get("/admin/settings")
	if r.Status != http.StatusOK {
		t.Fatalf("admin settings after setup: %d", r.Status)
	}
	mustContain(t, r.Body, "Setup complete. Next: configure single sign-on and e-mail.", "Rita Root")
	u, err := e.Store.GetUserByUsername(context.Background(), "root")
	if err != nil || !u.IsLocalAdmin || u.Name != "Rita Root" || !auth.CheckPassword(u.PasswordHash, "correct-horse-1") {
		t.Fatalf("administrator: %+v %v", u, err)
	}
	st := adminSettings(t, e)
	if st.SiteName != "ACME Status" || st.BaseURL != "https://status.example.com" || st.Timezone != "Europe/Vienna" || !st.PublicAccess {
		t.Fatalf("settings after setup: %+v", st)
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup token file not deleted: %v", err)
	}

	// The setup cannot run twice.
	for _, r := range []Response{e.get("/setup"), e.post("/setup", setupAdminForm())} {
		if r.Status != http.StatusSeeOther || r.Location != "/login" {
			t.Fatalf("setup after completion: %d %q", r.Status, r.Location)
		}
	}
	if users, _ := e.Store.ListUsers(context.Background()); len(users) != 1 {
		t.Fatalf("got %d users, want 1", len(users))
	}
}

func TestSetupAdminStepToken(t *testing.T) {
	e := newTestEnvNoAdmin(t)
	proxied := map[string]string{"X-Forwarded-For": "203.0.113.7"}

	req, _ := http.NewRequest(http.MethodGet, e.Server.URL+"/setup", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	r := e.do(req)
	mustContain(t, r.Body, `name="setup_token"`, config.SetupTokenFile)

	// Missing or wrong token.
	for _, token := range []string{"", "wrong-token"} {
		f := setupAdminForm()
		f.Set("setup_token", token)
		r := setupPostHeaders(e, "/setup", f, proxied)
		if r.Status != http.StatusForbidden {
			t.Fatalf("token %q: got %d, want 403", token, r.Status)
		}
		mustContain(t, r.Body, "Enter the correct setup token.", `name="setup_token"`)
	}
	if setupHasAdmin(t, e) {
		t.Fatal("administrator created without the setup token")
	}

	// A correct token is remembered even if other fields are invalid.
	f := setupAdminForm()
	f.Set("setup_token", " "+setupTestToken+" ")
	f.Set("password_confirm", "typo")
	r = setupPostHeaders(e, "/setup", f, proxied)
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422", r.Status)
	}
	if strings.Contains(r.Body, `name="setup_token"`) {
		t.Error("the setup token is asked again after it was entered correctly")
	}
	r = setupPostHeaders(e, "/setup", setupAdminForm(), map[string]string{"Forwarded": "for=203.0.113.7"})
	if r.Status != http.StatusSeeOther {
		t.Fatalf("setup with remembered token: %d %s", r.Status, r.Body)
	}
	if !setupHasAdmin(t, e) {
		t.Fatal("no administrator created")
	}
}

func TestSetupNeedsToken(t *testing.T) {
	s := &Server{Deps: Deps{SetupToken: "x"}}
	tests := []struct {
		remote string
		header string
		want   bool
	}{
		{"127.0.0.1:5000", "", false},
		{"[::1]:5000", "", false},
		{"192.0.2.10:5000", "", true},
		{"[2001:db8::1]:5000", "", true},
		{"127.0.0.1:5000", "X-Forwarded-For", true},
		{"127.0.0.1:5000", "Forwarded", true},
		{"127.0.0.1:5000", "X-Real-Ip", true},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/setup", nil)
		r.RemoteAddr = tt.remote
		if tt.header != "" {
			r.Header.Set(tt.header, "198.51.100.1")
		}
		if got := s.setupNeedsToken(r); got != tt.want {
			t.Errorf("%s %s: got %v, want %v", tt.remote, tt.header, got, tt.want)
		}
	}
	s.SetupToken = ""
	r := httptest.NewRequest(http.MethodGet, "/setup", nil)
	r.RemoteAddr = "192.0.2.10:5000"
	if s.setupNeedsToken(r) {
		t.Error("token required although none is configured")
	}
}

// ------------------------------------------------------------------ setup mode

// setupModeEnv is a server in setup mode (no database) with a fake Activate.
type setupModeEnv struct {
	t         *testing.T
	DataDir   string
	Server    *httptest.Server
	Client    *http.Client
	Activated []*config.Config
}

func newSetupModeEnv(t *testing.T, activateErr error) *setupModeEnv {
	t.Helper()
	return newSetupModeEnvListen(t, activateErr, "")
}

// newSetupModeEnvListen is newSetupModeEnv for a server started with the
// listen address listen (http.Server.Addr, like app.Run sets it).
func newSetupModeEnvListen(t *testing.T, activateErr error, listen string) *setupModeEnv {
	t.Helper()
	m := &setupModeEnv{t: t, DataDir: t.TempDir()}
	h, err := NewSetupHandler(Deps{
		DataDir: m.DataDir, Version: "test", SetupToken: setupTestToken,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Activate: func(cfg *config.Config) error {
			m.Activated = append(m.Activated, cfg)
			return activateErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Server = httptest.NewUnstartedServer(h)
	m.Server.Config.Addr = listen
	m.Server.Start()
	t.Cleanup(m.Server.Close)
	jar, _ := cookiejar.New(nil)
	m.Client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return m
}

func (m *setupModeEnv) do(req *http.Request) Response {
	m.t.Helper()
	resp, err := m.Client.Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return Response{Status: resp.StatusCode, Location: resp.Header.Get("Location"), Body: string(b), Header: resp.Header}
}

func (m *setupModeEnv) get(path string) Response {
	req, _ := http.NewRequest(http.MethodGet, m.Server.URL+path, nil)
	return m.do(req)
}

// csrf returns the setup-mode CSRF cookie (after a GET of the form).
func (m *setupModeEnv) csrf() string {
	u, _ := url.Parse(m.Server.URL)
	for _, c := range m.Client.Jar.Cookies(u) {
		if c.Name == setupCSRFCookie {
			return c.Value
		}
	}
	m.get("/setup")
	for _, c := range m.Client.Jar.Cookies(u) {
		if c.Name == setupCSRFCookie {
			return c.Value
		}
	}
	m.t.Fatal("no setup CSRF cookie")
	return ""
}

// post submits the database form with a valid CSRF token unless the form
// has one; headers are added to the request.
func (m *setupModeEnv) post(form url.Values, headers map[string]string) Response {
	m.t.Helper()
	if _, ok := form[auth.CSRFField]; !ok {
		form.Set(auth.CSRFField, m.csrf())
	}
	req, _ := http.NewRequest(http.MethodPost, m.Server.URL+"/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", m.Server.URL)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return m.do(req)
}

func TestSetupDatabaseStepSQLite(t *testing.T) {
	m := newSetupModeEnv(t, nil)

	if r := m.get("/admin/settings"); r.Status != http.StatusSeeOther || r.Location != "/setup" {
		t.Fatalf("GET /admin/settings in setup mode: %d %q", r.Status, r.Location)
	}
	r := m.get("/setup")
	if r.Status != http.StatusOK {
		t.Fatalf("GET /setup: %d", r.Status)
	}
	token := m.csrf()
	mustContain(t, r.Body, "SQLite (built in)", "PostgreSQL", `data-show-when="postgres"`, "Test connection",
		"Save and continue", `name="csrf_token" value="`+token+`"`, filepath.Join(m.DataDir, "statuspage.db"),
		`aria-current="step"`)
	if strings.Contains(r.Body, `name="setup_token"`) || strings.Contains(r.Body, `href="/login"`) {
		t.Error("unexpected setup token field or navigation in setup mode")
	}

	// CSRF: missing/wrong token and foreign origins are rejected.
	sqlite := func() url.Values { return url.Values{"driver": {"sqlite"}, "action": {"save"}} }
	f := sqlite()
	f.Set(auth.CSRFField, "")
	if r := m.post(f, nil); r.Status != http.StatusForbidden {
		t.Fatalf("POST without CSRF token: %d", r.Status)
	}
	f = sqlite()
	f.Set(auth.CSRFField, "wrong-token-wrong-token")
	if r := m.post(f, nil); r.Status != http.StatusForbidden {
		t.Fatalf("POST with wrong CSRF token: %d", r.Status)
	}
	if r := m.post(sqlite(), map[string]string{"Origin": "https://evil.example.com"}); r.Status != http.StatusForbidden {
		t.Fatalf("POST from a foreign origin: %d", r.Status)
	}

	// Setup token for proxied requests.
	if r := m.post(sqlite(), map[string]string{"X-Forwarded-For": "203.0.113.7"}); r.Status != http.StatusForbidden {
		t.Fatalf("proxied POST without setup token: %d", r.Status)
	} else {
		mustContain(t, r.Body, "setup token", `name="setup_token"`)
	}

	// Validation of the PostgreSQL fields.
	r = m.post(url.Values{"driver": {"postgres"}, "pg_host": {"db"}, "pg_port": {"abc"}, "pg_database": {"sp"}, "pg_user": {"sp"}, "action": {"test"}}, nil)
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("bad port: %d", r.Status)
	}
	mustContain(t, r.Body, "must be a number", `value="abc"`, `value="db"`)
	r = m.post(url.Values{"driver": {"postgres"}, "pg_host": {""}, "pg_database": {"sp"}, "pg_user": {"sp"}, "action": {"test"}}, nil)
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("no host: %d", r.Status)
	}
	mustContain(t, r.Body, "Please enter the PostgreSQL server name")

	// Test connection writes nothing.
	r = m.post(url.Values{"driver": {"sqlite"}, "action": {"test"}}, nil)
	if r.Status != http.StatusOK {
		t.Fatalf("test SQLite: %d %s", r.Status, r.Body)
	}
	mustContain(t, r.Body, "Everything is fine")
	if _, err := os.Stat(config.Path(m.DataDir)); err == nil || len(m.Activated) != 0 {
		t.Fatal("test connection wrote the configuration")
	}

	// Save (from another computer with the setup token).
	f = url.Values{"driver": {"sqlite"}, "sqlite_path": {"db/status.db"}, "action": {"save"}, "setup_token": {setupTestToken}}
	r = m.post(f, map[string]string{"X-Forwarded-For": "203.0.113.7"})
	if r.Status != http.StatusSeeOther || r.Location != "/setup" {
		t.Fatalf("save: %d %q %s", r.Status, r.Location, r.Body)
	}
	if len(m.Activated) != 1 {
		t.Fatalf("Activate called %d times", len(m.Activated))
	}
	cfg, err := config.Load(m.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	port := m.Server.URL[strings.LastIndex(m.Server.URL, ":"):]
	if cfg.DBDriver != config.DriverSQLite || cfg.DBDSN != filepath.Join("db", "status.db") || cfg.Listen != port || *m.Activated[0] != *cfg {
		t.Fatalf("config: %+v, activated with %+v", cfg, m.Activated[0])
	}
	if _, err := os.Stat(filepath.Join(m.DataDir, "db", "status.db")); err != nil {
		t.Fatalf("database file: %v", err)
	}
	// The token is remembered for the administrator step.
	if sc := strings.Join(r.Header.Values("Set-Cookie"), "\n"); !strings.Contains(sc, setupTokenCookie+"="+setupTestToken+"; Path=/setup") {
		t.Errorf("setup token not remembered: %s", sc)
	}

	// A second submission that still reaches the setup handler (another tab,
	// or a click while the first one was saving) continues with the next
	// step and neither rewrites config.json nor activates again.
	before, err := os.ReadFile(config.Path(m.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	r = m.post(url.Values{"driver": {"postgres"}, "pg_host": {"db.example.com"}, "action": {"save"}, "setup_token": {setupTestToken}}, nil)
	if r.Status != http.StatusSeeOther || r.Location != "/setup" {
		t.Fatalf("second save: %d %q %s", r.Status, r.Location, r.Body)
	}
	if after, _ := os.ReadFile(config.Path(m.DataDir)); string(after) != string(before) || len(m.Activated) != 1 {
		t.Fatalf("second save changed the configuration (activated %d times):\n%s", len(m.Activated), after)
	}
}

// TestSetupDatabaseStepKeepsListenHost: a server started with
// -listen 127.0.0.1:<port> (e.g. behind a reverse proxy) keeps listening on
// the loopback interface only after a restart without -listen.
func TestSetupDatabaseStepKeepsListenHost(t *testing.T) {
	m := newSetupModeEnvListen(t, nil, "127.0.0.1:0")
	r := m.get("/setup")
	port := m.Server.URL[strings.LastIndex(m.Server.URL, ":")+1:]
	mustContain(t, r.Body, "keeps listening on port "+port+".")
	if r := m.post(url.Values{"driver": {"sqlite"}, "action": {"save"}}, nil); r.Status != http.StatusSeeOther {
		t.Fatalf("save: %d %s", r.Status, r.Body)
	}
	cfg, err := config.Load(m.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := "127.0.0.1:" + port; cfg.Listen != want {
		t.Errorf("listen = %q, want %q", cfg.Listen, want)
	}
}

func TestSetupDatabaseStepActivateError(t *testing.T) {
	m := newSetupModeEnv(t, errors.New("database is locked"))
	r := m.post(url.Values{"driver": {"sqlite"}, "action": {"save"}}, nil)
	if r.Status != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", r.Status)
	}
	mustContain(t, r.Body, "could not switch to the new database: database is locked", "restart")
}

// TestSetupDatabaseStepPostgres runs the database step against the
// PostgreSQL database of STATUSPAGE_TEST_PG_DSN (its public schema is reset).
func TestSetupDatabaseStepPostgres(t *testing.T) {
	dsn := os.Getenv("STATUSPAGE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("STATUSPAGE_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	d, err := db.Open(ctx, config.DriverPostgres, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	d.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	sslmode := u.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = "prefer"
	}
	form := func(action, password string) url.Values {
		return url.Values{
			"driver": {"postgres"}, "pg_host": {u.Hostname()}, "pg_port": {u.Port()},
			"pg_database": {strings.TrimPrefix(u.Path, "/")}, "pg_user": {u.User.Username()},
			"pg_password": {password}, "pg_sslmode": {sslmode}, "action": {action},
		}
	}

	m := newSetupModeEnv(t, nil)
	r := m.post(form("test", pw+"-wrong"), nil)
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("wrong password: %d", r.Status)
	}
	mustContain(t, r.Body, "Could not connect to PostgreSQL")
	r = m.post(form("test", pw), nil)
	if r.Status != http.StatusOK {
		t.Fatalf("test: %d %s", r.Status, r.Body)
	}
	mustContain(t, r.Body, "Connection successful")
	r = m.post(form("save", pw), nil)
	if r.Status != http.StatusSeeOther || len(m.Activated) != 1 || m.Activated[0].DBDriver != config.DriverPostgres {
		t.Fatalf("save: %d %s", r.Status, r.Body)
	}
}
