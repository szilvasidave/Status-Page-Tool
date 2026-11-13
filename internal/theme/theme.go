// Package theme is the branding of the web interface (Admin → Branding):
// the colour tokens of light and dark mode derived from the colours an
// administrator chose, the CSS that overrides the defaults of
// static/style.css, the colour of the e-mail button, the default favicon in
// the accent colour, and the validation of uploaded logos and favicons.
//
// It is a leaf package (standard library only) of pure functions. Colours
// are "#rrggbb" strings; "" means the default.
//
// Derivation rules (see Resolve): a custom accent gets readable button text
// (contrast at least 4.5), a darker hover shade in light mode and a lighter
// one in dark mode (the other way round where that would make the button
// text less readable), a link colour adjusted towards black (light mode) or
// white (dark mode) until it reaches a contrast of 4.5 with the page
// surfaces, and a colour for focus outlines and other controls with a
// contrast of at least 3. Without a dark accent, dark mode uses the light
// accent, lightened until it reaches a contrast of 3 with the dark page
// background. A custom header colour gets white or dark text (contrast at
// least 4.5) and muted text, hover and line shades mixed from the two; the
// text keeps a contrast of 4.5 on the hover and current shades.
package theme

import (
	"fmt"
	"strings"
)

// Appearance modes (Colors.Mode, store.Settings.ThemeMode).
const (
	ModeAuto  = "auto"  // follow the device setting (prefers-color-scheme)
	ModeLight = "light" // always light
	ModeDark  = "dark"  // always dark
)

// ValidMode reports whether m is one of ModeAuto, ModeLight and ModeDark.
func ValidMode(m string) bool { return m == ModeAuto || m == ModeLight || m == ModeDark }

// Colors are the branding colour settings ("#rrggbb", or "" for the
// default) and the appearance mode. Colours that are not valid hex codes
// count as "" (see NormalizeHex).
type Colors struct {
	Mode                    string
	AccentLight, AccentDark string
	HeaderLight, HeaderDark string
}

// normalized returns c with every colour in "#rrggbb" form or "".
func (c Colors) normalized() Colors {
	c.AccentLight = normalize(c.AccentLight)
	c.AccentDark = normalize(c.AccentDark)
	c.HeaderLight = normalize(c.HeaderLight)
	c.HeaderDark = normalize(c.HeaderDark)
	return c
}

// Customized reports whether any colour is set (the mode does not count).
func (c Colors) Customized() bool {
	n := c.normalized()
	return n.AccentLight != "" || n.AccentDark != "" || n.HeaderLight != "" || n.HeaderDark != ""
}

// Palette is the resolved set of colours of one mode, all "#rrggbb".
type Palette struct {
	Dark bool

	// Fixed page tokens: the defaults of static/style.css, which cannot be
	// customised. Used for the preview and for the contrast calculations.
	Bg, Surface, Surface2, SurfaceHover, Text, TextMuted, Border string

	// Accent: buttons, links, focus rings, the active tab.
	Primary, PrimaryHover, PrimaryText, Link string

	// PrimaryUI is the accent as a non-text UI colour (focus outlines, the
	// border of a focused input, the marker of the active tab): Primary,
	// adjusted where needed to a contrast of at least 3 with the page
	// surfaces. For the default accents it equals Primary.
	PrimaryUI string

	// Header (--header-* tokens).
	HeaderBg, HeaderText, HeaderTextMuted, HeaderHover, HeaderCurrent, HeaderLine string

	// LinkAdjusted reports that Link differs from Primary because Primary
	// is not readable as text on the page surfaces. Only a custom accent is
	// adjusted; the branding page shows a hint then.
	LinkAdjusted bool
}

// The default palettes: the tokens of static/style.css (a test in
// internal/web checks that they agree). The header tokens default to the
// surface tokens of the mode; PrimaryUI is derived by Resolve and equals
// Primary for these defaults.
var (
	defaultLight = Palette{
		Bg: "#f5f7fa", Surface: "#ffffff", Surface2: "#f3f5f8", SurfaceHover: "#eef1f5",
		Text: "#17202c", TextMuted: "#5b6676", Border: "#e2e6ec",
		Primary: "#1f5bd8", PrimaryHover: "#1a4dba", PrimaryText: white, Link: "#1f5bd8",
	}
	defaultDark = Palette{
		Dark: true,
		Bg:   "#0d1117", Surface: "#151b23", Surface2: "#1b222c", SurfaceHover: "#212a35",
		Text: "#e6ebf1", TextMuted: "#9ba7b6", Border: "#2a3340",
		Primary: "#2f6be0", PrimaryHover: "#3b78ee", PrimaryText: white, Link: "#7aa9ff",
	}
)

// headerTextDark is the dark text candidate of a custom header: the text
// colour of light mode.
const headerTextDark = "#17202c"

// Contrast targets: normal text (WCAG AA) and user interface components.
const (
	minTextContrast = 4.5
	minUIContrast   = 3.0
)

// Resolve returns the palettes of light and dark mode for c.
//
// Light mode: without AccentLight the default accent tokens. Else Primary =
// AccentLight, PrimaryText = white or #111111, whichever contrasts more
// (white or black if neither reaches 4.5), PrimaryHover = 14 % black mixed
// in (14 % white if black lowers the contrast with PrimaryText), Link =
// Primary darkened until its contrast on Surface2 (the darkest page
// surface) is at least 4.5.
//
// Dark mode: Primary = AccentDark; without it AccentLight lightened until
// its contrast on Bg is at least 3; without both the default dark accent
// tokens. A custom Primary gets PrimaryText as above, PrimaryHover = 14 %
// white mixed in (14 % black if white lowers the contrast with PrimaryText)
// and Link = Primary lightened until its contrast on Surface2 (the lightest
// page surface) is at least 4.5.
//
// PrimaryUI of both modes (default accents included) = Primary darkened
// (light mode) or lightened (dark mode) until its contrast on Surface2 is
// at least 3; the default accents already reach it.
//
// Header of each mode (HeaderLight, HeaderDark; dark mode does not fall back
// to HeaderLight): without a colour the surface tokens of the mode. Else
// HeaderBg = the colour, HeaderText = white or #17202c, whichever contrasts
// more (black instead of #17202c if that is needed for a contrast of 4.5),
// HeaderTextMuted = HeaderText with 22 % (or less, down to 0 %) HeaderBg
// mixed in so that its contrast stays at least 4.5, HeaderHover and
// HeaderCurrent = HeaderBg with 10 and 16 % HeaderText mixed in (or, where
// that lowers the contrast of HeaderText below 4.5, with 10 and 16 % of
// the opposite of HeaderText: black for white text, white for dark text),
// and HeaderLine = HeaderBg with 20 % HeaderText.
func Resolve(c Colors) (light, dark Palette) {
	c = c.normalized()

	light = defaultLight
	if c.AccentLight != "" {
		setAccent(&light, c.AccentLight, black)
		light.Link = ensureContrast(light.Primary, light.Surface2, minTextContrast, black)
		light.LinkAdjusted = light.Link != light.Primary
	}
	light.PrimaryUI = ensureContrast(light.Primary, light.Surface2, minUIContrast, black)
	setHeader(&light, c.HeaderLight)

	dark = defaultDark
	primary := c.AccentDark
	if primary == "" && c.AccentLight != "" {
		// Also computed by static/app.js of internal/web (darkAccentFallback): keep in sync.
		primary = ensureContrast(c.AccentLight, dark.Bg, minUIContrast, white)
	}
	if primary != "" {
		setAccent(&dark, primary, white)
		dark.Link = ensureContrast(primary, dark.Surface2, minTextContrast, white)
		dark.LinkAdjusted = dark.Link != primary
	}
	dark.PrimaryUI = ensureContrast(dark.Primary, dark.Surface2, minUIContrast, white)
	setHeader(&dark, c.HeaderDark)
	return light, dark
}

// hoverShare is the share of black or white mixed into a custom accent for
// its hover shade.
const hoverShare = 0.14

// setAccent sets Primary, PrimaryText and PrimaryHover of p for the custom
// accent primary. The hover shade has hoverShare of hover (black in light
// mode, white in dark mode) mixed in, or of the opposite colour where hover
// would make the button text less readable than on Primary itself.
func setAccent(p *Palette, primary, hover string) {
	p.Primary = primary
	p.PrimaryText = buttonText(primary)
	p.PrimaryHover = mix(hover, primary, hoverShare)
	if Contrast(p.PrimaryText, p.PrimaryHover) < Contrast(p.PrimaryText, primary) {
		p.PrimaryHover = mix(opposite(hover), primary, hoverShare)
	}
}

// setHeader sets the header tokens of p for the header colour bg ("" = the
// default header of the mode).
func setHeader(p *Palette, bg string) {
	if bg == "" {
		p.HeaderBg, p.HeaderText, p.HeaderTextMuted = p.Surface, p.Text, p.TextMuted
		p.HeaderHover, p.HeaderCurrent, p.HeaderLine = p.SurfaceHover, p.Surface2, p.Border
		return
	}
	text := bestText(bg, headerTextDark)
	if Contrast(text, bg) < minTextContrast {
		// A mid-tone: white or black always reaches 4.5.
		text = bestText(bg, black)
	}
	p.HeaderBg, p.HeaderText = bg, text
	p.HeaderTextMuted = text
	for pct := 78; pct < 100; pct += 2 {
		if m := mix(text, bg, float64(pct)/100); Contrast(m, bg) >= minTextContrast {
			p.HeaderTextMuted = m
			break
		}
	}
	p.HeaderHover = headerShade(text, bg, 0.10)
	p.HeaderCurrent = headerShade(text, bg, 0.16)
	p.HeaderLine = mix(text, bg, 0.20)
}

// headerShade returns the header colour bg with the share s of the header
// text colour mixed in (the hover and current shades of the navigation),
// or, if the text would then fall below a contrast of 4.5, with the share s
// of the opposite of the text colour (black for white text, white for dark
// text), which raises the contrast instead.
func headerShade(text, bg string, s float64) string {
	if m := mix(text, bg, s); Contrast(text, m) >= minTextContrast {
		return m
	}
	return mix(opposite(text), bg, s)
}

// cssVars lists the custom properties that CSS sets for every mode.
var cssVars = []string{
	"--primary", "--primary-hover", "--primary-text", "--primary-ui", "--link", "--focus",
	"--header-bg", "--header-text", "--header-text-muted", "--header-hover", "--header-current", "--header-line",
}

// declarations returns the declarations of all cssVars for p; the focus
// ring is the link colour with focusPct % opacity.
func declarations(p Palette, focusPct int) string {
	values := []string{
		p.Primary, p.PrimaryHover, p.PrimaryText, p.PrimaryUI, p.Link,
		fmt.Sprintf("color-mix(in srgb, %s %d%%, transparent)", p.Link, focusPct),
		p.HeaderBg, p.HeaderText, p.HeaderTextMuted, p.HeaderHover, p.HeaderCurrent, p.HeaderLine,
	}
	var b strings.Builder
	for i, name := range cssVars {
		b.WriteString(name + ":" + values[i] + ";")
	}
	return b.String()
}

// CSS returns the content of a <style> block that overrides the default
// tokens of static/style.css, or "" when nothing is customised
// (c.Customized() is false).
//
// Both modes get the complete set of variables (defaults included), so that
// a colour customised for one mode never leaks into the other. The
// selectors and media queries are those of static/style.css (the dark
// tokens apply only on screen, so printing always uses the light ones); the
// block follows the stylesheet, so it wins at equal specificity:
//
//	:root{…light…}
//	@media screen and (prefers-color-scheme: dark){:root:not([data-theme="light"]){…dark…}}
//	@media screen{:root[data-theme="dark"]{…dark…}}
//
// The values are "#rrggbb" strings produced here, so the result is safe to
// use as template.CSS.
func CSS(c Colors) string {
	if !c.Customized() {
		return ""
	}
	light, dark := Resolve(c)
	l, d := declarations(light, 45), declarations(dark, 55)
	return ":root{" + l + "}\n" +
		`@media screen and (prefers-color-scheme: dark){:root:not([data-theme="light"]){` + d + "}}\n" +
		`@media screen{:root[data-theme="dark"]{` + d + "}}\n"
}

// Default colours of the e-mail button.
const (
	emailButtonBg   = "#1d5fd1"
	emailButtonText = white
)

// EmailButton returns the background and text colour of the button of the
// notification e-mails: #1d5fd1 and #ffffff (the e-mail of before branding)
// when accentLight is empty or invalid, else accentLight with the button
// text of the web interface: #ffffff or #111111, whichever contrasts more
// (#ffffff or #000000 if neither reaches 4.5).
func EmailButton(accentLight string) (bg, text string) {
	accent := normalize(accentLight)
	if accent == "" {
		return emailButtonBg, emailButtonText
	}
	return accent, buttonText(accent)
}
