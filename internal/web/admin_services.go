package web

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	Service   store.Service
	SortOrder string // as entered
	IsNew     bool
	Groups    []store.Group  // groups with a role or services, and the groups of this service
	Other     []store.Group  // all other groups (shown collapsed)
	SameName  map[int64]bool // groups whose name another group has too (adminSameNames)

	// The category is chosen in a select: "No category" (""), one of
	// Categories, or "New category…" (adminNewCategory) with its name
	// entered in the field category_new.
	Categories  []string // existing categories, sorted
	Category    string   // the selected option
	NewCategory string   // name of a new category as entered
	// MonitorNames are the names in monitoring tools as entered, one per
	// line (Service.MonitorNames).
	MonitorNames string
	Errors       adminFormErrors
}

// adminNewCategory is the value of the option "New category…" of the
// category select.
const adminNewCategory = "(new)"

// NewCategoryValue returns adminNewCategory (for the template).
func (f *adminServiceForm) NewCategoryValue() string { return adminNewCategory }

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
	// The categories of the services, once each regardless of case (like
	// the headings of the status page). The service's own category is
	// always offered, e.g. one that was entered when the form had errors.
	f.Categories = nil
	seen := map[string]string{}
	for _, svc := range services {
		if c := svc.Category; c != "" && seen[strings.ToLower(c)] == "" {
			seen[strings.ToLower(c)] = c
			f.Categories = append(f.Categories, c)
		}
	}
	if f.Category != adminNewCategory {
		f.Category = f.Service.Category
		if c := seen[strings.ToLower(f.Category)]; c != "" {
			f.Category = c
		} else if f.Category != "" {
			f.Categories = append(f.Categories, f.Category)
		}
	}
	slices.SortStableFunc(f.Categories, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	// Like the Groups page, provisioned groups are listed even without a
	// role or services: they were assigned to the provisioning app on purpose.
	f.Groups, f.Other = adminSplitGroups(groups, func(g *store.Group) bool {
		return g.Provisioned || slices.Contains(f.Service.GroupIDs, g.ID)
	})
	f.SameName = adminSameNames(groups)
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
	s.adminRenderServiceForm(w, r, http.StatusOK, &adminServiceForm{Service: *svc, SortOrder: strconv.Itoa(svc.SortOrder),
		MonitorNames: strings.Join(svc.MonitorNames, "\n")})
}

// adminParseServiceForm reads the service form into f and validates it.
func adminParseServiceForm(r *http.Request, f *adminServiceForm) {
	f.Errors = adminFormErrors{}
	f.Service.Name = formTrim(r, "name")
	f.Service.Description = formTrim(r, "description")
	f.Category = formTrim(r, "category")
	f.Service.Category = f.Category
	categoryField := "category"
	if f.Category == adminNewCategory {
		f.NewCategory = formTrim(r, "category_new")
		f.Service.Category, categoryField = f.NewCategory, "category_new"
		if f.NewCategory == "" {
			f.Errors.add("category_new", "Enter the name of the new category, or choose an existing one.")
		}
	}
	f.Service.GroupIDs = formInt64s(r, "groups")
	f.MonitorNames = formTrim(r, "monitor_names")
	f.Service.MonitorNames = store.NormalizeMonitorNames(strings.Split(f.MonitorNames, "\n"))
	switch {
	case len(f.Service.MonitorNames) > store.MaxMonitorNames:
		f.Errors.add("monitor_names", fmt.Sprintf("Enter at most %d names.", store.MaxMonitorNames))
	case slices.ContainsFunc(f.Service.MonitorNames, func(n string) bool { return tooLong(n, store.MaxMonitorNameLength) }):
		f.Errors.add("monitor_names", fmt.Sprintf("A name is too long (at most %d characters each).", store.MaxMonitorNameLength))
	}
	f.SortOrder = formTrim(r, "sort_order")
	switch {
	case f.Service.Name == "":
		f.Errors.add("name", "Enter a name for the service.")
	case tooLong(f.Service.Name, 100):
		f.Errors.add("name", "The name is too long (at most 100 characters).")
	}
	if tooLong(f.Service.Description, 300) {
		f.Errors.add("description", "The description is too long (at most 300 characters).")
	}
	if tooLong(f.Service.Category, 100) {
		f.Errors.add(categoryField, "The category is too long (at most 100 characters).")
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
	var inUse *store.ServiceInUseError
	if errors.As(err, &inUse) {
		s.redirectWithFlash(w, r, "/admin/services/"+strconv.FormatInt(id, 10), "error", serviceInUseText(svc.Name, inUse))
		return
	} else if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("service deleted", "service", svc.Name, "user", adminActor(r))
	s.redirectWithFlash(w, r, "/admin/services", "success", "Service “"+svc.Name+"” deleted.")
}

// serviceInUseText says why the service cannot be deleted now (the store
// checks it in the deletion's transaction, so that no monitoring tool opens
// an incident of the service in between).
func serviceInUseText(name string, e *store.ServiceInUseError) string {
	if e.Incident != "" {
		return fmt.Sprintf("The service “%s” cannot be deleted while the open incident “%s” affects it. Resolve the incident first.", name, e.Incident)
	}
	return fmt.Sprintf("The service “%s” cannot be deleted while the maintenance “%s” affects it. Wait until it has ended, or remove the service from it first.", name, e.Maintenance)
}
