package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// manageMaintenanceData is the data of the maintenance form (new and edit).
type manageMaintenanceData struct {
	Maintenance *store.Maintenance // nil when scheduling a new window
	State       store.MaintenanceState
	Editable    bool   // false for completed and cancelled windows
	Window      string // formatted window of the stored maintenance
	Services    string // names of the stored services

	// Form values.
	Title       string
	Description string
	StartsAt    string // datetime-local values
	EndsAt      string
	Notify      bool
	Groups      []manageServiceGroup // service checkboxes
	Errors      []string
}

// CanEnd reports whether the window can be ended now.
func (d *manageMaintenanceData) CanEnd() bool { return d.State == store.MaintenanceInProgress }

// CanCancel reports whether the window can be cancelled: only before it
// has started (a window in progress is ended with "End now").
func (d *manageMaintenanceData) CanCancel() bool { return d.State == store.MaintenanceScheduled }

// PageTitle returns the title of the page.
func (d *manageMaintenanceData) PageTitle() string {
	switch {
	case d.Maintenance == nil:
		return "Schedule maintenance"
	case d.Editable:
		return "Edit maintenance"
	}
	return d.Maintenance.Title
}

// manageEditable reports whether a maintenance in state st may be changed.
func manageEditable(st store.MaintenanceState) bool {
	return st == store.MaintenanceScheduled || st == store.MaintenanceInProgress
}

// manageServiceChecks builds the service checkboxes with ids checked.
func manageServiceChecks(services []store.Service, ids []int64) []manageServiceGroup {
	return manageGroupServices(services, func(row *manageServiceRow) { row.Checked = slices.Contains(ids, row.ID) })
}

// newManageMaintenanceData returns the form data for an existing maintenance.
func (s *Server) newManageMaintenanceData(m *store.Maintenance, services []store.Service) *manageMaintenanceData {
	st := m.State(time.Now())
	return &manageMaintenanceData{
		Maintenance: m,
		State:       st,
		Editable:    manageEditable(st),
		Window:      s.manageWindow(m.StartsAt, m.EndsAt),
		Services:    manageServiceNames(m.Services),
		Title:       m.Title,
		Description: m.Description,
		StartsAt:    s.formatTime(m.StartsAt, InputTimeLayout),
		EndsAt:      s.formatTime(m.EndsAt, InputTimeLayout),
		Notify:      m.Notify,
		Groups:      manageServiceChecks(services, m.ServiceIDs()),
	}
}

// manageParseMaintenance reads the maintenance form into d (values and error
// messages) and returns the parsed window and services. old is the
// maintenance being edited (nil for a new one): times that were not changed
// in the form keep their exact stored value.
func (s *Server) manageParseMaintenance(r *http.Request, services []store.Service, old *store.Maintenance, d *manageMaintenanceData) (start, end time.Time, refs []store.ServiceRef) {
	d.Title = formTrim(r, "title")
	d.Description = formTrim(r, "description")
	d.StartsAt = formTrim(r, "starts_at")
	d.EndsAt = formTrim(r, "ends_at")
	d.Notify = formBool(r, "notify")
	ids := formInt64s(r, "services")
	d.Groups = manageServiceChecks(services, ids)

	if msg := manageCheckTitle(d.Title); msg != "" {
		d.Errors = append(d.Errors, msg)
	}
	if msg := manageCheckText(d.Description, "description"); msg != "" {
		d.Errors = append(d.Errors, msg)
	}
	start, startErr := s.manageParseTime(d.StartsAt, "start time")
	end, endErr := s.manageParseTime(d.EndsAt, "end time")
	if old != nil && startErr == "" && d.StartsAt == s.formatTime(old.StartsAt, InputTimeLayout) {
		start = old.StartsAt
	}
	if old != nil && endErr == "" && d.EndsAt == s.formatTime(old.EndsAt, InputTimeLayout) {
		end = old.EndsAt
	}
	for _, msg := range []string{startErr, endErr} {
		if msg != "" {
			d.Errors = append(d.Errors, msg)
		}
	}
	if startErr == "" && endErr == "" {
		switch {
		case !end.After(start):
			d.Errors = append(d.Errors, "The end time must be after the start time.")
		case old == nil && !end.After(time.Now()):
			d.Errors = append(d.Errors, "The end time is in the past. Please enter a window that has not ended yet.")
		}
	}
	for _, svc := range services {
		if slices.Contains(ids, svc.ID) {
			refs = append(refs, store.ServiceRef{ID: svc.ID, Name: svc.Name})
		}
	}
	// A service is required, except for a maintenance whose services were
	// all deleted (it can still be changed).
	if len(refs) == 0 && (old == nil || len(old.Services) > 0) {
		d.Errors = append(d.Errors, "Please select at least one affected service.")
	}
	return start, end, refs
}

// manageLoadMaintenance loads the maintenance of the {id} path value. It
// writes a 404 or 500 response and returns nil on failure.
func (s *Server) manageLoadMaintenance(w http.ResponseWriter, r *http.Request) *store.Maintenance {
	id, ok := s.pathID(w, r)
	if !ok {
		return nil
	}
	m, err := s.Store.GetMaintenance(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return nil
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil
	}
	return m
}

func manageMaintenanceURL(id int64) string { return fmt.Sprintf("/manage/maintenance/%d", id) }

// ------------------------------------------------------------------ handlers

func (s *Server) handleMaintenanceNew(w http.ResponseWriter, r *http.Request) {
	services, err := s.Store.ListServices(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// Suggest the next full hour (in the display time zone) for one hour.
	now := time.Now().In(s.settings(r).Location())
	start := time.Date(now.Year(), now.Month(), now.Day(), now.Hour()+1, 0, 0, 0, now.Location())
	d := &manageMaintenanceData{
		State:    store.MaintenanceScheduled,
		Editable: true,
		StartsAt: start.Format(InputTimeLayout),
		EndsAt:   start.Add(time.Hour).Format(InputTimeLayout),
		Notify:   true,
		Groups:   manageServiceChecks(services, nil),
	}
	s.render(w, r, "manage_maintenance_form", d.PageTitle(), d)
}

func (s *Server) handleMaintenanceCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.settings(r) // load the display time zone used by parseInputTime
	services, err := s.Store.ListServices(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := &manageMaintenanceData{State: store.MaintenanceScheduled, Editable: true}
	start, end, refs := s.manageParseMaintenance(r, services, nil, d)
	if len(d.Errors) > 0 {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "manage_maintenance_form", d.PageTitle(), d)
		return
	}
	m := &store.Maintenance{
		Title: d.Title, Description: d.Description, StartsAt: start, EndsAt: end,
		Notify: d.Notify, Services: refs, CreatedBy: manageUserID(r),
	}
	if err := s.Store.CreateMaintenance(ctx, m); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("maintenance scheduled", "id", m.ID, "title", m.Title, "user", m.CreatedBy)
	var note string
	var warn bool
	if m.Notify {
		note, warn = s.manageNotify(r, "maintenance scheduled", func(ctx context.Context) (int, error) {
			return s.Notifier.MaintenanceScheduled(ctx, m.ID)
		})
	}
	s.manageDone(w, r, "/manage", "The maintenance has been scheduled.", note, warn)
}

func (s *Server) handleMaintenanceEdit(w http.ResponseWriter, r *http.Request) {
	s.settings(r) // display time zone for the form values
	m := s.manageLoadMaintenance(w, r)
	if m == nil {
		return
	}
	services, err := s.Store.ListServices(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := s.newManageMaintenanceData(m, services)
	s.render(w, r, "manage_maintenance_form", d.PageTitle(), d)
}

func (s *Server) handleMaintenanceUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.settings(r)
	old := s.manageLoadMaintenance(w, r)
	if old == nil {
		return
	}
	state := old.State(time.Now())
	if !manageEditable(state) {
		s.redirectWithFlash(w, r, manageMaintenanceURL(old.ID), "error",
			fmt.Sprintf("This maintenance is %s and can no longer be changed.", manageStateWord(state)))
		return
	}
	services, err := s.Store.ListServices(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := &manageMaintenanceData{Maintenance: old, State: state, Editable: true}
	start, end, refs := s.manageParseMaintenance(r, services, old, d)
	if len(d.Errors) > 0 {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "manage_maintenance_form", d.PageTitle(), d)
		return
	}

	m := *old
	m.Title, m.Description, m.StartsAt, m.EndsAt, m.Notify, m.Services = d.Title, d.Description, start, end, d.Notify, refs
	err = s.Store.UpdateMaintenance(ctx, &m)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("maintenance updated", "id", m.ID, "title", m.Title, "user", manageUserID(r))
	changed := m.Title != old.Title || m.Description != old.Description ||
		!m.StartsAt.Equal(old.StartsAt) || !m.EndsAt.Equal(old.EndsAt) ||
		!manageSameIDs(m.ServiceIDs(), old.ServiceIDs())
	var note string
	var warn bool
	if m.Notify && changed {
		previous := old.ServiceIDs()
		note, warn = s.manageNotify(r, "maintenance updated", func(ctx context.Context) (int, error) {
			return s.Notifier.MaintenanceUpdated(ctx, m.ID, previous)
		})
	}
	s.manageDone(w, r, "/manage", "The maintenance has been updated.", note, warn)
}

func (s *Server) handleMaintenanceEndNow(w http.ResponseWriter, r *http.Request) {
	m := s.manageLoadMaintenance(w, r)
	if m == nil {
		return
	}
	now := time.Now().Truncate(time.Second)
	if st := m.State(now); st != store.MaintenanceInProgress {
		s.redirectWithFlash(w, r, manageMaintenanceURL(m.ID), "error",
			fmt.Sprintf("Only a maintenance in progress can be ended; this one is %s.", manageStateWord(st)))
		return
	}
	m.EndsAt = now
	if !m.EndsAt.After(m.StartsAt) {
		m.EndsAt = m.StartsAt.Add(time.Second)
	}
	err := s.Store.UpdateMaintenance(r.Context(), m)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("maintenance ended", "id", m.ID, "title", m.Title, "user", manageUserID(r))
	s.redirectWithFlash(w, r, "/manage", "success", "The maintenance has been ended.")
}

func (s *Server) handleMaintenanceCancel(w http.ResponseWriter, r *http.Request) {
	m := s.manageLoadMaintenance(w, r)
	if m == nil {
		return
	}
	switch st := m.State(time.Now()); st {
	case store.MaintenanceScheduled:
	case store.MaintenanceInProgress:
		s.redirectWithFlash(w, r, manageMaintenanceURL(m.ID), "error",
			"This maintenance has already started and can no longer be cancelled. Use “End now” if the work is finished.")
		return
	default:
		s.redirectWithFlash(w, r, manageMaintenanceURL(m.ID), "error",
			fmt.Sprintf("This maintenance is %s and can no longer be cancelled.", manageStateWord(st)))
		return
	}
	err := s.Store.CancelMaintenance(r.Context(), m.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("maintenance cancelled", "id", m.ID, "title", m.Title, "user", manageUserID(r))
	var note string
	var warn bool
	if m.Notify {
		note, warn = s.manageNotify(r, "maintenance cancelled", func(ctx context.Context) (int, error) {
			return s.Notifier.MaintenanceCancelled(ctx, m.ID)
		})
	}
	s.manageDone(w, r, "/manage", "The maintenance has been cancelled.", note, warn)
}

func (s *Server) handleMaintenanceDelete(w http.ResponseWriter, r *http.Request) {
	m := s.manageLoadMaintenance(w, r)
	if m == nil {
		return
	}
	err := s.Store.DeleteMaintenance(r.Context(), m.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("maintenance deleted", "id", m.ID, "title", m.Title, "user", manageUserID(r))
	s.redirectWithFlash(w, r, "/manage", "success", fmt.Sprintf("The maintenance “%s” has been deleted.", m.Title))
}

// manageStateWord returns the maintenance state in lower case for messages.
func manageStateWord(st store.MaintenanceState) string {
	switch st {
	case store.MaintenanceInProgress:
		return "in progress"
	case store.MaintenanceScheduled:
		return "scheduled"
	case store.MaintenanceCompleted:
		return "completed"
	case store.MaintenanceCancelled:
		return "cancelled"
	}
	return string(st)
}
