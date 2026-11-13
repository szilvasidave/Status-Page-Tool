// Package theme is the branding of the web interface (Admin → Branding):
// the colour tokens of light and dark mode derived from the colours an
// administrator chose, the CSS that overrides the defaults of
// static/style.css, the colour of the e-mail button, the default favicon in
// the accent colour, and the validation of uploaded logos and favicons.
//
// It is a leaf package (standard library only) of pure functions. Colours
// are "#rrggbb" strings; "" means the default.
//
// Derivation rules (see Resolve): a custom accent gets the better of white
// and near-black text, a darker hover shade in light mode and a lighter one
// in dark mode, and a link colour adjusted towards black (light mode) or
// white (dark mode) until it reaches a contrast of 4.5 with the page
// surfaces. Without a dark accent, dark mode uses the light accent,
// lightened until it reaches a contrast of 3 with the dark page background.
// A custom header colour gets white or dark text (contrast at least 4.5) and
// muted text, hover and line shades mixed from the two.
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

	// Header (--header-* tokens).
	HeaderBg, HeaderText, HeaderTextMuted, HeaderHover, HeaderCurrent, HeaderLine string

	// LinkAdjusted reports that Link differs from Primary because Primary
	// is not readable as text on the page surfaces. Only a custom accent is
	// adjusted; the branding page shows a hint then.
	LinkAdjusted bool
}

// The default palettes: the tokens of static/style.css (a test in
// internal/web checks that they agree). The header tokens default to the
// surface tokens of the mode.
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
// AccentLight, PrimaryHover = 14 % black mixed in, PrimaryText = white or
// #111111 (whichever contrasts more), Link = Primary darkened until its
// contrast on Surface2 (the darkest page surface) is at least 4.5.
//
// Dark mode: Primary = AccentDark; without it AccentLight lightened until
// its contrast on Bg is at least 3; without both the default dark accent
// tokens. A custom Primary gets PrimaryHover = 14 % white mixed in,
// PrimaryText as above and Link = Primary lightened until its contrast on
// Surface2 (the lightest page surface) is at least 4.5.
//
// Header of each mode (HeaderLight, HeaderDark; dark mode does not fall back
// to HeaderLight): without a colour the surface tokens of the mode. Else
// HeaderBg = the colour, HeaderText = white or #17202c, whichever contrasts
// more (black instead of #17202c if that is needed for a contrast of 4.5),
// HeaderTextMuted = HeaderText with 22 % (or less, down to 0 %) HeaderBg
// mixed in so that its contrast stays at least 4.5, and HeaderHover,
// HeaderCurrent and HeaderLine = HeaderText with 90, 84 and 80 % HeaderBg.
func Resolve(c Colors) (light, dark Palette) {
	c = c.normalized()

	light = defaultLight
	if c.AccentLight != "" {
		light.Primary = c.AccentLight
		light.PrimaryHover = mix(black, light.Primary, 0.14)
		light.PrimaryText = bestText(light.Primary, nearBlack)
		light.Link = ensureContrast(light.Primary, light.Surface2, minTextContrast, black)
		light.LinkAdjusted = light.Link != light.Primary
	}
	setHeader(&light, c.HeaderLight)

	dark = defaultDark
	primary := c.AccentDark
	if primary == "" && c.AccentLight != "" {
		primary = ensureContrast(c.AccentLight, dark.Bg, minUIContrast, white)
	}
	if primary != "" {
		dark.Primary = primary
		dark.PrimaryHover = mix(white, primary, 0.14)
		dark.PrimaryText = bestText(primary, nearBlack)
		dark.Link = ensureContrast(primary, dark.Surface2, minTextContrast, white)
		dark.LinkAdjusted = dark.Link != primary
	}
	setHeader(&dark, c.HeaderDark)
	return light, dark
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
	p.HeaderHover = mix(text, bg, 0.10)
	p.HeaderCurrent = mix(text, bg, 0.16)
	p.HeaderLine = mix(text, bg, 0.20)
}

// cssVars lists the custom properties that CSS sets for every mode.
var cssVars = []string{
	"--primary", "--primary-hover", "--primary-text", "--link", "--focus",
	"--header-bg", "--header-text", "--header-text-muted", "--header-hover", "--header-current", "--header-line",
}

// declarations returns the declarations of all cssVars for p; the focus
// ring is the link colour with focusPct % opacity.
func declarations(p Palette, focusPct int) string {
	values := []string{
		p.Primary, p.PrimaryHover, p.PrimaryText, p.Link,
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
// selectors are those of static/style.css; the block follows the
// stylesheet, so it wins at equal specificity:
//
//	:root{…light…}
//	@media (prefers-color-scheme: dark){:root:not([data-theme="light"]){…dark…}}
//	:root[data-theme="dark"]{…dark…}
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
		`@media (prefers-color-scheme: dark){:root:not([data-theme="light"]){` + d + "}}\n" +
		`:root[data-theme="dark"]{` + d + "}\n"
}

// Default colours of the e-mail button.
const (
	emailButtonBg   = "#1d5fd1"
	emailButtonText = white
)

// EmailButton returns the background and text colour of the button of the
// notification e-mails: #1d5fd1 and #ffffff (the e-mail of before branding)
// when accentLight is empty or invalid, else accentLight with #ffffff or
// #111111, whichever contrasts more.
func EmailButton(accentLight string) (bg, text string) {
	accent := normalize(accentLight)
	if accent == "" {
		return emailButtonBg, emailButtonText
	}
	return accent, bestText(accent, nearBlack)
}
