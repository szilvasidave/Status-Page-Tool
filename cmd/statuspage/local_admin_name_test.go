package main

import (
	"context"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/auth"
)

// The user name of the local administrator account can be changed on the
// account page. When single sign-on is down nobody can look it up there, so
// the command line names it: "reset-password" with an unknown name and
// "local-login status".
func TestLocalAdminNameOnCommandLine(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := runCLI(t, "setup", "-data", dir, "-db", "sqlite"); code != 0 {
		t.Fatalf("setup: %s", errOut)
	}
	ctx := context.Background()
	st, closeDB, err := openStore(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	// No local administrator account yet: nothing to name.
	code, _, errOut := runCLI(t, "reset-password", "-username", "admin", "-data", dir)
	if code != 1 || !strings.HasSuffix(errOut, "there is no local account named \"admin\"\n") {
		t.Errorf("reset-password without a local administrator: %d %q", code, errOut)
	}

	hash, _ := auth.HashPassword("admin-password-1")
	admin, err := st.CreateLocalAdmin(ctx, "admin", "Ada Admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateLocalAdmin(ctx, admin.ID, "ops-breakglass", "Ada Admin"); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runCLI(t, "reset-password", "-username", "admin", "-data", dir)
	if code != 1 || !strings.Contains(errOut, `there is no local account named "admin"; local administrator account: ops-breakglass`) {
		t.Errorf("reset-password with the old name: %d %q", code, errOut)
	}
	code, out, errOut := runCLI(t, "local-login", "status", "-data", dir)
	if code != 0 || !strings.Contains(out, "is on.\nLocal administrator account: ops-breakglass\n") {
		t.Errorf("local-login status: %d %q %q", code, out, errOut)
	}
}
