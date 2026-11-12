package web

import (
	"errors"
	"net/http"

	"github.com/szilvasidave/status-page-tool/internal/auth"
)

type loginData struct {
	Next        string
	OIDCEnabled bool
	ButtonLabel string
	Error       string
	Username    string
	ShowLocal   bool // expand the local administrator form
}

func (s *Server) loginData(r *http.Request, next string) *loginData {
	st := s.settings(r)
	d := &loginData{
		Next:        auth.SafeReturnTo(next),
		OIDCEnabled: st.OIDCConfigured(),
		ButtonLabel: st.OIDCButtonLabel,
	}
	if d.ButtonLabel == "" {
		d.ButtonLabel = "Sign in with SSO"
	}
	d.ShowLocal = !d.OIDCEnabled || r.URL.Query().Get("local") == "1"
	return d
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	next := auth.SafeReturnTo(r.URL.Query().Get("next"))
	if auth.FromContext(r.Context()) != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", "Sign in", s.loginData(r, next))
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
	case errors.Is(err, auth.ErrInvalidCredentials):
		s.Log.Warn("failed local login", "user", d.Username, "ip", auth.ClientIP(r))
		d.Error = "Invalid username or password."
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
