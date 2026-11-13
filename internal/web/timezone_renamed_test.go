package web

import (
	"io/fs"
	"regexp"
	"slices"
	"testing"
	"time"
)

// TestBrowserTimezoneRenamedZones: the setup wizard (static/app.js)
// preselects the browser's time zone. Chromium reports some zones under
// their old (CLDR) names, so app.js maps them to the offered names. Every
// mapping must lead to an offered zone with the same rules, and the old
// names Chromium uses for offered zones must be mapped.
func TestBrowserTimezoneRenamedZones(t *testing.T) {
	js, err := fs.ReadFile(assets, "static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)var renamedZones = \{(.*?)\};`).FindSubmatch(js)
	if block == nil {
		t.Fatal("renamedZones not found in app.js")
	}
	renamed := map[string]string{}
	for _, m := range regexp.MustCompile(`"([^"]+)": "([^"]+)"`).FindAllStringSubmatch(string(block[1]), -1) {
		renamed[m[1]] = m[2]
	}

	// The old names of offered zones that Chromium reports (checked with
	// Chromium 153 for all offered zones).
	for _, old := range []string{
		"Asia/Calcutta", "Asia/Saigon", "Asia/Katmandu", "Asia/Rangoon", "Europe/Kiev", "America/Godthab",
		"America/Buenos_Aires", "America/Catamarca", "America/Cordoba", "America/Jujuy", "America/Mendoza",
		"America/Indianapolis", "America/Louisville", "America/Coral_Harbour", "Atlantic/Faeroe", "Africa/Asmera",
		"Pacific/Truk", "Pacific/Ponape", "Pacific/Enderbury",
	} {
		if renamed[old] == "" {
			t.Errorf("%s is not mapped", old)
		}
	}

	when := []time.Time{time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	for old, zone := range renamed {
		if !slices.Contains(adminTimezones, zone) {
			t.Errorf("%s is mapped to %s, which is not offered", old, zone)
		}
		if slices.Contains(adminTimezones, old) {
			t.Errorf("%s is offered itself and must not be mapped", old)
		}
		a, errA := time.LoadLocation(old)
		b, errB := time.LoadLocation(zone)
		if errA != nil || errB != nil {
			t.Errorf("%s → %s: %v %v", old, zone, errA, errB)
			continue
		}
		for _, w := range when {
			_, offA := w.In(a).Zone()
			_, offB := w.In(b).Zone()
			if offA != offB {
				t.Errorf("%s and %s differ on %s: %d and %d", old, zone, w.Format("2006-01-02"), offA, offB)
			}
		}
	}
}
