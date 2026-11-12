package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

const (
	// publicHistoryDays is the number of days of the service history bars and
	// the length of one page of the history page. The legend in status.html
	// and the responsive rules for .history-bar in style.css assume 90.
	publicHistoryDays = 90
	// publicRecentDays is the period of the "Recent incidents" list on the status page.
	publicRecentDays = 7
	// publicDateLayout is the layout of the ?before= parameter of /history.
	publicDateLayout = "2006-01-02"
)

// ------------------------------------------------------------------ current status

// publicCurrentStatus is the live status shared by the status page and the JSON API.
type publicCurrentStatus struct {
	Services     []store.Service
	Statuses     map[int64]store.Status
	Overall      store.Status
	Incidents    []store.Incident    // open incidents, newest first
	Maintenances []store.Maintenance // in progress and upcoming, by start time
}

func (s *Server) loadPublicStatus(ctx context.Context, now time.Time) (*publicCurrentStatus, error) {
	services, err := s.Store.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	incidents, err := s.Store.ListOpenIncidents(ctx)
	if err != nil {
		return nil, err
	}
	maintenances, err := s.Store.ListCurrentMaintenances(ctx, now)
	if err != nil {
		return nil, err
	}
	statuses := store.ServiceStatuses(services, incidents, maintenances, now)
	return &publicCurrentStatus{
		Services:     services,
		Statuses:     statuses,
		Overall:      store.OverallStatus(statuses),
		Incidents:    incidents,
		Maintenances: maintenances,
	}, nil
}

// ------------------------------------------------------------------ status page

type publicStatusData struct {
	Overall      store.Status
	Updated      time.Time
	Incidents    []store.Incident
	Maintenances []store.Maintenance
	Groups       []publicServiceGroup
	Recent       []store.Incident // incidents of the last publicRecentDays days
	HasServices  bool
}

// publicServiceGroup is a category of services on the status page. The services
// without a category come first, in a group with an empty Category.
type publicServiceGroup struct {
	Category string
	Services []publicServiceRow
}

type publicServiceRow struct {
	Service store.Service
	Status  store.Status
	Days    []publicHistoryDay
	Summary string // text alternative of the history bar
}

// publicHistoryDay is one bar of a service's history.
type publicHistoryDay struct {
	Class string // "status-<status>" or "nodata"
	Title string // tooltip, e.g. "12 Nov 2026 – Partial outage (1 incident)"
}

func (s *Server) handleStatusPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	loc := s.settings(r).Location()
	now := time.Now()

	cur, err := s.loadPublicStatus(ctx, now)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	from := publicStartOfDay(now.In(loc)).AddDate(0, 0, 1-publicHistoryDays)
	incidents, err := s.Store.ListIncidentsBetween(ctx, from, now)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	maintenances, err := s.Store.ListMaintenancesBetween(ctx, from, now)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	history := store.DailyHistory(cur.Services, incidents, maintenances, publicHistoryDays, now, loc)

	data := &publicStatusData{
		Overall:      cur.Overall,
		Updated:      now,
		Incidents:    cur.Incidents,
		Maintenances: cur.Maintenances,
		HasServices:  len(cur.Services) > 0,
	}
	for _, svc := range cur.Services {
		row := publicServiceRow{Service: svc, Status: cur.Statuses[svc.ID]}
		row.Days, row.Summary = publicHistoryBar(history[svc.ID])
		if n := len(data.Groups); n == 0 || !strings.EqualFold(data.Groups[n-1].Category, svc.Category) {
			data.Groups = append(data.Groups, publicServiceGroup{Category: svc.Category})
		}
		g := &data.Groups[len(data.Groups)-1]
		g.Services = append(g.Services, row)
	}
	recentFrom := now.AddDate(0, 0, -publicRecentDays)
	for _, inc := range incidents {
		if inc.ResolvedAt == nil || !inc.ResolvedAt.Before(recentFrom) {
			data.Recent = append(data.Recent, inc)
		}
	}
	s.render(w, r, "status", "", data)
}

// publicHistoryBar converts the daily history of a service into bars with
// tooltips and a short text summary for screen readers.
func publicHistoryBar(days []store.DayStatus) ([]publicHistoryDay, string) {
	out := make([]publicHistoryDay, len(days))
	incidentDays, maintenanceDays := 0, 0
	for i, d := range days {
		date := d.Date.Format("2 Jan 2006")
		switch {
		case d.NoData:
			out[i] = publicHistoryDay{Class: "nodata", Title: date + " – No data"}
			continue
		case d.Incidents > 0:
			incidentDays++
			out[i] = publicHistoryDay{Title: fmt.Sprintf("%s – %s (%s)", date, d.Status.Label(), publicPlural(d.Incidents, "incident"))}
		default:
			if d.Status == store.StatusMaintenance {
				maintenanceDays++
			}
			out[i] = publicHistoryDay{Title: date + " – " + d.Status.Label()}
		}
		out[i].Class = "status-" + string(d.Status)
	}
	summary := fmt.Sprintf("Last %d days: ", len(days))
	switch {
	case incidentDays == 0 && maintenanceDays == 0:
		summary += "no incidents"
	case maintenanceDays == 0:
		summary += "incidents on " + publicPlural(incidentDays, "day")
	case incidentDays == 0:
		summary += "maintenance on " + publicPlural(maintenanceDays, "day")
	default:
		summary += "incidents on " + publicPlural(incidentDays, "day") + ", maintenance on " + publicPlural(maintenanceDays, "day")
	}
	return out, summary
}

func publicPlural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

func publicStartOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// ------------------------------------------------------------------ history

type publicHistoryData struct {
	From     time.Time // first day of the window
	Last     time.Time // last day of the window (inclusive)
	Months   []publicHistoryMonth
	OlderURL string
	NewerURL string // empty for the current window
}

type publicHistoryMonth struct {
	Label   string // e.g. "November 2026"
	Entries []publicHistoryEntry
}

// publicHistoryEntry is either an incident or a maintenance window.
type publicHistoryEntry struct {
	Incident    *store.Incident
	Maintenance *store.Maintenance
	start       time.Time
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	loc := s.settings(r).Location()
	// The window is [from, to) in whole days; the current window ends at the
	// end of today.
	latest := publicStartOfDay(time.Now().In(loc)).AddDate(0, 0, 1)
	to := latest
	if v := strings.TrimSpace(r.URL.Query().Get("before")); v != "" {
		d, err := time.ParseInLocation(publicDateLayout, v, loc)
		if err != nil {
			s.badRequest(w, r, fmt.Sprintf("Invalid date %q. Please use the format YYYY-MM-DD.", publicTruncate(v, 40)))
			return
		}
		if d.Before(latest) {
			to = d
		}
	}
	from := to.AddDate(0, 0, -publicHistoryDays)

	incidents, err := s.Store.ListIncidentsBetween(ctx, from, to)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	maintenances, err := s.Store.ListMaintenancesBetween(ctx, from, to)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// List every entry once: in the window in which it started.
	var entries []publicHistoryEntry
	for i := range incidents {
		if inc := &incidents[i]; !inc.StartedAt.Before(from) {
			entries = append(entries, publicHistoryEntry{Incident: inc, start: inc.StartedAt})
		}
	}
	for i := range maintenances {
		if m := &maintenances[i]; !m.StartsAt.Before(from) {
			entries = append(entries, publicHistoryEntry{Maintenance: m, start: m.StartsAt})
		}
	}
	slices.SortStableFunc(entries, func(a, b publicHistoryEntry) int { return b.start.Compare(a.start) })

	data := &publicHistoryData{
		From:     from,
		Last:     to.AddDate(0, 0, -1),
		OlderURL: "/history?before=" + from.Format(publicDateLayout),
	}
	if to.Before(latest) {
		data.NewerURL = "/history"
		if newer := to.AddDate(0, 0, publicHistoryDays); newer.Before(latest) {
			data.NewerURL += "?before=" + newer.Format(publicDateLayout)
		}
	}
	for _, e := range entries {
		label := e.start.In(loc).Format("January 2006")
		if n := len(data.Months); n == 0 || data.Months[n-1].Label != label {
			data.Months = append(data.Months, publicHistoryMonth{Label: label})
		}
		m := &data.Months[len(data.Months)-1]
		m.Entries = append(m.Entries, e)
	}
	s.render(w, r, "history", "History", data)
}

func publicTruncate(v string, n int) string {
	if r := []rune(v); len(r) > n {
		return string(r[:n]) + "…"
	}
	return v
}

// ------------------------------------------------------------------ incident and maintenance pages

func (s *Server) handleIncident(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	inc, err := s.Store.GetIncident(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "incident", inc.Title, inc)
}

func (s *Server) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	m, err := s.Store.GetMaintenance(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "maintenance", m.Title, m)
}

// ------------------------------------------------------------------ JSON API

type publicAPIStatus struct {
	Status       store.Status           `json:"status"`
	StatusLabel  string                 `json:"status_label"`
	UpdatedAt    string                 `json:"updated_at"`
	Services     []publicAPIService     `json:"services"`
	Incidents    []publicAPIIncident    `json:"incidents"`
	Maintenances []publicAPIMaintenance `json:"maintenances"`
}

type publicAPIService struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	Category    string       `json:"category"`
	Status      store.Status `json:"status"`
	StatusLabel string       `json:"status_label"`
}

type publicAPIIncident struct {
	ID        int64                `json:"id"`
	Title     string               `json:"title"`
	Status    store.IncidentStatus `json:"status"`
	Impact    store.Status         `json:"impact"`
	StartedAt string               `json:"started_at"`
	URL       string               `json:"url"`
}

type publicAPIMaintenance struct {
	ID       int64                  `json:"id"`
	Title    string                 `json:"title"`
	State    store.MaintenanceState `json:"state"`
	StartsAt string                 `json:"starts_at"`
	EndsAt   string                 `json:"ends_at"`
	URL      string                 `json:"url"`
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	cur, err := s.loadPublicStatus(r.Context(), now)
	if err != nil {
		s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		publicJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
		return
	}
	base := strings.TrimRight(s.settings(r).BaseURL, "/")
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	out := publicAPIStatus{
		Status:       cur.Overall,
		StatusLabel:  store.OverallLabel(cur.Overall),
		UpdatedAt:    rfc(now),
		Services:     make([]publicAPIService, 0, len(cur.Services)),
		Incidents:    make([]publicAPIIncident, 0, len(cur.Incidents)),
		Maintenances: make([]publicAPIMaintenance, 0, len(cur.Maintenances)),
	}
	for _, svc := range cur.Services {
		st := cur.Statuses[svc.ID]
		out.Services = append(out.Services, publicAPIService{ID: svc.ID, Name: svc.Name, Category: svc.Category, Status: st, StatusLabel: st.Label()})
	}
	for i := range cur.Incidents {
		inc := &cur.Incidents[i]
		out.Incidents = append(out.Incidents, publicAPIIncident{
			ID: inc.ID, Title: inc.Title, Status: inc.Status, Impact: inc.Impact(),
			StartedAt: rfc(inc.StartedAt), URL: base + "/incidents/" + strconv.FormatInt(inc.ID, 10),
		})
	}
	for i := range cur.Maintenances {
		m := &cur.Maintenances[i]
		out.Maintenances = append(out.Maintenances, publicAPIMaintenance{
			ID: m.ID, Title: m.Title, State: m.State(now),
			StartsAt: rfc(m.StartsAt), EndsAt: rfc(m.EndsAt), URL: base + "/maintenance/" + strconv.FormatInt(m.ID, 10),
		})
	}
	publicJSON(w, http.StatusOK, out)
}

func publicJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
