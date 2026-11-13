package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// categoryOptions returns the options of the category select (value and
// whether it is selected) in their order.
func categoryOptions(t *testing.T, body string) []string {
	t.Helper()
	start := strings.Index(body, `<select id="category"`)
	end := strings.Index(body[max(start, 0):], "</select>")
	if start < 0 || end < 0 {
		t.Fatal("no category select")
	}
	var out []string
	for _, opt := range strings.Split(body[start:start+end], "<option value=\"")[1:] {
		value, rest, _ := strings.Cut(opt, `"`)
		if strings.HasPrefix(rest, " selected") {
			value += " (selected)"
		}
		out = append(out, value)
	}
	return out
}

func TestServiceCategory(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	category := func(name string) string {
		t.Helper()
		services, err := e.Store.ListServices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, svc := range services {
			if svc.Name == name {
				return svc.Category
			}
		}
		t.Fatalf("no service %q", name)
		return ""
	}
	create := func(form url.Values) {
		t.Helper()
		if r := e.post("/admin/services", form); r.Status != http.StatusSeeOther {
			t.Fatalf("create %v: %d %s", form, r.Status, r.Body)
		}
	}

	// Without services: "No category" and "New category…", which shows the
	// name field.
	r := e.get("/admin/services/new")
	mustContain(t, r.Body, `data-show-when="(new)" data-target="#category-new-field"`, `<option value="">No category</option>`,
		`<option value="(new)">New category…</option>`, `<div class="field" id="category-new-field" hidden>`,
		"Services with the same category are shown together on the status page.")
	if strings.Contains(r.Body, "<datalist") {
		t.Error("the form still has a datalist")
	}

	// A new category, then the existing one, and no category.
	create(url.Values{"name": {"VPN"}, "category": {"(new)"}, "category_new": {" Network "}})
	create(url.Values{"name": {"Firewall"}, "category": {"Network"}, "category_new": {"ignored"}})
	create(url.Values{"name": {"ERP"}, "category": {"(new)"}, "category_new": {"applications"}})
	create(url.Values{"name": {"Wiki"}, "category": {""}})
	for name, want := range map[string]string{"VPN": "Network", "Firewall": "Network", "ERP": "applications", "Wiki": ""} {
		if got := category(name); got != want {
			t.Errorf("category of %s: %q, want %q", name, got, want)
		}
	}

	// The existing categories are offered sorted ("No category", the first
	// option, is the default), the service's own category is selected; a
	// different spelling selects the existing category.
	got := strings.Join(categoryOptions(t, e.get("/admin/services/new").Body), ", ")
	if want := ", applications, Network, (new)"; got != want {
		t.Errorf("options of a new service: %q, want %q", got, want)
	}
	vpn := &store.Service{Name: "Router", Category: "NETWORK"}
	if err := e.Store.CreateService(ctx, vpn); err != nil {
		t.Fatal(err)
	}
	got = strings.Join(categoryOptions(t, e.get("/admin/services/"+strconv.FormatInt(vpn.ID, 10)).Body), ", ")
	if want := ", applications, Network (selected), (new)"; got != want {
		t.Errorf("options of a service with another spelling: %q, want %q", got, want)
	}

	// Errors keep the chosen option and the entered name (the field stays
	// visible); a category that is not offered (e.g. sent by a script) is
	// kept as well.
	r = e.post("/admin/services", url.Values{"name": {""}, "category": {"(new)"}, "category_new": {"Storage"}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("create without a name: %d", r.Status)
	}
	mustContain(t, r.Body, `<option value="(new)" selected>New category…</option>`, `<div class="field" id="category-new-field">`,
		`name="category_new" type="text" value="Storage"`)
	r = e.post("/admin/services", url.Values{"name": {"NAS"}, "category": {"(new)"}, "category_new": {" "}})
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("new category without a name: %d", r.Status)
	}
	mustContain(t, r.Body, "Enter the name of the new category", `name="category_new" type="text" value="" maxlength="100" aria-invalid="true"`)
	r = e.post("/admin/services", url.Values{"name": {"NAS"}, "category": {"(new)"}, "category_new": {strings.Repeat("x", 101)}})
	mustContain(t, r.Body, "The category is too long", `name="category_new" type="text" value="`+strings.Repeat("x", 101)+`" maxlength="100" aria-invalid="true"`)
	r = e.post("/admin/services", url.Values{"name": {""}, "category": {"Legacy"}})
	if got := strings.Join(categoryOptions(t, r.Body), ", "); got != ", applications, Legacy (selected), Network, (new)" {
		t.Errorf("options after an error with another category: %q", got)
	}
	create(url.Values{"name": {"Mainframe"}, "category": {"Legacy"}})
	if got := category("Mainframe"); got != "Legacy" {
		t.Errorf("category not offered: %q", got)
	}
}
