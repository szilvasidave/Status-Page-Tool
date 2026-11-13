package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kardianos/service"

	"github.com/szilvasidave/status-page-tool/internal/app"
	"github.com/szilvasidave/status-page-tool/internal/config"
)

const (
	serviceName        = "StatusPage"
	serviceDisplayName = "Status Page"
	serviceDescription = "Status page for IT services: incidents, maintenance windows and e-mail notifications."
)

// serviceConfig describes the service: it runs "<this exe> run -data DIR".
func serviceConfig(dataDir string) *service.Config {
	c := &service.Config{
		Name:        serviceName,
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		Arguments:   []string{"run", "-data", dataDir},
		Option: service.KeyValue{
			// Windows: start automatically (delayed, so that the network
			// and a local database are up) and restart after a crash.
			"StartType":              "automatic",
			"DelayedAutoStart":       true,
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "30s",
			"OnFailureResetPeriod":   3600,
		},
	}
	if runtime.GOOS == "linux" {
		// systemd: lines added to the [Unit] section.
		c.Dependencies = []string{"Wants=network-online.target", "After=network-online.target"}
	}
	return c
}

const serviceUsage = `statuspage service install|uninstall|start|stop|restart|status [-data DIR]

Manages the "Status Page" service (Windows service or systemd unit), which
runs "statuspage run -data DIR". Requires administrator / root rights.
On Windows, "install" changes an existing StatusPage service to run this
program with DIR. "start" and "restart" wait up to 15 seconds until the
status page answers and fail otherwise.`

func cmdService(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("service", serviceUsage, stderr)
	dataDir := dataDirFlag(fs)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			fs.Usage()
			return nil
		}
		fmt.Fprintln(stderr, "missing action: install, uninstall, start, stop, restart or status")
		fs.Usage()
		return errUsage
	}
	action := args[0]
	if err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	dir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	conf := serviceConfig(dir)
	s, err := service.New(&program{}, conf)
	if err != nil {
		return fmt.Errorf("services are not supported on this system: %w", err)
	}
	status, statusErr := s.Status()
	installed := !errors.Is(statusErr, service.ErrNotInstalled)

	switch action {
	case "install":
		if installed && statusErr == nil {
			changed, err := updateServiceCommand(conf.Name, conf.Arguments)
			if err != nil {
				return serviceError(s, "update", err)
			}
			if !changed {
				fmt.Fprintln(stdout, "The service is already installed.")
			} else {
				fmt.Fprintf(stdout, "The service was updated: it now runs this program with the data directory %s.\n", dir)
				if status == service.StatusRunning {
					fmt.Fprintf(stdout, "Restart it to apply the change: statuspage service restart -data \"%s\"\n", dir)
				}
			}
		} else {
			if err := s.Install(); err != nil {
				return serviceError(s, "install", err)
			}
			fmt.Fprintf(stdout, "The service %q was installed (data directory %s).\nStart it with: statuspage service start -data \"%s\"\n", serviceDisplayName, dir, dir)
		}
		// The service creates the folder too, but restrict it right away.
		return config.EnsureDataDir(dir)

	case "uninstall":
		if !installed {
			fmt.Fprintln(stdout, "The service is not installed.")
			return nil
		}
		if status == service.StatusRunning {
			if err := s.Stop(); err != nil {
				return serviceError(s, "stop", err)
			}
		}
		if err := s.Uninstall(); err != nil {
			return serviceError(s, "uninstall", err)
		}
		fmt.Fprintf(stdout, "The service was removed. The data directory %s was kept.\n", dir)

	case "start":
		if err := requireInstalled(installed); err != nil {
			return err
		}
		if status == service.StatusRunning {
			fmt.Fprintln(stdout, "The service is already running.")
			return nil
		}
		if err := s.Start(); err != nil {
			return serviceError(s, "start", err)
		}
		if err := waitUntilServing(s.Status, dir, startTimeout); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "The service was started.")

	case "stop":
		if err := requireInstalled(installed); err != nil {
			return err
		}
		if status == service.StatusStopped {
			fmt.Fprintln(stdout, "The service is not running.")
			return nil
		}
		if err := s.Stop(); err != nil {
			return serviceError(s, "stop", err)
		}
		fmt.Fprintln(stdout, "The service was stopped.")

	case "restart":
		if err := requireInstalled(installed); err != nil {
			return err
		}
		if status == service.StatusRunning {
			err = s.Restart()
		} else {
			err = s.Start()
		}
		if err != nil {
			return serviceError(s, "restart", err)
		}
		if err := waitUntilServing(s.Status, dir, startTimeout); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "The service was restarted.")

	case "status":
		switch {
		case !installed:
			fmt.Fprintln(stdout, "not installed")
		case statusErr != nil:
			return serviceError(s, "query", statusErr)
		case status == service.StatusRunning:
			fmt.Fprintln(stdout, "running")
		case status == service.StatusStopped:
			fmt.Fprintln(stdout, "stopped")
		default:
			fmt.Fprintln(stdout, "unknown")
		}

	default:
		fmt.Fprintf(stderr, "unknown action %q\n", action)
		fs.Usage()
		return errUsage
	}
	return nil
}

// startTimeout is how long "service start" and "service restart" wait for
// the status page to answer.
const startTimeout = 15 * time.Second

// waitUntilServing waits until the status page of data directory dir answers
// on /healthz, so that a server that stops right after starting (port in
// use, database unreachable, failed migration, …) is reported as an error
// together with the log file. Only a 200 response with the header
// X-Status-Page (set by the status page's /healthz) counts, and redirects
// are not followed, so that another program on the port does not count.
// status reports the service state; a stopped service fails at once, even
// if something answers.
func waitUntilServing(status func() (service.Status, error), dir string, timeout time.Duration) error {
	base, err := app.LocalURL(dir)
	if err != nil {
		return fmt.Errorf("the service was started, but its configuration cannot be read: %w", err)
	}
	logFile := filepath.Join(dir, logFileName)
	client := &http.Client{
		Timeout: 2 * time.Second,
		// Only checks that the server on this computer answers; its
		// certificate is usually issued for another name.
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	deadline := time.Now().Add(timeout)
	for {
		serving := false
		if resp, err := client.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			serving = resp.StatusCode == http.StatusOK && resp.Header.Get("X-Status-Page") == "ok"
		}
		if st, err := status(); err == nil && st == service.StatusStopped {
			return fmt.Errorf("the service stopped right after starting; see the log file %s", logFile)
		}
		if serving {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service is running, but the status page does not answer at %s; see the log file %s (if the service uses another data directory, pass it with -data)", base, logFile)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func requireInstalled(installed bool) error {
	if !installed {
		return errors.New("the service is not installed (run: statuspage service install)")
	}
	return nil
}

// serviceError adds the service manager and a hint when the command lacks
// administrator rights.
func serviceError(s service.Service, action string, err error) error {
	msg := strings.ToLower(err.Error())
	if errors.Is(err, fs.ErrPermission) || strings.Contains(msg, "access is denied") || strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "interactive authentication required") {
		hint := "run this command as root, e.g. with sudo"
		if runtime.GOOS == "windows" {
			hint = `run this command in a command prompt opened with "Run as administrator"`
		}
		return fmt.Errorf("could not %s the service: %v (%s)", action, err, hint)
	}
	return fmt.Errorf("could not %s the service (%s): %w", action, s.Platform(), err)
}
