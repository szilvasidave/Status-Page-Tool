package web

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminServiceRow is one row of the services list.
type adminServiceRow struct {
	store.Service
	Status     store.Status
	GroupNames []string
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	services, err := s.Store.ListServices(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	groups, err := s.Store.ListGroups(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	open, err := s.Store.ListOpenIncidents(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	now := time.Now()
	maint, err := s.Store.ListCurrentMaintenances(ctx, now)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	statuses := store.ServiceStatuses(services, open, maint, now)
	names := make(map[int64]string, len(groups))
	for _, g := range groups {
		names[g.ID] = g.DisplayName()
	}
	rows := make([]adminServiceRow, len(services))
	for i, svc := range services {
		rows[i] = adminServiceRow{Service: svc, Status: statuses[svc.ID]}
		for _, gid := range svc.GroupIDs {
			if n, ok := names[gid]; ok {
				rows[i].GroupNames = append(rows[i].GroupNames, n)
			}
		}
		sort.Slice(rows[i].GroupNames, func(a, b int) bool {
			return strings.ToLower(rows[i].GroupNames[a]) < strings.ToLower(rows[i].GroupNames[b])
		})
	}
	s.render(w, r, "admin_services", "Services", rows)
}

// adminServiceForm is the data of the new/edit service form.
type adminServiceForm struct {
	Service    store.Service
	SortOrder  string // as entered
	IsNew      bool
	Groups     []store.Group
	Categories []string
	Errors     adminFormErrors
}

func (s *Server) adminRenderServiceForm(w http.ResponseWriter, r *http.Request, status int, f *adminServiceForm) {
	ctx := r.Context()
	groups, err := s.Store.ListGroups(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	services, err := s.Store.ListServices(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	seen := map[string]bool{}
	for _, svc := range services {
		if c := svc.Category; c != "" && !seen[strings.ToLower(c)] {
			seen[strings.ToLower(c)] = true
			f.Categories = append(f.Categories, c)
		}
	}
	f.Groups = groups
	if f.Errors == nil {
		f.Errors = adminFormErrors{}
	}
	title := "Edit service"
	if f.IsNew {
		title = "New service"
	}
	s.renderStatus(w, r, status, "admin_service_form", title, f)
}

func (s *Server) handleServiceNew(w http.ResponseWriter, r *http.Request) {
	s.adminRenderServiceForm(w, r, http.StatusOK, &adminServiceForm{IsNew: true, SortOrder: "0"})
}

func (s *Server) handleServiceEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	svc, err := s.Store.GetService(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.adminRenderServiceForm(w, r, http.StatusOK, &adminServiceForm{Service: *svc, SortOrder: strconv.Itoa(svc.SortOrder)})
}

// adminParseServiceForm reads the service form into f and validates it.
func adminParseServiceForm(r *http.Request, f *adminServiceForm) {
	f.Errors = adminFormErrors{}
	f.Service.Name = formTrim(r, "name")
	f.Service.Description = formTrim(r, "description")
	f.Service.Category = formTrim(r, "category")
	f.Service.GroupIDs = formInt64s(r, "groups")
	f.SortOrder = formTrim(r, "sort_order")
	switch {
	case f.Service.Name == "":
		f.Errors.add("name", "Enter a name for the service.")
	case len(f.Service.Name) > 100:
		f.Errors.add("name", "The name is too long (at most 100 characters).")
	}
	if len(f.Service.Category) > 100 {
		f.Errors.add("category", "The category is too long (at most 100 characters).")
	}
	f.Service.SortOrder = 0
	if f.SortOrder != "" {
		n, err := strconv.Atoi(f.SortOrder)
		if err != nil || n < -100000 || n > 100000 {
			f.Errors.add("sort_order", "Enter a whole number, e.g. 10. Services with lower numbers are shown first.")
		}
		f.Service.SortOrder = n
	}
}

func (s *Server) handleServiceCreate(w http.ResponseWriter, r *http.Request) {
	f := &adminServiceForm{IsNew: true}
	adminParseServiceForm(r, f)
	if len(f.Errors) > 0 {
		s.adminRenderServiceForm(w, r, http.StatusUnprocessableEntity, f)
		return
	}
	if err := s.Store.CreateService(r.Context(), &f.Service); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("service created", "service", f.Service.Name, "user", adminActor(r))
	s.redirectWithFlash(w, r, "/admin/services", "success", "Service “"+f.Service.Name+"” created.")
}

func (s *Server) handleServiceUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	f := &adminServiceForm{Service: store.Service{ID: id}}
	adminParseServiceForm(r, f)
	if len(f.Errors) > 0 {
		s.adminRenderServiceForm(w, r, http.StatusUnprocessableEntity, f)
		return
	}
	err := s.Store.UpdateService(r.Context(), &f.Service)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("service updated", "service", f.Service.Name, "user", adminActor(r))
	s.redirectWithFlash(w, r, "/admin/services", "success", "Service “"+f.Service.Name+"” saved.")
}

func (s *Server) handleServiceDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	svc, err := s.Store.GetService(ctx, id)
	if err == nil {
		err = s.Store.DeleteService(ctx, id)
	}
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("service deleted", "service", svc.Name, "user", adminActor(r))
	s.redirectWithFlash(w, r, "/admin/services", "success", "Service “"+svc.Name+"” deleted.")
}
