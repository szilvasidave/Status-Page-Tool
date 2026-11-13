package web

// Branding of every page (Page.Theme, used by templates/layout.html) and the
// brand images (GET /brand/{name}). The settings are made on Admin →
// Branding (admin_branding.go); the colour rules are in package theme.

import (
	"bytes"
	"cmp"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/theme"
)

// defaultFaviconURL is the favicon while none is uploaded and the accent
// colour of light mode is the default.
const defaultFaviconURL = "/static/favicon.svg"

// pageTheme is the branding of a page (see Page.Theme). Without branding
// settings it is the default: no data-theme, no <style> block, the static
// favicon and the default mark next to the site name.
type pageTheme struct {
	Mode        string       // <html data-theme>: "" (follow the device), theme.ModeLight or theme.ModeDark
	ColorScheme string       // content of <meta name="color-scheme">: "light dark", "light" or "dark"
	CSS         template.CSS // overrides of the colour tokens of static/style.css ("" = none)
	FaviconURL  string
	FaviconSVG  bool   // the favicon is an SVG image (the default or the generated one): type attribute of the icon link
	LogoURL     string // the <img> of the header ("" = the default mark)
	LogoDarkURL string // <source> for devices in dark mode ("" = LogoURL in both modes)
	LogoAlt     string // alternative text of the logo: "" while the site name is shown next to it
	ShowName    bool   // show the site name in the header
}

// newPageTheme returns the branding of st (nil: the defaults); siteName is
// the alternative text of a logo shown without the name.
//
// Logos: a logo uploaded for one mode only is used in both. In "follow the
// device" mode with both logos the header uses a <picture> that switches to
// the dark one on devices in dark mode; a forced mode shows only its own
// logo. Favicon: an uploaded one, else the default icon drawn in the
// light-mode accent colour (generated, see handleBrandImage) when that is
// customised, else the static default.
func newPageTheme(st *store.Settings, siteName string) *pageTheme {
	t := &pageTheme{ColorScheme: "light dark", FaviconURL: defaultFaviconURL, FaviconSVG: true, ShowName: true}
	if st == nil {
		return t
	}
	c := st.ThemeColors()
	if c.Mode == theme.ModeLight || c.Mode == theme.ModeDark {
		t.Mode, t.ColorScheme = c.Mode, c.Mode
	}
	t.CSS = template.CSS(theme.CSS(c))

	if st.Favicon != "" {
		t.FaviconURL, t.FaviconSVG = brandURL(store.BrandFavicon, st.Favicon), false
	} else if accent, _ := theme.NormalizeHex(c.AccentLight); accent != "" {
		light, _ := theme.Resolve(c)
		t.FaviconURL = brandURL(store.BrandFavicon, brandFaviconVersion(light))
	}

	var light, dark string
	if st.LogoLight != "" {
		light = brandURL(store.BrandLogoLight, st.LogoLight)
	}
	if st.LogoDark != "" {
		dark = brandURL(store.BrandLogoDark, st.LogoDark)
	}
	switch t.Mode {
	case theme.ModeDark:
		t.LogoURL = cmp.Or(dark, light)
	default:
		t.LogoURL = cmp.Or(light, dark)
		if t.Mode == "" && light != "" && dark != "" {
			t.LogoDarkURL = dark
		}
	}
	if t.LogoURL != "" && !st.ThemeShowSiteName {
		t.ShowName, t.LogoAlt = false, siteName
	}
	return t
}

// brandURL returns the address of a brand image of the given version. The
// version in the query makes it cacheable for good (see handleBrandImage).
func brandURL(name, version string) string {
	return "/brand/" + name + "?v=" + version
}

// brandFaviconVersion returns the version of the generated favicon: "c" and
// the hex digits of the light-mode accent it is drawn in, e.g. "c1f5bd8".
func brandFaviconVersion(light theme.Palette) string {
	return "c" + strings.TrimPrefix(light.Primary, "#")
}

// handleBrandImage serves an uploaded logo or favicon (GET /brand/{name},
// name logo-light, logo-dark or favicon). It is public, also while public
// access is off: the sign-in page shows the logo. Without an uploaded
// favicon it serves the default icon in the light-mode accent colour; a logo
// that was not uploaded is not found.
//
// The ETag is the version of the image. A request whose query v is that
// version may be cached for good (the pages link to the current version, so
// a new upload gets a new address); any other is revalidated. SVG images
// get a CSP of their own that forbids scripts and outside resources, in case
// one is opened directly.
func (s *Server) handleBrandImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != store.BrandLogoLight && name != store.BrandLogoDark && name != store.BrandFavicon {
		s.notFound(w, r)
		return
	}
	img, err := s.Store.BrandImage(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrNotFound) && name == store.BrandFavicon:
		light, _ := theme.Resolve(s.settings(r).ThemeColors())
		img = &store.BrandImage{
			Name: name, ContentType: theme.TypeSVG,
			Data:    theme.FaviconSVG(light.Primary, light.PrimaryText),
			Version: brandFaviconVersion(light),
		}
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", img.ContentType)
	h.Set("ETag", `"`+img.Version+`"`)
	if r.URL.Query().Get("v") == img.Version {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if img.ContentType == theme.TypeSVG {
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	}
	http.ServeContent(w, r, "", img.UpdatedAt, bytes.NewReader(img.Data))
}
