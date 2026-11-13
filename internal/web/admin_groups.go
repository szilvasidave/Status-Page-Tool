package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/groupnames"
	"github.com/szilvasidave/status-page-tool/internal/msgraph"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminGroupsData is the data of the groups list.
type adminGroupsData struct {
	InUse      []store.Group // groups with a role or services, and provisioned groups
	Other      []store.Group // all other groups (shown collapsed)
	Names      adminGroupNames
	ExternalID string // "add group" form values
	Name       string
	Errors     adminFormErrors
}

// adminGroupNames is what the groups page shows about looking up the names
// of groups in Microsoft Entra ID (package groupnames).
type adminGroupNames struct {
	Active bool              // names are looked up (store.Settings.GroupNamesActive)
	Status groupnames.Status // the outcome of the latest lookup, while Active
	// Hint: the lookup is off, but single sign-on uses Microsoft Entra ID
	// and groups are shown by their object IDs.
	Hint bool
}

const (
	// adminLookUpAllTime is how long "Look up names now" may take; the
	// worker looks up the rest.
	adminLookUpAllTime = 25 * time.Second
	// adminLookUpGroupTime is how long adding a group waits for its name.
	adminLookUpGroupTime = 10 * time.Second
)

// adminGroupNamesView returns what the groups page shows about the names of
// groups.
func (s *Server) adminGroupNamesView(r *http.Request, groups []store.Group) adminGroupNames {
	st := s.settings(r)
	if st.GroupNamesActive() {
		return adminGroupNames{Active: true, Status: s.GroupNames.Status()}
	}
	if _, err := msgraph.Tenant(st.OIDCIssuer); err != nil || !st.OIDCConfigured() {
		return adminGroupNames{} // other identity providers can send the names in the claim
	}
	for i := range groups {
		if g := &groups[i]; !g.Provisioned && g.DisplayName() == g.ExternalID && store.IsGUID(g.ExternalID) {
			return adminGroupNames{Hint: true}
		}
	}
	return adminGroupNames{}
}

// adminGroupNameMax is the longest display name of a group (characters):
// the same as provisioning accepts, which is the limit of Microsoft Entra ID,
// so that a provisioned name can be saved unchanged with the group's role.
const adminGroupNameMax = 256

const adminGroupNameTooLong = "The display name is too long (at most 256 characters)."

// adminSplitGroups splits groups into the groups in use (with a role or
// services, or for which keep returns true) and the others. Every group of
// every user's groups claim is recorded at sign-in, so with Microsoft Entra
// ID there are often many unnamed groups (object IDs) that nobody uses; the
// pages show them in a collapsed section.
func adminSplitGroups(groups []store.Group, keep func(*store.Group) bool) (inUse, other []store.Group) {
	for i := range groups {
		g := &groups[i]
		if g.Role > store.RoleViewer || len(g.ServiceIDs) > 0 || (keep != nil && keep(g)) {
			inUse = append(inUse, *g)
		} else {
			other = append(other, *g)
		}
	}
	return inUse, other
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	s.adminRenderGroups(w, r, http.StatusOK, &adminGroupsData{})
}

func (s *Server) adminRenderGroups(w http.ResponseWriter, r *http.Request, status int, d *adminGroupsData) {
	groups, err := s.Store.ListGroups(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// Provisioned groups were assigned to the provisioning app on purpose:
	// they are listed even before they get a role or services.
	d.InUse, d.Other = adminSplitGroups(groups, func(g *store.Group) bool { return g.Provisioned })
	d.Names = s.adminGroupNamesView(r, groups)
	if d.Errors == nil {
		d.Errors = adminFormErrors{}
	}
	s.renderStatus(w, r, status, "admin_groups", "Groups", d)
}

func (s *Server) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := &adminGroupsData{ExternalID: formTrim(r, "external_id"), Name: formTrim(r, "name"), Errors: adminFormErrors{}}
	switch {
	case d.ExternalID == "":
		d.Errors.add("external_id", "Enter the group ID as it appears in the groups claim (Entra ID: object ID; Keycloak: group name).")
	case tooLong(d.ExternalID, 255):
		d.Errors.add("external_id", "The group ID is too long (at most 255 characters).")
	}
	if tooLong(d.Name, adminGroupNameMax) {
		d.Errors.add("name", adminGroupNameTooLong)
	}
	if len(d.Errors) > 0 {
		s.adminRenderGroups(w, r, http.StatusUnprocessableEntity, d)
		return
	}
	existing, err := s.adminGroupByExternalID(ctx, d.ExternalID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if existing != nil {
		s.redirectWithFlash(w, r, "/admin/groups/"+strconv.FormatInt(existing.ID, 10), "info",
			"The group “"+existing.DisplayName()+"” already exists.")
		return
	}
	g, err := s.Store.CreateGroup(ctx, d.ExternalID, d.Name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("group added", "group", g.ExternalID, "user", adminActor(r))
	kind, note := "success", ""
	if s.settings(r).GroupNamesActive() && store.IsGUID(g.ExternalID) {
		g, kind, note = s.adminLookUpNewGroup(ctx, g)
	}
	s.redirectWithFlash(w, r, "/admin/groups/"+strconv.FormatInt(g.ID, 10), kind,
		"Group “"+g.DisplayName()+"” added."+note+" Choose its role and the services its members are notified about.")
}

// adminLookUpNewGroup looks up the name of the group g, just added by its
// object ID. It returns the group (with its directory name, if the lookup
// worked), and the kind of the flash message and a note for it.
func (s *Server) adminLookUpNewGroup(ctx context.Context, g *store.Group) (_ *store.Group, kind, note string) {
	lctx, cancel := context.WithTimeout(ctx, adminLookUpGroupTime)
	defer cancel()
	updated, found, err := s.GroupNames.LookUpGroup(lctx, g)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		s.GroupNames.Wake()
		return g, "success", " Its name in Microsoft Entra ID is looked up in the background."
	case err != nil:
		return g, "warning", " Its name could not be looked up in Microsoft Entra ID: " + err.Error() + "."
	case !found:
		return updated, "warning", " Microsoft Entra ID has no group with this object ID – check the ID."
	}
	return updated, "success", ""
}

// handleGroupNamesLookup looks up the names of all groups in Microsoft
// Entra ID now ("Look up names now"), e.g. after granting the permission or
// renaming groups; the worker would only look them up within a day.
func (s *Server) handleGroupNamesLookup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), adminLookUpAllTime)
	defer cancel()
	res, err := s.GroupNames.LookUpAll(ctx)
	const back = "/admin/groups"
	switch {
	case errors.Is(err, groupnames.ErrOff):
		s.redirectWithFlash(w, r, back, "error", "Looking up group names in Microsoft Entra ID is turned off: turn it on in "+
			"Admin → Settings → Single sign-on.")
	case errors.Is(err, context.DeadlineExceeded):
		s.redirectWithFlash(w, r, back, "info", fmt.Sprintf("The names of %s were looked up in Microsoft Entra ID; the others "+
			"follow in the background.", plural(res.Groups, "group")))
	case err != nil:
		s.redirectWithFlash(w, r, back, "error", "The names of the groups could not be looked up in Microsoft Entra ID: "+err.Error())
	case res.Groups == 0:
		s.redirectWithFlash(w, r, back, "info", "No group to look up: names are looked up for the groups whose ID is a Microsoft "+
			"Entra ID object ID, except provisioned groups, which get their names from provisioning.")
	default:
		msg := fmt.Sprintf("The names of %s were looked up in Microsoft Entra ID.", plural(res.Groups, "group"))
		if missing := res.Groups - res.Found; missing == 1 {
			msg += " 1 group was not found there (deleted, or the ID is not one of a group)."
		} else if missing > 1 {
			msg += fmt.Sprintf(" %d groups were not found there (deleted, or the ID is not one of a group).", missing)
		}
		s.redirectWithFlash(w, r, back, "success", msg)
	}
}

// adminGroupByExternalID returns the group with the given external id, or nil.
func (s *Server) adminGroupByExternalID(ctx context.Context, externalID string) (*store.Group, error) {
	groups, err := s.Store.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	for i := range groups {
		if store.NormExternalID(groups[i].ExternalID) == store.NormExternalID(externalID) {
			return &groups[i], nil
		}
	}
	return nil, nil
}

// adminGroupForm is the data of the group edit page.
type adminGroupForm struct {
	Group    store.Group
	Role     string // viewer, editor or admin
	Members  []store.User
	Services []store.Service
	Errors   adminFormErrors
}

func (s *Server) adminRenderGroupForm(w http.ResponseWriter, r *http.Request, status int, f *adminGroupForm) {
	ctx := r.Context()
	var err error
	if f.Members, err = s.Store.GroupMembers(ctx, f.Group.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if f.Services, err = s.Store.ListServices(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	if f.Errors == nil {
		f.Errors = adminFormErrors{}
	}
	s.renderStatus(w, r, status, "admin_group_edit", f.Group.DisplayName(), f)
}

// adminLoadGroup loads the group of the {id} path value. It writes a 404 or
// 500 page and returns nil on failure.
func (s *Server) adminLoadGroup(w http.ResponseWriter, r *http.Request) *store.Group {
	id, ok := s.pathID(w, r)
	if !ok {
		return nil
	}
	g, err := s.Store.GetGroup(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return nil
	} else if err != nil {
		s.serverError(w, r, err)
		return nil
	}
	return g
}

func (s *Server) handleGroupEdit(w http.ResponseWriter, r *http.Request) {
	g := s.adminLoadGroup(w, r)
	if g == nil {
		return
	}
	role := g.Role.String()
	if role == "" {
		role = store.RoleViewer.String()
	}
	s.adminRenderGroupForm(w, r, http.StatusOK, &adminGroupForm{Group: *g, Role: role})
}

func (s *Server) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	g := s.adminLoadGroup(w, r)
	if g == nil {
		return
	}
	ctx := r.Context()
	f := &adminGroupForm{Group: *g, Role: r.FormValue("role"), Errors: adminFormErrors{}}
	f.Group.Name = formTrim(r, "name")
	f.Group.ServiceIDs = formInt64s(r, "services")
	role := store.ParseRole(f.Role)
	if role == store.RoleNone {
		f.Errors.add("role", "Choose a role.")
	} else if role != store.RoleAdmin {
		last, err := s.adminLastAdminGroup(r, g)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if last {
			f.Errors.add("role", adminLastAdminGroupMsg(s.settings(r).OIDCAppRoles))
		}
	}
	if tooLong(f.Group.Name, adminGroupNameMax) {
		f.Errors.add("name", adminGroupNameTooLong)
	}
	if len(f.Errors) > 0 {
		s.adminRenderGroupForm(w, r, http.StatusUnprocessableEntity, f)
		return
	}
	if !g.Provisioned && (f.Group.Name == g.ExternalID || f.Group.Name == g.DirectoryName) {
		// No name of its own: the group shows its directory name, which
		// follows renames in Microsoft Entra ID, else its ID.
		f.Group.Name = ""
	}
	err := s.Store.UpdateGroup(ctx, g.ID, f.Group.Name, role)
	if err == nil {
		err = s.Store.SetGroupServices(ctx, g.ID, f.Group.ServiceIDs)
	}
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("group updated", "group", g.ExternalID, "role", role.String(), "services", len(f.Group.ServiceIDs), "user", adminActor(r))
	s.redirectWithFlash(w, r, "/admin/groups", "success", "Group “"+f.Group.DisplayName()+"” saved.")
}

func (s *Server) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	g := s.adminLoadGroup(w, r)
	if g == nil {
		return
	}
	last, err := s.adminLastAdminGroup(r, g)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if last {
		s.redirectWithFlash(w, r, "/admin/groups/"+strconv.FormatInt(g.ID, 10), "error", adminLastAdminGroupMsg(s.settings(r).OIDCAppRoles))
		return
	}
	err = s.Store.DeleteGroup(r.Context(), g.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("group deleted", "group", g.ExternalID, "user", adminActor(r))
	s.redirectWithFlash(w, r, "/admin/groups", "success", "Group “"+g.DisplayName()+"” deleted.")
}

// adminLastAdminGroupMsg explains why a group keeps the Administrator role
// (appRoles: roles from app roles are on).
func adminLastAdminGroupMsg(appRoles bool) string {
	if appRoles {
		return "This is the only group with the Administrator role, nobody has signed in with the app role for administrators, " +
			"and the local administrator sign-in is turned off, so nobody could administer the status page any more. Give another " +
			"group the Administrator role or sign in with the app role for administrators first, or turn the local administrator " +
			"sign-in on again in Admin → Settings."
	}
	return "This is the only group with the Administrator role and the local administrator sign-in is turned off, so nobody " +
		"could administer the status page any more. Give another group the Administrator role first, or turn the local " +
		"administrator sign-in on again in Admin → Settings."
}

// adminLastAdminGroup reports whether g is the last way to sign in as an
// administrator while the sign-in with the local administrator account is
// off: the only group with the Administrator role, and nobody has the
// administrator app role (store.HasSSOAdmin). Then it can neither lose the
// role nor be deleted, like the sign-in cannot be turned off without such a
// group (store.CheckLocalLoginOff).
func (s *Server) adminLastAdminGroup(r *http.Request, g *store.Group) (bool, error) {
	st := s.settings(r)
	if g.Role != store.RoleAdmin || st.LocalLoginEnabled {
		return false, nil
	}
	ok, err := s.Store.HasSSOAdmin(r.Context(), st, g.ID)
	return !ok, err
}
