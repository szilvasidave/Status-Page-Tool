package web

// The branding page (/admin/branding): logos, favicon, appearance and the
// colours of light and dark mode. Template: templates/admin_branding.html.
// Every page shows the result in its header (brand.go); the colour rules are
// in package theme.

import (
	"cmp"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/theme"
)

// adminBrandingData is the data of the branding page.
type adminBrandingData struct {
	St           store.Settings  // form values (branding fields) and the versions of the stored images
	Errors       adminFormErrors // keyed by form field
	Remove       map[string]bool // checked "<field>_remove" boxes, when the form is shown again
	FilesDropped bool            // shown again with errors after image files were chosen: choose them again

	Light, Dark theme.Palette    // resolved from the form values
	Defaults    adminBrandColors // what an empty colour field means
	Images      adminBrandImages // the stored images
	Previews    []adminBrandPreview
}

// adminBrandColors are the colours of the four colour fields.
type adminBrandColors struct {
	AccentLight, HeaderLight, AccentDark, HeaderDark string
}

// adminBrandImages are the images shown above the file fields.
type adminBrandImages struct {
	LogoLight, LogoDark, Favicon adminBrandImage
}

// adminBrandImage is a stored image (or the generated favicon) shown on a
// chip in the colour it is seen on: a logo on the header colour of its mode,
// the favicon on a light surface.
type adminBrandImage struct {
	URL      string       // "" = none uploaded
	Uploaded bool         // false for the generated default favicon
	Chip     template.CSS // background of the chip ("" = the default)
}

// adminBrandPreview is one panel of the preview: a page header and a few
// elements in the colours of one mode.
type adminBrandPreview struct {
	Title    string       // "Light mode", "Dark mode"
	Style    template.CSS // the colour tokens of the mode, as custom properties
	LogoURL  string       // "" = the default mark
	ShowName bool
}

// adminBrandField is an image field of the branding form.
type adminBrandField struct {
	name    string // store.BrandLogoLight, store.BrandLogoDark or store.BrandFavicon
	field   string // form field of the file; field + "_remove" is its remove checkbox
	favicon bool   // favicon formats (PNG, GIF, SVG, ICO) instead of logo formats
}

// adminBrandFields are the image fields of the branding form.
var adminBrandFields = []adminBrandField{
	{store.BrandLogoLight, "logo_light", false},
	{store.BrandLogoDark, "logo_dark", false},
	{store.BrandFavicon, "favicon", true},
}

// adminBrandColorFields are the colour fields of the branding form.
var adminBrandColorFields = []struct {
	field string
	value func(*store.Settings) *string
}{
	{"accent_light", func(st *store.Settings) *string { return &st.ThemeAccentLight }},
	{"header_light", func(st *store.Settings) *string { return &st.ThemeHeaderLight }},
	{"accent_dark", func(st *store.Settings) *string { return &st.ThemeAccentDark }},
	{"header_dark", func(st *store.Settings) *string { return &st.ThemeHeaderDark }},
}

// adminBrandingView returns the page data for the form values st (a copy
// of the stored settings with the submitted branding fields).
func adminBrandingView(st store.Settings, errs adminFormErrors) *adminBrandingData {
	if errs == nil {
		errs = adminFormErrors{}
	}
	d := &adminBrandingData{St: st, Errors: errs, Remove: map[string]bool{}}
	c := st.ThemeColors()
	d.Light, d.Dark = theme.Resolve(c)
	defLight, defDark := theme.Resolve(theme.Colors{})
	_, fallbackDark := theme.Resolve(theme.Colors{AccentLight: c.AccentLight})
	d.Defaults = adminBrandColors{
		AccentLight: defLight.Primary, HeaderLight: defLight.HeaderBg,
		AccentDark: fallbackDark.Primary, HeaderDark: defDark.HeaderBg,
	}

	var light, dark string
	if st.LogoLight != "" {
		light = brandURL(store.BrandLogoLight, st.LogoLight)
	}
	if st.LogoDark != "" {
		dark = brandURL(store.BrandLogoDark, st.LogoDark)
	}
	d.Images.LogoLight = adminBrandImage{URL: light, Uploaded: light != "", Chip: template.CSS("background:" + d.Light.HeaderBg)}
	d.Images.LogoDark = adminBrandImage{URL: dark, Uploaded: dark != "", Chip: template.CSS("background:" + d.Dark.HeaderBg)}
	d.Images.Favicon = adminBrandImage{URL: newPageTheme(&st, "").FaviconURL, Uploaded: st.Favicon != "", Chip: template.CSS("background:" + d.Light.Surface2)}

	for _, p := range []struct {
		title string
		pal   theme.Palette
		logo  string
	}{{"Light mode", d.Light, cmp.Or(light, dark)}, {"Dark mode", d.Dark, cmp.Or(dark, light)}} {
		d.Previews = append(d.Previews, adminBrandPreview{
			Title: p.title, Style: adminBrandPreviewStyle(p.pal), LogoURL: p.logo,
			ShowName: p.logo == "" || st.ThemeShowSiteName,
		})
	}
	return d
}

// adminBrandPreviewStyle returns the tokens of p as custom properties for
// the style attribute of a preview panel: the components inside it use them
// instead of those of the page. The values are "#rrggbb" from theme.Resolve.
func adminBrandPreviewStyle(p theme.Palette) template.CSS {
	scheme := "light"
	if p.Dark {
		scheme = "dark"
	}
	var b strings.Builder
	for _, v := range [][2]string{
		{"--bg", p.Bg}, {"--surface", p.Surface}, {"--surface-2", p.Surface2}, {"--surface-hover", p.SurfaceHover},
		{"--border", p.Border}, {"--text", p.Text}, {"--text-muted", p.TextMuted},
		{"--primary", p.Primary}, {"--primary-hover", p.PrimaryHover}, {"--primary-text", p.PrimaryText}, {"--link", p.Link},
		{"--header-bg", p.HeaderBg}, {"--header-text", p.HeaderText}, {"--header-text-muted", p.HeaderTextMuted},
		{"--header-hover", p.HeaderHover}, {"--header-current", p.HeaderCurrent}, {"--header-line", p.HeaderLine},
	} {
		b.WriteString(v[0] + ":" + v[1] + ";")
	}
	b.WriteString("color-scheme:" + scheme)
	return template.CSS(b.String())
}

func (s *Server) handleBranding(w http.ResponseWriter, r *http.Request) {
	cur, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "admin_branding", "Branding", adminBrandingView(*cur, nil))
}

// adminBrandUpload is a newly chosen image or the removal of a stored one.
type adminBrandUpload struct {
	adminBrandField
	contentType string
	data        []byte // the chosen file (nil = none)
	remove      bool   // remove the stored image (a chosen file wins)
}

// handleBrandingSave saves the branding form (multipart). Everything is
// checked first: with any error nothing is stored and the form is shown
// again (422); chosen files must then be chosen again.
func (s *Server) handleBrandingSave(w http.ResponseWriter, r *http.Request) {
	if !s.adminBrandParseForm(w, r) {
		return
	}
	ctx := r.Context()
	cur, err := s.Store.GetSettings(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	st := *cur
	errs := adminFormErrors{}
	for _, f := range adminBrandColorFields {
		v := strings.TrimSpace(r.FormValue(f.field))
		hex, ok := theme.NormalizeHex(v)
		if !ok {
			errs.add(f.field, "Enter a colour as a hex code such as #1f5bd8, or leave the field empty for the default.")
			hex = truncate(v, 50) // shown again as entered
		}
		*f.value(&st) = hex
	}
	st.ThemeMode = r.FormValue("theme_mode")
	if !theme.ValidMode(st.ThemeMode) {
		errs.add("theme_mode", "Choose one of the offered appearances.")
	}
	st.ThemeShowSiteName = formBool(r, "show_site_name")

	uploads := make([]adminBrandUpload, len(adminBrandFields))
	chosen := false
	for i, f := range adminBrandFields {
		u := &uploads[i]
		u.adminBrandField = f
		picked, msg, err := adminBrandReadImage(r, u)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		chosen = chosen || picked
		if msg != "" {
			errs.add(f.field, msg)
		}
		u.remove = !picked && formBool(r, f.field+"_remove")
	}

	if len(errs) > 0 {
		d := adminBrandingView(st, errs)
		d.FilesDropped = chosen
		for _, u := range uploads {
			d.Remove[u.field+"_remove"] = u.remove
		}
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "admin_branding", "Branding", d)
		return
	}
	for _, u := range uploads {
		var err error
		switch {
		case u.data != nil:
			_, err = s.Store.SetBrandImage(ctx, u.name, u.contentType, u.data)
		case u.remove:
			err = s.Store.DeleteBrandImage(ctx, u.name)
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if err := s.Store.SaveSettings(ctx, &st); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("branding saved", "user", adminActor(r))
	s.redirectWithFlash(w, r, "/admin/branding", "success", "Branding saved.")
}

// handleBrandingReset removes the logos and the favicon and sets the
// colours, the appearance and "show the site name" back to the defaults.
func (s *Server) handleBrandingReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	for _, f := range adminBrandFields {
		if err := s.Store.DeleteBrandImage(ctx, f.name); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	cur, err := s.Store.GetSettings(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	st, def := *cur, store.DefaultSettings()
	st.ThemeMode, st.ThemeShowSiteName = def.ThemeMode, def.ThemeShowSiteName
	st.ThemeAccentLight, st.ThemeAccentDark = def.ThemeAccentLight, def.ThemeAccentDark
	st.ThemeHeaderLight, st.ThemeHeaderDark = def.ThemeHeaderLight, def.ThemeHeaderDark
	if err := s.Store.SaveSettings(ctx, &st); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Log.Info("branding reset", "user", adminActor(r))
	s.redirectWithFlash(w, r, "/admin/branding", "success", "The default branding is restored.")
}

// adminBrandParseForm reads the form of the branding page. The auth
// middleware has usually read it already, to find the CSRF token; only a
// request that sent the token in a header still has an unread body. A body
// over auth.MaxUploadBytes is answered with 413, like the middleware does,
// and a form that cannot be read with 400; false is returned then.
func (s *Server) adminBrandParseForm(w http.ResponseWriter, r *http.Request) bool {
	if r.MultipartForm != nil {
		return true
	}
	err := r.ParseMultipartForm(auth.MaxUploadBytes)
	if errors.Is(err, http.ErrNotMultipart) {
		err = r.ParseForm() // urlencoded: no files
	}
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return true
	case errors.As(err, &tooLarge):
		http.Error(w, fmt.Sprintf("Request too large: at most %d MB can be sent at once. Please go back and try again with less data or smaller files.", auth.MaxUploadBytes>>20), http.StatusRequestEntityTooLarge)
	default:
		s.badRequest(w, r, "The form could not be read. Please try again.")
	}
	return false
}

// adminBrandReadImage reads the file chosen in the form field u.field, if
// any, and checks it with theme.CheckImage; a valid image is stored in u.
// It returns whether a file was chosen and the error message for the user
// ("" if the file is valid or none was chosen).
func adminBrandReadImage(r *http.Request, u *adminBrandUpload) (chosen bool, msg string, err error) {
	if r.MultipartForm == nil || len(r.MultipartForm.File[u.field]) == 0 {
		return false, "", nil
	}
	fh := r.MultipartForm.File[u.field][0]
	if fh.Size > theme.MaxImageBytes {
		return true, adminBrandImageError(theme.ErrImageTooLarge, fh.Size, u.favicon), nil
	}
	f, err := fh.Open()
	if err != nil {
		return true, "", fmt.Errorf("read uploaded %s: %w", u.field, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, theme.MaxImageBytes+1))
	if err != nil {
		return true, "", fmt.Errorf("read uploaded %s: %w", u.field, err)
	}
	ct, err := theme.CheckImage(data, u.favicon)
	if err != nil {
		return true, adminBrandImageError(err, int64(len(data)), u.favicon), nil
	}
	u.contentType, u.data = ct, data
	return true, "", nil
}

// adminBrandImageError returns the message for an image that
// theme.CheckImage refused; size is the size of the file in bytes.
func adminBrandImageError(err error, size int64, favicon bool) string {
	switch {
	case errors.Is(err, theme.ErrImageTooLarge):
		return fmt.Sprintf("The image is too large (%d KB); at most %d KB.", (size+1023)/1024, theme.MaxImageBytes>>10)
	case errors.Is(err, theme.ErrSVGInvalid):
		return "This file is not a valid SVG image."
	case errors.Is(err, theme.ErrSVGActive):
		return "This SVG contains scripts or event handlers, which are not allowed. Export it again as a plain SVG, or use a PNG."
	case favicon:
		return "Use a PNG, GIF, SVG or ICO image."
	}
	return "Use a PNG, JPEG, GIF, WebP or SVG image."
}
