package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/config"
)

// buildCLI builds the statuspage command into a temporary directory.
func buildCLI(t *testing.T, version string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "statuspage")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", bin,
		"github.com/szilvasidave/status-page-tool/cmd/statuspage")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// runCLI runs the command and returns its exit code and output.
func runCLI(t *testing.T, bin, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out, errOut bytes.Buffer
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String(), errOut.String()
	} else if err != nil {
		t.Fatalf("%s %v: %v", bin, args, err)
	}
	return 0, out.String(), errOut.String()
}

// server is "statuspage run" running as a subprocess.
type server struct {
	t    *testing.T
	cmd  *exec.Cmd
	out  *logBuffer
	done chan error
	URL  string
}

func startServer(t *testing.T, bin, dataDir, addr string) *server {
	t.Helper()
	s := &server{t: t, out: &logBuffer{}, done: make(chan error, 1), URL: "http://" + addr}
	s.cmd = exec.Command(bin, "run", "-data", dataDir)
	s.cmd.Stdout, s.cmd.Stderr = s.out, s.out
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { s.done <- s.cmd.Wait() }()
	t.Cleanup(func() {
		if s.cmd != nil {
			_ = s.cmd.Process.Kill()
			<-s.done
		}
		if t.Failed() {
			t.Logf("output of statuspage run:\n%s", s.out)
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		if resp, err := http.Get(s.URL + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return s
			}
		}
		select {
		case err := <-s.done:
			s.cmd = nil
			t.Fatalf("statuspage run exited during start: %v\n%s", err, s.out)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("statuspage run did not answer on %s\n%s", s.URL, s.out)
		}
	}
}

// stop interrupts the server (Ctrl+C) and waits for a clean exit.
func (s *server) stop() {
	s.t.Helper()
	if err := s.cmd.Process.Signal(os.Interrupt); err != nil {
		s.t.Fatal(err)
	}
	select {
	case err := <-s.done:
		s.cmd = nil
		if err != nil {
			s.t.Fatalf("statuspage run exited with %v after Ctrl+C\n%s", err, s.out)
		}
	case <-time.After(30 * time.Second):
		s.t.Fatalf("statuspage run did not stop after Ctrl+C\n%s", s.out)
	}
}

func TestCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stopping the server needs Ctrl+C (SIGINT)")
	}
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildCLI(t, "9.8.7-e2e")

	// version
	code, out, _ := runCLI(t, bin, "", "version")
	if code != 0 || !strings.HasPrefix(out, "statuspage 9.8.7-e2e (go") {
		t.Fatalf("version: exit %d, output %q", code, out)
	}

	// setup on the command line, as the installer does.
	dataDir := t.TempDir()
	addr := freeAddr(t)
	resultFile := filepath.Join(t.TempDir(), "result.txt")
	code, out, errOut := runCLI(t, bin, "", "setup", "-data", dataDir, "-db", "sqlite", "-listen", addr, "-result-file", resultFile)
	if code != 0 || !strings.HasPrefix(out, "OK:") {
		t.Fatalf("setup: exit %d\n%s%s", code, out, errOut)
	}
	if b, err := os.ReadFile(resultFile); err != nil || string(b) != "OK" {
		t.Fatalf("result file: %q, %v", b, err)
	}
	var cfg config.Config
	if b, err := os.ReadFile(filepath.Join(dataDir, config.FileName)); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(b, &cfg); err != nil || cfg.Listen != addr || cfg.DBDriver != "sqlite" {
		t.Fatalf("config.json: %+v, %v", cfg, err)
	}
	// A wrong database type is reported in the result file.
	code, _, _ = runCLI(t, bin, "", "setup", "-data", t.TempDir(), "-db", "oracle", "-result-file", resultFile)
	if b, _ := os.ReadFile(resultFile); code != 1 || !strings.Contains(string(b), "oracle") {
		t.Fatalf("setup with a wrong database: exit %d, result %q", code, b)
	}

	// run: the database is configured, so the wizard asks for the
	// administrator only.
	srv := startServer(t, bin, dataDir, addr)
	b := newBrowser(t, "admin")
	b.get(srv.URL + "/").expectRedirect("/setup")
	p := b.get(srv.URL + "/setup").expectStatus(200).expectText("Create the administrator").expectNoText("Welcome to Status Page")
	p = p.form("/setup").set("username", "admin").set("password", "first-password-1").set("password_confirm", "first-password-1").
		set("site_name", "CLI Status").post()
	p.expectRedirect("/admin/settings")
	p.follow().expectStatus(200).expectText("CLI Status")
	srv.stop()
	if _, err := os.Stat(filepath.Join(dataDir, "statuspage.log")); err != nil {
		t.Errorf("no log file in the data directory: %v", err)
	}

	// reset-password reads the new password from standard input.
	code, out, errOut = runCLI(t, bin, "second-password-2\n", "reset-password", "-username", "admin", "-data", dataDir)
	if code != 0 || !strings.Contains(out, `The password of "admin" was changed.`) {
		t.Fatalf("reset-password: exit %d\n%s%s", code, out, errOut)
	}
	code, _, errOut = runCLI(t, bin, "short\n", "reset-password", "-username", "admin", "-data", dataDir)
	if code != 1 || !strings.Contains(errOut, "at least 10 characters") {
		t.Fatalf("reset-password with a short password: exit %d, %q", code, errOut)
	}
	code, _, errOut = runCLI(t, bin, "third-password-3\n", "reset-password", "-username", "nobody", "-data", dataDir)
	if code != 1 || !strings.Contains(errOut, `no local account named "nobody"`) {
		t.Fatalf("reset-password for an unknown user: exit %d, %q", code, errOut)
	}

	// The old browser session ends with the password reset; the new
	// password works after the restart, the old one does not.
	srv = startServer(t, bin, dataDir, addr)
	b.get(srv.URL + "/admin/settings").expectRedirect("/login?next=%2Fadmin%2Fsettings")
	login := func(password string) *page {
		b := newBrowser(t, "login")
		return b.get(srv.URL+"/login").expectStatus(200).form("/login").
			set("username", "admin").set("password", password).post()
	}
	login("first-password-1").expectStatus(401)
	p = login("second-password-2")
	p.expectRedirect("/")
	p.follow().expectStatus(200).expectText("CLI Status")
	srv.stop()
}
