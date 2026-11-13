package web

// Settings page section "group members from the identity provider"
// (currently: the Microsoft Entra directory sync via Microsoft Graph).
// Template: templates/partials/settings_sync.html.

import (
	"context"
	"net/http"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/directory"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminSyncData is the view data of the group member sync section.
type adminSyncData struct {
	DirectoryEnabled bool // directory sync is enabled and configured ("Sync now")
}

func (s *Server) adminDirectoryEnabled(ctx context.Context) bool {
	return s.Directory != nil && s.Directory.Enabled(ctx)
}

func (s *Server) adminSyncView(r *http.Request, cur *store.Settings, d *adminSettingsData) {
	d.Sync.DirectoryEnabled = s.adminDirectoryEnabled(r.Context())
}

func (s *Server) adminSyncSave(r *http.Request, cur, st *store.Settings, d *adminSettingsData) {
	st.DirectorySyncEnabled = formBool(r, "directory_sync_enabled")
	if st.DirectorySyncEnabled {
		if _, err := directory.NewGraphClient(st.OIDCIssuer, st.OIDCClientID, st.OIDCClientSecret); err != nil {
			msg := strings.TrimPrefix(err.Error(), directory.ErrNotEnabled.Error()+": ")
			d.Errors.add("directory_sync_enabled", "Directory sync works only with Microsoft Entra ID and uses the single sign-on client credentials: "+msg+".")
		}
	}
}
