package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

const localLoginUsage = `statuspage local-login enable|disable|status [-data DIR]

Turns the sign-in with the local administrator account on or off, or shows
whether it is on and the user name of that account. "enable" always works:
use it when single sign-on is unavailable and nobody can sign in. "disable"
works only while single sign-on is set up and at least one group has the
Administrator role, so that somebody can still sign in as an administrator.
Changing it signs out all sessions of the local administrator account. A
running status page applies the change within about 10 seconds; no restart
is needed.`

func cmdLocalLogin(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("local-login", localLoginUsage, stderr)
	dataDir := dataDirFlag(fs)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			fs.Usage()
			return nil
		}
		fmt.Fprintln(stderr, "missing action: enable, disable or status")
		fs.Usage()
		return errUsage
	}
	action := args[0]
	if err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	if action != "enable" && action != "disable" && action != "status" {
		fmt.Fprintf(stderr, "unknown action %q: use enable, disable or status\n", action)
		fs.Usage()
		return errUsage
	}
	dir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st, closeDB, err := openStore(ctx, dir)
	if err != nil {
		return err
	}
	defer closeDB()
	settings, err := st.GetSettings(ctx)
	if err != nil {
		return err
	}
	enableHint := fmt.Sprintf("Turn it on again with: statuspage local-login enable -data \"%s\"", dir)

	switch action {
	case "status":
		names, err := st.LocalAdminUsernames(ctx)
		if err != nil {
			return err
		}
		if settings.LocalLoginEnabled {
			fmt.Fprintln(stdout, "Sign-in with the local administrator account is on.")
		} else {
			fmt.Fprintln(stdout, "Sign-in with the local administrator account is off.")
			fmt.Fprintln(stdout, enableHint)
		}
		if len(names) > 0 {
			fmt.Fprintf(stdout, "Local administrator account: %s\n", strings.Join(names, ", "))
		}
		return nil

	case "enable":
		changed, err := st.SetLocalLoginEnabled(ctx, true)
		if err != nil {
			return err
		}
		if !changed {
			fmt.Fprintln(stdout, "Sign-in with the local administrator account is already on.")
			return nil
		}
		fmt.Fprintln(stdout, "Sign-in with the local administrator account is now on. A running status page applies this within about 10 seconds.")
		return nil

	default: // disable
		switch err := st.CheckLocalLoginOff(ctx, settings); {
		// (ASCII only: the Windows console may not show other characters.)
		case errors.Is(err, store.ErrSSONotConfigured):
			return errors.New("sign-in with the local administrator account cannot be turned off while single sign-on is not set up: " +
				"enable and configure it in Admin > Settings first (issuer URL, client ID and client secret)")
		case errors.Is(err, store.ErrNoAdminGroup):
			return errors.New("sign-in with the local administrator account cannot be turned off while no group has the Administrator role: " +
				"give a group the Administrator role in Admin > Groups first, so that somebody can still sign in as an administrator")
		case err != nil:
			return err
		}
		changed, err := st.SetLocalLoginEnabled(ctx, false)
		if err != nil {
			return err
		}
		if !changed {
			fmt.Fprintln(stdout, "Sign-in with the local administrator account is already off.")
			return nil
		}
		fmt.Fprintln(stdout, "Sign-in with the local administrator account is now off and its sessions were signed out. "+
			"A running status page applies this within about 10 seconds.")
		fmt.Fprintln(stdout, enableHint)
		return nil
	}
}

// openStore opens the database of the data directory dir for a command line
// tool; closeDB closes it.
func openStore(ctx context.Context, dir string) (st *store.Store, closeDB func(), err error) {
	cfg, err := config.Load(dir)
	if errors.Is(err, config.ErrNotConfigured) {
		// Usually the data directory was not given: without -data it is the
		// default one, e.g. next to the program, not that of the service.
		example := " (e.g. -data /var/lib/statuspage)"
		if runtime.GOOS == "windows" {
			example = ""
		}
		return nil, nil, fmt.Errorf("%s not found: pass the data directory of the status page with -data DIR, the one the service uses%s; "+
			"if the setup is not finished yet, finish it in the browser first", config.Path(dir), example)
	}
	if err != nil {
		return nil, nil, err
	}
	key, err := cfg.Key()
	if err != nil {
		return nil, nil, err
	}
	d, err := db.OpenConfig(ctx, cfg, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("could not open the database: %w", err)
	}
	return store.New(d, key), func() { d.Close() }, nil
}
