package web

import (
	"net/http"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminUserRow is one row of the users list.
type adminUserRow struct {
	store.UserWithGroups
	SignIn string // how the user signs in
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows := make([]adminUserRow, len(users))
	for i, u := range users {
		rows[i] = adminUserRow{UserWithGroups: u}
		switch {
		case u.IsLocalAdmin:
			rows[i].SignIn = "Local administrator"
		case u.OIDCSubject != "":
			rows[i].SignIn = "SSO"
		default:
			rows[i].SignIn = "Directory only (never signed in)"
		}
	}
	s.render(w, r, "admin_users", "Users", rows)
}
