package web

import (
	"errors"
	"net/http"

	"github.com/szilvasidave/status-page-tool/internal/auth"
)

type loginData struct {
	Next         string
	OIDCEnabled  bool
	ButtonLabel  string
	Error        string
	Notice       string // information, e.g. why the local administrator was signed out
	Username     string
	LocalEnabled bool // sign-in with the local administrator account is allowed (Settings.LocalLoginEnabled)
	ShowLocal    bool // expand the local administrator form
}

// Messages of the sign-in page while sign-in with the local administrator
// account is turned off.
const (
	loginLocalOff = "Sign-in with the local administrator account is turned off. Please sign in with single sign-on. " +
		"If single sign-on is unavailable, an administrator can turn the local sign-in on again on the server with the command “statuspage local-login enable”."
	loginLocalSignedOut = "Sign-in with the local administrator account has been turned off, so you were signed out. " +
		"Sign in with single sign-on."
)

func (s *Server) loginData(r *http.Request, next string) *loginData {
	st := s.settings(r)
	d := &loginData{
		Next:         auth.SafeReturnTo(next),
		OIDCEnabled:  st.OIDCConfigured(),
		ButtonLabel:  st.OIDCButtonLabel,
		LocalEnabled: st.LocalLoginEnabled,
	}
	if d.ButtonLabel == "" {
		d.ButtonLabel = "Sign in with SSO"
	}
	d.ShowLocal = d.LocalEnabled && (!d.OIDCEnabled || r.URL.Query().Get("local") == "1")
	return d
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	next := auth.SafeReturnTo(r.URL.Query().Get("next"))
	if auth.FromContext(r.Context()) != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	d := s.loginData(r, next)
	if !d.LocalEnabled {
		if s.Auth.LocalSessionEnded(r) {
			// The local administrator turned the local sign-in off (or it
			// was turned off by another administrator or on the command
			// line): end the session that no longer works and say why.
			s.Auth.Logout(w, r)
			d.Notice = loginLocalSignedOut
		} else if r.URL.Query().Get("local") == "1" {
			d.Notice = loginLocalOff
		}
	}
	s.render(w, r, "login", "Sign in", d)
}

func (s *Server) handleLocalLogin(w http.ResponseWriter, r *http.Request) {
	d := s.loginData(r, r.FormValue("next"))
	d.Username = formTrim(r, "username")
	d.ShowLocal = true
	err := s.Auth.LoginLocal(w, r, d.Username, r.FormValue("password"))
	switch {
	case err == nil:
		s.Log.Info("local login", "user", d.Username, "ip", auth.ClientIP(r))
		http.Redirect(w, r, d.Next, http.StatusSeeOther)
	case errors.Is(err, auth.ErrLocalLoginOff):
		s.Log.Warn("local login refused: turned off", "user", d.Username, "ip", auth.ClientIP(r))
		d.LocalEnabled, d.ShowLocal = false, false
		d.Error = loginLocalOff
		s.renderStatus(w, r, http.StatusForbidden, "login", "Sign in", d)
	case errors.Is(err, auth.ErrInvalidCredentials):
		s.Log.Warn("failed local login", "user", d.Username, "ip", auth.ClientIP(r))
		d.Error = "Invalid user name or password."
		s.renderStatus(w, r, http.StatusUnauthorized, "login", "Sign in", d)
	case errors.Is(err, auth.ErrTooManyAttempts):
		d.Error = "Too many failed attempts. Please wait a few minutes and try again."
		s.renderStatus(w, r, http.StatusTooManyRequests, "login", "Sign in", d)
	default:
		s.serverError(w, r, err)
	}
}

func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	next := auth.SafeReturnTo(r.URL.Query().Get("next"))
	if err := s.Auth.StartOIDC(w, r, next); err != nil {
		s.Log.Error("start OIDC login", "err", err)
		d := s.loginData(r, next)
		d.Error = "Single sign-on is not available: " + err.Error()
		s.renderStatus(w, r, http.StatusServiceUnavailable, "login", "Sign in", d)
	}
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	returnTo, err := s.Auth.FinishOIDC(w, r)
	if err != nil {
		s.Log.Warn("OIDC login failed", "err", err, "ip", auth.ClientIP(r))
		d := s.loginData(r, "/")
		d.Error = "Sign-in failed: " + err.Error()
		s.renderStatus(w, r, http.StatusUnauthorized, "login", "Sign in", d)
		return
	}
	http.Redirect(w, r, auth.SafeReturnTo(returnTo), http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.Auth.Logout(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
