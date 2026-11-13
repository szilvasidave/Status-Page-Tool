package web

// Settings page section "Single sign-on (OpenID Connect)". Template:
// templates/partials/settings_sso.html.

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminSSOData is the view data of the single sign-on section.
type adminSSOData struct {
	CallbackURL string
	HasSecret   bool   // a client secret is stored
	newSecret   string // secret entered in a form that failed validation
}

func (s *Server) adminSSOView(r *http.Request, cur *store.Settings, d *adminSettingsData) {
	d.SSO.CallbackURL = auth.CallbackURL(r, d.St.BaseURL)
	d.SSO.HasSecret = cur.OIDCClientSecret != ""
	// Show a newly entered secret again so it need not be retyped; the
	// stored secret is never sent to the browser.
	d.St.OIDCClientSecret = d.SSO.newSecret
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
	// The OIDC fields are hidden while single sign-on is off, so they are
	// only checked when it is enabled.
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
