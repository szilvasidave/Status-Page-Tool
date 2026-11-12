// Package web contains the HTTP handlers and HTML templates.
//
// Conventions for handler files:
//   - every page template lives in templates/<name>.html and defines
//     {{define "content"}} (and optionally {{define "head"}}); it is rendered
//     inside templates/layout.html via s.render(w, r, "<name>", title, data)
//   - shared snippets live in templates/partials/*.html and are available to
//     every page
//   - the page data passed to templates is a *Page; page specific data is in
//     .Data
//   - no inline <script> or on*= handlers (CSP); use data attributes handled
//     by static/app.js (e.g. data-confirm="Really delete?")
//   - every POST form must include {{template "csrf" $}} (hidden csrf_token
//     input; $ is the *Page)
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/directory"
	"github.com/szilvasidave/status-page-tool/internal/notify"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

//go:embed templates static
var assets embed.FS

// Deps are the dependencies of the web handlers.
type Deps struct {
	Config    *config.Config
	DataDir   string
	Store     *store.Store     // nil in setup mode
	Auth      *auth.Manager    // nil in setup mode
	Notifier  *notify.Notifier // nil in setup mode
	Directory *directory.Syncer
	Version   string
	Log       *slog.Logger

	// Activate is called by the setup wizard (setup mode only) after the
	// database connection was configured and the config file was written. It
	// opens the database and switches the running server to normal mode.
	Activate func(cfg *config.Config) error

	// SetupToken must be entered in the setup wizard when it is opened from
	// another computer (not from localhost). Empty disables the check.
	SetupToken string
}

// Server serves the web UI.
type Server struct {
	Deps
	setupMode bool
	mux       *http.ServeMux
	pages     map[string]*template.Template
	loc       atomic.Pointer[time.Location] // display time zone used by template funcs
	setupDone atomic.Bool                   // cached "a local admin exists"
}

// NewHandler returns the handler for normal operation.
func NewHandler(d Deps) (http.Handler, error) {
	return newServer(d, false)
}

// NewSetupHandler returns the handler used while no database is configured:
// it only serves the database step of the setup wizard and static files.
func NewSetupHandler(d Deps) (http.Handler, error) {
	return newServer(d, true)
}

func newServer(d Deps, setupMode bool) (http.Handler, error) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &Server{Deps: d, setupMode: setupMode, mux: http.NewServeMux()}
	s.loc.Store(time.UTC)
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}
	static, _ := fs.Sub(assets, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	if setupMode {
		s.setupRoutes()
		return s.wrapSetup(s.mux), nil
	}
	s.routes()
	return s.wrap(s.mux), nil
}

// wrap applies the middleware chain for normal mode.
func (s *Server) wrap(h http.Handler) http.Handler {
	h = s.requireSetupDone(h)
	h = s.Auth.Middleware(h)
	h = securityHeaders(h)
	h = s.recoverer(h)
	h = s.logRequests(h)
	return h
}

// wrapSetup applies the middleware chain for setup mode. CSRF protection is
// provided by a small double-submit cookie implementation in setup.go.
func (s *Server) wrapSetup(h http.Handler) http.Handler {
	h = securityHeaders(h)
	h = s.recoverer(h)
	h = s.logRequests(h)
	return h
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if s.Store != nil {
		if err := s.Store.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	fmt.Fprintln(w, "ok")
}

// requireSetupDone redirects everything to /setup until a local administrator
// account exists.
func (s *Server) requireSetupDone(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.setupDone.Load() || r.URL.Path == "/setup" || strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		ok, err := s.Store.HasLocalAdmin(r.Context())
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if !ok {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		s.setupDone.Store(true)
		next.ServeHTTP(w, r)
	})
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				s.Log.Error("panic in handler", "path", r.URL.Path, "panic", p)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			return
		}
		lvl := slog.LevelDebug
		if rec.status >= 500 {
			lvl = slog.LevelError
		} else if r.Method != http.MethodGet {
			lvl = slog.LevelInfo
		}
		s.Log.Log(r.Context(), lvl, "http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start).Round(time.Millisecond), "ip", auth.ClientIP(r))
	})
}

// ------------------------------------------------------------------ templates

func (s *Server) loadTemplates() error {
	s.pages = map[string]*template.Template{}
	partials, err := fs.Glob(assets, "templates/partials/*.html")
	if err != nil {
		return err
	}
	files, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return err
	}
	for _, f := range files {
		name := strings.TrimSuffix(path.Base(f), ".html")
		if name == "layout" {
			continue
		}
		patterns := append([]string{"templates/layout.html"}, partials...)
		patterns = append(patterns, f)
		t, err := template.New("layout.html").Funcs(s.funcs()).ParseFS(assets, patterns...)
		if err != nil {
			return fmt.Errorf("parse template %s: %w", f, err)
		}
		s.pages[name] = t
	}
	return nil
}

// Page is the data passed to every template.
type Page struct {
	Title     string
	Nav       string // active navigation item: status, history, manage, admin, account
	Settings  *store.Settings
	Identity  *auth.Identity
	CSRF      string
	Flash     *Flash
	Version   string
	Now       time.Time
	SetupMode bool
	Data      any
}

// User returns the signed-in user or nil.
func (p *Page) User() *store.User {
	if p.Identity == nil {
		return nil
	}
	return p.Identity.User
}

// IsEditor reports whether the user may post incidents/maintenance.
func (p *Page) IsEditor() bool { return p.Identity.Has(store.RoleEditor) }

// IsAdmin reports whether the user is an administrator.
func (p *Page) IsAdmin() bool { return p.Identity.Has(store.RoleAdmin) }

// SiteName returns the configured site name.
func (p *Page) SiteName() string {
	if p.Settings == nil || p.Settings.SiteName == "" {
		return "Status"
	}
	return p.Settings.SiteName
}

// render renders page template name with status 200.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	s.renderStatus(w, r, http.StatusOK, name, title, data)
}

// renderStatus renders a page template with the given HTTP status.
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	t, ok := s.pages[name]
	if !ok {
		s.Log.Error("unknown template", "name", name)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	p := s.newPage(w, r, title, data)
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		s.Log.Error("render template", "name", name, "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(buf.String()))
}

func (s *Server) newPage(w http.ResponseWriter, r *http.Request, title string, data any) *Page {
	p := &Page{
		Title:     title,
		Identity:  auth.FromContext(r.Context()),
		CSRF:      auth.CSRFToken(r),
		Flash:     s.popFlash(w, r),
		Version:   s.Version,
		Now:       time.Now(),
		SetupMode: s.setupMode,
		Data:      data,
	}
	p.Settings = s.settings(r)
	p.Nav = navFor(r.URL.Path)
	return p
}

// settings returns the current settings (defaults in setup mode or on error).
// It also updates the display time zone used by the template functions.
func (s *Server) settings(r *http.Request) *store.Settings {
	if s.Store == nil {
		d := store.DefaultSettings()
		return &d
	}
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.Log.Error("load settings", "err", err)
		d := store.DefaultSettings()
		return &d
	}
	s.loc.Store(st.Location())
	return st
}

func navFor(p string) string {
	switch {
	case p == "/":
		return "status"
	case strings.HasPrefix(p, "/history"), strings.HasPrefix(p, "/incidents/"), strings.HasPrefix(p, "/maintenance/"):
		return "history"
	case strings.HasPrefix(p, "/manage"):
		return "manage"
	case strings.HasPrefix(p, "/admin"):
		return "admin"
	case strings.HasPrefix(p, "/account"):
		return "account"
	}
	return ""
}

// ------------------------------------------------------------------ errors

type errorData struct {
	Status  int
	Heading string
	Message string
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, heading, msg string) {
	s.renderStatus(w, r, status, "error", heading, errorData{Status: status, Heading: heading, Message: msg})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r, http.StatusNotFound, "Not found", "The page you are looking for does not exist.")
}

func (s *Server) forbidden(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r, http.StatusForbidden, "Access denied", "You do not have permission to view this page.")
}

func (s *Server) badRequest(w http.ResponseWriter, r *http.Request, msg string) {
	s.errorPage(w, r, http.StatusBadRequest, "Bad request", msg)
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	s.errorPage(w, r, http.StatusInternalServerError, "Something went wrong", "An unexpected error occurred. Please try again; if the problem persists check the server log.")
}

// ------------------------------------------------------------------ access control

// require wraps h so that it is only reachable for users with at least role.
// Anonymous users are redirected to the login page.
func (s *Server) require(role store.Role, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := auth.FromContext(r.Context())
		if id == nil {
			s.redirectToLogin(w, r)
			return
		}
		if !id.Has(role) {
			s.forbidden(w, r)
			return
		}
		h(w, r)
	})
}

// viewer wraps public pages: reachable by everyone when public access is
// enabled, otherwise only by signed-in users.
func (s *Server) viewer(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.settings(r).PublicAccess && auth.FromContext(r.Context()) == nil {
			s.redirectToLogin(w, r)
			return
		}
		h(w, r)
	})
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.RequestURI()
	if r.Method != http.MethodGet {
		next = "/"
	}
	http.Redirect(w, r, "/login?next="+urlQueryEscape(next), http.StatusSeeOther)
}

func (s *Server) notImplemented(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r, http.StatusNotImplemented, "Not implemented", "This page is not implemented yet.")
}
