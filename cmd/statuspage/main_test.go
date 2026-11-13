package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kardianos/service"

	"github.com/szilvasidave/status-page-tool/internal/app"
	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCommands(t *testing.T) {
	if code, out, _ := runCLI(t, "version"); code != 0 || !strings.Contains(out, version) {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "-h"); code != 0 || !strings.Contains(out, "reset-password") {
		t.Fatalf("help: %d %q", code, out)
	}
	if code, _, errOut := runCLI(t, "bogus"); code != 2 || !strings.Contains(errOut, `unknown command "bogus"`) {
		t.Fatalf("unknown command: %d %q", code, errOut)
	}
	if code, _, _ := runCLI(t, "setup", "-nope"); code != 2 {
		t.Fatalf("bad flag: exit code %d", code)
	}
	if code, _, errOut := runCLI(t, "service"); code != 2 || !strings.Contains(errOut, "missing action") {
		t.Fatalf("service without action: %d %q", code, errOut)
	}
	if code, _, errOut := runCLI(t, "reset-password", "-data", t.TempDir()); code != 2 || !strings.Contains(errOut, "-username is required") {
		t.Fatalf("reset-password: %d %q", code, errOut)
	}
	code, _, errOut := runCLI(t, "reset-password", "-username", "admin", "-data", t.TempDir())
	if code != 1 || !strings.Contains(errOut, "not configured yet") {
		t.Fatalf("reset-password without config: %d %q", code, errOut)
	}
}

func TestSetupSQLite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	result := filepath.Join(t.TempDir(), "result.txt")

	// -check-only writes nothing except the result file.
	code, out, errOut := runCLI(t, "setup", "-data", dir, "-db", "sqlite", "-check-only", "-result-file", result)
	if code != 0 {
		t.Fatalf("exit code %d: %s", code, errOut)
	}
	assertResult(t, result, "OK")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("check-only created the data directory (%v); output %q", err, out)
	}

	code, _, errOut = runCLI(t, "setup", "-data", dir, "-db", "sqlite", "-listen", ":9123", "-result-file", result)
	if code != 0 {
		t.Fatalf("exit code %d: %s", code, errOut)
	}
	assertResult(t, result, "OK")
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9123" || cfg.DBDriver != config.DriverSQLite {
		t.Fatalf("unexpected config %+v", cfg)
	}

	// Running setup again (e.g. for a new database password) without
	// -listen keeps the listen address and the secret key.
	code, _, errOut = runCLI(t, "setup", "-data", dir, "-db", "sqlite")
	if code != 0 {
		t.Fatalf("exit code %d: %s", code, errOut)
	}
	cfg2, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Listen != ":9123" || cfg2.SecretKey != cfg.SecretKey {
		t.Fatalf("setup without -listen changed the config: %+v", cfg2)
	}

	// An invalid command line exits with 2 and writes no result file.
	if err := os.Remove(result); err != nil {
		t.Fatal(err)
	}
	if code, _, _ = runCLI(t, "setup", "-data", dir, "-db", "sqlite", "-bogus", "-result-file", result); code != 2 {
		t.Fatalf("invalid flag: exit code %d", code)
	}
	if _, err := os.Stat(result); !os.IsNotExist(err) {
		t.Fatalf("result file written for an invalid command line: %v", err)
	}
}

// TestWaitUntilServing: "service start" reports success only when the status
// page answers, and an error with the log file when the service stops.
func TestWaitUntilServing(t *testing.T) {
	running := func() (service.Status, error) { return service.StatusRunning, nil }
	stopped := func() (service.Status, error) { return service.StatusStopped, nil }
	wantStopped := func(what, dir string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "stopped right after starting") || !strings.Contains(err.Error(), filepath.Join(dir, logFileName)) {
			t.Errorf("%s, service stopped: %v", what, err)
		}
	}

	// The status page itself (in setup mode, on the installer's port).
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if err := os.WriteFile(filepath.Join(dir, config.SetupListenFile), []byte(addr), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx, app.Options{DataDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}()
	if err := waitUntilServing(running, dir, 10*time.Second); err != nil {
		t.Errorf("status page: %v", err)
	}
	// Something answers (e.g. a status page started by hand), but the
	// service stopped.
	wantStopped("status page", dir, waitUntilServing(stopped, dir, 5*time.Second))
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Another program on the port: one that sends unknown paths to its login
	// page, and one with its own /healthz.
	for name, h := range map[string]http.HandlerFunc{
		"redirect to login": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/login" {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			fmt.Fprintln(w, "<html>Sign in</html>")
		},
		"other health check": func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") },
	} {
		srv := httptest.NewServer(h)
		dir := t.TempDir()
		cfg := &config.Config{Listen: strings.TrimPrefix(srv.URL, "http://"), DBDriver: config.DriverSQLite, SecretKey: config.NewSecretKey()}
		if err := cfg.Save(dir); err != nil {
			t.Fatal(err)
		}
		wantStopped(name, dir, waitUntilServing(stopped, dir, 5*time.Second))
		start := time.Now()
		err := waitUntilServing(running, dir, time.Second)
		if err == nil || !strings.Contains(err.Error(), "does not answer at "+srv.URL) || time.Since(start) > 10*time.Second {
			t.Errorf("%s, service running: %v", name, err)
		}
		srv.Close()
	}

	// Nothing answers.
	wantStopped("nothing on the port", dir, waitUntilServing(stopped, dir, 5*time.Second))
	start := time.Now()
	err = waitUntilServing(running, dir, time.Second)
	if err == nil || !strings.Contains(err.Error(), "does not answer at http://"+addr) || time.Since(start) > 10*time.Second {
		t.Errorf("running service that does not answer: %v", err)
	}
}

func TestSetupParamsFile(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "data")
	result := filepath.Join(tmp, "result.txt")
	params := filepath.Join(tmp, "params.json")

	// As written by the installer: ASCII only, \u escapes, optional BOM.
	json := "\xef\xbb\xbf" + `{"driver":"postgres","listen":":8080","postgres":{"host":"127.0.0.1","port":1,"database":"stätus","user":"sp","password":"p\"w\\ü","sslmode":"disable"}}`
	if err := os.WriteFile(params, []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := readParams(params)
	if err != nil {
		t.Fatal(err)
	}
	if p.Postgres.Password != `p"w\ü` || p.Postgres.Database != "stätus" || p.Postgres.Port != 1 {
		t.Fatalf("unexpected params %+v", p)
	}

	// Port 1 is closed: the check fails with a readable message in the
	// result file and exit code 1.
	code, _, errOut := runCLI(t, "setup", "-data", dir, "-params-file", params, "-check-only", "-result-file", result)
	if code != 1 {
		t.Fatalf("exit code %d", code)
	}
	b, _ := os.ReadFile(result)
	if !strings.HasPrefix(string(b), "could not connect to PostgreSQL at 127.0.0.1:1:") || !strings.Contains(errOut, string(b)) {
		t.Fatalf("result %q, stderr %q", b, errOut)
	}

	// Mixing the file with database flags is rejected.
	code, _, errOut = runCLI(t, "setup", "-data", dir, "-params-file", params, "-db", "sqlite", "-result-file", result)
	if code != 1 || !strings.Contains(errOut, "cannot be combined with -db") {
		t.Fatalf("exit code %d: %q", code, errOut)
	}

	// No database chosen.
	code, _, errOut = runCLI(t, "setup", "-data", dir)
	if code != 1 || !strings.Contains(errOut, "-db sqlite or -db postgres") {
		t.Fatalf("exit code %d: %q", code, errOut)
	}
}

func assertResult(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Fatalf("result file: %q, want %q", b, want)
	}
}

func TestLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFileName)
	big := bytes.Repeat([]byte("x"), maxLogSize+1)
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATUSPAGE_LOG_LEVEL", "debug")
	defer slog.SetDefault(slog.Default())

	log, closeLog := newLogger(dir, false)
	log.Debug("hello", "n", 1)
	closeLog()

	if fi, err := os.Stat(path + ".1"); err != nil || fi.Size() != int64(len(big)) {
		t.Fatalf("old log not rotated: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "level=DEBUG msg=hello n=1") {
		t.Fatalf("log file content %q", b)
	}
}

func TestLogLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"": slog.LevelInfo, "debug": slog.LevelDebug, "WARN": slog.LevelWarn, "error": slog.LevelError} {
		if got, err := logLevel(in, "test"); err != nil || got != want {
			t.Errorf("logLevel(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := logLevel("loud", "log_level in config.json"); err == nil || !strings.Contains(err.Error(), `invalid log level "loud" in log_level in config.json`) {
		t.Errorf("expected an error for an invalid level, got %v", err)
	}
}

// TestLogLevelFromConfig: the Windows service does not see environment
// variables set after the last reboot, so the level can be set in
// config.json; the environment variable still overrides it.
func TestLogLevelFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DBDriver: config.DriverSQLite, SecretKey: config.NewSecretKey(), LogLevel: "debug"}
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	defer slog.SetDefault(slog.Default())

	t.Setenv("STATUSPAGE_LOG_LEVEL", "")
	log, closeLog := newLogger(dir, false)
	closeLog()
	if !log.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("log_level debug from config.json is not used")
	}
	t.Setenv("STATUSPAGE_LOG_LEVEL", "warn")
	log, closeLog = newLogger(dir, false)
	closeLog()
	if log.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("STATUSPAGE_LOG_LEVEL does not override log_level")
	}
}

func TestReadPasswordLine(t *testing.T) {
	for in, want := range map[string]string{"secret-pass\r\n": "secret-pass", "abc def\nrest": "abc def", "no newline": "no newline"} {
		got, err := readPasswordLine(strings.NewReader(in))
		if err != nil || got != want {
			t.Errorf("readPasswordLine(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := readPasswordLine(strings.NewReader("")); err == nil {
		t.Error("expected an error for empty input")
	}
}

// TestResetPassword checks that reset-password sets the new password and
// signs out the existing sessions of the account (like a password change in
// the web UI).
func TestResetPassword(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := runCLI(t, "setup", "-data", dir, "-db", "sqlite"); code != 0 {
		t.Fatalf("setup: %s", errOut)
	}
	ctx := context.Background()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := cfg.Key()
	d, err := db.OpenConfig(ctx, cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st := store.New(d, key)
	hash, _ := auth.HashPassword("old-password-1")
	u, err := st.CreateLocalAdmin(ctx, "admin", "", hash)
	if err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{ID: "session-hash", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// The password is read from standard input.
	in := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(in, []byte("new-password-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	oldStdin := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = oldStdin }()

	code, out, errOut := runCLI(t, "reset-password", "-username", "admin", "-data", dir)
	if code != 0 || !strings.Contains(out, "was changed") {
		t.Fatalf("reset-password: %d %q %q", code, out, errOut)
	}
	u, err = st.GetUserByUsername(ctx, "admin")
	if err != nil || !auth.CheckPassword(u.PasswordHash, "new-password-2") {
		t.Fatalf("the new password does not work: %v", err)
	}
	if _, err := st.GetSession(ctx, sess.ID, time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the session survived the password reset: %v", err)
	}
}
