package web

import (
	"net/http"

	"github.com/szilvasidave/status-page-tool/internal/scim"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// routes registers all routes for normal mode. Handler implementations live
// in public.go, login.go, setup.go, manage.go, brand.go and admin_*.go.
func (s *Server) routes() {
	m := s.mux
	viewer := s.viewer
	editor := func(h http.HandlerFunc) http.Handler { return s.require(store.RoleEditor, h) }
	admin := func(h http.HandlerFunc) http.Handler { return s.require(store.RoleAdmin, h) }
	signedIn := func(h http.HandlerFunc) http.Handler { return s.require(store.RoleViewer, h) }

	// Public status pages (login required when public access is disabled).
	m.Handle("GET /{$}", viewer(s.handleStatusPage))
	m.Handle("GET /history", viewer(s.handleHistory))
	m.Handle("GET /incidents/{id}", viewer(s.handleIncident))
	m.Handle("GET /maintenance/{id}", viewer(s.handleMaintenance))
	m.Handle("GET /api/status", viewer(s.handleAPIStatus))

	// Brand images (logos, favicon): public even while public access is
	// off, because the sign-in page shows them.
	m.HandleFunc("GET /brand/{name}", s.handleBrandImage)

	// Authentication.
	m.HandleFunc("GET /login", s.handleLoginPage)
	m.HandleFunc("POST /login", s.handleLocalLogin)
	m.HandleFunc("GET /auth/oidc/login", s.handleOIDCStart)
	m.HandleFunc("GET /auth/oidc/callback", s.handleOIDCCallback)
	m.HandleFunc("POST /logout", s.handleLogout)
	m.Handle("GET /account", signedIn(s.handleAccount))
	m.Handle("POST /account/profile", signedIn(s.handleAccountProfile))
	m.Handle("POST /account/password", signedIn(s.handleAccountPassword))

	// First-run setup: create the local administrator account.
	m.HandleFunc("GET /setup", s.handleSetup)
	m.HandleFunc("POST /setup", s.handleSetup)

	// Editors: incidents and maintenance windows.
	m.Handle("GET /manage", editor(s.handleManageDashboard))
	m.Handle("GET /manage/incidents/new", editor(s.handleIncidentNew))
	m.Handle("POST /manage/incidents", editor(s.handleIncidentCreate))
	m.Handle("GET /manage/incidents/{id}", editor(s.handleIncidentManage))
	m.Handle("POST /manage/incidents/{id}/updates", editor(s.handleIncidentAddUpdate))
	m.Handle("POST /manage/incidents/{id}/title", editor(s.handleIncidentTitle))
	m.Handle("POST /manage/incidents/{id}/delete", admin(s.handleIncidentDelete))
	m.Handle("GET /manage/maintenance/new", editor(s.handleMaintenanceNew))
	m.Handle("POST /manage/maintenance", editor(s.handleMaintenanceCreate))
	m.Handle("GET /manage/maintenance/{id}", editor(s.handleMaintenanceEdit))
	m.Handle("POST /manage/maintenance/{id}", editor(s.handleMaintenanceUpdate))
	m.Handle("POST /manage/maintenance/{id}/end", editor(s.handleMaintenanceEndNow))
	m.Handle("POST /manage/maintenance/{id}/cancel", editor(s.handleMaintenanceCancel))
	m.Handle("POST /manage/maintenance/{id}/delete", admin(s.handleMaintenanceDelete))

	// Administrators: configuration.
	m.Handle("GET /admin", admin(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
	}))
	m.Handle("GET /admin/settings", admin(s.handleSettings))
	m.Handle("POST /admin/settings", admin(s.handleSettingsSave))
	m.Handle("POST /admin/settings/test-email", admin(s.handleTestEmail))
	m.Handle("POST /admin/settings/scim-token", admin(s.handleSCIMTokenNew))
	m.Handle("POST /admin/settings/scim-token/delete", admin(s.handleSCIMTokenDelete))
	m.Handle("GET /admin/branding", admin(s.handleBranding))
	m.Handle("POST /admin/branding", admin(s.handleBrandingSave))
	m.Handle("POST /admin/branding/reset", admin(s.handleBrandingReset))
	m.Handle("GET /admin/services", admin(s.handleServices))
	m.Handle("GET /admin/services/new", admin(s.handleServiceNew))
	m.Handle("POST /admin/services", admin(s.handleServiceCreate))
	m.Handle("GET /admin/services/{id}", admin(s.handleServiceEdit))
	m.Handle("POST /admin/services/{id}", admin(s.handleServiceUpdate))
	m.Handle("POST /admin/services/{id}/delete", admin(s.handleServiceDelete))
	m.Handle("GET /admin/groups", admin(s.handleGroups))
	m.Handle("POST /admin/groups", admin(s.handleGroupCreate))
	m.Handle("GET /admin/groups/{id}", admin(s.handleGroupEdit))
	m.Handle("POST /admin/groups/{id}", admin(s.handleGroupUpdate))
	m.Handle("POST /admin/groups/{id}/delete", admin(s.handleGroupDelete))
	m.Handle("GET /admin/users", admin(s.handleUsers))
	m.Handle("POST /admin/users/{id}/delete", admin(s.handleUserDelete))
	m.Handle("GET /admin/notifications", admin(s.handleNotifications))

	// SCIM provisioning from Microsoft Entra ID (Tenant URL <Base URL>/scim/v2).
	// It authenticates with its own bearer token: the middleware passes
	// /scim/ through without session, CSRF check or setup redirect.
	m.Handle("/scim/", scim.New(s.Store, s.Log))

	// Everything else.
	m.HandleFunc("/", s.notFound)
}

// setupRoutes registers the routes for setup mode (no database yet).
func (s *Server) setupRoutes() {
	s.mux.HandleFunc("GET /setup", s.handleSetupDatabase)
	s.mux.HandleFunc("POST /setup", s.handleSetupDatabase)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	})
}
