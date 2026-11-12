package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// publicFixture holds the sample data created by publicSeed.
type publicFixture struct {
	ERP, VPN, Mail  *store.Service
	Author          *store.User
	Incident        *store.Incident    // open, major outage on VPN, degraded ERP
	Resolved        *store.Incident    // resolved 10 days ago, Mail
	Old             *store.Incident    // resolved 120 days ago, ERP
	InProgress      *store.Maintenance // in progress on Mail
	Upcoming        *store.Maintenance // starts tomorrow, ERP
	Cancelled       *store.Maintenance // cancelled, 5 days ago
	UpdateMessage   string
	FirstUpdateText string
}

func publicSeed(t *testing.T, e *testEnv) *publicFixture {
	t.Helper()
	ctx := context.Background()
	st := e.Store
	now := time.Now()
	f := &publicFixture{UpdateMessage: "A failover\nis in progress.", FirstUpdateText: "We are investigating."}

	svc := func(name, cat string) *store.Service {
		s := &store.Service{Name: name, Description: name + " description", Category: cat}
		if err := st.CreateService(ctx, s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	f.ERP = svc("ERP", "Business")
	f.VPN = svc("VPN", "Infrastructure")
	f.Mail = svc("Mail", "")

	var err error
	f.Author, err = st.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "sub-erika", Email: "erika@example.com", Name: "Erika Editor"})
	if err != nil {
		t.Fatal(err)
	}

	f.Incident = &store.Incident{Title: "VPN connections dropping", Status: store.IncidentInvestigating, StartedAt: now.Add(-time.Hour),
		Services: []store.ServiceImpact{{ServiceID: f.VPN.ID, Impact: store.StatusMajorOutage}, {ServiceID: f.ERP.ID, Impact: store.StatusDegraded}}}
	if err := st.CreateIncident(ctx, f.Incident, &store.IncidentUpdate{Message: f.FirstUpdateText, CreatedBy: f.Author.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddIncidentUpdate(ctx, f.Incident.ID, &store.IncidentUpdate{Status: store.IncidentIdentified, Message: f.UpdateMessage, CreatedBy: f.Author.ID}, nil); err != nil {
		t.Fatal(err)
	}

	resolved := func(title string, started time.Time, svcID int64) *store.Incident {
		end := started.Add(90 * time.Minute)
		inc := &store.Incident{Title: title, Status: store.IncidentResolved, StartedAt: started, ResolvedAt: &end,
			Services: []store.ServiceImpact{{ServiceID: svcID, Impact: store.StatusPartialOutage}}}
		if err := st.CreateIncident(ctx, inc, &store.IncidentUpdate{Message: "Fixed."}); err != nil {
			t.Fatal(err)
		}
		return inc
	}
	f.Resolved = resolved("Mail delayed", now.AddDate(0, 0, -10), f.Mail.ID)
	f.Old = resolved("Old ERP outage", now.AddDate(0, 0, -120), f.ERP.ID)

	maint := func(title string, start, end time.Time, svcID int64) *store.Maintenance {
		m := &store.Maintenance{Title: title, Description: "Line one\nline two", StartsAt: start, EndsAt: end, Services: []store.ServiceRef{{ID: svcID}}}
		if err := st.CreateMaintenance(ctx, m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	f.InProgress = maint("Mail server patching", now.Add(-30*time.Minute), now.Add(time.Hour), f.Mail.ID)
	f.Upcoming = maint("ERP kernel upgrade", now.Add(24*time.Hour), now.Add(27*time.Hour), f.ERP.ID)
	f.Cancelled = maint("Cancelled VPN work", now.AddDate(0, 0, -5), now.AddDate(0, 0, -5).Add(time.Hour), f.VPN.ID)
	if err := st.CancelMaintenance(ctx, f.Cancelled.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

func publicSetSettings(t *testing.T, e *testEnv, change func(*store.Settings)) {
	t.Helper()
	ctx := context.Background()
	st, err := e.Store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	change(st)
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
}

func publicMustNotContain(t *testing.T, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(body, s) {
			t.Errorf("response unexpectedly contains %q", s)
		}
	}
}

func TestPublicStatusPageEmpty(t *testing.T) {
	e := newTestEnv(t)
	r := e.get("/")
	if r.Status != http.StatusOK {
		t.Fatalf("status %d", r.Status)
	}
	mustContain(t, r.Body, "All systems operational", "No services have been set up yet.", "No incidents reported.",
		`<meta http-equiv="refresh" content="60">`)
	publicMustNotContain(t, r.Body, "/admin/services/new", "Active incidents")

	e.loginAdmin()
	r = e.get("/")
	mustContain(t, r.Body, `href="/admin/services/new"`)
}

func TestPublicStatusPage(t *testing.T) {
	e := newTestEnv(t)
	f := publicSeed(t, e)
	r := e.get("/")
	if r.Status != http.StatusOK {
		t.Fatalf("status %d: %s", r.Status, r.Body)
	}
	mustContain(t, r.Body,
		`class="banner status-major_outage"`, "Major system outage",
		// active incident with latest update
		"Active incidents", fmt.Sprintf(`href="/incidents/%d"`, f.Incident.ID), "VPN connections dropping",
		"A failover<br>is in progress.", `class="badge incident-identified"`,
		// maintenance in progress and upcoming
		fmt.Sprintf(`href="/maintenance/%d"`, f.InProgress.ID), "Mail server patching", `class="badge maintenance-in_progress"`,
		fmt.Sprintf(`href="/maintenance/%d"`, f.Upcoming.ID), "ERP kernel upgrade", `class="badge maintenance-scheduled"`,
		// services grouped by category with current status and history
		"Business", "Infrastructure", "ERP description",
		`class="pill status-major_outage"`, `class="pill status-maintenance"`, `class="pill status-degraded"`,
		`class="day status-major_outage"`, `class="day nodata"`, "90 days ago", "Today",
		`<meta http-equiv="refresh" content="60">`,
	)
	// Recent incidents list only the last 7 days; cancelled maintenance is not shown.
	publicMustNotContain(t, r.Body, "Mail delayed", "Cancelled VPN work", "Old ERP outage", "No incidents reported.")

	// The uncategorised service comes first, before any category heading.
	if i, j := strings.Index(r.Body, ">Mail<"), strings.Index(r.Body, `public-group-title">Business<`); i < 0 || j < 0 || i > j {
		t.Errorf("uncategorised service should be listed first (Mail at %d, Business heading at %d)", i, j)
	}
	// 90 bars per service.
	if n := strings.Count(r.Body, `<span class="day `); n != 3*publicHistoryDays {
		t.Errorf("got %d history days, want %d", n, 3*publicHistoryDays)
	}
	mustContain(t, r.Body, "(1 incident)")
}

func TestPublicStatusPageMaintenanceOnly(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	s := &store.Service{Name: "Wiki"}
	if err := e.Store.CreateService(ctx, s); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := e.Store.CreateMaintenance(ctx, &store.Maintenance{Title: "Wiki upgrade", StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour), Services: []store.ServiceRef{{ID: s.ID}}}); err != nil {
		t.Fatal(err)
	}
	r := e.get("/")
	mustContain(t, r.Body, "Maintenance in progress", `class="banner status-maintenance"`, "Wiki upgrade", "No incidents reported.")
}

func TestPublicHistory(t *testing.T) {
	e := newTestEnv(t)
	f := publicSeed(t, e)

	r := e.get("/history")
	if r.Status != http.StatusOK {
		t.Fatalf("status %d", r.Status)
	}
	mustContain(t, r.Body,
		"VPN connections dropping", "ongoing", "Mail delayed", "lasted 1 hour 30 minutes",
		"Cancelled VPN work", `class="badge maintenance-cancelled"`, "Mail server patching",
		`class="pill status-major_outage"`, "Affected:", time.Now().Format("January 2006"),
		fmt.Sprintf(`href="/incidents/%d"`, f.Incident.ID), fmt.Sprintf(`href="/maintenance/%d"`, f.Cancelled.ID))
	// Not in the window: older than 90 days, or starting after today.
	publicMustNotContain(t, r.Body, "Old ERP outage", "ERP kernel upgrade", "Newer")

	// Newest first.
	if i, j := strings.Index(r.Body, "VPN connections dropping"), strings.Index(r.Body, "Mail delayed"); i > j {
		t.Error("history is not sorted newest first")
	}

	// The "Older" link leads to the previous window, which contains the old incident.
	today := time.Now().UTC()
	from := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1-publicHistoryDays)
	older := "/history?before=" + from.Format("2006-01-02")
	mustContain(t, r.Body, `href="`+older+`"`)
	r = e.get(older)
	if r.Status != http.StatusOK {
		t.Fatalf("older: status %d", r.Status)
	}
	mustContain(t, r.Body, "Old ERP outage", `href="/history"`, "Newer")
	publicMustNotContain(t, r.Body, "VPN connections dropping", "Mail delayed")

	// Two windows back: the "Newer" link points to a dated window.
	r = e.get("/history?before=" + from.AddDate(0, 0, -publicHistoryDays).Format("2006-01-02"))
	mustContain(t, r.Body, `href="`+older+`"`, "No incidents reported in this period.")

	// A date in the future shows the current window.
	r = e.get("/history?before=2999-01-01")
	mustContain(t, r.Body, "VPN connections dropping")
	publicMustNotContain(t, r.Body, "Newer")

	for _, bad := range []string{"yesterday", "2026-13-01", "12.11.2026", "2026-11-12T10:00"} {
		r = e.get("/history?before=" + bad)
		if r.Status != http.StatusBadRequest {
			t.Errorf("before=%s: status %d, want 400", bad, r.Status)
		}
	}
	mustContain(t, r.Body, "YYYY-MM-DD")
}

func TestPublicIncidentPage(t *testing.T) {
	e := newTestEnv(t)
	f := publicSeed(t, e)
	path := fmt.Sprintf("/incidents/%d", f.Incident.ID)

	r := e.get(path)
	if r.Status != http.StatusOK {
		t.Fatalf("status %d", r.Status)
	}
	mustContain(t, r.Body,
		"<title>VPN connections dropping · Status</title>",
		"VPN connections dropping", `class="badge incident-identified"`, `class="pill status-major_outage"`,
		"A failover<br>is in progress.", f.FirstUpdateText, "Investigating", "Identified",
		"VPN", "ERP", "Degraded performance", "Started", "(ongoing)")
	publicMustNotContain(t, r.Body, "Erika Editor", "Manage incident", "/manage/incidents/")

	// Updates are listed newest first.
	if i, j := strings.Index(r.Body, "A failover"), strings.Index(r.Body, f.FirstUpdateText); i > j {
		t.Error("updates are not sorted newest first")
	}

	// Viewers do not see authors either.
	e.loginAs("viewer@example.com", store.RoleViewer)
	r = e.get(path)
	publicMustNotContain(t, r.Body, "Erika Editor", "Manage incident")

	// Editors see the author and the manage button.
	e.logout()
	e.loginAs("editor@example.com", store.RoleEditor)
	r = e.get(path)
	mustContain(t, r.Body, "by Erika Editor", "Manage incident", `href="/manage/incidents/`+fmt.Sprint(f.Incident.ID)+`"`)

	// Resolved incident: resolved time and duration.
	r = e.get(fmt.Sprintf("/incidents/%d", f.Resolved.ID))
	mustContain(t, r.Body, "Resolved", "1 hour 30 minutes")
	publicMustNotContain(t, r.Body, "(ongoing)")

	for _, p := range []string{"/incidents/99999", "/incidents/abc", "/incidents/0"} {
		if r := e.get(p); r.Status != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, r.Status)
		}
	}
}

func TestPublicMaintenancePage(t *testing.T) {
	e := newTestEnv(t)
	f := publicSeed(t, e)
	path := fmt.Sprintf("/maintenance/%d", f.Upcoming.ID)

	r := e.get(path)
	if r.Status != http.StatusOK {
		t.Fatalf("status %d", r.Status)
	}
	mustContain(t, r.Body, "ERP kernel upgrade", `class="badge maintenance-scheduled"`, "Line one<br>line two",
		"3 hours", "ERP", "starts in")
	publicMustNotContain(t, r.Body, "/manage/maintenance/")

	r = e.get(fmt.Sprintf("/maintenance/%d", f.Cancelled.ID))
	mustContain(t, r.Body, `class="badge maintenance-cancelled"`, "Cancelled VPN work")

	e.loginAs("editor@example.com", store.RoleEditor)
	r = e.get(path)
	mustContain(t, r.Body, fmt.Sprintf(`href="/manage/maintenance/%d"`, f.Upcoming.ID), ">Edit<")

	for _, p := range []string{"/maintenance/99999", "/maintenance/x"} {
		if r := e.get(p); r.Status != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, r.Status)
		}
	}
}

func TestPublicAPIStatus(t *testing.T) {
	e := newTestEnv(t)

	// Empty installation: arrays are empty, not null.
	r := e.get("/api/status")
	if r.Status != http.StatusOK {
		t.Fatalf("status %d", r.Status)
	}
	mustContain(t, r.Body, `"services": []`, `"incidents": []`, `"maintenances": []`, `"status": "operational"`)

	f := publicSeed(t, e)
	r = e.get("/api/status")
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := r.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	var got struct {
		Status      string `json:"status"`
		StatusLabel string `json:"status_label"`
		UpdatedAt   string `json:"updated_at"`
		Services    []struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			Category    string `json:"category"`
			Status      string `json:"status"`
			StatusLabel string `json:"status_label"`
		} `json:"services"`
		Incidents []struct {
			ID        int64  `json:"id"`
			Title     string `json:"title"`
			Status    string `json:"status"`
			Impact    string `json:"impact"`
			StartedAt string `json:"started_at"`
			URL       string `json:"url"`
		} `json:"incidents"`
		Maintenances []struct {
			ID       int64  `json:"id"`
			Title    string `json:"title"`
			State    string `json:"state"`
			StartsAt string `json:"starts_at"`
			EndsAt   string `json:"ends_at"`
			URL      string `json:"url"`
		} `json:"maintenances"`
	}
	if err := json.Unmarshal([]byte(r.Body), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, r.Body)
	}
	if got.Status != "major_outage" || got.StatusLabel != "Major system outage" {
		t.Errorf("overall = %q / %q", got.Status, got.StatusLabel)
	}
	if _, err := time.Parse(time.RFC3339, got.UpdatedAt); err != nil {
		t.Errorf("updated_at %q: %v", got.UpdatedAt, err)
	}
	statuses := map[string]string{}
	for _, s := range got.Services {
		statuses[s.Name] = s.Status + "/" + s.StatusLabel + "/" + s.Category
	}
	want := map[string]string{
		"ERP":  "degraded/Degraded performance/Business",
		"VPN":  "major_outage/Major outage/Infrastructure",
		"Mail": "maintenance/Under maintenance/",
	}
	for name, w := range want {
		if statuses[name] != w {
			t.Errorf("service %s = %q, want %q", name, statuses[name], w)
		}
	}
	if len(got.Incidents) != 1 {
		t.Fatalf("got %d incidents, want 1", len(got.Incidents))
	}
	inc := got.Incidents[0]
	if inc.ID != f.Incident.ID || inc.Title != "VPN connections dropping" || inc.Status != "identified" || inc.Impact != "major_outage" {
		t.Errorf("incident = %+v", inc)
	}
	if inc.URL != fmt.Sprintf("/incidents/%d", f.Incident.ID) {
		t.Errorf("incident url = %q", inc.URL)
	}
	if ts, err := time.Parse(time.RFC3339, inc.StartedAt); err != nil || !ts.Equal(f.Incident.StartedAt) {
		t.Errorf("started_at = %q (%v), want %v", inc.StartedAt, err, f.Incident.StartedAt)
	}
	if len(got.Maintenances) != 2 {
		t.Fatalf("got %d maintenances, want 2", len(got.Maintenances))
	}
	if m := got.Maintenances[0]; m.ID != f.InProgress.ID || m.State != "in_progress" || m.StartsAt == "" || m.EndsAt == "" {
		t.Errorf("maintenance[0] = %+v", m)
	}
	if m := got.Maintenances[1]; m.ID != f.Upcoming.ID || m.State != "scheduled" {
		t.Errorf("maintenance[1] = %+v", m)
	}

	// With a base URL the links are absolute.
	publicSetSettings(t, e, func(s *store.Settings) { s.BaseURL = "https://status.example.com/" })
	r = e.get("/api/status")
	mustContain(t, r.Body,
		fmt.Sprintf(`"url": "https://status.example.com/incidents/%d"`, f.Incident.ID),
		fmt.Sprintf(`"url": "https://status.example.com/maintenance/%d"`, f.Upcoming.ID))
}

func TestPublicAccessDisabled(t *testing.T) {
	e := newTestEnv(t)
	f := publicSeed(t, e)
	publicSetSettings(t, e, func(s *store.Settings) { s.PublicAccess = false })

	paths := []string{"/", "/history", fmt.Sprintf("/incidents/%d", f.Incident.ID), fmt.Sprintf("/maintenance/%d", f.Upcoming.ID), "/api/status"}
	for _, p := range paths {
		r := e.get(p)
		if r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Location, "/login?next=") {
			t.Errorf("anonymous %s: status %d location %q, want redirect to /login", p, r.Status, r.Location)
		}
	}
	e.loginAs("viewer@example.com", store.RoleViewer)
	for _, p := range paths {
		if r := e.get(p); r.Status != http.StatusOK {
			t.Errorf("signed in %s: status %d, want 200", p, r.Status)
		}
	}
}
