package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/config"
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
		if got, err := logLevel(in); err != nil || got != want {
			t.Errorf("logLevel(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := logLevel("loud"); err == nil {
		t.Error("expected an error for an invalid level")
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
