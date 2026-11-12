package web

import (
	"net/http"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminNotificationsLimit is the number of outbox rows shown.
const adminNotificationsLimit = 200

// adminNotificationsData is the data of the notifications page.
type adminNotificationsData struct {
	Notifications  []store.Notification
	Limit          int
	SMTPConfigured bool
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := s.Store.ListNotifications(ctx, adminNotificationsLimit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	st, err := s.Store.GetSettings(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "admin_notifications", "Notifications", &adminNotificationsData{
		Notifications:  list,
		Limit:          adminNotificationsLimit,
		SMTPConfigured: st.SMTPConfigured(),
	})
}
