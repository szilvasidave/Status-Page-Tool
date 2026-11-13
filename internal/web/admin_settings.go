package web

// The settings page (/admin/settings) has one section per file:
//
//	admin_settings_general.go  General (site name, base URL, time zone, public access)
//	admin_settings_sso.go      Single sign-on (OpenID Connect)
//	admin_settings_sync.go     Users and groups from Microsoft Entra ID (SCIM provisioning)
//	admin_settings_mail.go     E-mail delivery (+ the test e-mail)
//
// with the matching template partials in templates/partials/settings_*.html.
// Each section has a view function (fills its part of adminSettingsData) and
// a save function (reads its form fields into the settings, validates them
// and records errors).

import (
	"net/http"

	"github.com/szilvasidave/status-page-tool/internal/auth"
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

// adminSettingsData is the data of the settings page.
type adminSettingsData struct {
	St     store.Settings // form values; secrets only hold newly entered values
	Errors adminFormErrors

	General adminGeneralData // admin_settings_general.go
	SSO     adminSSOData     // admin_settings_sso.go
	Sync    adminSyncData    // admin_settings_sync.go
	Mail    adminMailData    // admin_settings_mail.go
}

// adminSettingsView fills the page data for showing st; cur are the stored
// settings (used to tell which secrets are stored). Stored secrets are never
// sent to the browser.
func (s *Server) adminSettingsView(r *http.Request, st store.Settings, cur *store.Settings, d *adminSettingsData) {
	d.St = st
	if d.Errors == nil {
		d.Errors = adminFormErrors{}
	}
	s.adminGeneralView(r, cur, d)
	s.adminSSOView(r, cur, d)
	s.adminSyncView(r, cur, d)
	s.adminMailView(r, cur, d)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	cur, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := &adminSettingsData{}
	s.adminSettingsView(r, *cur, cur, d)
	s.render(w, r, "admin_settings", "Settings", d)
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cur, err := s.Store.GetSettings(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	st := *cur
	d := &adminSettingsData{Errors: adminFormErrors{}}
	s.adminGeneralSave(r, cur, &st, d)
	s.adminSSOSave(r, cur, &st, d)
	s.adminSyncSave(r, cur, &st, d)
	s.adminMailSave(r, cur, &st, d)

	if len(d.Errors) > 0 {
		s.adminSettingsView(r, st, cur, d)
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "admin_settings", "Settings", d)
		return
	}
	if st.LocalLoginEnabled && !cur.LocalLoginEnabled {
		// Turned on again: sessions of the local administrator from before
		// it was turned off must not come back to life (like "statuspage
		// local-login enable"). While it is off they are only ignored, so
		// that the sign-in page can tell each browser why it was signed
		// out. Deleted before saving: no new ones can start while it is off.
		if err := s.Store.DeleteLocalAdminSessions(ctx); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if err := s.Store.SaveSettings(ctx, &st); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Auth.ResetOIDC()
	s.Log.Info("settings saved", "user", adminActor(r))
	s.redirectWithFlash(w, r, "/admin/settings", "success", "Settings saved.")
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
