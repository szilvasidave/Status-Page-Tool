package web

// First-run setup wizard:
//
//  1. Database (setup mode, no config.json yet): handleSetupDatabase writes
//     config.json with package setup and switches the running server to
//     normal mode via Deps.Activate.
//  2. Administrator (normal mode, no local admin yet): handleSetup creates
//     the local administrator and the basic settings and signs the admin in.
//
// Requests from another computer (or through a reverse proxy) must supply
// the setup token from <data dir>/setup-token.txt.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/secret"
	"github.com/szilvasidave/status-page-tool/internal/setup"
)

const (
	// setupCSRFCookie holds the double-submit CSRF token in setup mode,
	// where the auth middleware (and its CSRF cookie) is not active.
	setupCSRFCookie = "sp_setup_csrf"
	// setupTokenCookie remembers a correctly entered setup token, so that it
	// is asked only once during the wizard.
	setupTokenCookie = "sp_setup_token"
	// setupTokenField is the form field for the setup token.
	setupTokenField = "setup_token"
)

// setupMu serialises the state-changing setup steps (a second request must
// not create a second administrator or configure the database twice).
var setupMu sync.Mutex

// ------------------------------------------------------------------ setup token

// setupNeedsToken reports whether the request must prove knowledge of the
// setup token: it comes from another computer or through a reverse proxy.
func (s *Server) setupNeedsToken(r *http.Request) bool {
	if s.SetupToken == "" {
		return false
	}
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	ip := net.ParseIP(auth.ClientIP(r))
	return ip == nil || !ip.IsLoopback()
}

func (s *Server) setupTokenMatches(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && subtle.ConstantTimeCompare([]byte(v), []byte(s.SetupToken)) == 1
}

// setupTokenRemembered reports whether the browser already entered the
// correct token in an earlier step.
func (s *Server) setupTokenRemembered(r *http.Request) bool {
	c, err := r.Cookie(setupTokenCookie)
	return err == nil && s.setupTokenMatches(c.Value)
}

// setupAskToken reports whether the form must show the setup token field.
func (s *Server) setupAskToken(r *http.Request) bool {
	return s.setupNeedsToken(r) && !s.setupTokenRemembered(r)
}

// setupCheckToken verifies the setup token of a POST request (form field or
// remembered cookie). A correct token from the form is remembered.
func (s *Server) setupCheckToken(w http.ResponseWriter, r *http.Request) bool {
	if !s.setupNeedsToken(r) || s.setupTokenRemembered(r) {
		return true
	}
	if !s.setupTokenMatches(r.PostFormValue(setupTokenField)) {
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name: setupTokenCookie, Value: s.SetupToken, Path: "/setup", MaxAge: 3600,
		HttpOnly: true, Secure: auth.IsHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
	return true
}

// setupTokenFile is the path of the setup token file shown to the user.
func (s *Server) setupTokenFile() string { return filepath.Join(s.DataDir, config.SetupTokenFile) }

// ------------------------------------------------------------------ CSRF (setup mode)

// setupModeCSRF protects the forms served in setup mode with a double-submit
// cookie and an Origin check (normal mode uses the auth middleware). It
// returns the request with the token in its context (for {{template "csrf"}})
// and false if a POST must be rejected.
func (s *Server) setupModeCSRF(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	token := ""
	if c, err := r.Cookie(setupCSRFCookie); err == nil && len(c.Value) >= 20 {
		token = c.Value
	}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, auth.MaxFormBytes)
		sent := r.PostFormValue(auth.CSRFField)
		if token == "" || sent == "" || subtle.ConstantTimeCompare([]byte(sent), []byte(token)) != 1 || !auth.CheckOrigin(r, "") {
			return r, false
		}
	} else if token == "" {
		token = secret.RandomToken(24)
		http.SetCookie(w, &http.Cookie{
			Name: setupCSRFCookie, Value: token, Path: "/",
			HttpOnly: true, Secure: auth.IsHTTPS(r), SameSite: http.SameSiteLaxMode,
		})
	}
	return r.WithContext(auth.WithCSRFToken(r.Context(), token)), true
}

// ------------------------------------------------------------------ step 1: database

// setupDBData is the data of the database step.
type setupDBData struct {
	Driver            string // sqlite or postgres
	SQLitePath        string
	DefaultSQLitePath string
	PG                config.PostgresParams
	PGPort            string // as entered
	SSLModes          []string
	Port              string // port the server listens on
	AskToken          bool
	TokenFile         string
	Success           string
	Error             string
}

var setupSSLModes = setup.SSLModes

func (s *Server) handleSetupDatabase(w http.ResponseWriter, r *http.Request) {
	r, ok := s.setupModeCSRF(w, r)
	if !ok {
		s.errorPage(w, r, http.StatusForbidden, "Request rejected",
			"The form has expired or was sent from another site. Please reload the setup page and try again.")
		return
	}
	listen := setupListen(r)
	_, port, _ := net.SplitHostPort(listen)
	d := &setupDBData{
		Driver:            config.DriverSQLite,
		DefaultSQLitePath: filepath.Join(s.DataDir, setup.DefaultSQLiteFile),
		PG:                config.PostgresParams{Host: "localhost", Database: "statuspage", User: "statuspage", SSLMode: "prefer"},
		PGPort:            strconv.Itoa(setup.DefaultPostgresPort),
		SSLModes:          setupSSLModes,
		Port:              port,
		TokenFile:         s.setupTokenFile(),
	}
	if r.Method != http.MethodPost {
		d.AskToken = s.setupAskToken(r)
		s.render(w, r, "setup_database", "Setup", d)
		return
	}

	setupMu.Lock()
	defer setupMu.Unlock()
	if s.activated {
		// A second submission (another tab, or a click while the first one
		// was saving) that reached this handler before the server switched
		// to normal mode: the database is configured already, so do not
		// write config.json again but continue with the next step.
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	d.Driver = r.FormValue("driver")
	d.SQLitePath = formTrim(r, "sqlite_path")
	d.PG = config.PostgresParams{
		Host:     formTrim(r, "pg_host"),
		Database: formTrim(r, "pg_database"),
		User:     formTrim(r, "pg_user"),
		Password: r.FormValue("pg_password"),
		SSLMode:  r.FormValue("pg_sslmode"),
	}
	d.PGPort = formTrim(r, "pg_port")
	render := func(status int) { s.renderStatus(w, r, status, "setup_database", "Setup", d) }
	if !s.setupCheckToken(w, r) {
		d.Error = "Please enter the correct setup token (see below)."
		d.AskToken = true
		render(http.StatusForbidden)
		return
	}
	if d.Driver != config.DriverSQLite && d.Driver != config.DriverPostgres {
		d.Error = "Please choose SQLite or PostgreSQL."
		d.Driver = config.DriverSQLite
		render(http.StatusUnprocessableEntity)
		return
	}
	if d.Driver == config.DriverPostgres && d.PGPort != "" {
		port, err := strconv.Atoi(d.PGPort)
		if err != nil {
			d.Error = "The PostgreSQL port must be a number, usually 5432."
			render(http.StatusUnprocessableEntity)
			return
		}
		d.PG.Port = port
	}
	params := setup.Params{Driver: d.Driver, Postgres: d.PG, Listen: listen}
	if d.Driver == config.DriverSQLite {
		params.SQLitePath = d.SQLitePath
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if r.FormValue("action") == "test" {
		if err := setup.Check(ctx, s.DataDir, params); err != nil {
			d.Error = setupSentence(err.Error())
			render(http.StatusUnprocessableEntity)
			return
		}
		if d.Driver == config.DriverSQLite {
			d.Success = "Everything is fine: the database file can be created."
		} else {
			d.Success = "Connection successful: the database is reachable and tables can be created."
		}
		render(http.StatusOK)
		return
	}

	cfg, err := setup.Apply(ctx, s.DataDir, params)
	if err != nil {
		d.Error = setupSentence(err.Error())
		render(http.StatusUnprocessableEntity)
		return
	}
	if s.Activate == nil {
		s.serverError(w, r, errors.New("setup: no Activate callback"))
		return
	}
	if err := s.Activate(cfg); err != nil {
		s.Log.Error("activate database after setup", "err", err)
		d.Error = fmt.Sprintf("The configuration was saved, but the server could not switch to the new database: %v. Please restart the Status Page service.", err)
		render(http.StatusInternalServerError)
		return
	}
	s.activated = true
	s.Log.Info("setup: database configured", "driver", cfg.DBDriver, "listen", cfg.Listen, "ip", auth.ClientIP(r))
	http.SetCookie(w, &http.Cookie{Name: setupCSRFCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: auth.IsHTTPS(r), SameSite: http.SameSiteLaxMode})
	// Neutral wording: if the database already has an administrator (e.g. a
	// reinstallation), the next page is the sign-in page.
	s.redirectWithFlash(w, r, "/setup", "success", "The database is ready.")
}

// setupListen returns the listen address written to config.json: the
// address the server listens on now, so that it does not change when the
// server is restarted without -listen (e.g. as a service). The host part is
// kept if one was given (e.g. -listen 127.0.0.1:8080 behind a reverse
// proxy); the port is taken from the connection, in case port 0 was given.
func setupListen(r *http.Request) string {
	a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return config.DefaultListen
	}
	_, port, err := net.SplitHostPort(a.String())
	if err != nil || port == "" || port == "0" {
		return config.DefaultListen
	}
	if srv, ok := r.Context().Value(http.ServerContextKey).(*http.Server); ok {
		if host, _, err := net.SplitHostPort(srv.Addr); err == nil && host != "" {
			return net.JoinHostPort(host, port)
		}
	}
	return ":" + port
}

// setupSentence capitalises the first letter of a message from package setup.
func setupSentence(msg string) string {
	r, n := utf8.DecodeRuneInString(msg)
	if n == 0 {
		return msg
	}
	return string(unicode.ToUpper(r)) + msg[n:]
}

// ------------------------------------------------------------------ step 2: administrator

// setupAdminData is the data of the administrator step.
type setupAdminData struct {
	Username     string
	Name         string
	SiteName     string
	BaseURL      string
	Timezone     string
	PublicAccess bool
	MinPassword  int
	Timezones    []string
	AskToken     bool
	TokenFile    string
	Errors       adminFormErrors
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == http.MethodPost {
		setupMu.Lock()
		defer setupMu.Unlock()
	}
	done, err := s.Store.HasLocalAdmin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if done {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	st := s.settings(r)
	// No user name is suggested: failed sign-ins lock a name out for a while
	// (for everybody behind a reverse proxy), so it should not be guessable.
	d := &setupAdminData{
		SiteName:     st.SiteName,
		BaseURL:      st.BaseURL,
		Timezone:     st.Timezone,
		PublicAccess: st.PublicAccess,
		MinPassword:  auth.MinPasswordLength,
		Timezones:    adminTimezones,
		TokenFile:    s.setupTokenFile(),
		Errors:       adminFormErrors{},
	}
	if d.BaseURL == "" {
		d.BaseURL = setupSuggestBaseURL(r)
	}
	if r.Method != http.MethodPost {
		d.AskToken = s.setupAskToken(r)
		s.render(w, r, "setup_admin", "Setup", d)
		return
	}

	d.Username = strings.ToLower(formTrim(r, "username"))
	d.Name = formTrim(r, "name")
	d.SiteName = formTrim(r, "site_name")
	d.PublicAccess = formBool(r, "public_access")
	password := r.FormValue("password")
	var msg string
	if d.BaseURL, msg = adminValidateBaseURL(r.FormValue("base_url")); msg != "" {
		d.Errors.add("base_url", msg)
	}
	if d.Timezone, msg = adminValidateTimezone(r.FormValue("timezone")); msg != "" {
		d.Errors.add("timezone", msg)
	}
	switch {
	case d.Username == "":
		d.Errors.add("username", "Enter a user name.")
	case len(d.Username) > 64 || strings.IndexFunc(d.Username, setupBadUsernameRune) >= 0:
		d.Errors.add("username", "Use at most 64 letters, digits and the characters . _ - @ (no spaces).")
	}
	if tooLong(d.Name, 100) {
		d.Errors.add("name", "The display name is too long (at most 100 characters).")
	}
	switch {
	case utf8.RuneCountInString(password) < auth.MinPasswordLength:
		d.Errors.add("password", fmt.Sprintf("The password must be at least %d characters long.", auth.MinPasswordLength))
	case len(password) > 72:
		d.Errors.add("password", "The password is too long (at most 72 bytes).")
	case password != r.FormValue("password_confirm"):
		d.Errors.add("password_confirm", "The passwords do not match.")
	}
	switch {
	case d.SiteName == "":
		d.Errors.add("site_name", "Enter a name for the status page.")
	case tooLong(d.SiteName, 100):
		d.Errors.add("site_name", "The name is too long (at most 100 characters).")
	}
	status := http.StatusUnprocessableEntity
	if !s.setupCheckToken(w, r) {
		d.Errors.add(setupTokenField, "Enter the correct setup token.")
		d.AskToken = true
		status = http.StatusForbidden
	}
	if len(d.Errors) > 0 {
		s.renderStatus(w, r, status, "setup_admin", "Setup", d)
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	st.SiteName, st.BaseURL, st.Timezone, st.PublicAccess = d.SiteName, d.BaseURL, d.Timezone, d.PublicAccess
	if err := s.Store.SaveSettings(ctx, st); err != nil {
		s.serverError(w, r, err)
		return
	}
	u, err := s.Store.CreateLocalAdmin(ctx, d.Username, d.Name, hash)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.setupDone.Store(true)
	if err := os.Remove(s.setupTokenFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.Log.Warn("could not delete the setup token file", "file", s.setupTokenFile(), "err", err)
	}
	http.SetCookie(w, &http.Cookie{Name: setupTokenCookie, Value: "", Path: "/setup", MaxAge: -1, HttpOnly: true, Secure: auth.IsHTTPS(r), SameSite: http.SameSiteStrictMode})
	s.Log.Info("setup complete: local administrator created", "user", u.Username, "ip", auth.ClientIP(r))
	if err := s.Auth.StartSession(ctx, w, r, u.ID); err != nil {
		s.Log.Error("start session after setup", "err", err)
		s.redirectWithFlash(w, r, "/login", "success", "Setup complete. Sign in with the administrator account.")
		return
	}
	s.redirectWithFlash(w, r, "/admin/settings", "success", "Setup complete. Next: configure single sign-on and e-mail.")
}

// setupHostname returns the name of this computer (replaced in tests).
var setupHostname = os.Hostname

// setupSuggestBaseURL returns the base URL suggested by the setup wizard:
// the address of the request, except that localhost is replaced by the name
// of this computer. The Windows installer opens the wizard as
// http://localhost:<port>, but other computers cannot use that address for
// links in e-mails or for the single sign-on redirect. Returns "" if no
// usable address is known.
func setupSuggestBaseURL(r *http.Request) string {
	scheme := "http"
	if auth.IsHTTPS(r) {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = strings.Trim(r.Host, "[]"), ""
	}
	if adminIsLocalhost(host) {
		name, err := setupHostname()
		if err != nil || name == "" {
			return ""
		}
		host = strings.ToLower(name)
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]" // IPv6 address
	}
	u, msg := adminValidateBaseURL(scheme + "://" + host)
	if msg != "" {
		return ""
	}
	return u
}

// setupBadUsernameRune reports characters not allowed in local user names.
func setupBadUsernameRune(r rune) bool {
	return !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-@", r))
}
