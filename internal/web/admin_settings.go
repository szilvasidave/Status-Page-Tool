package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/directory"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminFormErrors maps form field names to error messages. Templates show
// them with {{template "field-error" .Errors.<field>}}.
type adminFormErrors map[string]string

// add records msg for field unless the field already has an error.
func (e adminFormErrors) add(field, msg string) {
	if _, ok := e[field]; !ok {
		e[field] = msg
	}
}

// adminTimezones are offered in the time zone <datalist>; any IANA name is
// accepted.
var adminTimezones = []string{
	"UTC",
	"Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Paris", "Europe/Brussels",
	"Europe/Amsterdam", "Europe/Berlin", "Europe/Zurich", "Europe/Vienna", "Europe/Rome",
	"Europe/Madrid", "Europe/Prague", "Europe/Budapest", "Europe/Warsaw", "Europe/Stockholm",
	"Europe/Oslo", "Europe/Copenhagen", "Europe/Helsinki", "Europe/Athens", "Europe/Bucharest",
	"Europe/Istanbul", "Europe/Kyiv", "Europe/Moscow",
	"America/New_York", "America/Chicago", "America/Denver", "America/Phoenix",
	"America/Los_Angeles", "America/Anchorage", "America/Toronto", "America/Vancouver",
	"America/Mexico_City", "America/Bogota", "America/Sao_Paulo", "America/Argentina/Buenos_Aires",
	"Africa/Johannesburg", "Africa/Lagos", "Africa/Cairo", "Africa/Nairobi",
	"Asia/Dubai", "Asia/Kolkata", "Asia/Bangkok", "Asia/Singapore", "Asia/Hong_Kong",
	"Asia/Shanghai", "Asia/Seoul", "Asia/Tokyo",
	"Australia/Perth", "Australia/Brisbane", "Australia/Sydney", "Pacific/Auckland",
}

// adminValidateBaseURL checks the base URL setting: empty, or an absolute
// http(s) URL without path, query or fragment. It returns the value without
// a trailing slash and an error message for the user ("" if valid).
func adminValidateBaseURL(v string) (string, string) {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v == "" {
		return "", ""
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" {
		return v, "Enter the full address starting with http:// or https://, e.g. https://status.example.com."
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(v, "?#") {
		return v, "Enter only the scheme and host name (and port), e.g. https://status.example.com; the status page cannot be served from a sub-path."
	}
	return v, ""
}

// adminValidateTimezone checks an IANA time zone name ("" means UTC). It
// returns the name and an error message for the user ("" if valid).
func adminValidateTimezone(v string) (string, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "UTC", ""
	}
	if _, err := time.LoadLocation(v); err != nil || v == "Local" {
		return v, fmt.Sprintf("Unknown time zone %q. Use a name such as Europe/Berlin, America/New_York or UTC.", v)
	}
	return v, ""
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

func adminIsLocalhost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// adminSettingsData is the data of the settings page.
type adminSettingsData struct {
	St               store.Settings // form values; secrets only hold newly entered values
	SMTPPort         string
	HasOIDCSecret    bool // a client secret is stored
	HasSMTPPassword  bool // an SMTP password is stored
	Errors           adminFormErrors
	CallbackURL      string
	LocalBaseURL     bool // the base URL points to localhost: other computers cannot use it
	DirectoryEnabled bool // directory sync is enabled and configured ("Sync now")
	TestEmailTo      string
	Timezones        []string
}

func (s *Server) adminDirectoryEnabled(ctx context.Context) bool {
	return s.Directory != nil && s.Directory.Enabled(ctx)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	cur, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := s.adminSettingsData(r, *cur)
	d.HasOIDCSecret = cur.OIDCClientSecret != ""
	d.HasSMTPPassword = cur.SMTPPassword != ""
	d.St.OIDCClientSecret, d.St.SMTPPassword = "", ""
	s.render(w, r, "admin_settings", "Settings", d)
}

func (s *Server) adminSettingsData(r *http.Request, st store.Settings) *adminSettingsData {
	d := &adminSettingsData{
		St:               st,
		SMTPPort:         strconv.Itoa(st.SMTPPort),
		Errors:           adminFormErrors{},
		CallbackURL:      auth.CallbackURL(r, st.BaseURL),
		DirectoryEnabled: s.adminDirectoryEnabled(r.Context()),
		Timezones:        adminTimezones,
	}
	if st.SMTPPort == 0 {
		d.SMTPPort = ""
	}
	if u, err := url.Parse(st.BaseURL); err == nil && st.BaseURL != "" {
		d.LocalBaseURL = adminIsLocalhost(u.Hostname())
	}
	if id := auth.FromContext(r.Context()); id != nil && id.User != nil {
		d.TestEmailTo = id.User.Email
	}
	return d
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cur, err := s.Store.GetSettings(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	st := *cur
	errs := adminFormErrors{}

	// General.
	st.SiteName = formTrim(r, "site_name")
	switch {
	case st.SiteName == "":
		errs.add("site_name", "Enter a name for the status page.")
	case tooLong(st.SiteName, 100):
		errs.add("site_name", "The name is too long (at most 100 characters).")
	}
	var msg string
	if st.BaseURL, msg = adminValidateBaseURL(r.FormValue("base_url")); msg != "" {
		errs.add("base_url", msg)
	}
	if st.Timezone, msg = adminValidateTimezone(r.FormValue("timezone")); msg != "" {
		errs.add("timezone", msg)
	}
	st.PublicAccess = formBool(r, "public_access")

	// Single sign-on.
	newOIDCSecret := strings.TrimSpace(r.FormValue("oidc_client_secret"))
	st.OIDCEnabled = formBool(r, "oidc_enabled")
	st.OIDCIssuer = formTrim(r, "oidc_issuer")
	st.OIDCClientID = formTrim(r, "oidc_client_id")
	if newOIDCSecret != "" {
		st.OIDCClientSecret = newOIDCSecret
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
	if st.OIDCEnabled {
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

	// Directory sync.
	st.DirectorySyncEnabled = formBool(r, "directory_sync_enabled")
	if st.DirectorySyncEnabled {
		if _, err := directory.NewGraphClient(st.OIDCIssuer, st.OIDCClientID, st.OIDCClientSecret); err != nil {
			msg := strings.TrimPrefix(err.Error(), directory.ErrNotEnabled.Error()+": ")
			errs.add("directory_sync_enabled", "Directory sync works only with Microsoft Entra ID and uses the single sign-on client credentials: "+msg+".")
		}
	}

	// E-mail.
	newSMTPPassword := r.FormValue("smtp_password")
	st.SMTPHost = formTrim(r, "smtp_host")
	st.SMTPSecurity = r.FormValue("smtp_security")
	if st.SMTPSecurity != "starttls" && st.SMTPSecurity != "tls" && st.SMTPSecurity != "none" {
		errs.add("smtp_security", "Choose STARTTLS, TLS or none.")
		st.SMTPSecurity = "starttls"
	}
	smtpPort := formTrim(r, "smtp_port")
	switch {
	case smtpPort == "":
		st.SMTPPort = map[string]int{"starttls": 587, "tls": 465, "none": 25}[st.SMTPSecurity]
	default:
		p, err := strconv.Atoi(smtpPort)
		if err != nil || p < 1 || p > 65535 {
			errs.add("smtp_port", "Enter a port number between 1 and 65535 (usually 587 for STARTTLS, 465 for TLS).")
		} else {
			st.SMTPPort = p
		}
	}
	st.SMTPUsername = formTrim(r, "smtp_username")
	if newSMTPPassword != "" {
		st.SMTPPassword = newSMTPPassword
	}
	if st.SMTPUsername == "" {
		st.SMTPPassword = "" // no authentication: do not keep an unused password
	}
	st.SMTPFrom = formTrim(r, "smtp_from")
	if st.SMTPFrom != "" {
		if _, err := mail.ParseAddress(st.SMTPFrom); err != nil {
			errs.add("smtp_from", "Enter a valid sender address, e.g. status@example.com or IT Status <status@example.com>.")
		}
	} else if st.SMTPHost != "" {
		errs.add("smtp_from", "Enter the sender address for e-mails.")
	}
	if st.SMTPHost == "" && st.SMTPFrom != "" {
		errs.add("smtp_host", "Enter the SMTP server, or clear the sender address to turn e-mail off.")
	}

	if len(errs) > 0 {
		d := s.adminSettingsData(r, st)
		d.SMTPPort = smtpPort
		d.Errors = errs
		d.HasOIDCSecret = cur.OIDCClientSecret != ""
		d.HasSMTPPassword = cur.SMTPPassword != "" && st.SMTPUsername != ""
		// Show newly entered secrets again so they need not be retyped;
		// stored secrets are never sent to the browser.
		d.St.OIDCClientSecret, d.St.SMTPPassword = newOIDCSecret, newSMTPPassword
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "admin_settings", "Settings", d)
		return
	}
	if err := s.Store.SaveSettings(ctx, &st); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Auth.ResetOIDC()
	s.Log.Info("settings saved", "user", adminActor(r))
	s.redirectWithFlash(w, r, "/admin/settings", "success", "Settings saved.")
}

func (s *Server) handleTestEmail(w http.ResponseWriter, r *http.Request) {
	const back = "/admin/settings"
	to := formTrim(r, "to")
	if _, err := mail.ParseAddress(to); err != nil || to == "" {
		s.redirectWithFlash(w, r, back, "error", "Enter a valid e-mail address to send the test e-mail to.")
		return
	}
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !st.SMTPConfigured() {
		s.redirectWithFlash(w, r, back, "error", "E-mail is not configured yet: enter and save the SMTP server and sender address first.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.Notifier.SendTestEmail(ctx, to); err != nil {
		s.Log.Warn("test e-mail failed", "to", to, "err", err)
		s.redirectWithFlash(w, r, back, "error", "The test e-mail could not be sent: "+err.Error())
		return
	}
	s.redirectWithFlash(w, r, back, "success", "A test e-mail was sent to "+to+". Check the inbox (and the spam folder).")
}

// adminActor names the signed-in user for log messages.
func adminActor(r *http.Request) string {
	if id := auth.FromContext(r.Context()); id != nil && id.User != nil {
		if id.User.Username != "" {
			return id.User.Username
		}
		return id.User.DisplayName()
	}
	return ""
}
