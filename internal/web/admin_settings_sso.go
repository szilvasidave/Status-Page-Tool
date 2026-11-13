package web

// Settings page section "Single sign-on (OpenID Connect)", with the switch
// for the sign-in with the local administrator account. Template:
// templates/partials/settings_sso.html.

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminSSOData is the view data of the single sign-on section.
type adminSSOData struct {
	CallbackURL       string
	HasSecret         bool   // a client secret is stored
	LocalLoginCommand string // turns the local sign-in on again on this server
	newSecret         string // secret entered in a form that failed validation
}

func (s *Server) adminSSOView(r *http.Request, cur *store.Settings, d *adminSettingsData) {
	d.SSO.CallbackURL = auth.CallbackURL(r, d.St.BaseURL)
	d.SSO.HasSecret = cur.OIDCClientSecret != ""
	exe, _ := os.Executable()
	d.SSO.LocalLoginCommand = localLoginCommand(runtime.GOOS, exe, s.DataDir)
	// Show a newly entered secret again so it need not be retyped; the
	// stored secret is never sent to the browser.
	d.St.OIDCClientSecret = d.SSO.newSecret
}

// localLoginCommand returns the command that turns the sign-in with the
// local administrator account on again on this server: with its data
// directory and, on Windows, whose installer does not put the program folder
// on the PATH, with the path of the executable. Only administrators see it;
// the public sign-in page shows the general form.
func localLoginCommand(goos, exe, dataDir string) string {
	cmd := "statuspage"
	if goos == "windows" && exe != "" {
		cmd = `"` + exe + `"`
	}
	cmd += " local-login enable"
	if dataDir != "" {
		cmd += ` -data "` + dataDir + `"`
	}
	return cmd
}

func (s *Server) adminSSOSave(r *http.Request, cur, st *store.Settings, d *adminSettingsData) {
	errs := d.Errors
	d.SSO.newSecret = strings.TrimSpace(r.FormValue("oidc_client_secret"))
	st.OIDCEnabled = formBool(r, "oidc_enabled")
	st.OIDCIssuer = formTrim(r, "oidc_issuer")
	st.OIDCClientID = formTrim(r, "oidc_client_id")
	if d.SSO.newSecret != "" {
		st.OIDCClientSecret = d.SSO.newSecret
	}
	st.OIDCScopes = strings.Join(strings.Fields(r.FormValue("oidc_scopes")), " ")
	if st.OIDCScopes == "" {
		st.OIDCScopes = store.DefaultSettings().OIDCScopes
	}
	st.OIDCGroupsClaim = formTrim(r, "oidc_groups_claim")
	if st.OIDCGroupsClaim == "" {
		st.OIDCGroupsClaim = store.DefaultSettings().OIDCGroupsClaim
	}
	st.OIDCButtonLabel = formTrim(r, "oidc_button_label")
	if st.OIDCButtonLabel == "" {
		st.OIDCButtonLabel = store.DefaultSettings().OIDCButtonLabel
	}
	s.adminLocalLoginSave(r, st, d)
	// The fields are only checked while sign-in is enabled. The issuer,
	// client ID and client secret are always shown, because e-mail with
	// Microsoft Graph uses them, too (adminGraphAppProblem checks them for
	// it); the other fields are hidden while sign-in is off.
	if !st.OIDCEnabled {
		return
	}
	if st.OIDCIssuer == "" {
		errs.add("oidc_issuer", "Enter the issuer URL of your identity provider.")
	} else if msg := adminValidateIssuer(st.OIDCIssuer); msg != "" {
		errs.add("oidc_issuer", msg)
	}
	if st.OIDCClientID == "" {
		errs.add("oidc_client_id", "Enter the client ID of the application registration.")
	}
	if st.OIDCClientSecret == "" {
		errs.add("oidc_client_secret", "Enter the client secret of the application registration.")
	}
	if !strings.Contains(" "+st.OIDCScopes+" ", " openid ") {
		errs.add("oidc_scopes", `The scopes must include "openid".`)
	}
	if tooLong(st.OIDCButtonLabel, 60) {
		errs.add("oidc_button_label", "The label is too long (at most 60 characters).")
	}
}

// adminLocalLoginSave reads the checkbox "Allow sign-in with the local
// administrator account". It can only be turned off while nobody is locked
// out by it: single sign-on (as entered in this form) is enabled and
// configured, and a group has the Administrator role. "statuspage
// local-login disable" applies the same check (store.CheckLocalLoginOff).
func (s *Server) adminLocalLoginSave(r *http.Request, st *store.Settings, d *adminSettingsData) {
	st.LocalLoginEnabled = formBool(r, "local_login_enabled")
	if st.LocalLoginEnabled {
		return
	}
	switch err := s.Store.CheckLocalLoginOff(r.Context(), st); {
	case errors.Is(err, store.ErrSSONotConfigured):
		d.Errors.add("local_login_enabled", "The local administrator sign-in can only be turned off while single sign-on is enabled "+
			"and configured (issuer URL, client ID and client secret).")
	case errors.Is(err, store.ErrNoAdminGroup):
		d.Errors.add("local_login_enabled", "The local administrator sign-in can only be turned off when at least one group has the "+
			"Administrator role (Admin → Groups), so that somebody can still sign in as an administrator.")
	case err != nil:
		s.Log.Error("check whether the local sign-in can be turned off", "err", err)
		d.Errors.add("local_login_enabled", "The groups could not be checked, so the local administrator sign-in stays on. Please try again.")
	}
}

// adminValidateIssuer checks an OIDC issuer URL (https, or http for
// localhost) and returns an error message for the user ("" if valid).
func adminValidateIssuer(v string) string {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "Enter the issuer URL, e.g. https://login.microsoftonline.com/<tenant ID>/v2.0."
	}
	if u.Scheme == "http" && !adminIsLocalhost(u.Hostname()) {
		return "The issuer URL must start with https:// (http is only allowed for localhost)."
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "The issuer URL must not contain a query (?) or fragment (#)."
	}
	return ""
}
