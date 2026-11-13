package web

import (
	"html"
	"slices"
	"strings"
	"testing"
	"time"
)

// tzSelected returns the selected options of c.
func tzSelected(c adminTZChoices) []adminTZOption {
	var out []adminTZOption
	for _, o := range c.Top {
		if o.Selected {
			out = append(out, o)
		}
	}
	for _, g := range c.Groups {
		for _, o := range g.Options {
			if o.Selected {
				out = append(out, o)
			}
		}
	}
	return out
}

// tzLabel returns the label of the option with value v ("" if not offered).
func tzLabel(c adminTZChoices, v string) string {
	for _, g := range c.Groups {
		for _, o := range g.Options {
			if o.Value == v {
				return o.Label
			}
		}
	}
	return ""
}

func TestTimezoneChoices(t *testing.T) {
	winter := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	summer := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	// Every offered zone can be loaded and is listed once.
	if n := len(adminZones()); n != len(adminTimezones) || n < 400 {
		t.Errorf("%d of %d time zones loaded", n, len(adminTimezones))
	}
	if s := slices.Clone(adminTimezones); len(slices.Compact(slices.Sorted(slices.Values(s)))) != len(s) {
		t.Error("a time zone is listed twice")
	}

	// UTC comes first, then the regions in alphabetical order, each sorted
	// by city; the labels show the current offset.
	c := adminTimezoneChoices("Europe/Berlin", winter)
	if len(c.Top) != 1 || c.Top[0] != (adminTZOption{Value: "UTC", Label: "UTC"}) {
		t.Errorf("top options: %+v", c.Top)
	}
	var regions []string
	for _, g := range c.Groups {
		regions = append(regions, g.Label)
		if !slices.IsSortedFunc(g.Options, func(a, b adminTZOption) int {
			return strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label))
		}) {
			t.Errorf("region %s is not sorted", g.Label)
		}
	}
	want := []string{"Africa", "Americas", "Antarctica", "Arctic", "Asia", "Atlantic Ocean", "Australia", "Europe", "Indian Ocean", "Pacific Ocean"}
	if !slices.Equal(regions, want) {
		t.Errorf("regions %q, want %q", regions, want)
	}
	if got := tzSelected(c); len(got) != 1 || got[0].Value != "Europe/Berlin" || got[0].Label != "Berlin (UTC+01:00)" {
		t.Errorf("selected: %+v", got)
	}
	for zone, label := range map[string]string{
		"America/Argentina/Buenos_Aires": "Buenos Aires, Argentina (UTC-03:00)",
		"America/North_Dakota/New_Salem": "New Salem, North Dakota (UTC-06:00)",
		"America/St_Johns":               "St Johns (UTC-03:30)",
		"Asia/Kolkata":                   "Kolkata (UTC+05:30)",
		"Asia/Kathmandu":                 "Kathmandu (UTC+05:45)",
		"Europe/London":                  "London (UTC+00:00)",
	} {
		if got := tzLabel(c, zone); got != label {
			t.Errorf("label of %s: %q, want %q", zone, got, label)
		}
	}
	if got := tzLabel(adminTimezoneChoices("UTC", summer), "Europe/Berlin"); got != "Berlin (UTC+02:00)" {
		t.Errorf("label in summer: %q", got)
	}

	// UTC is the default; a value that is not offered is added after UTC and
	// selected, so that saving the form keeps it.
	for _, v := range []string{"", "UTC"} {
		if got := tzSelected(adminTimezoneChoices(v, winter)); len(got) != 1 || got[0].Value != "UTC" {
			t.Errorf("%q: selected %+v", v, got)
		}
	}
	for v, label := range map[string]string{"US/Eastern": "US/Eastern (UTC-05:00)", "Mars/Olympus": "Mars/Olympus"} {
		c := adminTimezoneChoices(v, winter)
		if len(c.Top) != 2 || c.Top[1] != (adminTZOption{Value: v, Label: label, Selected: true}) || len(tzSelected(c)) != 1 {
			t.Errorf("%q: top options %+v", v, c.Top)
		}
	}
}

func TestTimezoneSelectSettings(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()

	r := e.get("/admin/settings")
	mustContain(t, html.UnescapeString(r.Body), `<select id="timezone" name="timezone" aria-describedby="timezone_help">`,
		`<option value="UTC" selected>UTC</option>`, `<optgroup label="Europe">`, `<option value="Europe/Berlin">Berlin (UTC+0`,
		"Times on the status page and in e-mails are shown in this time zone, and times you enter are read in it.")
	if strings.Contains(r.Body, "<datalist") || strings.Contains(r.Body, "data-browser-timezone") {
		t.Error("the settings page has a datalist or selects the browser's time zone")
	}

	f := adminSettingsForm(adminSettings(t, e))
	f.Set("timezone", "Europe/Berlin")
	r = adminFollow(t, e, e.post("/admin/settings", f))
	mustContain(t, html.UnescapeString(r.Body), "Settings saved.", `<option value="Europe/Berlin" selected>Berlin (UTC+0`)

	// A time zone that is not offered (a valid IANA name) stays selected and
	// is kept when the settings are saved again.
	f.Set("timezone", "US/Pacific")
	r = adminFollow(t, e, e.post("/admin/settings", f))
	mustContain(t, r.Body, `<option value="US/Pacific" selected>US/Pacific (UTC-0`)
	f = adminSettingsForm(adminSettings(t, e))
	adminFollow(t, e, e.post("/admin/settings", f))
	if st := adminSettings(t, e); st.Timezone != "US/Pacific" {
		t.Errorf("time zone after saving again: %q", st.Timezone)
	}

	// An invalid value is shown with the error.
	f.Set("timezone", "Mars/Olympus")
	r = e.post("/admin/settings", f)
	mustContain(t, r.Body, `aria-invalid="true">`, `<option value="Mars/Olympus" selected>Mars/Olympus</option>`, "Unknown time zone")
}

func TestTimezoneSelectSetup(t *testing.T) {
	e := newTestEnvNoAdmin(t)

	// The browser's time zone is selected by app.js on the first display
	// only (while the select shows the default UTC).
	r := e.get("/setup")
	mustContain(t, r.Body, `data-browser-timezone="UTC"`, `<option value="UTC" selected>UTC</option>`, `<optgroup label="Americas">`)

	f := setupAdminForm()
	f.Set("timezone", "Asia/Tokyo")
	f.Set("password_confirm", "something-else")
	r = e.post("/setup", f)
	if r.Status != 422 {
		t.Fatalf("setup with an error: %d", r.Status)
	}
	mustContain(t, html.UnescapeString(r.Body), `<option value="Asia/Tokyo" selected>Tokyo (UTC+09:00)</option>`)
	if strings.Contains(r.Body, "data-browser-timezone") {
		t.Error("the form shown again after an error replaces the chosen time zone with the browser's")
	}

	f.Set("password_confirm", f.Get("password"))
	if r := e.post("/setup", f); r.Status != 303 {
		t.Fatalf("setup: %d %s", r.Status, r.Body)
	}
	if st := adminSettings(t, e); st.Timezone != "Asia/Tokyo" {
		t.Errorf("time zone after setup: %q", st.Timezone)
	}
}
