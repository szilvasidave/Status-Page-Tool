package web

// Admin → Monitoring: the integrations of monitoring tools (package
// monitoring). Each has a webhook URL with a random token, of which only the
// hash is stored: the URL is shown once, right after it was generated, like
// the provisioning token (Post/Redirect/Get with newTokens, see
// admin_settings_sync.go). The integration page also shows the setup
// instructions of its tool, its firing alerts and its latest requests.
// Templates: admin_monitoring.html (list), admin_monitoring_form.html (new
// and edit).

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/monitoring"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// newURLParam is the query parameter of the integration page with the id of
// a slot in newTokens (never the token itself).
const newURLParam = "new-url"

// adminIntegrationRow is one row of the integrations list.
type adminIntegrationRow struct {
	store.Integration
	ProviderName string
}

// providerName is the name of a tool; an ID this version does not know
// (e.g. after going back to an older version) is read as the generic
// format.
func providerName(id string) string {
	if p := monitoring.Lookup(id); p != nil {
		return p.Name
	}
	return "Unknown tool “" + id + "” (read as " + monitoring.Lookup(monitoring.Generic).Name + ")"
}

func (s *Server) handleIntegrations(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListIntegrations(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows := make([]adminIntegrationRow, len(list))
	for i, in := range list {
		rows[i] = adminIntegrationRow{Integration: in, ProviderName: providerName(in.Provider)}
	}
	s.render(w, r, "admin_monitoring", "Monitoring", rows)
}

// adminIntegrationForm is the data of the new/edit integration page.
type adminIntegrationForm struct {
	Integration store.Integration
	IsNew       bool
	Providers   []*monitoring.Provider
	Errors      adminFormErrors

	// Edit page only.
	Heading     string               // the saved name
	SavedPaused bool                 // the saved pause (the form may differ)
	Provider    *monitoring.Provider // the saved tool, for its setup instructions
	// ProviderName is the saved tool's name (see providerName).
	ProviderName string
	// UnknownProvider is the saved tool's ID if this version does not know
	// it: the select keeps it, so that saving other settings keeps it.
	UnknownProvider string
	HookURL         string // <base>/hooks, for the Authorization header
	NewURL          string // the webhook URL generated just now (shown once)
	Replaced        bool   // NewURL replaced a previous URL
	Example         string // the tool's example with the webhook URL (or a placeholder)
	BaseURLSet      bool
	Alerts          []store.IntegrationAlert // the first adminIntegrationAlerts
	MoreAlerts      int                      // firing alerts not listed
	Events          []store.IntegrationEvent
	ShowSetup       bool // open the setup instructions: new URL, or nothing received yet
	EventsLimit     int
}

// adminIntegrationAlerts is the number of firing alerts listed on the page.
const adminIntegrationAlerts = 100

// ImpactChoices returns the options of the impact select ("" = by the
// alert's severity).
func (f *adminIntegrationForm) ImpactChoices() []store.Status {
	return append([]store.Status{""}, store.Impacts...)
}

// adminIntegrationEvents is the number of requests listed on the page.
const adminIntegrationEvents = store.KeepIntegrationEvents

func (s *Server) adminRenderIntegration(w http.ResponseWriter, r *http.Request, status int, f *adminIntegrationForm) {
	f.Providers = monitoring.Providers()
	if f.Errors == nil {
		f.Errors = adminFormErrors{}
	}
	title := "New integration"
	if !f.IsNew {
		if err := s.adminIntegrationDetails(r, f); err != nil {
			s.serverError(w, r, err)
			return
		}
		title = f.Heading
	}
	s.renderStatus(w, r, status, "admin_monitoring_form", title, f)
}

// adminIntegrationDetails fills the parts of the edit page that are not
// form fields: URL, setup instructions, alerts and requests.
func (s *Server) adminIntegrationDetails(r *http.Request, f *adminIntegrationForm) error {
	ctx := r.Context()
	saved, err := s.Store.GetIntegration(ctx, f.Integration.ID)
	if err != nil {
		return err
	}
	f.Heading, f.SavedPaused = saved.Name, saved.Paused
	f.Integration.Firing, f.Integration.LastEventAt, f.Integration.TokenCreatedAt = saved.Firing, saved.LastEventAt, saved.TokenCreatedAt
	st := s.settings(r)
	f.BaseURLSet = st.BaseURL != ""
	f.HookURL = monitoring.URL(r, st.BaseURL, "")
	if id := r.URL.Query().Get(newURLParam); id != "" {
		if t, ok := newTokens.take(id, tokenOwner(r), f.Integration.ID); ok {
			f.NewURL, f.Replaced = monitoring.URL(r, st.BaseURL, t.token), t.replaced
		}
	}
	f.Provider, f.ProviderName = monitoring.Lookup(saved.Provider), providerName(saved.Provider)
	if f.Provider == nil {
		f.Provider, f.UnknownProvider = monitoring.Lookup(monitoring.Generic), saved.Provider
	}
	url := f.NewURL
	if url == "" {
		url = f.HookURL + "/<token>"
	}
	f.Example = monitoring.Expand(f.Provider.Example, url)
	if f.Alerts, err = s.Store.ListIntegrationAlerts(ctx, f.Integration.ID); err != nil {
		return err
	}
	if len(f.Alerts) > adminIntegrationAlerts {
		f.Alerts, f.MoreAlerts = f.Alerts[:adminIntegrationAlerts], len(f.Alerts)-adminIntegrationAlerts
	}
	f.EventsLimit = adminIntegrationEvents
	if f.Events, err = s.Store.ListIntegrationEvents(ctx, f.Integration.ID, adminIntegrationEvents); err != nil {
		return err
	}
	f.ShowSetup = f.NewURL != "" || len(f.Events) == 0
	return nil
}

// adminParseIntegrationForm reads the form into f and validates it. On the
// edit page the saved tool is accepted even if this version does not know
// it (savedProvider).
func adminParseIntegrationForm(r *http.Request, f *adminIntegrationForm, savedProvider string) {
	f.Errors = adminFormErrors{}
	in := &f.Integration
	in.Name = formTrim(r, "name")
	in.Provider = formTrim(r, "provider")
	in.Impact = store.Status(formTrim(r, "impact"))
	in.Notify = formBool(r, "notify")
	in.Paused = !f.IsNew && formBool(r, "paused")
	switch {
	case in.Name == "":
		f.Errors.add("name", "Enter a name, e.g. the monitoring tool and where it runs.")
	case tooLong(in.Name, 100):
		f.Errors.add("name", "The name is too long (at most 100 characters).")
	}
	if monitoring.Lookup(in.Provider) == nil && (f.IsNew || in.Provider != savedProvider) {
		f.Errors.add("provider", "Choose the monitoring tool.")
	}
	if in.Impact != "" && !store.ValidImpact(in.Impact) {
		f.Errors.add("impact", "Choose an impact.")
	}
}

func (s *Server) handleIntegrationNew(w http.ResponseWriter, r *http.Request) {
	f := &adminIntegrationForm{IsNew: true, Integration: store.Integration{Notify: true}}
	s.adminRenderIntegration(w, r, http.StatusOK, f)
}

func (s *Server) handleIntegrationCreate(w http.ResponseWriter, r *http.Request) {
	f := &adminIntegrationForm{IsNew: true}
	adminParseIntegrationForm(r, f, "")
	if len(f.Errors) > 0 {
		s.adminRenderIntegration(w, r, http.StatusUnprocessableEntity, f)
		return
	}
	token, hash := monitoring.NewToken()
	if err := s.Store.CreateIntegration(r.Context(), &f.Integration, hash); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("monitoring integration created", "integration", f.Integration.Name, "provider", f.Integration.Provider, "user", adminActor(r))
	id := newTokens.put(tokenSlot{token: token, userID: tokenOwner(r), subject: f.Integration.ID})
	http.Redirect(w, r, adminIntegrationPath(f.Integration.ID)+"?"+newURLParam+"="+id+"#webhook", http.StatusSeeOther)
}

func adminIntegrationPath(id int64) string { return "/admin/monitoring/" + strconv.FormatInt(id, 10) }

// adminLoadIntegration loads the integration of the {id} path value. It
// writes a 404 or 500 response and returns nil on failure.
func (s *Server) adminLoadIntegration(w http.ResponseWriter, r *http.Request) *store.Integration {
	id, ok := s.pathID(w, r)
	if !ok {
		return nil
	}
	in, err := s.Store.GetIntegration(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return nil
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil
	}
	return in
}

func (s *Server) handleIntegrationEdit(w http.ResponseWriter, r *http.Request) {
	in := s.adminLoadIntegration(w, r)
	if in == nil {
		return
	}
	s.adminRenderIntegration(w, r, http.StatusOK, &adminIntegrationForm{Integration: *in})
}

func (s *Server) handleIntegrationUpdate(w http.ResponseWriter, r *http.Request) {
	in := s.adminLoadIntegration(w, r)
	if in == nil {
		return
	}
	f := &adminIntegrationForm{Integration: store.Integration{ID: in.ID}}
	adminParseIntegrationForm(r, f, in.Provider)
	if len(f.Errors) > 0 {
		s.adminRenderIntegration(w, r, http.StatusUnprocessableEntity, f)
		return
	}
	err := s.Store.UpdateIntegration(r.Context(), &f.Integration)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("monitoring integration saved", "integration", f.Integration.Name, "paused", f.Integration.Paused, "user", adminActor(r))
	s.redirectWithFlash(w, r, adminIntegrationPath(in.ID), "success", "The integration “"+f.Integration.Name+"” has been saved.")
}

// handleIntegrationNewURL replaces the webhook token and shows the new URL
// once.
func (s *Server) handleIntegrationNewURL(w http.ResponseWriter, r *http.Request) {
	in := s.adminLoadIntegration(w, r)
	if in == nil {
		return
	}
	token, hash := monitoring.NewToken()
	err := s.Store.SetIntegrationToken(r.Context(), in.ID, hash)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("monitoring integration: new webhook URL", "integration", in.Name, "user", adminActor(r))
	id := newTokens.put(tokenSlot{token: token, replaced: true, userID: tokenOwner(r), subject: in.ID})
	http.Redirect(w, r, adminIntegrationPath(in.ID)+"?"+newURLParam+"="+id+"#webhook", http.StatusSeeOther)
}

// handleIntegrationClearAlert treats a firing alert as resolved, as if the
// tool had reported its recovery (the remedy for a recovery that was lost).
func (s *Server) handleIntegrationClearAlert(w http.ResponseWriter, r *http.Request) {
	in := s.adminLoadIntegration(w, r)
	if in == nil {
		return
	}
	back := adminIntegrationPath(in.ID) // the message is at the top
	lines, err := s.Monitoring.Clear(r.Context(), in, r.PostFormValue("key"), adminActor(r))
	if errors.Is(err, store.ErrNotFound) {
		s.redirectWithFlash(w, r, back, "info", "This alert is no longer firing.")
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectWithFlash(w, r, back, "success", "The alert has been cleared: "+strings.Join(lines, " "))
}

func (s *Server) handleIntegrationDelete(w http.ResponseWriter, r *http.Request) {
	in := s.adminLoadIntegration(w, r)
	if in == nil {
		return
	}
	alerts, err := s.Store.ListIntegrationAlerts(r.Context(), in.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	open := map[int64]bool{}
	for _, a := range alerts {
		if a.IncidentOpen {
			open[a.IncidentID] = true
		}
	}
	err = s.Store.DeleteIntegration(r.Context(), in.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("monitoring integration deleted", "integration", in.Name, "user", adminActor(r))
	msg := "The integration “" + in.Name + "” has been deleted. Its webhook URL no longer works."
	switch len(open) {
	case 0:
	case 1:
		msg += " The open incident of its alerts stays open until an editor resolves it (or other monitoring alerts attached to it clear)."
	default:
		msg += " The open incidents of its alerts stay open until an editor resolves them (or other monitoring alerts attached to them clear)."
	}
	s.redirectWithFlash(w, r, "/admin/monitoring", "success", msg)
}
