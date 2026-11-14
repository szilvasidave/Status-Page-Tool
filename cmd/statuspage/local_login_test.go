package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kardianos/service"

	"github.com/szilvasidave/status-page-tool/internal/app"
	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

func TestLocalLoginCommandLine(t *testing.T) {
	if code, out, _ := runCLI(t, "-h"); code != 0 || !strings.Contains(out, "statuspage local-login enable|disable|status") {
		t.Errorf("help: %d %q", code, out)
	}
	if code, _, errOut := runCLI(t, "local-login"); code != 2 || !strings.Contains(errOut, "missing action: enable, disable or status") {
		t.Errorf("without action: %d %q", code, errOut)
	}
	if code, _, errOut := runCLI(t, "local-login", "on", "-data", t.TempDir()); code != 2 || !strings.Contains(errOut, `unknown action "on"`) {
		t.Errorf("unknown action: %d %q", code, errOut)
	}
	if code, _, errOut := runCLI(t, "local-login", "-h"); code != 0 || !strings.Contains(errOut, "always works") {
		t.Errorf("-h: %d %q", code, errOut)
	}
	// Without a configuration (usually: the wrong or no -data), the error
	// names the file it looked for and the -data option.
	dir := t.TempDir()
	if code, _, errOut := runCLI(t, "local-login", "status", "-data", dir); code != 1 ||
		!strings.Contains(errOut, filepath.Join(dir, "config.json")+" not found: pass the data directory of the status page with -data DIR") {
		t.Errorf("without configuration: %d %q", code, errOut)
	}
}

// TestLocalLogin turns the sign-in with the local administrator account off
// and on again with "statuspage local-login" while the status page runs, as
// an administrator would on the server, and checks that the running server
// applies it without a restart.
func TestLocalLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the settings cache of the running server")
	}
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if code, _, errOut := runCLI(t, "setup", "-data", dir, "-db", "sqlite", "-listen", addr); code != 0 {
		t.Fatalf("setup: %s", errOut)
	}
	ctx := context.Background()
	st, closeDB, err := openStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	hash, _ := auth.HashPassword("admin-password-1")
	if _, err := st.CreateLocalAdmin(ctx, "admin", "Ada Admin", hash); err != nil {
		t.Fatal(err)
	}

	// The status page runs.
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- app.Run(runCtx, app.Options{DataDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	base := "http://" + addr
	if err := waitUntilServing(func() (service.Status, error) { return service.StatusRunning, nil }, dir, 15*time.Second); err != nil {
		t.Fatal(err)
	}

	// A browser signed in as the local administrator.
	admin := newLocalLoginBrowser(t, base)
	if status := admin.login("admin", "admin-password-1"); status != http.StatusSeeOther {
		t.Fatalf("local sign-in: %d", status)
	}
	if status := admin.get("/account"); status != http.StatusOK {
		t.Fatalf("account page: %d", status)
	}

	local := func(action string) (int, string) {
		t.Helper()
		code, out, errOut := runCLI(t, "local-login", action, "-data", dir)
		return code, out + errOut
	}
	if code, out := local("status"); code != 0 || !strings.Contains(out, "account is on.") {
		t.Fatalf("status: %d %q", code, out)
	}

	// It cannot be turned off before single sign-on is set up and a group
	// has the Administrator role.
	if code, out := local("disable"); code != 1 || !strings.Contains(out, "single sign-on is not set up") {
		t.Fatalf("disable without single sign-on: %d %q", code, out)
	}
	settings, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.OIDCEnabled, settings.OIDCIssuer, settings.OIDCClientID, settings.OIDCClientSecret = true, "https://idp.example.com", "status", "secret"
	if err := st.SaveSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if code, out := local("disable"); code != 1 || !strings.Contains(out, "no group has the Administrator role") {
		t.Fatalf("disable without an administrator group: %d %q", code, out)
	}

	// With roles from app roles, somebody who signed in with the app role
	// for administrators in the last 30 days counts as well.
	settings.OIDCAppRoles = true
	if err := st.SaveSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if code, out := local("disable"); code != 1 || !strings.Contains(out, "nobody has signed in with the app role for administrators in the last 30 days") {
		t.Fatalf("disable without an app role administrator: %d %q", code, out)
	}
	if _, err := st.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "s-boss", Email: "boss@example.com", AppRoles: []string{"Admin"}}); err != nil {
		t.Fatal(err)
	}
	if code, out := local("disable"); code != 0 || !strings.Contains(out, "is now off") {
		t.Fatalf("disable with an app role administrator: %d %q", code, out)
	}
	if code, out := local("enable"); code != 0 || !strings.Contains(out, "is now on") {
		t.Fatalf("enable: %d %q", code, out)
	}
	settings.OIDCAppRoles = false
	if err := st.SaveSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	g, err := st.CreateGroup(ctx, "it-admins", "IT admins")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateGroup(ctx, g.ID, g.Name, store.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	// Turned off: the administrator's session ends at once, and the running
	// server refuses the local sign-in within its settings cache time.
	if code, out := local("disable"); code != 0 || !strings.Contains(out, "is now off") || !strings.Contains(out, "local-login enable -data") {
		t.Fatalf("disable: %d %q", code, out)
	}
	if code, out := local("disable"); code != 0 || !strings.Contains(out, "already off") {
		t.Fatalf("disable again: %d %q", code, out)
	}
	if code, out := local("status"); code != 0 || !strings.Contains(out, "account is off.") {
		t.Fatalf("status: %d %q", code, out)
	}
	if status := admin.get("/account"); status != http.StatusSeeOther {
		t.Errorf("account page after disable: %d, want a redirect to the sign-in page", status)
	}
	waitFor(t, "the local sign-in to be refused", func() bool {
		return newLocalLoginBrowser(t, base).login("admin", "admin-password-1") == http.StatusForbidden
	})

	// reset-password reminds that the sign-in is off.
	in := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(in, []byte("admin-password-2\n"), 0o600); err != nil {
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
	if code, out, errOut := runCLI(t, "reset-password", "-username", "admin", "-data", dir); code != 0 || !strings.Contains(out, "is turned off. Turn it on with: statuspage local-login enable") {
		t.Fatalf("reset-password: %d %q %q", code, out, errOut)
	}

	// Turned on again: the running server accepts the local sign-in again.
	if code, out := local("enable"); code != 0 || !strings.Contains(out, "is now on") {
		t.Fatalf("enable: %d %q", code, out)
	}
	if code, out := local("enable"); code != 0 || !strings.Contains(out, "already on") {
		t.Fatalf("enable again: %d %q", code, out)
	}
	waitFor(t, "the local sign-in to work again", func() bool {
		return newLocalLoginBrowser(t, base).login("admin", "admin-password-2") == http.StatusSeeOther
	})
}

// waitFor polls cond for up to 15 seconds: longer than the settings cache of
// the running server (10 seconds).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// localLoginBrowser is an HTTP client with cookies that does not follow
// redirects.
type localLoginBrowser struct {
	t      *testing.T
	base   string
	client *http.Client
}

func newLocalLoginBrowser(t *testing.T, base string) *localLoginBrowser {
	jar, _ := cookiejar.New(nil)
	return &localLoginBrowser{t: t, base: base, client: &http.Client{
		Jar: jar, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *localLoginBrowser) get(path string) int {
	b.t.Helper()
	resp, err := b.client.Get(b.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// login submits the sign-in form of the local administrator and returns the
// status code.
func (b *localLoginBrowser) login(username, password string) int {
	b.t.Helper()
	b.get("/login") // sets the CSRF cookie
	u, _ := url.Parse(b.base)
	var token string
	for _, c := range b.client.Jar.Cookies(u) {
		if c.Name == auth.CSRFCookie {
			token = c.Value
		}
	}
	resp, err := b.client.PostForm(b.base+"/login", url.Values{
		"username": {username}, "password": {password}, auth.CSRFField: {token}, "next": {"/"},
	})
	if err != nil {
		b.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
