package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/kardianos/service"

	"github.com/szilvasidave/status-page-tool/internal/app"
)

// cmdRun runs the server, either interactively (console, Ctrl+C to stop) or,
// when started by the Windows Service Control Manager / systemd, as a
// service.
func cmdRun(args []string) error {
	fs := newFlagSet("run", "statuspage [run] [-data DIR] [-listen ADDR]", os.Stderr)
	dataDir := dataDirFlag(fs)
	listen := fs.String("listen", "", "listen address, overrides config.json (e.g. :8080)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	dir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("could not create the data directory %s: %w", dir, err)
	}

	interactive := service.Interactive()
	// A Windows service has no usable stdout; under systemd stdout goes to
	// the journal.
	log, closeLog := newLogger(dir, interactive || runtime.GOOS != "windows")
	defer closeLog()

	opts := app.Options{
		DataDir: dir,
		Listen:  *listen,
		Version: version,
		Log:     log,
		// Double-clicking the exe opens the setup wizard in the browser.
		OpenBrowser: interactive && (runtime.GOOS == "windows" || runtime.GOOS == "darwin"),
	}

	if interactive {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := app.Run(ctx, opts); err != nil {
			log.Error("status page stopped with an error", "err", err)
			return err
		}
		return nil
	}

	s, err := service.New(&program{opts: opts}, serviceConfig(dir))
	if err != nil {
		return err
	}
	if err := s.Run(); err != nil {
		log.Error("service stopped with an error", "err", err)
		return err
	}
	return nil
}

// program adapts app.Run to the kardianos/service interface.
type program struct {
	opts   app.Options
	cancel context.CancelFunc
	done   chan struct{}
}

// serviceStopTimeout is how long Stop waits for a graceful shutdown; Windows
// kills services that take longer than about 20 seconds.
const serviceStopTimeout = 18 * time.Second

func (p *program) Start(service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		err := app.Run(ctx, p.opts)
		if ctx.Err() == nil {
			// The server stopped on its own (e.g. the port is in use or the
			// database is unreachable). Exit with an error so that the
			// service manager restarts it after a delay.
			p.opts.Log.Error("status page stopped unexpectedly, exiting", "err", err)
			os.Exit(1)
		}
		if err != nil {
			p.opts.Log.Error("error during shutdown", "err", err)
		}
	}()
	return nil
}

func (p *program) Stop(service.Service) error {
	if p.cancel == nil {
		return nil
	}
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(serviceStopTimeout):
		p.opts.Log.Warn("shutdown is taking too long, exiting")
	}
	return nil
}
