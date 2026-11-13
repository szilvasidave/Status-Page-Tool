package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/notify"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// manageServices creates two services and returns their ids.
func manageServices(t *testing.T, e *testEnv) (erp, vpn int64) {
	t.Helper()
	ctx := context.Background()
	a := &store.Service{Name: "ERP", Category: "Business applications"}
	b := &store.Service{Name: "VPN", Category: "Infrastructure"}
	for _, svc := range []*store.Service{a, b} {
		if err := e.Store.CreateService(ctx, svc); err != nil {
			t.Fatal(err)
		}
	}
	return a.ID, b.ID
}

// manageIncident creates an open incident affecting the given service.
func manageIncident(t *testing.T, e *testEnv, title string, impacts ...store.ServiceImpact) *store.Incident {
	t.Helper()
	inc := &store.Incident{Title: title, Status: store.IncidentInvestigating, Services: impacts}
	if err := e.Store.CreateIncident(context.Background(), inc, &store.IncidentUpdate{Message: "First message of " + title}); err != nil {
		t.Fatal(err)
	}
	return inc
}

// manageMaintenance creates a maintenance window.
func manageMaintenance(t *testing.T, e *testEnv, title string, start, end time.Time, serviceIDs ...int64) *store.Maintenance {
	t.Helper()
	m := &store.Maintenance{Title: title, StartsAt: start, EndsAt: end}
	for _, id := range serviceIDs {
		m.Services = append(m.Services, store.ServiceRef{ID: id})
	}
	if err := e.Store.CreateMaintenance(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

// manageFollow asserts a 303 redirect to target and returns the target page.
func manageFollow(t *testing.T, e *testEnv, r Response, target string) Response {
	t.Helper()
	if r.Status != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body:\n%s", r.Status, r.Body)
	}
	if r.Location != target {
		t.Fatalf("redirect to %q, want %q", r.Location, target)
	}
	return e.get(target)
}

func manageStatus(t *testing.T, r Response, want int) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("status = %d, want %d; body:\n%s", r.Status, want, r.Body)
	}
}

// manageInput formats t for a datetime-local field in the UTC display time
// zone (the default setting).
func manageInput(t time.Time) string { return t.UTC().Format(InputTimeLayout) }

func manageImpactField(id int64) string { return "impact_" + strconv.FormatInt(id, 10) }

// manageSkipWithoutNotifier skips tests of notification counts while the
// notifier is not implemented yet.
func manageSkipWithoutNotifier(t *testing.T, e *testEnv) {
	t.Helper()
	if _, err := notify.New(e.Store, nil).IncidentCreated(context.Background(), 1<<40); err != nil && err.Error() == "not implemented" {
		t.Skip("notify.Notifier is not implemented yet")
	}
}

func TestManageAccessControl(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	erp, _ := manageServices(t, e)
	inc := manageIncident(t, e, "Access test", store.ServiceImpact{ServiceID: erp, Impact: store.StatusDegraded})
	m := manageMaintenance(t, e, "Window", time.Now().Add(time.Hour), time.Now().Add(2*time.Hour), erp)

	pages := []string{"/manage", "/manage/incidents/new", fmt.Sprintf("/manage/incidents/%d", inc.ID),
		"/manage/maintenance/new", fmt.Sprintf("/manage/maintenance/%d", m.ID)}

	// Anonymous: redirected to the login page.
	for _, p := range pages {
		r := e.get(p)
		if r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Location, "/login?next=") {
			t.Errorf("anonymous GET %s: %d %q, want redirect to /login", p, r.Status, r.Location)
		}
	}

	// Viewer: forbidden.
	e.loginAs("viewer@example.com", store.RoleViewer)
	for _, p := range pages {
		if r := e.get(p); r.Status != http.StatusForbidden {
			t.Errorf("viewer GET %s: %d, want 403", p, r.Status)
		}
	}
	if r := e.post("/manage/incidents", url.Values{"title": {"x"}}); r.Status != http.StatusForbidden {
		t.Errorf("viewer POST /manage/incidents: %d, want 403", r.Status)
	}

	// Editor: allowed, but may not delete.
	e.logout()
	e.loginAs("editor@example.com", store.RoleEditor)
	for _, p := range pages {
		if r := e.get(p); r.Status != http.StatusOK {
			t.Errorf("editor GET %s: %d, want 200", p, r.Status)
		}
	}
	r := e.get(fmt.Sprintf("/manage/incidents/%d", inc.ID))
	if strings.Contains(r.Body, "Delete incident") {
		t.Error("editor sees the delete button")
	}
	if r := e.post(fmt.Sprintf("/manage/incidents/%d/delete", inc.ID), nil); r.Status != http.StatusForbidden {
		t.Errorf("editor delete incident: %d, want 403", r.Status)
	}
	if r := e.post(fmt.Sprintf("/manage/maintenance/%d/delete", m.ID), nil); r.Status != http.StatusForbidden {
		t.Errorf("editor delete maintenance: %d, want 403", r.Status)
	}
	if _, err := e.Store.GetIncident(ctx, inc.ID); err != nil {
		t.Fatalf("incident was deleted by an editor: %v", err)
	}

	// Admin: may delete.
	e.logout()
	e.loginAdmin()
	page := e.get(fmt.Sprintf("/manage/incidents/%d", inc.ID))
	mustContain(t, page.Body, "Delete incident", `data-confirm="Delete this incident`)
	page = manageFollow(t, e, e.post(fmt.Sprintf("/manage/incidents/%d/delete", inc.ID), nil), "/manage")
	mustContain(t, page.Body, "The incident “Access test” has been deleted.")
	if _, err := e.Store.GetIncident(ctx, inc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("incident not deleted: %v", err)
	}
	page = manageFollow(t, e, e.post(fmt.Sprintf("/manage/maintenance/%d/delete", m.ID), nil), "/manage")
	mustContain(t, page.Body, "The maintenance “Window” has been deleted.")
	if _, err := e.Store.GetMaintenance(ctx, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("maintenance not deleted: %v", err)
	}
	if r := e.post(fmt.Sprintf("/manage/incidents/%d/delete", inc.ID), nil); r.Status != http.StatusNotFound {
		t.Errorf("delete unknown incident: %d, want 404", r.Status)
	}
}

func TestManageDashboard(t *testing.T) {
	e := newTestEnv(t)
	e.loginAs("editor@example.com", store.RoleEditor)

	r := e.get("/manage")
	manageStatus(t, r, http.StatusOK)
	mustContain(t, r.Body, `href="/manage/incidents/new"`, `href="/manage/maintenance/new"`,
		"No open incidents", "No maintenance is in progress or scheduled", "No incidents have been resolved recently")

	ctx := context.Background()
	erp, vpn := manageServices(t, e)
	open := manageIncident(t, e, "Open problem", store.ServiceImpact{ServiceID: erp, Impact: store.StatusMajorOutage})
	resolved := manageIncident(t, e, "Old problem", store.ServiceImpact{ServiceID: vpn, Impact: store.StatusDegraded})
	if err := e.Store.AddIncidentUpdate(ctx, resolved.ID, &store.IncidentUpdate{Status: store.IncidentResolved, Message: "Fixed."}, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	upcoming := manageMaintenance(t, e, "Upcoming window", now.Add(time.Hour), now.Add(2*time.Hour), erp)
	running := manageMaintenance(t, e, "Running window", now.Add(-time.Hour), now.Add(time.Hour), vpn)
	manageMaintenance(t, e, "Finished window", now.Add(-3*time.Hour), now.Add(-2*time.Hour), vpn)

	r = e.get("/manage")
	manageStatus(t, r, http.StatusOK)
	mustContain(t, r.Body,
		fmt.Sprintf(`href="/manage/incidents/%d"`, open.ID), "Open problem", "Major outage", "Investigating",
		fmt.Sprintf(`href="/manage/maintenance/%d"`, upcoming.ID), "Upcoming window", "Scheduled",
		fmt.Sprintf(`href="/manage/maintenance/%d"`, running.ID), "Running window", "In progress",
		fmt.Sprintf(`href="/manage/incidents/%d"`, resolved.ID), "Old problem", "Recently resolved")
	if strings.Contains(r.Body, "Finished window") {
		t.Error("completed maintenance is listed")
	}
	if strings.Contains(r.Body, "No open incidents") {
		t.Error("empty state shown although an incident is open")
	}
}

func TestManageIncidentCreate(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	erp, vpn := manageServices(t, e)
	user := e.loginAs("editor@example.com", store.RoleEditor)

	r := e.get("/manage/incidents/new")
	manageStatus(t, r, http.StatusOK)
	mustContain(t, r.Body, `name="`+manageImpactField(erp)+`"`, `name="`+manageImpactField(vpn)+`"`,
		"Business applications", "Infrastructure", "Not affected", "Degraded performance", "Major outage",
		`<option value="investigating" selected>`, `name="notify" value="on" checked`,
		"E-mail the members of the groups assigned to the affected services",
		"E-mail is not configured, so no notifications are sent")
	if strings.Contains(r.Body, `value="resolved"`) {
		t.Error("a new incident can be created as resolved")
	}

	// Validation errors.
	r = e.post("/manage/incidents", url.Values{"status": {"investigating"}})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "Please enter a title.", "Please select at least one affected service.", "Please enter a message")

	future := time.Now().Add(2 * time.Hour)
	r = e.post("/manage/incidents", url.Values{
		"title": {strings.Repeat("x", 201)}, "status": {"resolved"}, "message": {"Kept message"},
		manageImpactField(vpn): {"major_outage"}, "started_at": {manageInput(future)},
	})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "The title is too long", "Please choose a valid status.", "must not be in the future",
		"Kept message", `<option value="major_outage" selected>`, manageInput(future))
	if strings.Contains(r.Body, `name="notify" value="on" checked`) {
		t.Error("unticked notify checkbox is checked again after a validation error")
	}

	r = e.post("/manage/incidents", url.Values{"title": {"T"}, "status": {"investigating"}, "message": {"m"},
		manageImpactField(erp): {"broken"}, "started_at": {"yesterday"}})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "Please choose a valid impact for ERP.", "The start time is not a valid date and time.")

	if open, _ := e.Store.ListOpenIncidents(ctx); len(open) != 0 {
		t.Fatalf("invalid forms created %d incidents", len(open))
	}

	// Success.
	started := time.Now().Add(-30 * time.Minute).Truncate(time.Minute)
	r = e.post("/manage/incidents", url.Values{
		"title": {"Database down"}, "status": {"identified"}, "message": {"We know the cause."},
		manageImpactField(erp): {"major_outage"}, manageImpactField(vpn): {""},
		"started_at": {manageInput(started)},
	})
	open, err := e.Store.ListOpenIncidents(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("open incidents = %d, %v; want 1", len(open), err)
	}
	inc, err := e.Store.GetIncident(ctx, open[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	page := manageFollow(t, e, r, fmt.Sprintf("/manage/incidents/%d", inc.ID))
	mustContain(t, page.Body, "The incident has been reported.", "Database down", "We know the cause.")
	if inc.Title != "Database down" || inc.Status != store.IncidentIdentified || !inc.StartedAt.Equal(started) || inc.CreatedBy != user.ID {
		t.Errorf("incident = %+v", inc)
	}
	if len(inc.Services) != 1 || inc.Services[0].ServiceID != erp || inc.Services[0].Impact != store.StatusMajorOutage {
		t.Errorf("services = %+v, want ERP major outage", inc.Services)
	}
	if len(inc.Updates) != 1 || inc.Updates[0].Message != "We know the cause." || inc.Updates[0].Status != store.IncidentIdentified ||
		inc.Updates[0].CreatedBy != user.ID {
		t.Errorf("updates = %+v", inc.Updates)
	}
	if n, _ := e.Store.ListNotifications(ctx, 0); len(n) != 0 {
		t.Errorf("%d notifications queued although notify was not ticked", len(n))
	}

	// Without a start time the incident starts now.
	before := time.Now().Add(-time.Second)
	r = e.post("/manage/incidents", url.Values{"title": {"Second"}, "status": {"investigating"}, "message": {"m"},
		manageImpactField(vpn): {"degraded"}})
	manageStatus(t, r, http.StatusSeeOther)
	open, _ = e.Store.ListOpenIncidents(ctx)
	if len(open) != 2 || open[0].Title != "Second" || open[0].StartedAt.Before(before.Truncate(time.Second)) {
		t.Errorf("second incident = %+v", open)
	}
}

func TestManageIncidentNotifications(t *testing.T) {
	e := newTestEnv(t)
	manageSkipWithoutNotifier(t, e)
	ctx := context.Background()
	erp, vpn := manageServices(t, e)

	e.loginAs("editor@example.com", store.RoleEditor)

	// E-mail is not configured yet.
	r := e.post("/manage/incidents", url.Values{"title": {"No mail"}, "status": {"investigating"}, "message": {"m"},
		manageImpactField(erp): {"degraded"}, "notify": {"on"}})
	manageStatus(t, r, http.StatusSeeOther)
	mustContain(t, e.get(r.Location).Body, "The incident has been reported. E-mail is not configured, so no notifications were sent.")

	st, err := e.Store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.SMTPHost, st.SMTPFrom = "localhost", "status@example.com"
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	g, err := e.Store.CreateGroup(ctx, "erp-users", "ERP users")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SetGroupServices(ctx, g.ID, []int64{erp}); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"a@example.com", "b@example.com"} {
		u, err := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: email, Email: email, Name: email})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Store.SetUserGroups(ctx, u.ID, []store.GroupRef{{ExternalID: "erp-users"}}); err != nil {
			t.Fatal(err)
		}
	}
	if r := e.get("/manage/incidents/new"); strings.Contains(r.Body, "E-mail is not configured") {
		t.Error("e-mail notice shown although SMTP is configured")
	}

	count := func() int {
		n, err := e.Store.ListNotifications(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(n)
	}

	r = e.post("/manage/incidents", url.Values{"title": {"ERP down"}, "status": {"investigating"}, "message": {"m"},
		manageImpactField(erp): {"major_outage"}, "notify": {"on"}})
	manageStatus(t, r, http.StatusSeeOther)
	page := e.get(r.Location)
	if count() != 2 {
		t.Fatalf("queued e-mails = %d, want 2", count())
	}
	mustContain(t, page.Body, "The incident has been reported. 2 notification e-mails queued.")

	// Update: the notification goes to the members of the services before
	// and after the update.
	id := strings.TrimPrefix(r.Location, "/manage/incidents/")
	r = e.post("/manage/incidents/"+id+"/updates", url.Values{"status": {"resolved"}, "message": {"Fixed."},
		manageImpactField(vpn): {"degraded"}, "notify": {"on"}})
	page = manageFollow(t, e, r, "/manage/incidents/"+id)
	if count() != 4 {
		t.Fatalf("queued e-mails = %d, want 4", count())
	}
	mustContain(t, page.Body, "The incident has been resolved. 2 notification e-mails queued.")

	// Nobody is subscribed to VPN.
	r = e.post("/manage/incidents", url.Values{"title": {"VPN down"}, "status": {"investigating"}, "message": {"m"},
		manageImpactField(vpn): {"major_outage"}, "notify": {"on"}})
	manageStatus(t, r, http.StatusSeeOther)
	mustContain(t, e.get(r.Location).Body, "nobody is subscribed to the affected services")
	if count() != 4 {
		t.Errorf("queued e-mails = %d, want 4", count())
	}
}

func TestManageIncidentUpdate(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	erp, vpn := manageServices(t, e)
	inc := manageIncident(t, e, "Slow ERP",
		store.ServiceImpact{ServiceID: erp, Impact: store.StatusMajorOutage},
		store.ServiceImpact{ServiceID: vpn, Impact: store.StatusDegraded})
	user := e.loginAs("editor@example.com", store.RoleEditor)
	path := fmt.Sprintf("/manage/incidents/%d", inc.ID)

	r := e.get(path)
	manageStatus(t, r, http.StatusOK)
	mustContain(t, r.Body, "Slow ERP", "Post an update", "First message of Slow ERP",
		`<option value="investigating" selected>`, `<option value="resolved">`,
		`<option value="major_outage" selected>`, `<option value="degraded" selected>`,
		fmt.Sprintf(`action="%s/updates"`, path), fmt.Sprintf(`action="%s/title"`, path),
		fmt.Sprintf(`href="/incidents/%d"`, inc.ID))

	// Validation.
	r = e.post(path+"/updates", url.Values{"status": {"monitoring"}})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "Please enter a message for the update.", "Please select at least one affected service.",
		`<option value="monitoring" selected>`)
	r = e.post(path+"/updates", url.Values{"status": {"bogus"}, "message": {"m"}, manageImpactField(erp): {"degraded"}})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "Please choose a valid status.")

	// Update with changed impacts.
	r = e.post(path+"/updates", url.Values{"status": {"monitoring"}, "message": {"A fix is deployed."},
		manageImpactField(erp): {"partial_outage"}, manageImpactField(vpn): {""}})
	page := manageFollow(t, e, r, path)
	mustContain(t, page.Body, "The update has been posted.", "A fix is deployed.", "editor@example.com")
	got, err := e.Store.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.IncidentMonitoring || len(got.Updates) != 2 || got.Updates[0].CreatedBy != user.ID {
		t.Errorf("incident after update = %+v", got)
	}
	if len(got.Services) != 1 || got.Services[0].ServiceID != erp || got.Services[0].Impact != store.StatusPartialOutage {
		t.Errorf("services = %+v, want ERP partial outage", got.Services)
	}

	// Resolve.
	r = e.post(path+"/updates", url.Values{"status": {"resolved"}, "message": {"All good."}, manageImpactField(erp): {"partial_outage"}})
	page = manageFollow(t, e, r, path)
	mustContain(t, page.Body, "The incident has been resolved.", `<option value="resolved" selected>`)
	got, _ = e.Store.GetIncident(ctx, inc.ID)
	if !got.Resolved() || got.ResolvedAt == nil || len(got.Updates) != 3 {
		t.Errorf("incident not resolved: %+v", got)
	}

	// Reopen.
	r = e.post(path+"/updates", url.Values{"status": {"investigating"}, "message": {"It is back."}, manageImpactField(erp): {"major_outage"}})
	mustContain(t, manageFollow(t, e, r, path).Body, "The incident has been reopened.")

	// Rename.
	r = e.post(path+"/title", url.Values{"title": {"  "}})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "Please enter a title.", "<details class=\"disclosure\" open>")
	r = e.post(path+"/title", url.Values{"title": {"ERP is slow"}})
	mustContain(t, manageFollow(t, e, r, path).Body, "The title has been changed.", "ERP is slow")
	if got, _ := e.Store.GetIncident(ctx, inc.ID); got.Title != "ERP is slow" {
		t.Errorf("title = %q", got.Title)
	}

	// Unknown incidents.
	for _, p := range []string{"/manage/incidents/9999", "/manage/incidents/abc"} {
		if r := e.get(p); r.Status != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, r.Status)
		}
	}
	if r := e.post("/manage/incidents/9999/updates", url.Values{"status": {"resolved"}, "message": {"m"}}); r.Status != http.StatusNotFound {
		t.Errorf("update unknown incident: %d, want 404", r.Status)
	}
	if r := e.post("/manage/incidents/9999/title", url.Values{"title": {"x"}}); r.Status != http.StatusNotFound {
		t.Errorf("rename unknown incident: %d, want 404", r.Status)
	}
}

func TestManageMaintenance(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	erp, vpn := manageServices(t, e)

	// Times are entered in the display time zone.
	st, _ := e.Store.GetSettings(ctx)
	st.Timezone = "Europe/Berlin"
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	user := e.loginAs("editor@example.com", store.RoleEditor)

	r := e.get("/manage/maintenance/new")
	manageStatus(t, r, http.StatusOK)
	mustContain(t, r.Body, `action="/manage/maintenance"`, `name="services" value="`+strconv.FormatInt(erp, 10)+`"`,
		`name="notify" value="on" checked`, "when it starts and when it is completed", "Europe/Berlin")

	// Validation.
	r = e.post("/manage/maintenance", url.Values{"starts_at": {""}, "ends_at": {"nonsense"}})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "Please enter a title.", "Please enter the start time.", "The end time is not a valid date and time.",
		"Please select at least one affected service.")
	r = e.post("/manage/maintenance", url.Values{"title": {"Kept title"}, "starts_at": {"2030-01-15T12:00"}, "ends_at": {"2030-01-15T11:00"},
		"services": {strconv.FormatInt(vpn, 10)}})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "The end time must be after the start time.", "Kept title", "2030-01-15T12:00",
		`value="`+strconv.FormatInt(vpn, 10)+`" checked`)
	r = e.post("/manage/maintenance", url.Values{"title": {"Past"}, "starts_at": {"2020-01-15T10:00"}, "ends_at": {"2020-01-15T11:00"},
		"services": {strconv.FormatInt(vpn, 10)}})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "The end time is in the past.")

	// Create.
	r = e.post("/manage/maintenance", url.Values{"title": {"Upgrade"}, "description": {"New version"},
		"starts_at": {"2030-01-15T10:00"}, "ends_at": {"2030-01-15T12:30"}, "services": {strconv.FormatInt(erp, 10)}})
	mustContain(t, manageFollow(t, e, r, "/manage").Body, "The maintenance has been scheduled.", "Upgrade")
	list, err := e.Store.ListCurrentMaintenances(ctx, time.Now())
	if err != nil || len(list) != 1 {
		t.Fatalf("maintenances = %d, %v; want 1", len(list), err)
	}
	m := list[0]
	wantStart := time.Date(2030, 1, 15, 10, 0, 0, 0, berlin)
	if m.Title != "Upgrade" || m.Description != "New version" || !m.StartsAt.Equal(wantStart) ||
		!m.EndsAt.Equal(wantStart.Add(150*time.Minute)) || m.Notify || m.CreatedBy != user.ID {
		t.Errorf("maintenance = %+v", m)
	}
	if len(m.Services) != 1 || m.Services[0].ID != erp {
		t.Errorf("services = %+v", m.Services)
	}

	// Edit form.
	path := fmt.Sprintf("/manage/maintenance/%d", m.ID)
	r = e.get(path)
	manageStatus(t, r, http.StatusOK)
	mustContain(t, r.Body, "Edit maintenance", `value="Upgrade"`, `value="2030-01-15T10:00"`, `value="2030-01-15T12:30"`,
		`value="`+strconv.FormatInt(erp, 10)+`" checked`, "Cancel maintenance", fmt.Sprintf(`action="%s"`, path))
	if strings.Contains(r.Body, "End now") || strings.Contains(r.Body, `name="notify" value="on" checked`) {
		t.Error("scheduled maintenance: unexpected End now button or notify checked")
	}

	// Update.
	r = e.post(path, url.Values{"title": {"Upgrade v2"}, "description": {""}, "starts_at": {"2030-01-15T10:00"},
		"ends_at": {"2030-01-15T13:00"}, "services": {strconv.FormatInt(erp, 10), strconv.FormatInt(vpn, 10)}})
	mustContain(t, manageFollow(t, e, r, "/manage").Body, "The maintenance has been updated.")
	got, _ := e.Store.GetMaintenance(ctx, m.ID)
	if got.Title != "Upgrade v2" || got.Description != "" || !got.EndsAt.Equal(wantStart.Add(3*time.Hour)) || len(got.Services) != 2 {
		t.Errorf("maintenance after update = %+v", got)
	}
	r = e.post(path, url.Values{"title": {""}, "starts_at": {"2030-01-15T10:00"}, "ends_at": {"2030-01-15T13:00"}})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "Please enter a title.", "Please select at least one affected service.", "Save changes")

	// End now is only possible while in progress.
	r = e.post(path+"/end", nil)
	mustContain(t, manageFollow(t, e, r, path).Body, "Only a maintenance in progress can be ended")

	// In progress: end now.
	now := time.Now()
	running := manageMaintenance(t, e, "Running", now.Add(-time.Hour), now.Add(time.Hour), vpn)
	runPath := fmt.Sprintf("/manage/maintenance/%d", running.ID)
	r = e.get(runPath)
	mustContain(t, r.Body, "End now", "In progress")
	// A maintenance that has started cannot be cancelled, only ended.
	if strings.Contains(r.Body, "Cancel maintenance") || strings.Contains(r.Body, runPath+"/cancel") {
		t.Error("cancel offered for a maintenance in progress")
	}
	mustContain(t, manageFollow(t, e, e.post(runPath+"/cancel", nil), runPath).Body, "has already started and can no longer be cancelled", "End now")
	if got, _ := e.Store.GetMaintenance(ctx, running.ID); got.Cancelled {
		t.Fatal("a maintenance in progress was cancelled")
	}
	mustContain(t, manageFollow(t, e, e.post(runPath+"/end", nil), "/manage").Body, "The maintenance has been ended.")
	got, _ = e.Store.GetMaintenance(ctx, running.ID)
	if got.State(time.Now()) != store.MaintenanceCompleted || got.EndsAt.After(time.Now()) || got.Title != "Running" || len(got.Services) != 1 {
		t.Errorf("maintenance after end now = %+v", got)
	}

	// Completed: read only.
	r = e.get(runPath)
	manageStatus(t, r, http.StatusOK)
	mustContain(t, r.Body, "completed and can no longer be changed", "Completed")
	if strings.Contains(r.Body, `name="title"`) || strings.Contains(r.Body, "Cancel maintenance") {
		t.Error("completed maintenance can be edited")
	}
	r = e.post(runPath, url.Values{"title": {"Changed"}, "starts_at": {"2030-01-15T10:00"}, "ends_at": {"2030-01-15T13:00"},
		"services": {strconv.FormatInt(vpn, 10)}})
	mustContain(t, manageFollow(t, e, r, runPath).Body, "can no longer be changed")
	if got, _ := e.Store.GetMaintenance(ctx, running.ID); got.Title != "Running" {
		t.Errorf("completed maintenance was changed: %q", got.Title)
	}
	mustContain(t, manageFollow(t, e, e.post(runPath+"/cancel", nil), runPath).Body, "can no longer be cancelled")

	// Cancel.
	mustContain(t, manageFollow(t, e, e.post(path+"/cancel", nil), "/manage").Body, "The maintenance has been cancelled.")
	if got, _ := e.Store.GetMaintenance(ctx, m.ID); !got.Cancelled {
		t.Error("maintenance not cancelled")
	}
	mustContain(t, e.get(path).Body, "cancelled and can no longer be changed")

	// Unknown maintenance.
	for _, p := range []string{"/manage/maintenance/9999", "/manage/maintenance/x"} {
		if r := e.get(p); r.Status != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, r.Status)
		}
	}
	for _, p := range []string{"/manage/maintenance/9999", "/manage/maintenance/9999/end", "/manage/maintenance/9999/cancel"} {
		if r := e.post(p, url.Values{"title": {"x"}}); r.Status != http.StatusNotFound {
			t.Errorf("POST %s: %d, want 404", p, r.Status)
		}
	}
}

func TestManageMaintenanceNotifyOnChange(t *testing.T) {
	e := newTestEnv(t)
	manageSkipWithoutNotifier(t, e)
	ctx := context.Background()
	erp, _ := manageServices(t, e)
	st, _ := e.Store.GetSettings(ctx)
	st.SMTPHost, st.SMTPFrom = "localhost", "status@example.com"
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	g, _ := e.Store.CreateGroup(ctx, "erp-users", "ERP users")
	if err := e.Store.SetGroupServices(ctx, g.ID, []int64{erp}); err != nil {
		t.Fatal(err)
	}
	u, _ := e.Store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "a", Email: "a@example.com", Name: "A"})
	if err := e.Store.SetUserGroups(ctx, u.ID, []store.GroupRef{{ExternalID: "erp-users"}}); err != nil {
		t.Fatal(err)
	}
	e.loginAs("editor@example.com", store.RoleEditor)
	count := func() int {
		n, _ := e.Store.ListNotifications(ctx, 0)
		return len(n)
	}
	form := url.Values{"title": {"Upgrade"}, "starts_at": {"2030-01-15T10:00"}, "ends_at": {"2030-01-15T12:00"},
		"services": {strconv.FormatInt(erp, 10)}, "notify": {"on"}}
	mustContain(t, manageFollow(t, e, e.post("/manage/maintenance", form), "/manage").Body,
		"The maintenance has been scheduled. 1 notification e-mail queued.")
	if count() != 1 {
		t.Fatalf("queued e-mails = %d, want 1", count())
	}
	list, _ := e.Store.ListCurrentMaintenances(ctx, time.Now())
	path := fmt.Sprintf("/manage/maintenance/%d", list[0].ID)

	// Saved without a change: no e-mail.
	mustContain(t, manageFollow(t, e, e.post(path, form), "/manage").Body, "The maintenance has been updated.")
	if count() != 1 {
		t.Errorf("e-mail sent without a change: %d", count())
	}
	// The description changed (e.g. instructions for the users): e-mail.
	form.Set("description", "Please save your work, the ERP is unavailable for the first 15 minutes.")
	mustContain(t, manageFollow(t, e, e.post(path, form), "/manage").Body, "1 notification e-mail queued.")
	if count() != 2 {
		t.Errorf("queued e-mails after a description change = %d, want 2", count())
	}
	// The window changed: e-mail.
	form.Set("ends_at", "2030-01-15T13:00")
	mustContain(t, manageFollow(t, e, e.post(path, form), "/manage").Body, "1 notification e-mail queued.")
	if count() != 3 {
		t.Errorf("queued e-mails = %d, want 3", count())
	}
	// Cancel: e-mail because the maintenance has notify enabled.
	mustContain(t, manageFollow(t, e, e.post(path+"/cancel", nil), "/manage").Body, "The maintenance has been cancelled. 1 notification e-mail queued.")
	if count() != 4 {
		t.Errorf("queued e-mails = %d, want 4", count())
	}
}

func TestManageNotifyNote(t *testing.T) {
	s := &Server{Deps: Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Notifier: notify.New(nil, nil)}}
	r := httptest.NewRequest(http.MethodPost, "/manage/incidents", nil)
	for _, c := range []struct {
		n    int
		err  error
		want string
		warn bool
	}{
		{0, errors.New("database is locked"), "the notification e-mails could not be queued", true},
		{1, nil, "1 notification e-mail queued.", false},
		{12, nil, "12 notification e-mails queued.", false},
		{0, nil, "E-mail is not configured, so no notifications were sent.", false},
	} {
		note, warn := s.manageNotify(r, "test", func(context.Context) (int, error) { return c.n, c.err })
		if !strings.Contains(note, c.want) || warn != c.warn {
			t.Errorf("n=%d err=%v: got %q, %v; want %q, %v", c.n, c.err, note, warn, c.want, c.warn)
		}
	}
}

func TestManageWindow(t *testing.T) {
	s := &Server{}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	s.loc.Store(berlin)
	start := time.Date(2026, 11, 12, 14, 0, 0, 0, berlin)
	if got, want := s.manageWindow(start, start.Add(2*time.Hour)), "12 Nov 2026 14:00 – 16:00 CET"; got != want {
		t.Errorf("same day: %q, want %q", got, want)
	}
	if got, want := s.manageWindow(start.Add(8*time.Hour), start.Add(12*time.Hour)), "12 Nov 2026 22:00 CET – 13 Nov 2026 02:00 CET"; got != want {
		t.Errorf("across midnight: %q, want %q", got, want)
	}
}

// TestManageIncidentWithoutServices: an open incident whose services were
// all deleted can still be updated and resolved without attaching an
// unrelated service (which would show a false impact in its history).
func TestManageIncidentWithoutServices(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	erp, _ := manageServices(t, e)
	orphan := manageIncident(t, e, "Legacy VPN down") // its service was deleted
	e.loginAs("editor@example.com", store.RoleEditor)
	path := fmt.Sprintf("/manage/incidents/%d", orphan.ID)

	mustContain(t, e.get(path).Body, "No service is linked to this incident any more")
	r := e.post(path+"/updates", url.Values{"status": {"resolved"}, "message": {"The VPN was decommissioned."}})
	mustContain(t, manageFollow(t, e, r, path).Body, "The incident has been resolved.")
	got, err := e.Store.GetIncident(ctx, orphan.ID)
	if err != nil || !got.Resolved() || len(got.Services) != 0 {
		t.Fatalf("incident = %+v, %v; want resolved without services", got, err)
	}

	// An incident with services still needs at least one.
	inc := manageIncident(t, e, "Slow ERP", store.ServiceImpact{ServiceID: erp, Impact: store.StatusDegraded})
	r = e.post(fmt.Sprintf("/manage/incidents/%d/updates", inc.ID), url.Values{"status": {"resolved"}, "message": {"Fixed."}})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "Please select at least one affected service.")
}

// TestManageTextLimits: titles have at most 200, messages and descriptions
// at most 10000 characters (not bytes; a line break counts once, as in the
// browser, although it is sent as CRLF).
func TestManageTextLimits(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	erp, _ := manageServices(t, e)
	e.loginAs("editor@example.com", store.RoleEditor)
	title := strings.Repeat("ű", maxTitleLen)
	text := strings.Repeat("ő\r\nő", (maxTextLen-1)/3) + "ő" // 10000 characters in the browser
	impact := manageImpactField(erp)

	r := e.post("/manage/incidents", url.Values{"title": {title}, "status": {"investigating"}, "message": {text}, impact: {"degraded"}})
	manageStatus(t, r, http.StatusSeeOther)
	open, _ := e.Store.ListOpenIncidents(ctx)
	if len(open) != 1 {
		t.Fatalf("open incidents = %d, want 1", len(open))
	}
	path := fmt.Sprintf("/manage/incidents/%d", open[0].ID)
	r = e.post("/manage/incidents", url.Values{"title": {title + "ű"}, "status": {"investigating"}, "message": {text + "ő"}, impact: {"degraded"}})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "The title is too long (at most 200 characters).", "The message is too long (at most 10000 characters).",
		`maxlength="10000"`)
	r = e.post(path+"/updates", url.Values{"status": {"monitoring"}, "message": {text + "ő"}, impact: {"degraded"}})
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "The message is too long (at most 10000 characters).")
	manageStatus(t, e.post(path+"/updates", url.Values{"status": {"monitoring"}, "message": {text}, impact: {"degraded"}}), http.StatusSeeOther)

	form := url.Values{"title": {title}, "description": {text + "ő"}, "starts_at": {"2030-01-15T10:00"}, "ends_at": {"2030-01-15T12:00"},
		"services": {strconv.FormatInt(erp, 10)}}
	r = e.post("/manage/maintenance", form)
	manageStatus(t, r, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "The description is too long (at most 10000 characters).", `maxlength="10000"`)
	form.Set("description", text)
	manageStatus(t, e.post("/manage/maintenance", form), http.StatusSeeOther)
}
