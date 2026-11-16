package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// An administrator clears an alert whose recovery the tool did not send.
func TestMonitoringClearAlert(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	if err := e.Store.CreateService(ctx, &store.Service{Name: "VPN"}); err != nil {
		t.Fatal(err)
	}
	in, hook := monIntegration(t, e, "generic", false)
	monHook(t, e, hook, "application/json", `{"key": "gw", "service": "VPN", "status": "down", "title": "VPN gateway down"}`)
	page := fmt.Sprintf("/admin/monitoring/%d", in.ID)
	mustContain(t, e.get(page).Body, `action="/admin/monitoring/1/alerts/clear"`, `<input type="hidden" name="key" value="gw">`,
		`data-confirm="Clear the alert “VPN gateway down”?`)

	r := e.post(page+"/alerts/clear", url.Values{"key": {"gw"}})
	if r.Status != http.StatusSeeOther || r.Location != page {
		t.Fatalf("clear: %d %s", r.Status, r.Location)
	}
	body := e.get(page).Body
	mustContain(t, body, "The alert has been cleared: “VPN gateway down” (cleared by "+testAdminUser+"): resolved incident #1 of VPN.",
		"No alert of this integration is firing.")
	inc, err := e.Store.GetIncident(ctx, 1)
	if err != nil || inc.Status != store.IncidentResolved {
		t.Fatalf("incident: %+v %v", inc, err)
	}
	r = e.post(page+"/alerts/clear", url.Values{"key": {"gw"}})
	mustContain(t, e.get(page).Body, "This alert is no longer firing.")

	editor := monSecondBrowser(e)
	editor.loginAs("editor@example.com", store.RoleEditor)
	if r := editor.post(page+"/alerts/clear", url.Values{"key": {"gw"}}); r.Status != http.StatusForbidden {
		t.Fatalf("editor: %d", r.Status)
	}
}

// An integration whose tool this version does not know (e.g. after going
// back to an older version) keeps it when other settings are saved.
func TestMonitoringUnknownProvider(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	in := &store.Integration{Name: "New tool", Provider: "newtool", Notify: true}
	if err := e.Store.CreateIntegration(ctx, in, "hash-newtool"); err != nil {
		t.Fatal(err)
	}
	name := "Unknown tool “newtool” (read as Other tool or script)"
	mustContain(t, e.get("/admin/monitoring").Body, "<td>"+name+"</td>")
	page := fmt.Sprintf("/admin/monitoring/%d", in.ID)
	mustContain(t, e.get(page).Body, `<option value="newtool" selected>`+name+`</option>`, name+" · 0 firing alerts")

	r := e.post(page, url.Values{"name": {"Renamed"}, "provider": {"newtool"}, "notify": {"on"}})
	if r.Status != http.StatusSeeOther {
		t.Fatalf("save: %d", r.Status)
	}
	got, err := e.Store.GetIntegration(ctx, in.ID)
	if err != nil || got.Provider != "newtool" || got.Name != "Renamed" {
		t.Fatalf("saved: %+v %v", got, err)
	}
	if r := e.post(page, url.Values{"name": {"Renamed"}, "provider": {"othertool"}}); r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("another unknown tool: %d", r.Status)
	}
	if r := e.post("/admin/monitoring", url.Values{"name": {"X"}, "provider": {"newtool"}}); r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("new integration with an unknown tool: %d", r.Status)
	}
}

// A settings error is announced at the top of the long integration page.
func TestMonitoringSaveErrorAtTop(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	in, _ := monIntegration(t, e, "generic", true)
	r := e.post(fmt.Sprintf("/admin/monitoring/%d", in.ID), url.Values{"name": {"   "}, "provider": {"generic"}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("save: %d", r.Status)
	}
	top := strings.Index(r.Body, `The settings were not saved: <a href="#settings">correct the highlighted field`)
	if top < 0 || top > strings.Index(r.Body, `id="webhook"`) {
		t.Fatalf("no error notice above the webhook card (at %d)", top)
	}
}

// The incident page of an automatic incident lists at most 20 alerts and
// does not promise an automatic resolution when no alert is attached.
func TestMonitoringIncidentPageAlerts(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	if err := e.Store.CreateService(ctx, &store.Service{Name: "VPN"}); err != nil {
		t.Fatal(err)
	}
	in, hook := monIntegration(t, e, "generic", false)
	var alerts []string
	for i := range 105 {
		alerts = append(alerts, fmt.Sprintf(`{"key": "k%d", "service": "VPN", "status": "down", "title": "Check %d"}`, i, i))
	}
	monHook(t, e, hook, "application/json", "["+strings.Join(alerts, ",")+"]")

	body := e.get("/manage/incidents/1").Body
	mustContain(t, body, "is resolved automatically when its alerts have cleared", "<li>… and 85 more alerts</li>")
	if n := strings.Count(body, "firing since"); n != 20 {
		t.Errorf("%d alerts listed on the incident page, want 20", n)
	}
	page := e.get(fmt.Sprintf("/admin/monitoring/%d", in.ID)).Body
	mustContain(t, page, "… and 5 more firing alerts, not listed.")
	if n := strings.Count(page, `name="key"`); n != 100 {
		t.Errorf("%d alerts listed on the integration page, want 100", n)
	}

	r := e.post(fmt.Sprintf("/admin/monitoring/%d/delete", in.ID), nil)
	if r.Status != http.StatusSeeOther {
		t.Fatalf("delete: %d", r.Status)
	}
	mustContain(t, e.get("/admin/monitoring").Body,
		"The open incident of its alerts stays open until an editor resolves it (or other monitoring alerts attached to it clear).")
	body = e.get("/manage/incidents/1").Body
	mustContain(t, body, "but no monitoring alert is attached to it any more")
	if strings.Contains(body, "is resolved automatically") {
		t.Error("the page promises an automatic resolution")
	}
}

// Deleting an integration whose alerts have no open incident says nothing
// about open incidents.
func TestMonitoringDeleteWithoutOpenIncidents(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	if err := e.Store.CreateService(ctx, &store.Service{Name: "VPN"}); err != nil {
		t.Fatal(err)
	}
	in, hook := monIntegration(t, e, "generic", false)
	monHook(t, e, hook, "application/json", `{"service": "VPN", "status": "down"}`)
	if err := e.Store.AddIncidentUpdate(ctx, 1, &store.IncidentUpdate{Status: store.IncidentResolved, Message: "Fixed."}, nil); err != nil {
		t.Fatal(err)
	}
	e.post(fmt.Sprintf("/admin/monitoring/%d/delete", in.ID), nil)
	body := e.get("/admin/monitoring").Body
	mustContain(t, body, "The integration “Prometheus” has been deleted. Its webhook URL no longer works.")
	if strings.Contains(body, "stays open") || strings.Contains(body, "stay open") {
		t.Error("the message mentions open incidents")
	}
}

// In setup mode the webhook URLs answer 503 instead of a redirect to the
// setup page, which tools would take for a delivery.
func TestSetupModeWebhooks(t *testing.T) {
	m := newSetupModeEnv(t, nil)
	for _, path := range []string{"/hooks/abc", "/hooks"} {
		req, _ := http.NewRequest(http.MethodPost, m.Server.URL+path, strings.NewReader(`{"service": "VPN", "status": "down"}`))
		req.Header.Set("Content-Type", "application/json")
		r := m.do(req)
		if r.Status != http.StatusServiceUnavailable || r.Header.Get("Retry-After") == "" || !strings.Contains(r.Body, `"status":"error"`) {
			t.Fatalf("%s: %d %v %s", path, r.Status, r.Header, r.Body)
		}
	}
	if r := m.get("/"); r.Status != http.StatusSeeOther || r.Location != "/setup" {
		t.Fatalf("other pages: %d %s", r.Status, r.Location)
	}
}
