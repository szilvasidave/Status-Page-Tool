package theme

// FaviconSVG returns the default favicon (the drawing of static/favicon.svg)
// with the rounded square in fill and the pulse line in stroke; the web
// layer passes Primary and PrimaryText of the light palette. Colours that
// are not valid hex codes (or empty) are replaced by the defaults #1f5bd8
// and #ffffff, so the result is always a well-formed SVG.
func FaviconSVG(fill, stroke string) []byte {
	if fill = normalize(fill); fill == "" {
		fill = defaultLight.Primary
	}
	if stroke = normalize(stroke); stroke == "" {
		stroke = defaultLight.PrimaryText
	}
	return []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">` +
		`<rect width="32" height="32" rx="8" fill="` + fill + `"/>` +
		`<path d="M5.5 17h4.5l3-7.5 5 14 3.2-6.5h5.3" fill="none" stroke="` + stroke +
		`" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"/></svg>` + "\n")
}
