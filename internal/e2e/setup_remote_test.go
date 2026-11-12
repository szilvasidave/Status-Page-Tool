package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/config"
)

// TestSetupFromAnotherComputer runs the setup wizard through a reverse proxy
// (X-Forwarded-For), which requires the setup token from the data
// directory. The token entered in the database step must still be accepted
// by the administrator step, which is served after the switch to normal
// mode.
func TestSetupFromAnotherComputer(t *testing.T) {
	logs := &logBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("application log:\n%s", logs)
		}
	})
	dataDir := t.TempDir()
	app := startApp(t, dataDir, freeAddr(t), logs)
	b := newBrowser(t, "remote")
	b.header = http.Header{"X-Forwarded-For": {"203.0.113.7"}}

	raw, err := os.ReadFile(filepath.Join(dataDir, config.SetupTokenFile))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))

	// Database step: without the token the request is refused.
	p := b.get(app.URL + "/setup").expectStatus(200)
	f := p.form("/setup").choose("driver", "sqlite").press("action", "save")
	f.value("setup_token") // the field is shown
	f.post().expectStatus(http.StatusForbidden).expectText("setup token")
	if _, err := os.Stat(filepath.Join(dataDir, config.FileName)); !os.IsNotExist(err) {
		t.Fatalf("config.json written without the setup token: %v", err)
	}
	p = b.get(app.URL + "/setup").expectStatus(200)
	p = p.form("/setup").choose("driver", "sqlite").set("setup_token", token).press("action", "save").post()
	p.expectRedirect("/setup")

	// Administrator step: the token is remembered.
	p = p.follow().expectStatus(200).expectText("Create the administrator")
	f = p.form("/setup")
	for _, fl := range f.fields {
		if fl.name == "setup_token" {
			t.Fatal("the administrator step asks for the setup token again")
		}
	}
	p = f.set("username", "admin").set("password", adminPW).set("password_confirm", adminPW).set("site_name", "Remote").post()
	p.expectRedirect("/admin/settings")
	p.follow().expectStatus(200).expectFlash("success", "Setup complete")
	if _, err := os.Stat(filepath.Join(dataDir, config.SetupTokenFile)); !os.IsNotExist(err) {
		t.Errorf("the setup token file still exists: %v", err)
	}

	// Another remote browser cannot use the setup wizard any more.
	other := newBrowser(t, "other")
	other.header = b.header
	other.get(app.URL + "/setup").expectRedirect("/login")
}
