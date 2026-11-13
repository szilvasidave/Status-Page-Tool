package theme

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // registers the GIF format for image.DecodeConfig
	_ "image/jpeg" // registers the JPEG format
	_ "image/png"  // registers the PNG format
	"io"
	"net/http"
	"strings"
)

// MaxImageBytes is the largest logo or favicon that can be uploaded.
const MaxImageBytes = 512 << 10

// Errors of CheckImage. The web layer shows its own wording for each.
var (
	ErrImageTooLarge = errors.New("the image is larger than 512 KB")
	ErrImageType     = errors.New("unsupported image type")
	ErrSVGInvalid    = errors.New("not a valid SVG image")
	ErrSVGActive     = errors.New("the SVG image contains scripts or event handlers")
	// ErrSVGNamespace: the root svg element is not in the SVG namespace
	// (no xmlns="http://www.w3.org/2000/svg"). Browsers do not draw such a
	// file as an image.
	ErrSVGNamespace = errors.New("the SVG image does not declare the SVG namespace")
)

// Content types returned by CheckImage.
const (
	TypePNG  = "image/png"
	TypeJPEG = "image/jpeg"
	TypeGIF  = "image/gif"
	TypeWebP = "image/webp"
	TypeSVG  = "image/svg+xml"
	TypeICO  = "image/x-icon"
)

// CheckImage checks an uploaded image and returns its content type. Logos
// (favicon false) may be PNG, JPEG, GIF, WebP or SVG; favicons PNG, GIF, SVG
// or ICO. The errors are ErrImageTooLarge (more than MaxImageBytes),
// ErrImageType (empty, another type, a damaged PNG, JPEG, GIF or ICO file,
// or markup whose root element is not svg, e.g. HTML), ErrSVGInvalid
// (markup that is not well-formed XML, or has no root element or content
// after it), ErrSVGNamespace (an svg root element without the SVG
// namespace) and ErrSVGActive (scripts, event handlers and the like, see
// checkSVG).
//
// The type is taken from the content, never from a file name. PNG, JPEG and
// GIF images must have a readable header (image.DecodeConfig); WebP is
// recognised by its signature only.
func CheckImage(data []byte, favicon bool) (contentType string, err error) {
	if len(data) > MaxImageBytes {
		return "", ErrImageTooLarge
	}
	if len(data) == 0 {
		return "", ErrImageType
	}
	switch ct := http.DetectContentType(data); ct {
	case TypePNG, TypeJPEG, TypeGIF:
		if favicon && ct == TypeJPEG {
			return "", ErrImageType
		}
		if err := checkRaster(data, ct); err != nil {
			return "", err
		}
		return ct, nil
	case TypeWebP:
		if favicon {
			return "", ErrImageType
		}
		return ct, nil
	}
	if isICO(data) {
		if !favicon || !validICO(data) {
			return "", ErrImageType
		}
		return TypeICO, nil
	}
	if err := checkSVG(data); err != nil {
		return "", err
	}
	return TypeSVG, nil
}

// checkRaster checks that the header of a PNG, JPEG or GIF image can be read
// and matches its signature.
func checkRaster(data []byte, contentType string) error {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != contentType || cfg.Width <= 0 || cfg.Height <= 0 {
		return ErrImageType
	}
	return nil
}

// isICO reports whether data starts like an icon file (reserved 0, type 1).
func isICO(data []byte) bool {
	return len(data) >= 4 && data[0] == 0 && data[1] == 0 && data[2] == 1 && data[3] == 0
}

// validICO checks the icon directory: at least one image, every image inside
// the file.
func validICO(data []byte) bool {
	if len(data) < 6 {
		return false
	}
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	dirEnd := 6 + 16*count
	if count == 0 || len(data) < dirEnd {
		return false
	}
	for i := range count {
		entry := data[6+16*i : 6+16*(i+1)]
		size := int64(binary.LittleEndian.Uint32(entry[8:12]))
		offset := int64(binary.LittleEndian.Uint32(entry[12:16]))
		if size == 0 || offset < int64(dirEnd) || offset+size > int64(len(data)) {
			return false
		}
	}
	return true
}

// Namespaces of SVG documents.
const (
	nsSVG   = "http://www.w3.org/2000/svg"
	nsXHTML = "http://www.w3.org/1999/xhtml"
)

// activeElements are elements (local names, lower case) that run scripts or
// embed other documents.
var activeElements = map[string]bool{
	"script": true, "foreignobject": true, "handler": true, "listener": true,
	"iframe": true, "embed": true, "object": true,
}

// activeSchemes are URL schemes that run code when an attribute value
// (e.g. a link or an animated href) starts with them.
var activeSchemes = []string{"javascript:", "vbscript:", "data:text/html"}

// checkSVG checks that data is a well-formed SVG document without active
// content. Before the svg root element there may be a UTF-8 byte order
// mark, an XML declaration, comments and a plain document type declaration
// (only a name and an external identifier, such as the public identifier of
// SVG 1.1); after it comments and white space.
//
// ErrImageType: data does not start with markup, or its root element is not
// svg (in the SVG namespace or without a namespace), e.g. an HTML file.
// ErrSVGNamespace: the svg root element has no namespace (a prefixed root
// such as <s:svg xmlns:s="http://www.w3.org/2000/svg"> is in the SVG
// namespace and passes).
// ErrSVGInvalid: not well-formed XML, content outside the root element, or
// a document type declaration (or another markup declaration) after it.
// ErrSVGActive: an <!ENTITY declaration (entity expansion), a document type
// declaration with an internal subset ("[" or "<" in it: it can declare
// default attributes such as an onload handler, and quotes inside it are
// read differently by encoding/xml and by browsers, which could hide markup
// from these checks), a processing instruction other than the XML
// declaration (e.g. an XSLT style sheet), a script, foreignObject, handler,
// listener, iframe, embed or object element or any element in the XHTML
// namespace, an attribute whose name starts with "on" (event handlers), or
// an attribute value that starts with javascript:, vbscript: or
// data:text/html (ignoring case, white space and control characters; for
// href and also inside values such as those of an animation).
func checkSVG(data []byte) error {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte("<")) {
		return ErrImageType
	}
	if bytes.Contains(bytes.ToUpper(data), []byte("<!ENTITY")) {
		return ErrSVGActive
	}

	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	dec.CharsetReader = charsetReader
	depth, rootSeen := 0, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ErrSVGInvalid
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				if rootSeen {
					return ErrSVGInvalid // a second root element
				}
				rootSeen = true
				if t.Name.Local != "svg" || (t.Name.Space != "" && t.Name.Space != nsSVG) {
					return ErrImageType
				}
				if t.Name.Space == "" {
					return ErrSVGNamespace
				}
			}
			depth++
			if err := checkSVGElement(t); err != nil {
				return err
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) > 0 {
				return ErrSVGInvalid // text outside the root element
			}
		case xml.ProcInst:
			if t.Target != "xml" {
				return ErrSVGActive
			}
		case xml.Directive:
			if rootSeen {
				return ErrSVGInvalid // a document type declaration after the root
			}
			if bytes.ContainsAny(t, "[<") {
				return ErrSVGActive // an internal subset (see above)
			}
		}
	}
	if !rootSeen {
		return ErrSVGInvalid
	}
	return nil
}

// checkSVGElement checks one start element for active content.
func checkSVGElement(t xml.StartElement) error {
	if activeElements[strings.ToLower(t.Name.Local)] || t.Name.Space == nsXHTML {
		return ErrSVGActive
	}
	for _, a := range t.Attr {
		if strings.HasPrefix(strings.ToLower(a.Name.Local), "on") || activeValue(a.Value) {
			return ErrSVGActive
		}
	}
	return nil
}

// activeValue reports whether an attribute value, or one of the values of a
// list such as "a;b" (animation values), uses one of activeSchemes. Browsers
// ignore white space and control characters in URL schemes, so they are
// removed first.
func activeValue(v string) bool {
	v = strings.ToLower(strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return -1
		}
		return r
	}, v))
	for _, s := range activeSchemes {
		if strings.Contains(v, s) {
			return true
		}
	}
	return false
}

// charsetReader lets SVG files declare Latin-1 or ASCII instead of UTF-8.
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(label) {
	case "iso-8859-1", "latin1", "latin-1", "us-ascii", "ascii":
		b, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		var out strings.Builder
		for _, c := range b {
			out.WriteRune(rune(c))
		}
		return strings.NewReader(out.String()), nil
	}
	return nil, fmt.Errorf("unsupported encoding %q", label)
}
