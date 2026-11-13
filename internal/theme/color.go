package theme

import (
	"math"
	"strconv"
	"strings"
)

// Colours used by the derivation rules.
const (
	white = "#ffffff"
	black = "#000000"
	// nearBlack is the dark text candidate on accent colours (buttons).
	nearBlack = "#111111"
)

// NormalizeHex accepts "#abc", "#aabbcc", "abc" and "aabbcc" (any case,
// surrounding spaces) and returns "#aabbcc" in lower case and true. An empty
// string (or only spaces) returns "", true: the default colour. Anything else
// returns "", false.
func NormalizeHex(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	s = strings.TrimPrefix(s, "#")
	for i := 0; i < len(s); i++ {
		if !isHexDigit(s[i]) {
			return "", false
		}
	}
	s = strings.ToLower(s)
	switch len(s) {
	case 3:
		return "#" + string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]}), true
	case 6:
		return "#" + s, true
	}
	return "", false
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// normalize returns the "#rrggbb" form of s, or "" when s is empty or
// invalid (the default).
func normalize(s string) string {
	v, _ := NormalizeHex(s)
	return v
}

// rgb is a colour with 8-bit sRGB channels.
type rgb [3]uint8

// parseHex parses a colour in any form NormalizeHex accepts; empty or
// invalid colours are black.
func parseHex(s string) rgb {
	var c rgb
	v := normalize(s)
	if v == "" {
		return c
	}
	for i := range c {
		n, _ := strconv.ParseUint(v[1+2*i:3+2*i], 16, 8)
		c[i] = uint8(n)
	}
	return c
}

// hex formats c as "#rrggbb".
func (c rgb) hex() string {
	const digits = "0123456789abcdef"
	b := []byte{'#', 0, 0, 0, 0, 0, 0}
	for i, v := range c {
		b[1+2*i] = digits[v>>4]
		b[2+2*i] = digits[v&0x0f]
	}
	return string(b)
}

// luminance is the WCAG 2 relative luminance of c (0 = black, 1 = white).
func (c rgb) luminance() float64 {
	var lin [3]float64
	for i, v := range c {
		f := float64(v) / 255
		if f <= 0.04045 {
			lin[i] = f / 12.92
		} else {
			lin[i] = math.Pow((f+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*lin[0] + 0.7152*lin[1] + 0.0722*lin[2]
}

// Contrast returns the WCAG 2 contrast ratio of two colours (1 to 21), given
// in any form NormalizeHex accepts; empty or invalid colours count as black.
func Contrast(a, b string) float64 {
	la, lb := parseHex(a).luminance(), parseHex(b).luminance()
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// mix blends two colours channel by channel in sRGB (like the CSS
// color-mix(in srgb, a t, b)): t is the share of a (0 to 1). The result is
// rounded to whole channel values.
func mix(a, b string, t float64) string {
	ca, cb := parseHex(a), parseHex(b)
	var c rgb
	for i := range c {
		c[i] = uint8(math.Round(float64(ca[i])*t + float64(cb[i])*(1-t)))
	}
	return c.hex()
}

// ensureContrast returns c when its contrast with bg is at least minRatio,
// else c blended towards (black or white) in steps of 2 % until it is; at
// the end it is towards itself. The result is "#rrggbb".
//
// internal/web/static/app.js ports it (with luminance, Contrast and mix) for
// the live default of the dark accent field on the branding page: keep the
// two in sync.
func ensureContrast(c, bg string, minRatio float64, towards string) string {
	c = parseHex(c).hex()
	if Contrast(c, bg) >= minRatio {
		return c
	}
	for i := 1; i <= 50; i++ {
		r := mix(towards, c, float64(i)/50)
		if Contrast(r, bg) >= minRatio {
			return r
		}
	}
	return parseHex(towards).hex()
}

// bestText returns white or dark, whichever contrasts more with bg (white
// when both are equal).
func bestText(bg, dark string) string {
	if Contrast(white, bg) >= Contrast(dark, bg) {
		return white
	}
	return dark
}

// buttonText returns the text colour of a button (or another control) in
// the colour bg: white or #111111, whichever contrasts more; if that is
// below 4.5 (mid-tones), white or black, whichever contrasts more. One of
// white and black always reaches 4.58, so the result is always readable.
func buttonText(bg string) string {
	if text := bestText(bg, nearBlack); Contrast(text, bg) >= minTextContrast {
		return text
	}
	return bestText(bg, black)
}

// opposite returns black for a light colour (e.g. white text) and white for
// a dark one (e.g. dark text or black). Mixed into a background, the
// opposite of a text colour raises the contrast with that text.
func opposite(c string) string {
	if parseHex(c).luminance() > 0.5 {
		return black
	}
	return white
}
