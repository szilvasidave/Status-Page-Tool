package web

import (
	"errors"
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

// handleUserDelete removes a single sign-on or directory user with the
// user's group memberships and sessions, e.g. someone who left the company
// or changed teams and should no longer be notified. The user is created
// again at the next sign-in or directory sync.
func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	u, err := s.Store.GetUser(ctx, id)
	if err == nil {
		err = s.Store.DeleteUser(ctx, id)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
		return
	case errors.Is(err, store.ErrLocalAdmin):
		s.redirectWithFlash(w, r, "/admin/users", "error", "The local administrator account cannot be removed.")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("user removed", "user_id", u.ID, "email", u.Email, "by", adminActor(r))
	s.redirectWithFlash(w, r, "/admin/users", "success", "“"+u.DisplayName()+"” has been removed.")
}
