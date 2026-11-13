package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// manageIncidentForm holds the values of the "report incident" and the
// "post an update" forms.
type manageIncidentForm struct {
	Title     string // new incident only
	StartedAt string // new incident only, datetime-local value
	Status    store.IncidentStatus
	Message   string
	Notify    bool
	Groups    []manageServiceGroup // impact table
	Errors    []string
}

// manageIncidentData is the data of the incident management page.
type manageIncidentData struct {
	Incident    *store.Incident
	Update      manageIncidentForm
	RenameTitle string
	RenameError string
}

// newManageIncidentData returns the page data with the update form prefilled
// with the current status and impacts.
func newManageIncidentData(inc *store.Incident, services []store.Service) *manageIncidentData {
	current := make(map[int64]store.Status, len(inc.Services))
	for _, si := range inc.Services {
		current[si.ServiceID] = si.Impact
	}
	return &manageIncidentData{
		Incident:    inc,
		Update:      manageIncidentForm{Status: inc.Status, Notify: true, Groups: manageImpactGroups(services, current)},
		RenameTitle: inc.Title,
	}
}

// manageImpactGroups builds the impact table with the given impacts selected.
func manageImpactGroups(services []store.Service, impacts map[int64]store.Status) []manageServiceGroup {
	return manageGroupServices(services, func(row *manageServiceRow) { row.Impact = impacts[row.ID] })
}

// manageParseImpacts reads the impact_<serviceID> fields of all services.
// It returns the affected services, the selected impacts (for re-rendering
// the form) and error messages. required is false for an update of an
// incident that has no services left (they were deleted), so that it can
// still be resolved.
func manageParseImpacts(r *http.Request, services []store.Service, required bool) ([]store.ServiceImpact, map[int64]store.Status, []string) {
	var (
		impacts  []store.ServiceImpact
		selected = map[int64]store.Status{}
		errs     []string
	)
	for _, svc := range services {
		v := store.Status(formTrim(r, "impact_"+strconv.FormatInt(svc.ID, 10)))
		switch {
		case v == "":
			continue
		case !store.ValidImpact(v):
			errs = append(errs, fmt.Sprintf("Please choose a valid impact for %s.", svc.Name))
			continue
		}
		selected[svc.ID] = v
		impacts = append(impacts, store.ServiceImpact{ServiceID: svc.ID, Impact: v})
	}
	if required && len(impacts) == 0 && len(errs) == 0 {
		errs = append(errs, "Please select at least one affected service.")
	}
	return impacts, selected, errs
}

// manageParseStatus validates the status field. allowResolved is false for
// new incidents.
func manageParseStatus(r *http.Request, allowResolved bool) (store.IncidentStatus, string) {
	st := store.IncidentStatus(formTrim(r, "status"))
	if !store.ValidIncidentStatus(st) || (!allowResolved && st == store.IncidentResolved) {
		return st, "Please choose a valid status."
	}
	return st, ""
}

// ------------------------------------------------------------------ new incident

func (s *Server) handleIncidentNew(w http.ResponseWriter, r *http.Request) {
	services, err := s.Store.ListServices(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	f := &manageIncidentForm{
		Status: store.IncidentInvestigating,
		Notify: true,
		Groups: manageImpactGroups(services, nil),
	}
	s.render(w, r, "manage_incident_new", "Report incident", f)
}

func (s *Server) handleIncidentCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.settings(r) // load the display time zone used by parseInputTime
	services, err := s.Store.ListServices(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	f := &manageIncidentForm{
		Title:     formTrim(r, "title"),
		StartedAt: formTrim(r, "started_at"),
		Message:   formTrim(r, "message"),
		Notify:    formBool(r, "notify"),
	}
	if msg := manageCheckTitle(f.Title); msg != "" {
		f.Errors = append(f.Errors, msg)
	}
	var msg string
	if f.Status, msg = manageParseStatus(r, false); msg != "" {
		f.Errors = append(f.Errors, msg)
	}
	impacts, selected, errs := manageParseImpacts(r, services, true)
	f.Errors = append(f.Errors, errs...)
	if f.Message == "" {
		f.Errors = append(f.Errors, "Please enter a message that describes the problem.")
	} else if msg := manageCheckText(f.Message, "message"); msg != "" {
		f.Errors = append(f.Errors, msg)
	}
	var started time.Time
	if f.StartedAt != "" {
		if started, msg = s.manageParseTime(f.StartedAt, "start time"); msg != "" {
			f.Errors = append(f.Errors, msg)
		} else if started.After(time.Now()) {
			f.Errors = append(f.Errors, "The start time must not be in the future.")
		}
	}
	if len(f.Errors) > 0 {
		f.Groups = manageImpactGroups(services, selected)
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "manage_incident_new", "Report incident", f)
		return
	}

	inc := &store.Incident{Title: f.Title, Status: f.Status, StartedAt: started, Services: impacts, CreatedBy: manageUserID(r)}
	first := &store.IncidentUpdate{Status: f.Status, Message: f.Message, CreatedBy: inc.CreatedBy}
	if err := s.Store.CreateIncident(ctx, inc, first); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("incident created", "id", inc.ID, "title", inc.Title, "user", inc.CreatedBy)
	var note string
	var warn bool
	if f.Notify {
		note, warn = s.manageNotify(r, "incident created", func(ctx context.Context) (int, error) {
			return s.Notifier.IncidentCreated(ctx, inc.ID)
		})
	}
	s.manageDone(w, r, fmt.Sprintf("/manage/incidents/%d", inc.ID), "The incident has been reported.", note, warn)
}

// ------------------------------------------------------------------ manage incident

// manageLoadIncident loads the incident of the {id} path value. It writes a
// 404 or 500 response and returns nil on failure.
func (s *Server) manageLoadIncident(w http.ResponseWriter, r *http.Request) *store.Incident {
	id, ok := s.pathID(w, r)
	if !ok {
		return nil
	}
	inc, err := s.Store.GetIncident(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return nil
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil
	}
	return inc
}

func (s *Server) handleIncidentManage(w http.ResponseWriter, r *http.Request) {
	inc := s.manageLoadIncident(w, r)
	if inc == nil {
		return
	}
	services, err := s.Store.ListServices(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "manage_incident", inc.Title, newManageIncidentData(inc, services))
}

func (s *Server) handleIncidentAddUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	inc := s.manageLoadIncident(w, r)
	if inc == nil {
		return
	}
	services, err := s.Store.ListServices(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	f := manageIncidentForm{Message: formTrim(r, "message"), Notify: formBool(r, "notify")}
	var msg string
	if f.Status, msg = manageParseStatus(r, true); msg != "" {
		f.Errors = append(f.Errors, msg)
	}
	if f.Message == "" {
		f.Errors = append(f.Errors, "Please enter a message for the update.")
	} else if msg := manageCheckText(f.Message, "message"); msg != "" {
		f.Errors = append(f.Errors, msg)
	}
	impacts, selected, errs := manageParseImpacts(r, services, len(inc.Services) > 0)
	f.Errors = append(f.Errors, errs...)
	if len(f.Errors) > 0 {
		f.Groups = manageImpactGroups(services, selected)
		d := newManageIncidentData(inc, services)
		d.Update = f
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "manage_incident", inc.Title, d)
		return
	}

	previous := inc.ServiceIDs()
	upd := &store.IncidentUpdate{Status: f.Status, Message: f.Message, CreatedBy: manageUserID(r)}
	err = s.Store.AddIncidentUpdate(ctx, inc.ID, upd, impacts)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("incident updated", "id", inc.ID, "status", f.Status, "user", upd.CreatedBy)
	done := "The update has been posted."
	switch {
	case f.Status == store.IncidentResolved && !inc.Resolved():
		done = "The incident has been resolved."
	case f.Status != store.IncidentResolved && inc.Resolved():
		done = "The incident has been reopened."
	}
	var note string
	var warn bool
	if f.Notify {
		note, warn = s.manageNotify(r, "incident updated", func(ctx context.Context) (int, error) {
			return s.Notifier.IncidentUpdated(ctx, inc.ID, previous)
		})
	}
	s.manageDone(w, r, fmt.Sprintf("/manage/incidents/%d", inc.ID), done, note, warn)
}

func (s *Server) handleIncidentTitle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	inc := s.manageLoadIncident(w, r)
	if inc == nil {
		return
	}
	title := formTrim(r, "title")
	if msg := manageCheckTitle(title); msg != "" {
		services, err := s.Store.ListServices(ctx)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		d := newManageIncidentData(inc, services)
		d.RenameTitle, d.RenameError = title, msg
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "manage_incident", inc.Title, d)
		return
	}
	err := s.Store.UpdateIncidentTitle(ctx, inc.ID, title)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectWithFlash(w, r, fmt.Sprintf("/manage/incidents/%d", inc.ID), "success", "The title has been changed.")
}

func (s *Server) handleIncidentDelete(w http.ResponseWriter, r *http.Request) {
	inc := s.manageLoadIncident(w, r)
	if inc == nil {
		return
	}
	err := s.Store.DeleteIncident(r.Context(), inc.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("incident deleted", "id", inc.ID, "title", inc.Title, "user", manageUserID(r))
	s.redirectWithFlash(w, r, "/manage", "success", fmt.Sprintf("The incident “%s” has been deleted.", inc.Title))
}
