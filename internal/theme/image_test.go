package theme

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// testImage returns a small image with a few colours.
func testImage() image.Image {
	img := image.NewPaletted(image.Rect(0, 0, 8, 4), color.Palette{color.White, color.Black, color.RGBA{0x1f, 0x5b, 0xd8, 0xff}})
	for x := range 8 {
		img.SetColorIndex(x, x%4, uint8(x%3))
	}
	return img
}

func encodePNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, testImage()); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeJPEG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeGIF(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := gif.Encode(&b, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// encodeICO wraps a PNG in an icon file with one image (as modern ICO files
// do).
func encodeICO(t *testing.T) []byte {
	t.Helper()
	p := encodePNG(t)
	var b bytes.Buffer
	b.Write([]byte{0, 0, 1, 0, 1, 0})                     // reserved, type 1 (icon), 1 image
	b.Write([]byte{8, 4, 0, 0, 1, 0, 32, 0})              // width, height, colours, reserved, planes, bits
	binary.Write(&b, binary.LittleEndian, uint32(len(p))) // size
	binary.Write(&b, binary.LittleEndian, uint32(6+16))   // offset
	b.Write(p)
	return b.Bytes()
}

// webp is the start of a WebP file (RIFF container, VP8L bitstream).
var webp = []byte("RIFF\x1a\x00\x00\x00WEBPVP8L\x0d\x00\x00\x00\x2f\x00\x00\x00\x10\x07\x10\x11\x11\x88\x88\xfe\x07\x00")

const plainSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="#1f5bd8"/></svg>`

func TestCheckImage(t *testing.T) {
	pngData, jpegData, gifData, icoData := encodePNG(t), encodeJPEG(t), encodeGIF(t), encodeICO(t)
	big := append(append([]byte{}, pngData...), make([]byte, MaxImageBytes)...)
	exact := []byte(plainSVG + "<!--" + strings.Repeat("x", MaxImageBytes-len(plainSVG)-7) + "-->")
	if len(exact) != MaxImageBytes {
		t.Fatalf("test data has %d bytes", len(exact))
	}

	tests := []struct {
		name    string
		data    []byte
		favicon bool
		want    string
		err     error
	}{
		// raster images
		{"png logo", pngData, false, TypePNG, nil},
		{"png favicon", pngData, true, TypePNG, nil},
		{"jpeg logo", jpegData, false, TypeJPEG, nil},
		{"jpeg favicon", jpegData, true, "", ErrImageType},
		{"gif logo", gifData, false, TypeGIF, nil},
		{"gif favicon", gifData, true, TypeGIF, nil},
		{"webp logo", webp, false, TypeWebP, nil},
		{"webp favicon", webp, true, "", ErrImageType},
		{"ico favicon", icoData, true, TypeICO, nil},
		{"ico logo", icoData, false, "", ErrImageType},
		{"ico without images", []byte{0, 0, 1, 0, 0, 0}, true, "", ErrImageType},
		{"ico header only", []byte{0, 0, 1, 0}, true, "", ErrImageType},
		{"ico image outside the file", icoData[:len(icoData)-10], true, "", ErrImageType},
		{"truncated png", pngData[:20], false, "", ErrImageType},
		{"png signature only", []byte("\x89PNG\r\n\x1a\n"), false, "", ErrImageType},
		{"truncated gif", []byte("GIF89a"), true, "", ErrImageType},
		{"jpeg signature only", []byte("\xff\xd8\xff\xe0garbage"), false, "", ErrImageType},

		// size
		{"empty", nil, false, "", ErrImageType},
		{"too large", big, false, "", ErrImageTooLarge},
		{"too large favicon", big, true, "", ErrImageTooLarge},
		{"exactly the limit", exact, false, TypeSVG, nil},
		{"one byte over", append(exact, ' '), false, "", ErrImageTooLarge},

		// other files
		{"text", []byte("hello world"), false, "", ErrImageType},
		{"pdf", []byte("%PDF-1.7\n..."), false, "", ErrImageType},
		{"html", []byte("<!DOCTYPE html><html><body><script>alert(1)</script></body></html>"), false, "", ErrImageType},
		{"non-svg xml root", []byte(`<?xml version="1.0"?><note><to>x</to></note>`), false, "", ErrImageType},
		{"xhtml root", []byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body/></html>`), true, "", ErrImageType},
		{"svg in another namespace", []byte(`<svg xmlns="http://example.com/svg"/>`), false, "", ErrImageType},

		// valid SVG
		{"svg logo", []byte(plainSVG), false, TypeSVG, nil},
		{"svg favicon", []byte(plainSVG), true, TypeSVG, nil},
		{"svg without namespace", []byte(`<svg viewBox="0 0 1 1"><circle r="1"/></svg>`), false, TypeSVG, nil},
		{"svg with prefix", []byte(`<s:svg xmlns:s="http://www.w3.org/2000/svg"><s:rect width="1" height="1"/></s:svg>`), false, TypeSVG, nil},
		{"svg with declaration, comments and doctype", []byte("\xef\xbb\xbf<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"no\"?>\n" +
			"<!-- Generator: Some Editor 1.0 -->\n<!-- second comment -->\n" +
			`<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN" "http://www.w3.org/Graphics/SVG/1.1/DTD/svg11.dtd">` + "\n" +
			`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" version="1.1">` +
			`<defs><linearGradient id="g"><stop offset="0" stop-color="#fff"/></linearGradient></defs>` +
			`<style>.a{fill:url(#g)}</style><a xlink:href="https://example.com/"><text x="0" y="10">Acme &amp; Co</text></a>` +
			`<use href="#g"/><![CDATA[ some data ]]></svg>` + "\n<!-- trailing comment -->\n"), false, TypeSVG, nil},
		{"latin-1 svg", []byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><svg xmlns=\"http://www.w3.org/2000/svg\"><title>Caf\xe9</title></svg>"), false, TypeSVG, nil},
		{"svg with leading white space", []byte("\n\n  " + plainSVG), false, TypeSVG, nil},
		{"svg with opacity and offset attributes", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect opacity=".5" width="1" height="1"/></svg>`), false, TypeSVG, nil},

		// broken SVG
		{"unclosed svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect>`), false, "", ErrSVGInvalid},
		{"mismatched tags", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><g></rect></svg>`), false, "", ErrSVGInvalid},
		{"html entity", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><text>a&nbsp;b</text></svg>`), false, "", ErrSVGInvalid},
		{"two roots", []byte(plainSVG + plainSVG), false, "", ErrSVGInvalid},
		{"text after root", []byte(plainSVG + "junk"), false, "", ErrSVGInvalid},
		{"text before root", []byte("<!-- c -->junk" + plainSVG), false, "", ErrSVGInvalid},
		{"comment only", []byte("<!-- nothing -->"), false, "", ErrSVGInvalid},
		{"unknown encoding", []byte(`<?xml version="1.0" encoding="EBCDIC"?><svg/>`), false, "", ErrSVGInvalid},
		{"invalid utf-8", []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"><title>\xff\xfe</title></svg>"), false, "", ErrSVGInvalid},

		// active content
		{"script", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), false, "", ErrSVGActive},
		{"script in a group", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><g><g><script type="text/ecmascript"><![CDATA[alert(1)]]></script></g></g></svg>`), false, "", ErrSVGActive},
		{"upper-case script", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><SCRIPT>alert(1)</SCRIPT></svg>`), false, "", ErrSVGActive},
		{"xhtml script", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><h:script xmlns:h="http://www.w3.org/1999/xhtml">alert(1)</h:script></svg>`), false, "", ErrSVGActive},
		{"xhtml element", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><p xmlns="http://www.w3.org/1999/xhtml">x</p></svg>`), false, "", ErrSVGActive},
		{"onload", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), true, "", ErrSVGActive},
		{"ONCLICK on a child", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect ONCLICK="alert(1)"/></svg>`), false, "", ErrSVGActive},
		{"onbegin on an animation", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><animate onbegin="alert(1)" attributeName="x"/></svg>`), false, "", ErrSVGActive},
		{"javascript href", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><a href="javascript:alert(1)"><text>x</text></a></svg>`), false, "", ErrSVGActive},
		{"javascript xlink:href with spaces and case", []byte(`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><a xlink:href="  JavaScript:alert(1)"><text>x</text></a></svg>`), false, "", ErrSVGActive},
		{"javascript href with a tab and entities", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><a href="java&#9;scr&#105;pt:alert(1)"><text>x</text></a></svg>`), false, "", ErrSVGActive},
		{"data:text/html href", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><a href="data:text/html;base64,PHNjcmlwdD4="><text>x</text></a></svg>`), false, "", ErrSVGActive},
		{"animated javascript href", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><a><set attributeName="href" to="javascript:alert(1)"/><text>x</text></a></svg>`), false, "", ErrSVGActive},
		{"javascript in animation values", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><a><animate attributeName="href" values="#a;javascript:alert(1)"/></a></svg>`), false, "", ErrSVGActive},
		{"foreignObject", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><foreignObject width="10" height="10"><div>x</div></foreignObject></svg>`), false, "", ErrSVGActive},
		{"iframe", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><iframe src="https://example.com/"/></svg>`), false, "", ErrSVGActive},
		{"entity declaration", []byte(`<?xml version="1.0"?><!DOCTYPE svg [<!ENTITY a "aaaaaaaaaa"><!ENTITY b "&a;&a;&a;&a;">]><svg xmlns="http://www.w3.org/2000/svg"><text>&b;</text></svg>`), false, "", ErrSVGActive},
		{"lower-case entity declaration", []byte(`<!DOCTYPE svg [<!entity a "x">]><svg xmlns="http://www.w3.org/2000/svg"/>`), false, "", ErrSVGActive},
		{"xslt style sheet", []byte(`<?xml version="1.0"?><?xml-stylesheet type="text/xsl" href="https://example.com/x.xsl"?><svg xmlns="http://www.w3.org/2000/svg"/>`), false, "", ErrSVGActive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckImage(tc.data, tc.favicon)
			if got != tc.want || !errors.Is(err, tc.err) || (tc.err == nil) != (err == nil) {
				t.Errorf("CheckImage = %q, %v; want %q, %v", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestFaviconSVG(t *testing.T) {
	want := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="8" fill="#1f5bd8"/>` +
		`<path d="M5.5 17h4.5l3-7.5 5 14 3.2-6.5h5.3" fill="none" stroke="#ffffff" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"/></svg>` + "\n"
	if got := string(FaviconSVG("#1f5bd8", "#fff")); got != want {
		t.Errorf("default favicon:\n got %s\nwant %s", got, want)
	}

	light, _ := Resolve(Colors{AccentLight: "#ffd400"})
	got := FaviconSVG(light.Primary, light.PrimaryText)
	if !bytes.Contains(got, []byte(`fill="#ffd400"`)) || !bytes.Contains(got, []byte(`stroke="#111111"`)) {
		t.Errorf("favicon in the accent colour: %s", got)
	}
	if ct, err := CheckImage(got, true); err != nil || ct != TypeSVG {
		t.Errorf("generated favicon does not pass CheckImage: %q, %v", ct, err)
	}

	// Invalid colours fall back to the defaults instead of breaking the SVG.
	if got := string(FaviconSVG(`"/><script>alert(1)</script>`, "")); got != want {
		t.Errorf("favicon with invalid colours:\n got %s\nwant %s", got, want)
	}
}
