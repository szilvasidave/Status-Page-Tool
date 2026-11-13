package web

// Tests of the shared UI: layout chrome, flash kinds and the stylesheet
// vocabulary.

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// An action that succeeded although the notification e-mails could not be
// queued is reported with a warning, not an error.
func TestUIManageDoneWarning(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.manageDone(rec, httptest.NewRequest(http.MethodPost, "/manage/incidents", nil), "/manage", "The incident has been reported.", "However, …", true)
	r := httptest.NewRequest(http.MethodGet, "/manage", nil)
	for _, c := range rec.Result().Cookies() {
		r.AddCookie(c)
	}
	f := s.popFlash(httptest.NewRecorder(), r)
	if f == nil || f.Kind != "warning" || f.Message != "The incident has been reported. However, …" {
		t.Fatalf("flash = %+v, want a warning", f)
	}
}

func TestUIFlashKinds(t *testing.T) {
	s := &Server{}
	for kind, want := range map[string]bool{"success": true, "info": true, "warning": true, "error": true, "bogus": false} {
		rec := httptest.NewRecorder()
		s.flash(rec, httptest.NewRequest(http.MethodGet, "/", nil), kind, "msg")
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range rec.Result().Cookies() {
			r.AddCookie(c)
		}
		if got := s.popFlash(httptest.NewRecorder(), r) != nil; got != want {
			t.Errorf("kind %q accepted = %v, want %v", kind, got, want)
		}
	}
}

// Long flash messages (e.g. a directory sync error) are shortened so that the
// cookie stays small; multi-byte characters are not cut.
func TestUIFlashLong(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.flash(rec, httptest.NewRequest(http.MethodGet, "/", nil), "error", "Sync failed: "+strings.Repeat("é", 3000))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		if len(c.Value) > 1500 {
			t.Errorf("flash cookie has %d bytes", len(c.Value))
		}
		r.AddCookie(c)
	}
	f := s.popFlash(httptest.NewRecorder(), r)
	if f == nil || !strings.HasPrefix(f.Message, "Sync failed: éé") || !strings.HasSuffix(f.Message, "é…") || !utf8.ValidString(f.Message) {
		t.Fatalf("flash = %+v", f)
	}
	if got := plural(1, "day") + ", " + plural(3, "day"); got != "1 day, 3 days" {
		t.Errorf("plural: %q", got)
	}
}

func TestUILayout(t *testing.T) {
	e := newTestEnv(t)
	signIn := `<a href="/login" class="btn btn-small">Sign in</a>`
	mustContain(t, e.get("/history").Body, signIn, `<span class="brand-name">`)
	if r := e.get("/login"); strings.Contains(r.Body, signIn) {
		t.Error("the sign-in page links to itself in the header")
	}

	// The admin sections are shown on admin pages only, once, with the
	// current section marked, and the flash below them.
	e.loginAdmin()
	r := e.get("/admin/groups")
	if n := strings.Count(r.Body, `class="subnav"`); n != 1 {
		t.Errorf("admin page has %d sub navigations, want 1", n)
	}
	mustContain(t, r.Body, `href="/admin/groups" aria-current="page">Groups</a>`)
	r = e.post("/admin/groups", map[string][]string{"external_id": {"ops"}})
	r = e.get(r.Location)
	if i, j := strings.Index(r.Body, `class="subnav"`), strings.Index(r.Body, `class="flash flash-success"`); i < 0 || j < i {
		t.Errorf("flash (at %d) is not below the sub navigation (at %d)", j, i)
	}
	for _, path := range []string{"/", "/manage", "/admin/settings", "/account"} {
		if n := strings.Count(e.get(path).Body, `rel="stylesheet"`); n != 1 {
			t.Errorf("%s links %d stylesheets, want 1", path, n)
		}
	}

	e.logout()
	e.loginAs("editor@example.com", store.RoleEditor)
	if r := e.get("/admin/settings"); r.Status != http.StatusForbidden || strings.Contains(r.Body, `class="subnav"`) {
		t.Errorf("editor on an admin page: status %d, admin sections shown: %v", r.Status, strings.Contains(r.Body, `class="subnav"`))
	}
}

func TestUISetupHidesNavigation(t *testing.T) {
	e := newTestEnvNoAdmin(t)
	r := e.get("/setup")
	if strings.Contains(r.Body, `class="site-nav"`) || strings.Contains(r.Body, "Sign in") {
		t.Error("the setup wizard shows the navigation")
	}
}

// SSO users see on their account page that e-mails are not sent while
// e-mail is not configured.
func TestUIAccountEmailNotice(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.loginAs("viewer@example.com", store.RoleViewer)
	svc := &store.Service{Name: "VPN"}
	if err := e.Store.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	groups, err := e.Store.ListGroups(ctx)
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups: %v %v", groups, err)
	}
	if err := e.Store.SetGroupServices(ctx, groups[0].ID, []int64{svc.ID}); err != nil {
		t.Fatal(err)
	}
	const notice = "E-mail is not configured, so no notifications are sent."
	mustContain(t, e.get("/account").Body, "VPN", notice)

	st, err := e.Store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.SMTPHost, st.SMTPFrom = "localhost", "status@example.com"
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.get("/account").Body, notice) {
		t.Error("e-mail notice shown although e-mail is configured")
	}
}

// Every class used in a template is styled in style.css, and style.css has
// no rules for classes that no template uses (generated classes such as
// status-<status> excepted).
func TestUIStylesheetVocabulary(t *testing.T) {
	css, err := fs.ReadFile(assets, "static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	src := regexp.MustCompile(`(?s)/\*.*?\*/|"[^"]*"`).ReplaceAllString(string(css), "")
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.([a-zA-Z][\w-]*)`).FindAllStringSubmatch(src, -1) {
		defined[m[1]] = true
	}

	used := map[string]bool{}
	files, _ := fs.Glob(assets, "templates/*.html")
	partials, _ := fs.Glob(assets, "templates/partials/*.html")
	actions := regexp.MustCompile(`(?s){{.*?}}`)
	for _, f := range append(files, partials...) {
		b, err := fs.ReadFile(assets, f)
		if err != nil {
			t.Fatal(err)
		}
		html := actions.ReplaceAllString(string(b), " ")
		for _, m := range regexp.MustCompile(`class="([^"]*)"`).FindAllStringSubmatch(html, -1) {
			for _, c := range strings.Fields(m[1]) {
				if strings.HasSuffix(c, "-") { // prefix of a generated class, e.g. "status-{{.}}"
					continue
				}
				used[c] = true
				if !defined[c] {
					t.Errorf("%s: class %q is not defined in style.css", f, c)
				}
			}
		}
	}
	generated := regexp.MustCompile(`^(status|incident|maintenance|notif|flash)-|^nodata$`)
	for c := range defined {
		if !used[c] && !generated.MatchString(c) {
			t.Errorf("style.css styles class %q that no template uses", c)
		}
	}
}
