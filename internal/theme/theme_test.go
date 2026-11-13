package theme

import (
	"regexp"
	"strings"
	"testing"
)

// wantLight and wantDark are the default palettes as documented (the
// tokens of static/style.css), written out so that a change of the
// variables in theme.go is noticed.
var (
	wantLight = Palette{
		Bg: "#f5f7fa", Surface: "#ffffff", Surface2: "#f3f5f8", SurfaceHover: "#eef1f5",
		Text: "#17202c", TextMuted: "#5b6676", Border: "#e2e6ec",
		Primary: "#1f5bd8", PrimaryHover: "#1a4dba", PrimaryText: "#ffffff", Link: "#1f5bd8",
		HeaderBg: "#ffffff", HeaderText: "#17202c", HeaderTextMuted: "#5b6676",
		HeaderHover: "#eef1f5", HeaderCurrent: "#f3f5f8", HeaderLine: "#e2e6ec",
	}
	wantDark = Palette{
		Dark: true,
		Bg:   "#0d1117", Surface: "#151b23", Surface2: "#1b222c", SurfaceHover: "#212a35",
		Text: "#e6ebf1", TextMuted: "#9ba7b6", Border: "#2a3340",
		Primary: "#2f6be0", PrimaryHover: "#3b78ee", PrimaryText: "#ffffff", Link: "#7aa9ff",
		HeaderBg: "#151b23", HeaderText: "#e6ebf1", HeaderTextMuted: "#9ba7b6",
		HeaderHover: "#212a35", HeaderCurrent: "#1b222c", HeaderLine: "#2a3340",
	}
)

func TestValidMode(t *testing.T) {
	for _, m := range []string{ModeAuto, ModeLight, ModeDark} {
		if !ValidMode(m) {
			t.Errorf("ValidMode(%q) = false", m)
		}
	}
	for _, m := range []string{"", "Auto", "system", "dark "} {
		if ValidMode(m) {
			t.Errorf("ValidMode(%q) = true", m)
		}
	}
}

func TestCustomized(t *testing.T) {
	tests := []struct {
		c    Colors
		want bool
	}{
		{Colors{}, false},
		{Colors{Mode: ModeDark}, false}, // the mode does not count
		{Colors{AccentLight: "not a colour", HeaderDark: "  "}, false},
		{Colors{AccentLight: "#ffd400"}, true},
		{Colors{AccentDark: "abc"}, true},
		{Colors{HeaderLight: "#002855"}, true},
		{Colors{HeaderDark: "#000"}, true},
	}
	for _, tc := range tests {
		if got := tc.c.Customized(); got != tc.want {
			t.Errorf("%+v.Customized() = %v, want %v", tc.c, got, tc.want)
		}
	}
}

func TestResolveDefaults(t *testing.T) {
	for _, c := range []Colors{{}, {Mode: ModeDark}, {AccentLight: "bogus", HeaderLight: "#12345"}} {
		light, dark := Resolve(c)
		if light != wantLight {
			t.Errorf("light palette of %+v:\n got %+v\nwant %+v", c, light, wantLight)
		}
		if dark != wantDark {
			t.Errorf("dark palette of %+v:\n got %+v\nwant %+v", c, dark, wantDark)
		}
	}
}

// TestResolveLightAccent: a light accent gets dark button text and a darker
// link colour in light mode; in dark mode it is visible as it is.
func TestResolveLightAccent(t *testing.T) {
	light, dark := Resolve(Colors{AccentLight: "#FFD400"})

	if light.Primary != "#ffd400" || light.PrimaryText != "#111111" || light.PrimaryHover != "#dbb600" {
		t.Errorf("light accent tokens: %+v", light)
	}
	if !light.LinkAdjusted || light.Link == light.Primary {
		t.Errorf("light link not adjusted: %+v", light)
	}
	if light.Link != "#856e00" {
		t.Errorf("light link = %s, want #856e00", light.Link)
	}
	for _, bg := range []string{"#f5f7fa", "#ffffff", "#f3f5f8"} {
		if c := Contrast(light.Link, bg); c < 4.5 {
			t.Errorf("light link %s has contrast %.2f on %s", light.Link, c, bg)
		}
	}
	// Only the accent changes.
	rest := light
	rest.Primary, rest.PrimaryHover, rest.PrimaryText, rest.Link, rest.LinkAdjusted =
		wantLight.Primary, wantLight.PrimaryHover, wantLight.PrimaryText, wantLight.Link, false
	if rest != wantLight {
		t.Errorf("light palette changed beyond the accent: %+v", light)
	}

	if dark.Primary != "#ffd400" || dark.PrimaryText != "#111111" || dark.PrimaryHover != "#ffda24" {
		t.Errorf("dark accent tokens: %+v", dark)
	}
	if dark.Link != "#ffd400" || dark.LinkAdjusted {
		t.Errorf("dark link: %s (adjusted %v), want #ffd400", dark.Link, dark.LinkAdjusted)
	}
	if dark.HeaderBg != wantDark.HeaderBg || dark.HeaderText != wantDark.HeaderText {
		t.Errorf("dark header changed: %+v", dark)
	}
}

// TestResolveDarkAccentFromLight: a dark navy accent is fine in light mode
// but lightened for dark mode.
func TestResolveDarkAccentFromLight(t *testing.T) {
	light, dark := Resolve(Colors{AccentLight: "#002855"})

	if light.Primary != "#002855" || light.PrimaryText != "#ffffff" || light.Link != "#002855" || light.LinkAdjusted {
		t.Errorf("light accent tokens: %+v", light)
	}
	if light.PrimaryHover != "#002249" {
		t.Errorf("light hover = %s, want #002249", light.PrimaryHover)
	}

	if dark.Primary == "#002855" || parseHex(dark.Primary).luminance() <= parseHex("#002855").luminance() {
		t.Errorf("dark primary %s is not a lightened #002855", dark.Primary)
	}
	if c := Contrast(dark.Primary, "#0d1117"); c < 3 {
		t.Errorf("dark primary %s has contrast %.2f on #0d1117", dark.Primary, c)
	}
	if dark.Primary != "#476485" || dark.PrimaryHover != "#617a96" || dark.PrimaryText != "#ffffff" {
		t.Errorf("dark accent tokens: %+v", dark)
	}
	if c := Contrast(dark.Link, "#1b222c"); c < 4.5 {
		t.Errorf("dark link %s has contrast %.2f on #1b222c", dark.Link, c)
	}
	if dark.Link != "#778ca5" || !dark.LinkAdjusted {
		t.Errorf("dark link = %s (adjusted %v), want #778ca5", dark.Link, dark.LinkAdjusted)
	}
	if c := Contrast(dark.PrimaryText, dark.Primary); c < 4.5 {
		t.Errorf("dark button text contrast %.2f", c)
	}
}

// TestResolveAccentDark: an explicit dark accent is used as it is (only the
// link is adjusted), and it does not change light mode.
func TestResolveAccentDark(t *testing.T) {
	light, dark := Resolve(Colors{AccentLight: "#ffd400", AccentDark: "#0A2A5A"})
	if light.Primary != "#ffd400" {
		t.Errorf("light primary = %s", light.Primary)
	}
	if dark.Primary != "#0a2a5a" || dark.PrimaryText != "#ffffff" || dark.PrimaryHover != mix(white, "#0a2a5a", 0.14) {
		t.Errorf("dark accent tokens: %+v", dark)
	}
	if !dark.LinkAdjusted || Contrast(dark.Link, "#1b222c") < 4.5 {
		t.Errorf("dark link %s (adjusted %v) has contrast %.2f", dark.Link, dark.LinkAdjusted, Contrast(dark.Link, "#1b222c"))
	}

	// A dark accent alone leaves light mode at the defaults.
	light, dark = Resolve(Colors{AccentDark: "#ff8800"})
	if light != wantLight {
		t.Errorf("light palette with only a dark accent: %+v", light)
	}
	if dark.Primary != "#ff8800" || dark.PrimaryText != "#111111" || dark.Link != "#ff8800" || dark.LinkAdjusted {
		t.Errorf("dark accent tokens: %+v", dark)
	}
}

func TestResolveHeader(t *testing.T) {
	light, dark := Resolve(Colors{HeaderLight: "#002855"})
	want := wantLight
	want.HeaderBg, want.HeaderText, want.HeaderTextMuted = "#002855", "#ffffff", "#c7d0da"
	want.HeaderHover, want.HeaderCurrent, want.HeaderLine = "#1a3e66", "#294a70", "#335377"
	if light != want {
		t.Errorf("light palette with a navy header:\n got %+v\nwant %+v", light, want)
	}
	if dark != wantDark { // no fallback from dark to light
		t.Errorf("dark palette with only a light header:\n got %+v\nwant %+v", dark, wantDark)
	}

	light, dark = Resolve(Colors{HeaderDark: "#ffd400"})
	if light != wantLight {
		t.Errorf("light palette with only a dark header: %+v", light)
	}
	if dark.HeaderBg != "#ffd400" || dark.HeaderText != "#17202c" || dark.HeaderTextMuted == dark.HeaderText {
		t.Errorf("yellow dark header: %+v", dark)
	}
	for _, p := range []Palette{light, dark} {
		if c := Contrast(p.HeaderTextMuted, p.HeaderBg); c < 4.5 {
			t.Errorf("muted header text %s has contrast %.2f on %s", p.HeaderTextMuted, c, p.HeaderBg)
		}
	}

	// A mid-tone where neither white nor #17202c reaches 4.5: black.
	light, _ = Resolve(Colors{HeaderLight: "#808080"})
	if light.HeaderText != "#000000" {
		t.Errorf("text on a mid-grey header = %s, want #000000", light.HeaderText)
	}
}

// TestResolveContrastEverywhere checks the contrast guarantees for a grid
// of colours used as accents and headers.
func TestResolveContrastEverywhere(t *testing.T) {
	steps := []uint8{0x00, 0x11, 0x33, 0x55, 0x66, 0x77, 0x80, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xee, 0xff}
	for _, r := range steps {
		for _, g := range steps {
			for _, b := range steps {
				c := rgb{r, g, b}.hex()
				light, dark := Resolve(Colors{AccentLight: c, HeaderLight: c, HeaderDark: c})
				checks := []struct {
					what     string
					fg, bg   string
					minRatio float64
				}{
					{"light link on page", light.Link, light.Bg, 4.5},
					{"light link on surface 2", light.Link, light.Surface2, 4.5},
					{"dark primary on page", dark.Primary, dark.Bg, 3},
					{"dark link on surface 2", dark.Link, dark.Surface2, 4.5},
					{"dark link on page", dark.Link, dark.Bg, 4.5},
					{"light header text", light.HeaderText, light.HeaderBg, 4.5},
					{"light muted header text", light.HeaderTextMuted, light.HeaderBg, 4.5},
					{"dark header text", dark.HeaderText, dark.HeaderBg, 4.5},
					{"dark muted header text", dark.HeaderTextMuted, dark.HeaderBg, 4.5},
				}
				for _, ch := range checks {
					if got := Contrast(ch.fg, ch.bg); got < ch.minRatio {
						t.Errorf("%s: %s %s on %s = %.2f, want >= %v", c, ch.what, ch.fg, ch.bg, got, ch.minRatio)
					}
				}
				for _, p := range []Palette{light, dark} {
					if p.PrimaryText != "#ffffff" && p.PrimaryText != "#111111" {
						t.Errorf("%s: button text %s", c, p.PrimaryText)
					}
					if p.LinkAdjusted != (p.Link != p.Primary) {
						t.Errorf("%s: LinkAdjusted %v with link %s and primary %s", c, p.LinkAdjusted, p.Link, p.Primary)
					}
				}
			}
		}
	}
}

// cssBlock matches the three rule sets of CSS: the selector and the
// declarations.
var cssBlock = regexp.MustCompile(`(?m)^(:root|@media \(prefers-color-scheme: dark\)\{:root:not\(\[data-theme="light"\]\)|:root\[data-theme="dark"\])\{([^{}]*)\}\}?$`)

// cssDecls parses the declarations of one block into a map and fails on
// duplicates.
func cssDecls(t *testing.T, decls string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for d := range strings.SplitSeq(strings.TrimSuffix(decls, ";"), ";") {
		name, value, ok := strings.Cut(d, ":")
		if !ok {
			t.Fatalf("declaration without a colon: %q", d)
		}
		if _, dup := m[name]; dup {
			t.Fatalf("duplicate declaration of %s", name)
		}
		m[name] = value
	}
	return m
}

func TestCSS(t *testing.T) {
	for _, c := range []Colors{{}, {Mode: ModeLight}, {Mode: ModeDark}, {AccentLight: "javascript:", HeaderDark: "}"}} {
		if got := CSS(c); got != "" {
			t.Errorf("CSS(%+v) = %q, want \"\"", c, got)
		}
	}

	css := CSS(Colors{AccentLight: "#ffd400", HeaderLight: "#002855"})
	blocks := cssBlock.FindAllStringSubmatch(css, -1)
	if len(blocks) != 3 || strings.Count(css, "\n") != 3 {
		t.Fatalf("CSS has %d blocks, want 3:\n%s", len(blocks), css)
	}
	wantSelectors := []string{":root", `@media (prefers-color-scheme: dark){:root:not([data-theme="light"])`, `:root[data-theme="dark"]`}
	decls := make([]map[string]string, 3)
	for i, b := range blocks {
		if b[1] != wantSelectors[i] {
			t.Errorf("block %d selector %q, want %q", i, b[1], wantSelectors[i])
		}
		decls[i] = cssDecls(t, b[2])
		if len(decls[i]) != len(cssVars) {
			t.Errorf("block %d has %d declarations, want %d: %v", i, len(decls[i]), len(cssVars), decls[i])
		}
		for _, v := range cssVars {
			if decls[i][v] == "" {
				t.Errorf("block %d lacks %s", i, v)
			}
		}
	}
	light, dark := decls[0], decls[1]
	wantLightDecls := map[string]string{
		"--primary": "#ffd400", "--primary-hover": "#dbb600", "--primary-text": "#111111", "--link": "#856e00",
		"--focus":     "color-mix(in srgb, #856e00 45%, transparent)",
		"--header-bg": "#002855", "--header-text": "#ffffff", "--header-text-muted": "#c7d0da",
		"--header-hover": "#1a3e66", "--header-current": "#294a70", "--header-line": "#335377",
	}
	for k, v := range wantLightDecls {
		if light[k] != v {
			t.Errorf("light %s = %q, want %q", k, light[k], v)
		}
	}
	// The dark blocks carry the defaults for what is customised only in
	// light mode, so nothing leaks.
	wantDarkDecls := map[string]string{
		"--primary": "#ffd400", "--primary-hover": "#ffda24", "--primary-text": "#111111", "--link": "#ffd400",
		"--focus":     "color-mix(in srgb, #ffd400 55%, transparent)",
		"--header-bg": "#151b23", "--header-text": "#e6ebf1", "--header-text-muted": "#9ba7b6",
		"--header-hover": "#212a35", "--header-current": "#1b222c", "--header-line": "#2a3340",
	}
	for k, v := range wantDarkDecls {
		if dark[k] != v {
			t.Errorf("dark %s = %q, want %q", k, dark[k], v)
		}
	}
	if blocks[1][2] != blocks[2][2] {
		t.Errorf("the two dark blocks differ:\n%s\n%s", blocks[1][2], blocks[2][2])
	}

	// Only a dark header: light mode gets the complete default set.
	css = CSS(Colors{HeaderDark: "#000000"})
	blocks = cssBlock.FindAllStringSubmatch(css, -1)
	if len(blocks) != 3 {
		t.Fatalf("CSS has %d blocks, want 3:\n%s", len(blocks), css)
	}
	light = cssDecls(t, blocks[0][2])
	for k, v := range map[string]string{
		"--primary": "#1f5bd8", "--primary-hover": "#1a4dba", "--primary-text": "#ffffff", "--link": "#1f5bd8",
		"--focus":     "color-mix(in srgb, #1f5bd8 45%, transparent)",
		"--header-bg": "#ffffff", "--header-text": "#17202c", "--header-text-muted": "#5b6676",
		"--header-hover": "#eef1f5", "--header-current": "#f3f5f8", "--header-line": "#e2e6ec",
	} {
		if light[k] != v {
			t.Errorf("light %s = %q, want %q", k, light[k], v)
		}
	}
	dark = cssDecls(t, blocks[2][2])
	if dark["--header-bg"] != "#000000" || dark["--header-text"] != "#ffffff" || dark["--primary"] != "#2f6be0" || dark["--link"] != "#7aa9ff" {
		t.Errorf("dark declarations: %v", dark)
	}
	if dark["--focus"] != "color-mix(in srgb, #7aa9ff 55%, transparent)" {
		t.Errorf("dark focus = %q (the default of style.css)", dark["--focus"])
	}

	// Nothing that could end the <style> element or start another rule.
	for _, bad := range []string{"<", ">", "\\", "/*", "@import", "url("} {
		if strings.Contains(css, bad) {
			t.Errorf("CSS contains %q", bad)
		}
	}
}

func TestEmailButton(t *testing.T) {
	tests := []struct{ accent, bg, text string }{
		{"", "#1d5fd1", "#ffffff"},
		{"not a colour", "#1d5fd1", "#ffffff"},
		{"#FFD400", "#ffd400", "#111111"},
		{"#002855", "#002855", "#ffffff"},
		{"1f5bd8", "#1f5bd8", "#ffffff"},
		{"#eee", "#eeeeee", "#111111"},
	}
	for _, tc := range tests {
		bg, text := EmailButton(tc.accent)
		if bg != tc.bg || text != tc.text {
			t.Errorf("EmailButton(%q) = %s, %s; want %s, %s", tc.accent, bg, text, tc.bg, tc.text)
		}
	}
}
