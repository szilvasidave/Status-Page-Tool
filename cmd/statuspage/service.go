package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kardianos/service"
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
runs "statuspage run -data DIR". Requires administrator / root rights.`

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
	s, err := service.New(&program{}, serviceConfig(dir))
	if err != nil {
		return fmt.Errorf("services are not supported on this system: %w", err)
	}
	status, statusErr := s.Status()
	installed := !errors.Is(statusErr, service.ErrNotInstalled)

	switch action {
	case "install":
		if installed && statusErr == nil {
			fmt.Fprintln(stdout, "The service is already installed.")
			return nil
		}
		if err := s.Install(); err != nil {
			return serviceError(s, "install", err)
		}
		fmt.Fprintf(stdout, "The service %q was installed (data directory %s).\nStart it with: statuspage service start\n", serviceDisplayName, dir)

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
