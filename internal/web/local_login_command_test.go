package web

import (
	"context"
	"html/template"
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestLocalLoginCommand: the command that turns the local sign-in on again
// names the data directory of the server and, on Windows (whose installer
// does not put the program folder on the PATH), the program itself.
func TestLocalLoginCommand(t *testing.T) {
	for _, c := range []struct{ goos, exe, dir, want string }{
		{"linux", "/usr/local/bin/statuspage", "/var/lib/statuspage",
			`statuspage local-login enable -data "/var/lib/statuspage"`},
		{"darwin", "/usr/local/bin/statuspage", "/Users/it/status data",
			`statuspage local-login enable -data "/Users/it/status data"`},
		{"windows", `C:\Program Files\Status Page\statuspage.exe`, `C:\ProgramData\StatusPage`,
			`"C:\Program Files\Status Page\statuspage.exe" local-login enable -data "C:\ProgramData\StatusPage"`},
		{"windows", "", `C:\ProgramData\StatusPage`, `statuspage local-login enable -data "C:\ProgramData\StatusPage"`},
		{"linux", "/opt/statuspage", "", "statuspage local-login enable"},
	} {
		if got := localLoginCommand(c.goos, c.exe, c.dir); got != c.want {
			t.Errorf("localLoginCommand(%q, %q, %q) = %s, want %s", c.goos, c.exe, c.dir, got, c.want)
		}
	}
}

// TestLocalLoginCommandOnPages: Admin → Settings shows the exact command for
// this server (with its data directory), the public sign-in page only the
// general form, without the server's paths.
func TestLocalLoginCommandOnPages(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	exe, _ := os.Executable()
	want := localLoginCommand(runtime.GOOS, exe, e.DataDir)
	if !strings.Contains(want, `-data "`+e.DataDir+`"`) {
		t.Fatalf("command without the data directory: %s", want)
	}
	mustContain(t, e.get("/admin/settings").Body, "run this command on the server as administrator or",
		`<code class="code-box">`+template.HTMLEscapeString(want)+`</code>`)

	// Local sign-in off, single sign-on not set up: nobody can sign in.
	st := adminSettings(t, e)
	st.LocalLoginEnabled = false
	if err := e.Store.SaveSettings(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	e.logout()
	general := "statuspage local-login enable -data &lt;data directory&gt;"
	for _, path := range []string{"/login", "/login?local=1"} {
		r := e.get(path)
		mustContain(t, r.Body, general)
		if strings.Contains(r.Body, e.DataDir) || strings.Contains(r.Body, template.HTMLEscapeString(e.DataDir)) {
			t.Errorf("%s shows the data directory of the server", path)
		}
	}

	// With single sign-on, ?local=1 explains how to turn it on again.
	st.OIDCEnabled, st.OIDCIssuer, st.OIDCClientID, st.OIDCClientSecret = true, "https://idp.example.com", "status", "secret"
	if err := e.Store.SaveSettings(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	mustContain(t, e.get("/login?local=1").Body, "Sign-in with the local administrator account is turned off",
		"with the command “"+general+"”")
}
