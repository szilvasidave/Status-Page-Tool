// Package app wires everything together and runs the HTTP server. When no
// configuration exists yet it starts in setup mode, serving only the setup
// wizard; once the database is configured it switches to normal operation
// without a restart.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/directory"
	"github.com/szilvasidave/status-page-tool/internal/notify"
	"github.com/szilvasidave/status-page-tool/internal/secret"
	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/web"
)

// Options configure Run.
type Options struct {
	DataDir string
	// Listen overrides the listen address from the config file.
	Listen  string
	Version string
	Log     *slog.Logger
	// OpenBrowser opens the setup wizard in the default browser when setup
	// is required (for interactive runs, e.g. double-clicking the exe).
	OpenBrowser bool
}

type app struct {
	opts    Options
	log     *slog.Logger
	handler atomic.Pointer[http.Handler]

	mu       sync.Mutex
	active   bool
	db       *db.DB
	workers  context.Context
	stopWork context.CancelFunc
	wg       sync.WaitGroup
}

// Run starts the server and blocks until ctx is cancelled.
func Run(ctx context.Context, opts Options) error {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.DataDir == "" {
		opts.DataDir = config.DefaultDataDir()
	}
	if err := config.EnsureDataDir(opts.DataDir); err != nil {
		return err
	}
	a := &app{opts: opts, log: opts.Log}
	a.workers, a.stopWork = context.WithCancel(context.Background())
	defer func() {
		a.stopWork()
		a.wg.Wait()
		if a.db != nil {
			a.db.Close()
		}
	}()

	cfg, err := config.Load(opts.DataDir)
	needSetup := errors.Is(err, config.ErrNotConfigured)
	if err != nil && !needSetup {
		return err
	}
	listen, useTLS := address(opts.DataDir, cfg)
	if opts.Listen != "" {
		listen = opts.Listen
	}
	// Load the certificate first, so that a wrong path is reported clearly
	// instead of after "listening".
	var tlsCert tls.Certificate
	if useTLS {
		certFile, keyFile := cfg.TLSFiles(opts.DataDir)
		if tlsCert, err = tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			return fmt.Errorf("could not load the TLS certificate (tls_cert and tls_key in %s): %w", config.Path(opts.DataDir), err)
		}
	}

	if needSetup {
		token := a.setupToken()
		h, err := web.NewSetupHandler(web.Deps{DataDir: opts.DataDir, Version: opts.Version, Log: a.log, Activate: a.activate, SetupToken: token})
		if err != nil {
			return err
		}
		a.handler.Store(&h)
	} else if err := a.activate(cfg); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*a.handler.Load()).ServeHTTP(w, r) }),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}
	if useTLS {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{tlsCert}}
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listen, err)
	}
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	local := localURL(scheme, ln.Addr())
	a.log.Info("status page listening", "address", listen, "url", local, "data_dir", opts.DataDir, "version", opts.Version)
	if needSetup {
		a.log.Info("setup required: open the setup wizard in your browser", "url", local+"/setup")
		if opts.OpenBrowser {
			openBrowser(local + "/setup")
		}
	}

	errCh := make(chan error, 1)
	go func() {
		if useTLS {
			errCh <- srv.ServeTLS(ln, "", "")
		} else {
			errCh <- srv.Serve(ln)
		}
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	a.log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// address returns the listen address and whether TLS is used for a server
// started with dataDir and the configuration cfg (nil in setup mode, where
// the installer's SetupListenFile, if present, gives the address).
func address(dataDir string, cfg *config.Config) (listen string, useTLS bool) {
	if cfg != nil {
		return cfg.Listen, cfg.TLSCert != "" && cfg.TLSKey != ""
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, config.SetupListenFile)); err == nil {
		l := strings.TrimSpace(string(config.TrimBOM(b)))
		if _, _, err := net.SplitHostPort(l); err == nil {
			return l, false
		}
	}
	return config.DefaultListen, false
}

// LocalURL returns the URL under which "statuspage run -data dataDir" (as
// the service runs it) can be reached on this computer, e.g.
// "http://localhost:8080".
func LocalURL(dataDir string) (string, error) {
	cfg, err := config.Load(dataDir)
	if err != nil && !errors.Is(err, config.ErrNotConfigured) {
		return "", err
	}
	listen, useTLS := address(dataDir, cfg)
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q in %s", listen, config.Path(dataDir))
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "localhost"
	}
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

// activate opens the database, applies migrations and switches the server to
// normal mode. Called on start (configured) or by the setup wizard.
func (a *app) activate(cfg *config.Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active {
		return errors.New("already configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	key, err := cfg.Key()
	if err != nil {
		return err
	}
	d, err := db.OpenConfig(ctx, cfg, a.opts.DataDir)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	if err := d.Migrate(ctx); err != nil {
		d.Close()
		return fmt.Errorf("migrate database: %w", err)
	}
	st := store.New(d, key)
	am := auth.New(st, key, a.log)
	n := notify.New(st, a.log)
	dir := directory.NewSyncer(st, a.log)

	token := ""
	hasAdmin, err := st.HasLocalAdmin(ctx)
	if err != nil {
		d.Close()
		return err
	}
	if !hasAdmin {
		token = a.setupToken()
	}
	h, err := web.NewHandler(web.Deps{
		Config: cfg, DataDir: a.opts.DataDir, Store: st, Auth: am, Notifier: n, Directory: dir,
		Version: a.opts.Version, Log: a.log, SetupToken: token,
	})
	if err != nil {
		d.Close()
		return err
	}
	a.db = d
	a.active = true
	a.handler.Store(&h)

	a.goWorker(n.Run)
	a.goWorker(dir.Run)
	a.goWorker(func(ctx context.Context) { cleanupSessions(ctx, st, a.log) })
	a.log.Info("database ready", "driver", cfg.DBDriver)
	return nil
}

func (a *app) goWorker(fn func(context.Context)) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		fn(a.workers)
	}()
}

func cleanupSessions(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := st.DeleteExpiredSessions(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Warn("delete expired sessions", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// setupToken returns the setup token, creating the token file if needed.
func (a *app) setupToken() string {
	p := filepath.Join(a.opts.DataDir, config.SetupTokenFile)
	if b, err := os.ReadFile(p); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 16 {
			a.log.Info("setup token (needed when opening the setup wizard from another computer)", "file", p)
			return t
		}
	}
	t := secret.RandomToken(18)
	if err := os.WriteFile(p, []byte(t+"\n"), 0o600); err != nil {
		a.log.Warn("could not write setup token file", "err", err)
	}
	a.log.Info("setup token (needed when opening the setup wizard from another computer)", "token", t, "file", p)
	return t
}

func localURL(scheme string, addr net.Addr) string {
	port := ""
	if ta, ok := addr.(*net.TCPAddr); ok {
		port = fmt.Sprint(ta.Port)
	}
	return scheme + "://localhost:" + port
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
