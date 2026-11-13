package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

func renderSettings() *store.Settings {
	st := store.DefaultSettings()
	st.SiteName = "Acme IT"
	st.BaseURL = "https://status.example.com/"
	st.Timezone = "Europe/Berlin"
	return &st
}

func testIncident() *store.Incident {
	started := time.Date(2026, 11, 12, 13, 5, 0, 0, time.UTC) // 14:05 CET
	return &store.Incident{
		ID:        42,
		Title:     "Mail delivery delayed",
		Status:    store.IncidentInvestigating,
		StartedAt: started,
		Services: []store.ServiceImpact{
			{ServiceID: 1, ServiceName: "Exchange Online", Impact: store.StatusMajorOutage},
			{ServiceID: 2, ServiceName: "VPN", Impact: store.StatusDegraded},
		},
		Updates: []store.IncidentUpdate{
			{ID: 7, IncidentID: 42, Status: store.IncidentInvestigating, Message: "We are investigating.\nMore soon.", CreatedAt: started},
		},
	}
}

func assertContains(t *testing.T, what, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("%s does not contain %q:\n%s", what, w, s)
		}
	}
}

func assertNotContains(t *testing.T, what, s string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(s, w) {
			t.Errorf("%s contains %q:\n%s", what, w, s)
		}
	}
}

func TestRenderIncidentNew(t *testing.T) {
	inc := testIncident()
	e := RenderIncident(renderSettings(), inc, &inc.Updates[0], true)

	if want := "[Acme IT] Investigating: Mail delivery delayed"; e.Subject != want {
		t.Errorf("Subject = %q, want %q", e.Subject, want)
	}
	assertContains(t, "text", e.Text,
		"Mail delivery delayed\n",
		"A new incident has been reported.",
		"Status:  Investigating\n",
		"Started: Thu 12 Nov 2026 14:05 CET\n",
		"We are investigating.\nMore soon.\n",
		"Affected services:\n- Exchange Online: Major outage\n- VPN: Degraded performance\n",
		"View incident: https://status.example.com/incidents/42\n",
		"\n-- \n"+emailFooter+"\n",
	)
	assertContains(t, "html", e.HTML,
		"<!DOCTYPE html>",
		"Acme IT",
		"Mail delivery delayed",
		"Thu 12 Nov 2026 14:05 CET",
		"We are investigating.<br>\nMore soon.",
		"Exchange Online", "Major outage", "#d32f2f",
		"VPN", "Degraded performance", "#d4a106",
		`href="https://status.example.com/incidents/42"`,
		emailFooter,
	)
	assertNotContains(t, "html", e.HTML, "ZgotmplZ")
}

func TestRenderIncidentUpdateAndResolved(t *testing.T) {
	inc := testIncident()
	upd := store.IncidentUpdate{Status: store.IncidentMonitoring, Message: "A fix has been deployed."}
	inc.Updates = append([]store.IncidentUpdate{upd}, inc.Updates...)

	e := RenderIncident(renderSettings(), inc, nil, false) // nil → latest update
	if want := "[Acme IT] Update – Monitoring: Mail delivery delayed"; e.Subject != want {
		t.Errorf("Subject = %q, want %q", e.Subject, want)
	}
	assertContains(t, "text", e.Text, "Status:  Monitoring", "A fix has been deployed.", "There is an update on this incident.")
	assertNotContains(t, "text", e.Text, "We are investigating.")

	resolvedAt := time.Date(2026, 11, 12, 15, 30, 0, 0, time.UTC)
	inc.Status = store.IncidentResolved
	inc.ResolvedAt = &resolvedAt
	res := store.IncidentUpdate{Status: store.IncidentResolved, Message: "All good again."}
	e = RenderIncident(renderSettings(), inc, &res, false)
	if want := "[Acme IT] Resolved: Mail delivery delayed"; e.Subject != want {
		t.Errorf("Subject = %q, want %q", e.Subject, want)
	}
	assertContains(t, "text", e.Text, "Resolved: Thu 12 Nov 2026 16:30 CET", "This incident has been resolved.", "All good again.")
	assertContains(t, "html", e.HTML, "#1f9d55") // resolved badge is green
}

func TestRenderIncidentEscapingAndNoBaseURL(t *testing.T) {
	inc := testIncident()
	inc.Title = `<script>alert("x")</script> & 'Co'`
	inc.Services[0].ServiceName = "R&D <Lab>"
	upd := store.IncidentUpdate{Status: store.IncidentIdentified, Message: `<img src=x onerror=alert(1)> "quoted" & done`}
	st := renderSettings()
	st.BaseURL = ""
	st.SiteName = "IT <Status>"

	e := RenderIncident(st, inc, &upd, true)
	if want := `[IT <Status>] Identified: <script>alert("x")</script> & 'Co'`; e.Subject != want {
		t.Errorf("Subject = %q, want %q", e.Subject, want)
	}
	assertContains(t, "text", e.Text, `<script>alert("x")</script> & 'Co'`, `<img src=x onerror=alert(1)> "quoted" & done`, "- R&D <Lab>: Major outage")
	assertNotContains(t, "html", e.HTML, "<script>", "<img", "<Lab>", "<Status>")
	assertContains(t, "html", e.HTML,
		"&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt; &amp; &#39;Co&#39;",
		"&lt;img src=x onerror=alert(1)&gt; &#34;quoted&#34; &amp; done",
		"R&amp;D &lt;Lab&gt;",
		"IT &lt;Status&gt;",
	)
	// No link without a base URL.
	assertNotContains(t, "text", e.Text, "View incident", "/incidents/")
	assertNotContains(t, "html", e.HTML, "View incident", "/incidents/")
}

func TestRenderIncidentNilSettings(t *testing.T) {
	inc := testIncident()
	e := RenderIncident(nil, inc, nil, true)
	if want := "[Status] Investigating: Mail delivery delayed"; e.Subject != want {
		t.Errorf("Subject = %q, want %q", e.Subject, want)
	}
	assertContains(t, "text", e.Text, "Started: Thu 12 Nov 2026 13:05 UTC")
}

func TestRenderMaintenance(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("time zone database not available")
	}
	m := &store.Maintenance{
		ID:          9,
		Title:       "Firewall upgrade",
		Description: "Firmware update of the core firewall.\nVPN will reconnect.",
		StartsAt:    time.Date(2026, 11, 16, 22, 0, 0, 0, berlin),
		EndsAt:      time.Date(2026, 11, 16, 23, 30, 0, 0, berlin),
		Services:    []store.ServiceRef{{ID: 2, Name: "VPN"}, {ID: 3, Name: "Internet"}},
	}
	st := renderSettings()

	tests := []struct {
		ev      MaintenanceEvent
		subject string
		intro   string
	}{
		{MaintenanceEventScheduled, "[Acme IT] Scheduled maintenance: Firewall upgrade", "has been scheduled"},
		{MaintenanceEventUpdated, "[Acme IT] Maintenance updated: Firewall upgrade", "has been changed"},
		{MaintenanceEventCancelled, "[Acme IT] Maintenance cancelled: Firewall upgrade", "has been cancelled"},
		{MaintenanceEventStarted, "[Acme IT] Maintenance started: Firewall upgrade", "has started"},
		{MaintenanceEventCompleted, "[Acme IT] Maintenance completed: Firewall upgrade", "has been completed"},
	}
	for _, tc := range tests {
		e := RenderMaintenance(st, m, tc.ev)
		if e.Subject != tc.subject {
			t.Errorf("%s: Subject = %q, want %q", tc.ev, e.Subject, tc.subject)
		}
		assertContains(t, string(tc.ev)+" text", e.Text,
			tc.intro,
			"When: Mon 16 Nov 2026 22:00 – 23:30 CET\n",
			"Firmware update of the core firewall.\nVPN will reconnect.\n",
			"Affected services:\n- VPN\n- Internet\n",
			"View maintenance: https://status.example.com/maintenance/9\n",
			emailFooter,
		)
		assertContains(t, string(tc.ev)+" html", e.HTML,
			"Mon 16 Nov 2026 22:00 – 23:30 CET",
			"Firmware update of the core firewall.<br>\nVPN will reconnect.",
			`href="https://status.example.com/maintenance/9"`,
		)
	}
	assertContains(t, "scheduled html", RenderMaintenance(st, m, MaintenanceEventScheduled).HTML, "#2f6fdb")

	st.BaseURL = ""
	e := RenderMaintenance(st, m, MaintenanceEventScheduled)
	assertNotContains(t, "text", e.Text, "/maintenance/")
	assertNotContains(t, "html", e.HTML, "/maintenance/")
}

func TestFormatWindow(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("time zone database not available")
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("time zone database not available")
	}
	utc := func(y int, mo time.Month, d, h, mi int) time.Time { return time.Date(y, mo, d, h, mi, 0, 0, time.UTC) }

	tests := []struct {
		start, end time.Time
		loc        *time.Location
		want       string
	}{
		// same day
		{utc(2026, 11, 16, 21, 0), utc(2026, 11, 16, 22, 30), berlin, "Mon 16 Nov 2026 22:00 – 23:30 CET"},
		// summer time
		{utc(2026, 7, 1, 20, 0), utc(2026, 7, 1, 21, 0), berlin, "Wed 1 Jul 2026 22:00 – 23:00 CEST"},
		// over midnight
		{utc(2026, 11, 16, 22, 0), utc(2026, 11, 17, 1, 0), berlin, "Mon 16 Nov 2026 23:00 CET – Tue 17 Nov 2026 02:00 CET"},
		// end of summer time during the window (25 Oct 2026, 03:00 CEST → 02:00 CET)
		{utc(2026, 10, 24, 23, 0), utc(2026, 10, 25, 3, 0), berlin, "Sun 25 Oct 2026 01:00 CEST – Sun 25 Oct 2026 04:00 CET"},
		// other zone: same instant, different local day
		{utc(2026, 11, 16, 21, 0), utc(2026, 11, 16, 22, 30), ny, "Mon 16 Nov 2026 16:00 – 17:30 EST"},
		{utc(2026, 11, 16, 21, 0), utc(2026, 11, 16, 22, 30), time.UTC, "Mon 16 Nov 2026 21:00 – 22:30 UTC"},
	}
	for _, tc := range tests {
		if got := formatWindow(tc.start, tc.end, tc.loc); got != tc.want {
			t.Errorf("formatWindow(%v, %v, %v) = %q, want %q", tc.start, tc.end, tc.loc, got, tc.want)
		}
	}
}

// TestRenderButtonColor: the link button uses the light-mode accent colour
// of the branding with readable text; without one the e-mail is unchanged.
func TestRenderButtonColor(t *testing.T) {
	const defaultButton = `<td bgcolor="#1d5fd1" style="background-color:#1d5fd1;border-radius:6px;padding:10px 18px;">` +
		`<a href="https://status.example.com/incidents/42" style="color:#ffffff;text-decoration:none;font-weight:bold;">View incident</a></td>`
	inc := testIncident()
	st := renderSettings()
	assertContains(t, "default html", RenderIncident(st, inc, nil, true).HTML, defaultButton)
	st.ThemeAccentLight = "not a colour" // never stored, but harmless
	assertContains(t, "html with an invalid accent", RenderIncident(st, inc, nil, true).HTML, defaultButton)

	tests := []struct{ accent, bg, text string }{
		{"#ffd400", "#ffd400", "#111111"}, // light accent: dark text
		{"#002855", "#002855", "#ffffff"}, // dark accent: white text
	}
	for _, tc := range tests {
		st.ThemeAccentLight = tc.accent
		want := `<td bgcolor="` + tc.bg + `" style="background-color:` + tc.bg + `;border-radius:6px;padding:10px 18px;">` +
			`<a href="https://status.example.com/incidents/42" style="color:` + tc.text + `;text-decoration:none;font-weight:bold;">View incident</a></td>`
		e := RenderIncident(st, inc, nil, true)
		assertContains(t, tc.accent+" incident html", e.HTML, want)
		assertNotContains(t, tc.accent+" incident html", e.HTML, "#1d5fd1", "ZgotmplZ")
		m := &store.Maintenance{ID: 9, Title: "Firewall upgrade", StartsAt: inc.StartedAt, EndsAt: inc.StartedAt.Add(time.Hour)}
		e = RenderMaintenance(st, m, MaintenanceEventScheduled)
		assertContains(t, tc.accent+" maintenance html", e.HTML,
			`bgcolor="`+tc.bg+`" style="background-color:`+tc.bg+`;`, `style="color:`+tc.text+`;text-decoration:none;font-weight:bold;">View maintenance</a>`)
	}
}
