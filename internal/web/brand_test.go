package web

// Tests of the branding: the page Admin → Branding, the brand images
// (/brand/…), the branded layout and the agreement of package theme with
// static/style.css.

import (
	"bytes"
	"context"
	"html/template"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io/fs"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/theme"
)

// brandFile is a file of a multipart form.
type brandFile struct {
	name string // file name
	data []byte
}

// brandMultipart builds a multipart body of fields and files.
func brandMultipart(t *testing.T, fields url.Values, files map[string]brandFile) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for k, vs := range fields {
		for _, v := range vs {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	for field, f := range files {
		w, err := mw.CreateFormFile(field, f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return body, mw.FormDataContentType()
}

// brandPost submits the branding form (or another form at path) as
// multipart/form-data with a valid CSRF token, like a browser.
func brandPost(t *testing.T, e *testEnv, path string, fields url.Values, files map[string]brandFile) Response {
	t.Helper()
	f := url.Values{}
	for k, v := range fields {
		f[k] = v
	}
	if f.Get(auth.CSRFField) == "" {
		f.Set(auth.CSRFField, e.csrf())
	}
	body, ct := brandMultipart(t, f, files)
	req, _ := http.NewRequest(http.MethodPost, e.Server.URL+path, body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Origin", e.Server.URL)
	return e.do(req)
}

// brandFields returns the fields of an unchanged branding form for st.
func brandFields(st *store.Settings) url.Values {
	f := url.Values{
		"theme_mode":   {st.ThemeMode},
		"accent_light": {st.ThemeAccentLight},
		"header_light": {st.ThemeHeaderLight},
		"accent_dark":  {st.ThemeAccentDark},
		"header_dark":  {st.ThemeHeaderDark},
	}
	if st.ThemeShowSiteName {
		f.Set("show_site_name", "on")
	}
	return f
}

// brandPNG returns a small PNG image of the given colour.
func brandPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 40, 10))
	for x := 0; x < 40; x++ {
		for y := 0; y < 10; y++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// brandSVG is a valid logo for dark mode.
const brandSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 32"><rect width="120" height="32" fill="#ffffff"/></svg>`

// brandSettings returns the stored settings (read past the cache).
func brandSettings(t *testing.T, e *testEnv) *store.Settings {
	t.Helper()
	st, err := e.Store.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// brandStatus fails the test unless r has the status want.
func brandStatus(t *testing.T, what string, r Response, want int) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s: status %d, want %d\n%s", what, r.Status, want, r.Body)
	}
}

func TestBrandPageAccess(t *testing.T) {
	e := newTestEnv(t)
	if r := e.get("/admin/branding"); r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Location, "/login") {
		t.Errorf("anonymous GET: %d %q", r.Status, r.Location)
	}
	e.loginAs("editor@example.com", store.RoleEditor)
	if r := e.get("/admin/branding"); r.Status != http.StatusForbidden {
		t.Errorf("editor GET: %d", r.Status)
	}
	if r := brandPost(t, e, "/admin/branding", url.Values{"theme_mode": {"dark"}}, nil); r.Status != http.StatusForbidden {
		t.Errorf("editor POST: %d", r.Status)
	}
	if r := e.post("/admin/branding/reset", nil); r.Status != http.StatusForbidden {
		t.Errorf("editor reset: %d", r.Status)
	}
	if st := brandSettings(t, e); st.ThemeMode != theme.ModeAuto {
		t.Errorf("an editor changed the appearance: %q", st.ThemeMode)
	}

	e.logout()
	e.loginAdmin()
	r := e.get("/admin/branding")
	brandStatus(t, "admin GET", r, http.StatusOK)
	mustContain(t, r.Body,
		`<a href="/admin/branding" aria-current="page">Branding</a>`,
		`enctype="multipart/form-data"`, `data-unsaved=`, `name="csrf_token"`,
		`name="logo_light" type="file"`, `name="logo_dark" type="file"`, `name="favicon" type="file"`,
		`data-max-bytes="524288"`, `name="show_site_name" aria-describedby="show_site_name_help" checked`,
		`<option value="auto" selected>Follow the device setting</option>`,
		`placeholder="Default (#1f5bd8)" data-default="#1f5bd8"`, `placeholder="Default (#2f6be0)"`,
		`placeholder="Default (#ffffff)"`, `placeholder="Default (#151b23)"`,
		`<input type="color" data-color-for="accent_light" aria-label="Accent colour for light mode: colour picker" hidden>`,
		`<label for="header_dark">Header background</label>`,
		`class="brand-preview-page" style="--bg:#f5f7fa;`, `--primary-text:#ffffff;--primary-ui:#1f5bd8;--link:#1f5bd8;`,
		`--primary-ui:#2f6be0;--link:#7aa9ff;`, `color-scheme:dark"`,
		`action="/admin/branding/reset" data-confirm=`, "Default icon")
	if strings.Contains(r.Body, "_remove") {
		t.Error("remove checkboxes shown without stored images")
	}
}

// The default layout (no branding) is the one of before: no colour
// overrides, no forced mode, the static favicon and the default mark.
func TestBrandDefaultLayout(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{"/", "/login", "/history"} {
		r := e.get(path)
		mustContain(t, r.Body, `<html lang="en">`, `<meta name="color-scheme" content="light dark">`,
			`<link rel="icon" href="/static/favicon.svg" type="image/svg+xml">`,
			`<a class="brand" href="/"><span class="brand-mark" aria-hidden="true"></span><span class="brand-name">Status</span></a>`)
		publicMustNotContain(t, r.Body, "<style", "data-theme", "/brand/", "brand-logo")
	}
	// Header colours alone change neither the favicon nor the logo.
	publicSetSettings(t, e, func(st *store.Settings) { st.ThemeHeaderLight = "#c8102e" })
	r := e.get("/")
	mustContain(t, r.Body, `<style>:root{--primary:#1f5bd8;`, "--header-bg:#c8102e;", `href="/static/favicon.svg"`, `class="brand-mark"`)
}

func TestBrandSave(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	logo := brandPNG(t, color.RGBA{200, 16, 46, 255})

	f := brandFields(brandSettings(t, e))
	f.Set("theme_mode", "dark")
	f.Set("accent_light", " #FFD400 ")
	f.Set("header_light", "c8102e")
	f.Set("accent_dark", "")
	f.Set("header_dark", "#600")
	r := brandPost(t, e, "/admin/branding", f, map[string]brandFile{
		"logo_light": {"logo.png", logo},
		"logo_dark":  {"logo-dark.svg", []byte(brandSVG)},
		"favicon":    {"", nil}, // no file chosen
	})
	if r.Status != http.StatusSeeOther || r.Location != "/admin/branding" {
		t.Fatalf("save: %d %q\n%s", r.Status, r.Location, r.Body)
	}
	mustContain(t, e.get(r.Location).Body, "Branding saved.")
	st := brandSettings(t, e)
	if st.ThemeMode != "dark" || st.ThemeAccentLight != "#ffd400" || st.ThemeHeaderLight != "#c8102e" ||
		st.ThemeAccentDark != "" || st.ThemeHeaderDark != "#660000" || !st.ThemeShowSiteName {
		t.Fatalf("stored settings: %+v", st)
	}
	if len(st.LogoLight) != 16 || len(st.LogoDark) != 16 || st.Favicon != "" {
		t.Fatalf("image versions: %q %q %q", st.LogoLight, st.LogoDark, st.Favicon)
	}
	img, err := e.Store.BrandImage(context.Background(), store.BrandLogoLight)
	if err != nil || img.ContentType != "image/png" || !bytes.Equal(img.Data, logo) {
		t.Fatalf("stored logo: %+v %v", img, err)
	}

	// Forced dark mode: the dark logo only, the colours, the generated favicon.
	r = e.get("/")
	light, _ := theme.Resolve(st.ThemeColors())
	mustContain(t, r.Body, `<html lang="en" data-theme="dark">`, `<meta name="color-scheme" content="dark">`,
		"<style>"+theme.CSS(st.ThemeColors())+"</style>",
		`--primary:#ffd400;--primary-hover:#ffda24;--primary-text:#111111;--primary-ui:#a38800;--link:#856e00;`,
		`<link rel="icon" href="/brand/favicon?v=cffd400" type="image/svg+xml">`,
		`<a class="brand brand-with-logo" href="/"><img class="brand-logo" src="/brand/logo-dark?v=`+st.LogoDark+`" alt=""><span class="brand-name">Status</span></a>`)
	if light.Primary != "#ffd400" {
		t.Errorf("light primary %s", light.Primary)
	}

	// Following the device: a <picture> with both logos.
	f = brandFields(st)
	f.Set("theme_mode", "auto")
	f.Del("show_site_name")
	brandStatus(t, "save auto", brandPost(t, e, "/admin/branding", f, nil), http.StatusSeeOther)
	r = e.get("/")
	mustContain(t, r.Body, `<html lang="en">`, `<meta name="color-scheme" content="light dark">`,
		`<a class="brand brand-with-logo" href="/"><picture><source srcset="/brand/logo-dark?v=`+st.LogoDark+`" media="(prefers-color-scheme: dark)">`+
			`<img class="brand-logo" src="/brand/logo-light?v=`+st.LogoLight+`" alt="Status"></picture></a>`)
	publicMustNotContain(t, r.Body, `<span class="brand-name">`, `<html lang="en" data-theme`)
	if st2 := brandSettings(t, e); st2.LogoLight != st.LogoLight || st2.LogoDark != st.LogoDark || st2.ThemeShowSiteName {
		t.Errorf("saving without files changed the images or kept the site name: %+v", st2)
	}

	// Always light: the light logo only. The admin page shows the stored
	// images with their remove checkboxes and the adjusted link colour.
	f.Set("theme_mode", "light")
	brandStatus(t, "save light", brandPost(t, e, "/admin/branding", f, nil), http.StatusSeeOther)
	r = e.get("/admin/branding")
	mustContain(t, r.Body, `<html lang="en" data-theme="light">`,
		`<img class="brand-logo" src="/brand/logo-light?v=`+st.LogoLight+`" alt="Status"></a>`,
		`<span class="brand-image-chip" style="background:#c8102e"><img src="/brand/logo-light?v=`+st.LogoLight+`" alt="Current logo for light mode">`,
		`style="background:#660000"><img src="/brand/logo-dark?v=`+st.LogoDark+`"`,
		`name="logo_light_remove"> Remove the logo for light mode</label>`, `name="logo_dark_remove"> Remove the logo for dark mode</label>`,
		`<span class="brand-image-chip" style="background:#f3f5f8"><img src="/brand/favicon?v=cffd400" alt="Current favicon">`,
		"Links use the darker shade #856e00 so that they stay readable.",
		`value="#ffd400"`, `<option value="light" selected>Always light</option>`,
		"(the pages always use light mode)")
	if strings.Contains(r.Body, "<picture>") || strings.Contains(r.Body, `name="favicon_remove"`) {
		t.Error("forced light mode uses a <picture>, or the generated favicon can be removed")
	}

	// A favicon upload replaces the generated one.
	brandStatus(t, "save favicon", brandPost(t, e, "/admin/branding", f, map[string]brandFile{"favicon": {"icon.png", logo}}), http.StatusSeeOther)
	st = brandSettings(t, e)
	mustContain(t, e.get("/").Body, `<link rel="icon" href="/brand/favicon?v=`+st.Favicon+`">`)
	mustContain(t, e.get("/admin/branding").Body, `name="favicon_remove"> Remove the favicon</label>`)
}

// The site name is shown next to a logo unless turned off; a logo uploaded
// for one mode only is used in both.
func TestBrandPageTheme(t *testing.T) {
	for _, c := range []struct {
		mode, light, dark string
		showName          bool
		logo, logoDark    string
	}{
		{"auto", "l1", "", true, "/brand/logo-light?v=l1", ""},
		{"auto", "", "d1", true, "/brand/logo-dark?v=d1", ""},
		{"auto", "l1", "d1", false, "/brand/logo-light?v=l1", "/brand/logo-dark?v=d1"},
		{"light", "l1", "d1", true, "/brand/logo-light?v=l1", ""},
		{"light", "", "d1", true, "/brand/logo-dark?v=d1", ""},
		{"dark", "l1", "d1", true, "/brand/logo-dark?v=d1", ""},
		{"dark", "l1", "", false, "/brand/logo-light?v=l1", ""},
		{"dark", "", "", false, "", ""},
	} {
		st := store.DefaultSettings()
		st.ThemeMode, st.LogoLight, st.LogoDark, st.ThemeShowSiteName = c.mode, c.light, c.dark, c.showName
		pt := newPageTheme(&st, "ACME")
		wantShow := c.showName || c.logo == ""
		wantAlt := ""
		if !wantShow {
			wantAlt = "ACME"
		}
		if pt.LogoURL != c.logo || pt.LogoDarkURL != c.logoDark || pt.ShowName != wantShow || pt.LogoAlt != wantAlt {
			t.Errorf("%+v: got logo %q dark %q show %v alt %q", c, pt.LogoURL, pt.LogoDarkURL, pt.ShowName, pt.LogoAlt)
		}
	}
	if pt := newPageTheme(nil, "x"); pt.CSS != "" || pt.Mode != "" || pt.FaviconURL != "/static/favicon.svg" || !pt.ShowName {
		t.Errorf("no settings: %+v", pt)
	}
}

// With any error nothing is stored, and the form is shown again with the
// entered values and messages.
func TestBrandSaveInvalid(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	logo := brandPNG(t, color.Black)
	big := bytes.Repeat([]byte{0}, 600<<10)
	brandStatus(t, "upload", brandPost(t, e, "/admin/branding", brandFields(brandSettings(t, e)),
		map[string]brandFile{"logo_dark": {"d.png", logo}}), http.StatusSeeOther)
	for _, c := range []struct {
		name   string
		change func(url.Values)
		files  map[string]brandFile
		field  string
		msg    string
	}{
		{"colour", func(f url.Values) { f.Set("accent_dark", "#12345g") }, map[string]brandFile{"logo_light": {"logo.png", logo}},
			"accent_dark", "Enter a colour as a hex code such as #1f5bd8, or leave the field empty for the default."},
		{"mode", func(f url.Values) { f.Set("theme_mode", "sepia") }, nil, "theme_mode", "Choose one of the offered appearances."},
		{"script", nil, map[string]brandFile{"logo_dark": {"logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)}},
			"logo_dark", brandMsgSVGActive},
		{"onload", nil, map[string]brandFile{"logo_light": {"logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)}},
			"logo_light", brandMsgSVGActive},
		// A document type declaration with an internal subset can give the
		// root a default onload attribute.
		{"doctype subset", nil, map[string]brandFile{"favicon": {"icon.svg", []byte(`<?xml version="1.0"?>` + "\n" +
			`<!DOCTYPE svg [<!ATTLIST svg onload CDATA "alert(1)">]>` + "\n" + `<svg xmlns="http://www.w3.org/2000/svg"/>`)}},
			"favicon", brandMsgSVGActive},
		{"no namespace", nil, map[string]brandFile{"logo_light": {"logo.svg", []byte(`<svg viewBox="0 0 10 10"><rect width="10" height="10"/></svg>`)}},
			"logo_light", template.HTMLEscapeString(brandMsgSVGNamespace)},
		{"invalid svg", nil, map[string]brandFile{"logo_light": {"logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><g></svg>`)}},
			"logo_light", "This file is not a valid SVG image."},
		{"too large", nil, map[string]brandFile{"logo_light": {"logo.png", big}, "favicon": {"icon.png", logo}},
			"logo_light", "The image is too large (600 KB); at most 512 KB."},
		{"text", nil, map[string]brandFile{"logo_light": {"logo.png", []byte("hello")}}, "logo_light", "Use a PNG, JPEG, GIF, WebP or SVG image."},
		{"jpeg favicon", nil, map[string]brandFile{"favicon": {"icon.jpg", brandJPEG(t)}}, "favicon", "Use a PNG, GIF, SVG or ICO image."},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := brandFields(brandSettings(t, e))
			f.Set("accent_light", "#c8102e")
			f.Set("logo_dark_remove", "on")
			if c.change != nil {
				c.change(f)
			}
			r := brandPost(t, e, "/admin/branding", f, c.files)
			brandStatus(t, "save", r, http.StatusUnprocessableEntity)
			mustContain(t, r.Body, `Please correct the highlighted field below.`, c.msg, `id="`+c.field+`"`, `value="#c8102e"`)
			// The remove checkbox stays ticked, unless a new file was chosen (which wins).
			_, picked := c.files["logo_dark"]
			if got := strings.Contains(r.Body, `name="logo_dark_remove" checked`); got == picked {
				t.Errorf("remove checkbox of the dark logo ticked: %v", got)
			}
			if !regexp.MustCompile(`id="` + c.field + `"[^>]*aria-invalid="true"`).MatchString(r.Body) {
				t.Errorf("%s is not marked invalid", c.field)
			}
			if got, want := strings.Contains(r.Body, "Choose them again"), len(c.files) > 0; got != want {
				t.Errorf("note about choosing the files again shown: %v, want %v", got, want)
			}
			st := brandSettings(t, e)
			if st.ThemeAccentLight != "" || st.LogoLight != "" || st.Favicon != "" || st.LogoDark == "" {
				t.Errorf("something was stored or removed: %+v", st)
			}
		})
	}
}

// The messages for SVG images that theme.CheckImage refuses.
const (
	brandMsgSVGActive = "This SVG contains active content (scripts, event handlers, embedded documents, entity or document " +
		"type declarations, or style sheet links), which is not allowed. Export it again as a plain SVG, or use a PNG."
	brandMsgSVGNamespace = `This SVG has no xmlns="http://www.w3.org/2000/svg" on its svg element, so browsers would not ` +
		"show it. Add it, or export the file again."
)

// brandJPEG returns a small JPEG image.
func brandJPEG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A body over auth.MaxUploadBytes is refused with 413, also when the token
// comes in a header and the handler reads the form itself.
func TestBrandBodyTooLarge(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	huge := map[string]brandFile{"logo_light": {"logo.png", bytes.Repeat([]byte{1}, 5<<20)}}
	r := brandPost(t, e, "/admin/branding", brandFields(brandSettings(t, e)), huge)
	brandStatus(t, "huge upload", r, http.StatusRequestEntityTooLarge)
	mustContain(t, r.Body, "Request too large")

	body, ct := brandMultipart(t, brandFields(brandSettings(t, e)), huge)
	req, _ := http.NewRequest(http.MethodPost, e.Server.URL+"/admin/branding", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Origin", e.Server.URL)
	req.Header.Set(auth.CSRFHeader, e.csrf())
	brandStatus(t, "huge upload, token in a header", e.do(req), http.StatusRequestEntityTooLarge)

	// The same with a small file works.
	f := brandFields(brandSettings(t, e))
	f.Set("accent_light", "#123456")
	body, ct = brandMultipart(t, f, map[string]brandFile{"logo_light": {"logo.png", brandPNG(t, color.White)}})
	req, _ = http.NewRequest(http.MethodPost, e.Server.URL+"/admin/branding", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Origin", e.Server.URL)
	req.Header.Set(auth.CSRFHeader, e.csrf())
	brandStatus(t, "upload, token in a header", e.do(req), http.StatusSeeOther)
	if st := brandSettings(t, e); st.ThemeAccentLight != "#123456" || st.LogoLight == "" {
		t.Errorf("not saved: %+v", st)
	}
}

func TestBrandRemoveAndReset(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	logo := brandPNG(t, color.Black)
	f := brandFields(brandSettings(t, e))
	f.Set("accent_light", "#ffd400")
	f.Set("header_dark", "#002855")
	f.Set("theme_mode", "dark")
	f.Del("show_site_name")
	files := map[string]brandFile{"logo_light": {"l.png", logo}, "logo_dark": {"d.svg", []byte(brandSVG)}, "favicon": {"f.png", logo}}
	brandStatus(t, "save", brandPost(t, e, "/admin/branding", f, files), http.StatusSeeOther)
	st := brandSettings(t, e)

	// A newly chosen file wins over the remove checkbox; the others are removed.
	other := brandPNG(t, color.White)
	f = brandFields(st)
	f.Set("logo_light_remove", "on")
	f.Set("logo_dark_remove", "on")
	brandStatus(t, "remove", brandPost(t, e, "/admin/branding", f, map[string]brandFile{"logo_light": {"l2.png", other}}), http.StatusSeeOther)
	st2 := brandSettings(t, e)
	if st2.LogoLight == "" || st2.LogoLight == st.LogoLight || st2.LogoDark != "" || st2.Favicon != st.Favicon {
		t.Fatalf("after remove: %q %q %q (before %q %q %q)", st2.LogoLight, st2.LogoDark, st2.Favicon, st.LogoLight, st.LogoDark, st.Favicon)
	}
	if _, err := e.Store.BrandImage(context.Background(), store.BrandLogoDark); err != store.ErrNotFound {
		t.Errorf("dark logo still stored: %v", err)
	}

	r := e.post("/admin/branding/reset", nil)
	if r.Status != http.StatusSeeOther || r.Location != "/admin/branding" {
		t.Fatalf("reset: %d %q", r.Status, r.Location)
	}
	mustContain(t, e.get(r.Location).Body, "The default branding is restored.")
	st3 := brandSettings(t, e)
	if st3.ThemeMode != theme.ModeAuto || st3.ThemeAccentLight != "" || st3.ThemeHeaderDark != "" || !st3.ThemeShowSiteName ||
		st3.LogoLight != "" || st3.LogoDark != "" || st3.Favicon != "" {
		t.Fatalf("after reset: %+v", st3)
	}
	e.logout()
	r = e.get("/")
	mustContain(t, r.Body, `<link rel="icon" href="/static/favicon.svg" type="image/svg+xml">`, `<span class="brand-mark" aria-hidden="true"></span>`)
	publicMustNotContain(t, r.Body, "<style", "data-theme")
}

// brandOtherProcess returns a store on the database of e with a cache of
// its own, like the command "statuspage local-login" in another process:
// what it changes, the server sees only after its settings cache expires.
func brandOtherProcess(t *testing.T, e *testEnv) (*store.Store, *db.DB) {
	t.Helper()
	d, err := db.Open(context.Background(), config.DriverSQLite, filepath.Join(e.DataDir, "statuspage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return store.New(d, nil), d // no key: only SetLocalLoginEnabled is used
}

// brandOtherRows returns the stored settings rows (key → value as stored,
// secrets encrypted) except those of the branding.
func brandOtherRows(t *testing.T, d *db.DB) map[string]string {
	t.Helper()
	rows, err := d.QueryContext(context.Background(), `SELECT key, value FROM settings`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(k, "theme_") && !strings.HasPrefix(k, "brand_") {
			m[k] = v
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return m
}

// Saving and restoring the branding writes only the branding settings. The
// other settings stay exactly as stored (secrets are not encrypted again),
// also when the server still has an older copy of them in its settings
// cache: here the local sign-in is switched by the command line after the
// branding page was loaded.
func TestBrandKeepsOtherSettings(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	publicSetSettings(t, e, func(st *store.Settings) {
		st.SiteName, st.BaseURL, st.PublicAccess = "ACME Status", "https://status.example.com", false
		st.MailMethod, st.SMTPHost, st.SMTPPassword = store.MailSMTP, "smtp.example.com", "smtp-secret"
		st.OIDCClientSecret = "oidc-secret"
	})
	// An administrator from single sign-on: switching the local sign-in
	// ends the sessions of the local administrator account.
	e.loginAs("brand-admin@example.com", store.RoleAdmin)
	cli, d := brandOtherProcess(t, e)
	check := func(what string, rows map[string]string, localLogin bool) {
		t.Helper()
		st := brandSettings(t, e)
		if st.SiteName != "ACME Status" || st.BaseURL != "https://status.example.com" || st.PublicAccess ||
			st.MailMethod != store.MailSMTP || st.SMTPHost != "smtp.example.com" || st.SMTPPassword != "smtp-secret" ||
			st.OIDCClientSecret != "oidc-secret" || st.LocalLoginEnabled != localLogin {
			t.Errorf("%s changed other settings: %+v", what, st)
		}
		if got := brandOtherRows(t, d); !maps.Equal(got, rows) {
			t.Errorf("%s rewrote other settings rows:\n%v\nwant\n%v", what, got, rows)
		}
	}

	brandStatus(t, "branding page", e.get("/admin/branding"), http.StatusOK) // loads the settings into the cache
	if changed, err := cli.SetLocalLoginEnabled(ctx, false); err != nil || !changed {
		t.Fatalf("local-login disable: %v %v", changed, err)
	}
	if st := brandSettings(t, e); !st.LocalLoginEnabled {
		t.Fatal("the server's settings cache was not stale: the test does not check what it should")
	}
	rows := brandOtherRows(t, d)
	f := brandFields(brandSettings(t, e))
	f.Set("accent_light", "#c8102e")
	f.Set("theme_mode", "dark")
	brandStatus(t, "save", brandPost(t, e, "/admin/branding", f, nil), http.StatusSeeOther)
	check("saving the branding", rows, false)
	if st := brandSettings(t, e); st.ThemeAccentLight != "#c8102e" || st.ThemeMode != theme.ModeDark {
		t.Fatalf("branding not saved: %+v", st)
	}

	brandStatus(t, "branding page", e.get("/admin/branding"), http.StatusOK)
	if changed, err := cli.SetLocalLoginEnabled(ctx, true); err != nil || !changed {
		t.Fatalf("local-login enable: %v %v", changed, err)
	}
	rows = brandOtherRows(t, d)
	brandStatus(t, "reset", e.post("/admin/branding/reset", nil), http.StatusSeeOther)
	check("restoring the default branding", rows, true)
	if st := brandSettings(t, e); st.ThemeAccentLight != "" || st.ThemeMode != theme.ModeAuto {
		t.Fatalf("branding not reset: %+v", st)
	}
}

// The dark accent field carries what static/app.js needs to update its
// default while the light accent is edited; the default the page shows is
// the one of package theme.
func TestBrandDarkAccentFallback(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	_, defDark := theme.Resolve(theme.Colors{})
	field := regexp.MustCompile(`<input id="accent_dark"[^>]*>`)
	for _, accent := range []string{"", "#ffd400", "#002855", "#c8102e"} {
		f := brandFields(brandSettings(t, e))
		f.Set("accent_light", accent)
		brandStatus(t, "save "+accent, brandPost(t, e, "/admin/branding", f, nil), http.StatusSeeOther)
		body := e.get("/admin/branding").Body
		_, dark := theme.Resolve(theme.Colors{AccentLight: accent})
		tag := field.FindString(body)
		mustContain(t, tag, `placeholder="Default (`+dark.Primary+`)" data-default="`+dark.Primary+`"`,
			` data-fallback-for="accent_light" data-fallback-default="`+defDark.Primary+`" data-fallback-bg="`+defDark.Bg+`"`)
		if n := strings.Count(body, "data-fallback-for="); n != 1 {
			t.Errorf("%d fields with data-fallback-for", n)
		}
	}
	if defDark.Primary != "#2f6be0" || defDark.Bg != "#0d1117" {
		t.Errorf("default dark accent %s on %s", defDark.Primary, defDark.Bg)
	}
}

func TestBrandImages(t *testing.T) {
	// Before the setup is complete the favicon is served, not redirected.
	if r := newTestEnvNoAdmin(t).get("/brand/favicon"); r.Status != http.StatusOK || r.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("favicon before setup: %d %v", r.Status, r.Header)
	}
	e := newTestEnv(t)

	// Not found: unknown names and logos that were not uploaded.
	for _, p := range []string{"/brand/logo-light", "/brand/logo-dark?v=x", "/brand/other", "/brand/"} {
		if r := e.get(p); r.Status != http.StatusNotFound {
			t.Errorf("GET %s: %d", p, r.Status)
		}
	}

	// The generated favicon in the default accent colour.
	r := e.get("/brand/favicon?v=c1f5bd8")
	brandStatus(t, "default favicon", r, http.StatusOK)
	if r.Header.Get("Content-Type") != "image/svg+xml" || r.Header.Get("ETag") != `"c1f5bd8"` ||
		r.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" || !strings.Contains(r.Body, `fill="#1f5bd8"`) ||
		r.Header.Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'; sandbox" {
		t.Errorf("default favicon: %v\n%s", r.Header, r.Body)
	}

	e.loginAdmin()
	logo := brandPNG(t, color.RGBA{0, 40, 85, 255})
	f := brandFields(brandSettings(t, e))
	f.Set("accent_light", "#002855")
	brandStatus(t, "save", brandPost(t, e, "/admin/branding", f, map[string]brandFile{
		"logo_light": {"l.png", logo}, "logo_dark": {"d.svg", []byte(brandSVG)},
	}), http.StatusSeeOther)
	st := brandSettings(t, e)

	// Public access off: the images can still be loaded without signing in
	// (the sign-in page shows the logo), the pages cannot.
	publicSetSettings(t, e, func(s *store.Settings) { s.PublicAccess = false })
	e.logout()
	if r := e.get("/"); r.Status != http.StatusSeeOther {
		t.Fatalf("status page without sign-in: %d", r.Status)
	}
	mustContain(t, e.get("/login").Body, `src="/brand/logo-light?v=`+st.LogoLight+`"`)

	r = e.get("/brand/logo-light?v=" + st.LogoLight)
	brandStatus(t, "logo", r, http.StatusOK)
	if r.Header.Get("Content-Type") != "image/png" || r.Header.Get("ETag") != `"`+st.LogoLight+`"` ||
		r.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" || r.Body != string(logo) ||
		r.Header.Get("X-Content-Type-Options") != "nosniff" || strings.Contains(r.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("logo headers: %v", r.Header)
	}
	for _, p := range []string{"/brand/logo-light", "/brand/logo-light?v=old"} {
		if r := e.get(p); r.Status != http.StatusOK || r.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("GET %s: %d %q", p, r.Status, r.Header.Get("Cache-Control"))
		}
	}
	req, _ := http.NewRequest(http.MethodGet, e.Server.URL+"/brand/logo-light", nil)
	req.Header.Set("If-None-Match", `"`+st.LogoLight+`"`)
	if r := e.do(req); r.Status != http.StatusNotModified || r.Body != "" {
		t.Errorf("If-None-Match: %d", r.Status)
	}

	r = e.get("/brand/logo-dark?v=" + st.LogoDark)
	if r.Status != http.StatusOK || r.Header.Get("Content-Type") != "image/svg+xml" || r.Body != brandSVG ||
		r.Header.Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'; sandbox" {
		t.Errorf("SVG logo: %d %v", r.Status, r.Header)
	}

	// The generated favicon follows the accent colour (white line on a
	// dark accent).
	r = e.get("/brand/favicon?v=c002855")
	if r.Status != http.StatusOK || r.Header.Get("ETag") != `"c002855"` || !strings.Contains(r.Body, `fill="#002855"`) ||
		!strings.Contains(r.Body, `stroke="#ffffff"`) || r.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("generated favicon: %d %v\n%s", r.Status, r.Header, r.Body)
	}
	if r := e.get("/brand/favicon?v=c1f5bd8"); r.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("an outdated favicon version is cacheable: %v", r.Header)
	}
}

// ------------------------------------------------------------------ style.css and package theme

// brandCSSBlock returns the declarations (custom properties, in order) of
// the block that follows the selector sel in css.
func brandCSSBlock(t *testing.T, css, sel string) [][2]string {
	t.Helper()
	i := strings.Index(css, sel+" {\n")
	if i < 0 {
		t.Fatalf("style.css has no block %q", sel)
	}
	body := css[i+len(sel)+3:]
	body = body[:strings.Index(body, "}")]
	var decls [][2]string
	for _, m := range regexp.MustCompile(`(--[\w-]+)\s*:\s*([^;]+);`).FindAllStringSubmatch(body, -1) {
		decls = append(decls, [2]string{m[1], strings.TrimSpace(m[2])})
	}
	return decls
}

// brandResolve replaces var(--x) in v by the values in vars (recursively).
func brandResolve(v string, vars map[string]string) string {
	re := regexp.MustCompile(`var\((--[\w-]+)\)`)
	for i := 0; i < 5 && re.MatchString(v); i++ {
		v = re.ReplaceAllStringFunc(v, func(m string) string { return vars[re.FindStringSubmatch(m)[1]] })
	}
	return v
}

// brandStyleCSS returns static/style.css without comments.
func brandStyleCSS(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(assets, "static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	return regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(b), "")
}

// The media queries and selectors of the dark tokens, in style.css and in
// theme.CSS: only on screen (printing uses the light tokens), following the
// device unless forced to light mode, or forced to dark mode.
const (
	brandDarkAuto   = `@media screen and (prefers-color-scheme: dark) {` + "\n" + `  :root:not([data-theme="light"])`
	brandDarkForced = `@media screen {` + "\n" + `  :root[data-theme="dark"]`
)

// The defaults of package theme are the tokens of static/style.css, the
// two dark blocks of style.css (following the device, forced) agree, and
// theme.CSS uses the selectors of style.css.
func TestBrandCSSSync(t *testing.T) {
	css := brandStyleCSS(t)
	root := brandCSSBlock(t, css, ":root")
	auto := brandCSSBlock(t, css, brandDarkAuto)
	forced := brandCSSBlock(t, css, brandDarkForced)
	if len(auto) < 10 || !slices.Equal(auto, forced) {
		t.Fatalf("the dark blocks differ:\n%v\n%v", auto, forced)
	}
	// No dark tokens outside these two blocks (e.g. a forced block without
	// the screen media query).
	if n := strings.Count(css, "--bg: #0d1117;"); n != 2 {
		t.Errorf("style.css sets the dark --bg %d times", n)
	}

	lightVars, darkVars := map[string]string{}, map[string]string{}
	for _, d := range root {
		lightVars[d[0]], darkVars[d[0]] = d[1], d[1]
	}
	for _, d := range auto {
		darkVars[d[0]] = d[1]
	}
	light, dark := theme.Resolve(theme.Colors{})
	for _, m := range []struct {
		vars map[string]string
		p    theme.Palette
	}{{lightVars, light}, {darkVars, dark}} {
		for name, want := range map[string]string{
			"--bg": m.p.Bg, "--surface": m.p.Surface, "--surface-2": m.p.Surface2, "--surface-hover": m.p.SurfaceHover,
			"--border": m.p.Border, "--text": m.p.Text, "--text-muted": m.p.TextMuted,
			"--primary": m.p.Primary, "--primary-hover": m.p.PrimaryHover, "--primary-text": m.p.PrimaryText, "--link": m.p.Link,
			"--primary-ui": m.p.PrimaryUI, "--header-bg": m.p.HeaderBg, "--header-text": m.p.HeaderText, "--header-text-muted": m.p.HeaderTextMuted,
			"--header-hover": m.p.HeaderHover, "--header-current": m.p.HeaderCurrent, "--header-line": m.p.HeaderLine,
		} {
			if got := brandResolve(m.vars[name], m.vars); got != want {
				t.Errorf("dark=%v: style.css %s = %q, theme default %q", m.p.Dark, name, got, want)
			}
		}
	}

	// theme.CSS sets only tokens of style.css, and with the defaults of a
	// mode exactly their values (only the other mode is customised here).
	for _, c := range []struct {
		colors theme.Colors
		block  int // 0 = light, 1 = dark
		vars   map[string]string
	}{{theme.Colors{HeaderDark: "#000000"}, 0, lightVars}, {theme.Colors{HeaderLight: "#000000"}, 1, darkVars}} {
		lines := strings.Split(strings.TrimSpace(theme.CSS(c.colors)), "\n")
		if len(lines) != 3 {
			t.Fatalf("theme.CSS: %q", lines)
		}
		// The same selectors and media queries as style.css (without the
		// white space), so that the block after the stylesheet wins.
		compact := strings.NewReplacer("\n", "", "  ", "", " {", "{")
		for i, sel := range []string{":root", brandDarkAuto, brandDarkForced} {
			if want := compact.Replace(sel) + "{"; !strings.HasPrefix(lines[i], want) {
				t.Errorf("theme.CSS line %d = %.80q…, want the prefix %q", i, lines[i], want)
			}
		}
		n := 0
		for _, m := range regexp.MustCompile(`(--[\w-]+):([^;]+);`).FindAllStringSubmatch(lines[c.block], -1) {
			n++
			if _, ok := lightVars[m[1]]; !ok {
				t.Errorf("theme.CSS sets %s, which style.css does not define", m[1])
			}
			if got := brandResolve(c.vars[m[1]], c.vars); got != m[2] {
				t.Errorf("theme.CSS default %s = %q, style.css %q", m[1], m[2], got)
			}
		}
		if n < 12 {
			t.Errorf("theme.CSS block %d sets %d tokens", c.block, n)
		}
	}
}

// brandCSSRules returns the rules of css (comments removed) whose selector
// list, with white space collapsed, is sel: the declarations of each, also
// of rules inside media queries.
func brandCSSRules(css, sel string) []string {
	var decls []string
	for _, m := range regexp.MustCompile(`([^{};]+)\{([^{}]*)\}`).FindAllStringSubmatch(css, -1) {
		if strings.Join(strings.Fields(m[1]), " ") == sel {
			decls = append(decls, m[2])
		}
	}
	return decls
}

// The <style> block of the branding follows the stylesheet (so it wins at
// equal specificity), and the rules of style.css use the tokens it sets:
// the header the --header-* tokens, focus outlines, the border of a focused
// input and the marker of the current admin section --primary-ui.
func TestBrandCSSTokensUsed(t *testing.T) {
	e := newTestEnv(t)
	publicSetSettings(t, e, func(st *store.Settings) { st.ThemeAccentLight = "#ffd400" })
	body := e.get("/").Body
	link, style := strings.Index(body, `<link rel="stylesheet" href="/static/style.css">`), strings.Index(body, "<style>")
	if link < 0 || style < link || style > strings.Index(body, "</head>") {
		t.Errorf("the <style> block (at %d) does not follow the stylesheet link (at %d) in the head", style, link)
	}

	css := brandStyleCSS(t)
	focus := `button:focus-visible, .btn:focus-visible, a:focus-visible, summary:focus-visible, .table-wrap:focus-visible, ` +
		`input[type="checkbox"]:focus-visible, input[type="radio"]:focus-visible`
	for _, c := range []struct{ sel, want string }{
		{".site-header", "background: var(--header-bg);"},
		{".site-header", "border-bottom: 1px solid var(--header-line);"},
		{".brand", "color: var(--header-text);"},
		{".site-nav a", "color: var(--header-text-muted);"},
		{`.site-nav a[aria-current="page"]`, "background: var(--header-current);"},
		{".site-nav a:hover", "background: var(--header-hover);"},
		{".site-header a:focus-visible, .site-header button:focus-visible", "outline-color: var(--header-text);"},
		{focus, "outline: 2px solid var(--primary-ui);"},
		{"input:focus, select:focus, textarea:focus", "border-color: var(--primary-ui);"},
		{`.subnav a[aria-current="page"]`, "border-bottom-color: var(--primary-ui);"},
		{`.subnav a[aria-current="page"]`, "border-color: var(--primary-ui); box-shadow: inset 0 0 0 1px var(--primary-ui);"},
	} {
		rules := brandCSSRules(css, c.sel)
		if !slices.ContainsFunc(rules, func(d string) bool { return strings.Contains(d, c.want) }) {
			t.Errorf("style.css: no rule %q with %q (rules: %q)", c.sel, c.want, rules)
		}
	}
	// Outlines and the marker of the current section never use the plain
	// accent, which may be too light (or too dark) to be seen.
	for _, m := range regexp.MustCompile(`([^{};]+)\{([^{}]*)\}`).FindAllStringSubmatch(css, -1) {
		if (strings.Contains(m[1], ":focus") || strings.Contains(m[1], ".subnav")) && strings.Contains(m[2], "var(--primary)") {
			t.Errorf("style.css: %s uses var(--primary): %s", strings.TrimSpace(m[1]), strings.TrimSpace(m[2]))
		}
	}
}
