package web

import (
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// maxPasswordBytes is the longest password bcrypt accepts.
const maxPasswordBytes = 72

type accountData struct {
	User     *store.User
	Role     store.Role
	RoleHelp string
	Groups   []store.Group   // SSO users only
	Services []store.Service // services assigned to the user's groups

	MinPasswordLength int
	PasswordError     string
}

// accountData collects the data of the account page for the signed-in user.
func (s *Server) accountData(r *http.Request) (*accountData, error) {
	ctx := r.Context()
	id := auth.FromContext(ctx)
	d := &accountData{User: id.User, Role: id.Role, MinPasswordLength: auth.MinPasswordLength}
	switch {
	case id.User.IsLocalAdmin:
		d.RoleHelp = "The local administrator account always has the Administrator role. Use it for the initial setup and when single sign-on is unavailable."
	case id.Role >= store.RoleAdmin:
		d.RoleHelp = "Administrators can post incidents and maintenance windows and change the configuration. Your role is the highest role of your groups."
	case id.Role >= store.RoleEditor:
		d.RoleHelp = "Editors can post incidents and maintenance windows. Your role is the highest role of your groups."
	default:
		d.RoleHelp = "Viewers can see the status pages. Your role is the highest role of your groups."
	}
	if id.User.IsLocalAdmin {
		return d, nil // not a member of identity provider groups
	}

	groups, err := s.Store.UserGroups(ctx, id.User.ID)
	if err != nil {
		return nil, err
	}
	d.Groups = groups
	notified := map[int64]bool{}
	for _, g := range groups {
		for _, sid := range g.ServiceIDs {
			notified[sid] = true
		}
	}
	if len(notified) > 0 {
		services, err := s.Store.ListServices(ctx)
		if err != nil {
			return nil, err
		}
		for _, svc := range services {
			if notified[svc.ID] {
				d.Services = append(d.Services, svc)
			}
		}
	}
	return d, nil
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	d, err := s.accountData(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "account", "Your account", d)
}

// handleAccountPassword changes the password of the local administrator.
func (s *Server) handleAccountPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := auth.FromContext(ctx).User
	if !user.IsLocalAdmin {
		s.errorPage(w, r, http.StatusForbidden, "Access denied",
			"Only the local administrator account has a password here. Your password is managed by your identity provider.")
		return
	}

	current := r.FormValue("current_password")
	password := r.FormValue("new_password")
	var problem string
	switch {
	case current == "" || !auth.CheckPassword(user.PasswordHash, current):
		problem = "The current password is not correct."
	case utf8.RuneCountInString(password) < auth.MinPasswordLength:
		problem = fmt.Sprintf("The new password must be at least %d characters long.", auth.MinPasswordLength)
	case len(password) > maxPasswordBytes:
		problem = fmt.Sprintf("The new password is too long (at most %d bytes).", maxPasswordBytes)
	case password != r.FormValue("confirm_password"):
		problem = "The new password and its confirmation do not match."
	}
	if problem != "" {
		d, err := s.accountData(r)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		d.PasswordError = problem
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "account", "Your account", d)
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.Store.SetPasswordHash(ctx, user.ID, hash); err != nil {
		s.serverError(w, r, err)
		return
	}
	// Sign out everywhere else, but keep this browser signed in.
	if err := s.Store.DeleteUserSessions(ctx, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.Auth.StartSession(ctx, w, r, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("local administrator password changed", "user", user.Username, "ip", auth.ClientIP(r))
	s.redirectWithFlash(w, r, "/account", "success", "Your password was changed. All other sessions of this account were signed out.")
}
