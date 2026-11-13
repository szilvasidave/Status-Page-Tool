package web

// Editor area (/manage): the overview page and helpers shared by the
// incident (manage_incidents.go) and maintenance (manage_maintenance.go)
// handlers.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// manageResolvedCount is the number of recently resolved incidents shown on
// the overview page; manageResolvedWindow limits how far back they are
// looked up.
const (
	manageResolvedCount  = 10
	manageResolvedWindow = 365 * 24 * time.Hour
)

// ------------------------------------------------------------------ overview

type manageDashboardData struct {
	Open        []manageIncidentRow
	Maintenance []manageMaintenanceRow
	Resolved    []manageIncidentRow
}

// manageIncidentRow is an incident as listed on the overview page.
type manageIncidentRow struct {
	ID         int64
	Title      string
	Status     store.IncidentStatus
	Impact     store.Status
	Services   string // affected service names, comma separated
	StartedAt  time.Time
	LastUpdate time.Time
	ResolvedAt *time.Time
}

func newManageIncidentRow(inc *store.Incident) manageIncidentRow {
	names := make([]string, len(inc.Services))
	for i, svc := range inc.Services {
		names[i] = svc.ServiceName
	}
	row := manageIncidentRow{
		ID: inc.ID, Title: inc.Title, Status: inc.Status, Impact: inc.Impact(),
		Services: strings.Join(names, ", "), StartedAt: inc.StartedAt,
		LastUpdate: inc.UpdatedAt, ResolvedAt: inc.ResolvedAt,
	}
	if len(inc.Updates) > 0 {
		row.LastUpdate = inc.Updates[0].CreatedAt
	}
	return row
}

// manageMaintenanceRow is a maintenance window as listed on the overview page.
type manageMaintenanceRow struct {
	ID       int64
	Title    string
	State    store.MaintenanceState
	StartsAt time.Time
	EndsAt   time.Time
	Window   string // formatted start and end
	Services string
}

func (s *Server) newManageMaintenanceRow(m *store.Maintenance, now time.Time) manageMaintenanceRow {
	return manageMaintenanceRow{
		ID: m.ID, Title: m.Title, State: m.State(now), StartsAt: m.StartsAt, EndsAt: m.EndsAt,
		Window: s.manageWindow(m.StartsAt, m.EndsAt), Services: manageServiceNames(m.Services),
	}
}

// manageServiceNames returns the names of services, comma separated.
func manageServiceNames(services []store.ServiceRef) string {
	names := make([]string, len(services))
	for i, svc := range services {
		names[i] = svc.Name
	}
	return strings.Join(names, ", ")
}

// manageWindow formats a maintenance window in the display time zone, e.g.
// "12 Nov 2026 14:00 – 16:00 CET" or, across days,
// "12 Nov 2026 22:00 CET – 13 Nov 2026 02:00 CET".
func (s *Server) manageWindow(start, end time.Time) string {
	a, b := start.In(s.location()), end.In(s.location())
	if a.YearDay() == b.YearDay() && a.Year() == b.Year() && a.Format("MST") == b.Format("MST") {
		return a.Format("2 Jan 2006 15:04") + " – " + b.Format("15:04 MST")
	}
	return a.Format("2 Jan 2006 15:04 MST") + " – " + b.Format("2 Jan 2006 15:04 MST")
}

func (s *Server) handleManageDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	s.settings(r) // display time zone for manageWindow
	open, err := s.Store.ListOpenIncidents(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	maint, err := s.Store.ListCurrentMaintenances(ctx, now)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	recent, err := s.Store.ListIncidentsBetween(ctx, now.Add(-manageResolvedWindow), now.Add(time.Minute))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	d := &manageDashboardData{}
	for i := range open {
		d.Open = append(d.Open, newManageIncidentRow(&open[i]))
	}
	for i := range maint {
		d.Maintenance = append(d.Maintenance, s.newManageMaintenanceRow(&maint[i], now))
	}
	for i := range recent {
		if recent[i].Resolved() && recent[i].ResolvedAt != nil {
			d.Resolved = append(d.Resolved, newManageIncidentRow(&recent[i]))
		}
	}
	slices.SortStableFunc(d.Resolved, func(a, b manageIncidentRow) int { return b.ResolvedAt.Compare(*a.ResolvedAt) })
	if len(d.Resolved) > manageResolvedCount {
		d.Resolved = d.Resolved[:manageResolvedCount]
	}
	s.render(w, r, "manage_dashboard", "Manage", d)
}

// ------------------------------------------------------------------ services in forms

// manageServiceGroup is one category of services in the impact table of the
// incident forms and in the service checkboxes of the maintenance form.
type manageServiceGroup struct {
	Category string
	Services []manageServiceRow
}

type manageServiceRow struct {
	ID          int64
	Name        string
	Description string
	Impact      store.Status // incident forms: selected impact, "" = not affected
	Checked     bool         // maintenance form: service selected
}

// manageGroupServices groups services (ordered by category, as returned by
// the store) by category. fill sets the form state of each row.
func manageGroupServices(services []store.Service, fill func(*manageServiceRow)) []manageServiceGroup {
	var groups []manageServiceGroup
	for _, svc := range services {
		if len(groups) == 0 || !strings.EqualFold(groups[len(groups)-1].Category, svc.Category) {
			groups = append(groups, manageServiceGroup{Category: svc.Category})
		}
		row := manageServiceRow{ID: svc.ID, Name: svc.Name, Description: svc.Description}
		if fill != nil {
			fill(&row)
		}
		g := &groups[len(groups)-1]
		g.Services = append(g.Services, row)
	}
	return groups
}

// ------------------------------------------------------------------ validation helpers

// manageCheckTitle validates an incident or maintenance title and returns an
// error message ("" if valid).
func manageCheckTitle(title string) string {
	switch {
	case title == "":
		return "Please enter a title."
	case tooLong(title, maxTitleLen):
		return fmt.Sprintf("The title is too long (at most %d characters).", maxTitleLen)
	}
	return ""
}

// manageCheckText returns an error message if an incident message or a
// maintenance description (what) is too long, "" otherwise. Browsers count a
// line break in a textarea as one character but send it as CRLF.
func manageCheckText(text, what string) string {
	if tooLong(strings.ReplaceAll(text, "\r\n", "\n"), maxTextLen) {
		return fmt.Sprintf("The %s is too long (at most %d characters).", what, maxTextLen)
	}
	return ""
}

// manageParseTime parses a datetime-local field. what names the field in
// error messages ("start time"). Returns the zero time and an error message
// if the value is missing or invalid.
func (s *Server) manageParseTime(value, what string) (time.Time, string) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, "Please enter the " + what + "."
	}
	t, err := s.parseInputTime(value)
	if err != nil {
		return time.Time{}, "The " + what + " is not a valid date and time."
	}
	return t, ""
}

// ------------------------------------------------------------------ notifications

// manageNotify queues notification e-mails with fn (a Notifier method) and
// returns a sentence for the flash message. warn is true if queueing failed;
// the action itself has succeeded at that point, so the error is only
// logged and reported to the user.
func (s *Server) manageNotify(r *http.Request, event string, fn func(context.Context) (int, error)) (note string, warn bool) {
	if s.Notifier == nil {
		return "", false
	}
	// The change is already saved: queue the e-mails even if the browser
	// goes away in the meantime.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	n, err := fn(ctx)
	switch {
	case err != nil:
		s.Log.Error("queue notification e-mails", "event", event, "err", err)
		return "However, the notification e-mails could not be queued; please check the server log.", true
	case n == 1:
		return "1 notification e-mail queued.", false
	case n > 1:
		return fmt.Sprintf("%d notification e-mails queued.", n), false
	case !s.settings(r).SMTPConfigured():
		return "E-mail is not configured, so no notifications were sent.", false
	}
	return "No notification e-mails were sent: no member of the groups assigned to the affected services has an e-mail address.", false
}

// manageDone redirects to target with a flash message made of msg and the
// optional notification note. The change itself was saved, so a failure to
// queue the e-mails (warn) is a warning, not an error.
func (s *Server) manageDone(w http.ResponseWriter, r *http.Request, target, msg, note string, warn bool) {
	kind := "success"
	if warn {
		kind = "warning"
	}
	if note != "" {
		msg += " " + note
	}
	s.redirectWithFlash(w, r, target, kind, msg)
}

// manageUserID returns the id of the signed-in user (0 if unknown).
func manageUserID(r *http.Request) int64 {
	if id := auth.FromContext(r.Context()); id != nil && id.User != nil {
		return id.User.ID
	}
	return 0
}

// manageSameIDs reports whether a and b contain the same ids (any order).
func manageSameIDs(a, b []int64) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}
