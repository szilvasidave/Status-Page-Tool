package web

import (
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// ------------------------------------------------------------------ flash

// Flash is a one-time message shown on the next rendered page.
type Flash struct {
	Kind    string // success, info, warning (done, but with a problem), error
	Message string
}

// flashKinds are the allowed flash kinds (also CSS classes flash-<kind>).
var flashKinds = map[string]bool{"success": true, "info": true, "warning": true, "error": true}

const flashCookie = "sp_flash"

// flashMaxLen is the maximum length of a flash message in bytes, so that the
// cookie stays well below the 4 KB limit of browsers.
const flashMaxLen = 1000

// flash stores a message that is shown on the next page (use before a
// redirect). Longer messages are shortened.
func (s *Server) flash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	v := base64.RawURLEncoding.EncodeToString([]byte(kind + "|" + truncate(msg, flashMaxLen)))
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: v, Path: "/", HttpOnly: true, Secure: auth.IsHTTPS(r), SameSite: http.SameSiteLaxMode})
}

func (s *Server) popFlash(w http.ResponseWriter, r *http.Request) *Flash {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: auth.IsHTTPS(r), SameSite: http.SameSiteLaxMode})
	b, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	kind, msg, ok := strings.Cut(string(b), "|")
	if !ok || !flashKinds[kind] {
		return nil
	}
	return &Flash{Kind: kind, Message: msg}
}

// redirectWithFlash sets a flash message and redirects (303) to target.
func (s *Server) redirectWithFlash(w http.ResponseWriter, r *http.Request, target, kind, msg string) {
	s.flash(w, r, kind, msg)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// ------------------------------------------------------------------ request helpers

// pathID parses the {id} path value. Writes a 404 and returns false if invalid.
func (s *Server) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.notFound(w, r)
		return 0, false
	}
	return id, true
}

// formInt64s returns all integer values of a multi-value form field
// (e.g. checkboxes named "services"). Invalid values are ignored.
func formInt64s(r *http.Request, name string) []int64 {
	_ = r.ParseForm()
	var out []int64
	for _, v := range r.Form[name] {
		if id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}

// formBool reports whether a checkbox field was checked.
func formBool(r *http.Request, name string) bool {
	v := r.FormValue(name)
	return v == "on" || v == "true" || v == "1" || v == "yes"
}

// formTrim returns the trimmed value of a form field.
func formTrim(r *http.Request, name string) string { return strings.TrimSpace(r.FormValue(name)) }

// Maximum lengths (in characters) of the free-text fields of editors.
const (
	maxTitleLen = 200   // incident and maintenance titles
	maxTextLen  = 10000 // incident messages and maintenance descriptions
)

// tooLong reports whether s is longer than max characters. Length limits
// count characters (runes) like the maxlength attribute of the inputs, not
// bytes.
func tooLong(s string, max int) bool { return utf8.RuneCountInString(s) > max }

// ------------------------------------------------------------------ text helpers

// plural returns e.g. "1 group" or "3 groups".
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// truncate shortens s to at most n bytes, cut at a rune boundary, and
// appends "…" if it was shortened.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// InputTimeLayout is the layout of <input type="datetime-local"> values.
const InputTimeLayout = "2006-01-02T15:04"

// parseInputTime parses a datetime-local value in the display time zone.
func (s *Server) parseInputTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, fmt.Errorf("missing date/time")
	}
	t, err := time.ParseInLocation(InputTimeLayout, v, s.location())
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date/time %q", v)
	}
	return t, nil
}

func (s *Server) location() *time.Location {
	if l := s.loc.Load(); l != nil {
		return l
	}
	return time.UTC
}

func urlQueryEscape(v string) string { return url.QueryEscape(v) }

// ------------------------------------------------------------------ template funcs

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		// fmtTime formats a time.Time or *time.Time in the display time
		// zone, e.g. "12 Nov 2026 14:05 CET". nil/zero → "".
		"fmtTime": func(v any) string { return s.formatTime(v, "2 Jan 2006 15:04 MST") },
		// fmtDate formats a date, e.g. "12 Nov 2026".
		"fmtDate": func(v any) string { return s.formatTime(v, "2 Jan 2006") },
		// fmtClock formats the time of day, e.g. "14:05".
		"fmtClock": func(v any) string { return s.formatTime(v, "15:04") },
		// fmtInput formats a time for <input type="datetime-local">.
		"fmtInput": func(v any) string { return s.formatTime(v, InputTimeLayout) },
		// isoTime formats a time as RFC 3339 for <time datetime="...">.
		"isoTime": func(v any) string {
			t, ok := toTime(v)
			if !ok {
				return ""
			}
			return t.UTC().Format(time.RFC3339)
		},
		// ago returns a relative time such as "5 minutes ago".
		"ago": func(v any) string {
			t, ok := toTime(v)
			if !ok {
				return ""
			}
			return humanizeAgo(time.Since(t))
		},
		// duration returns a human readable duration between two times
		// (second argument may be nil → now).
		"duration": func(a, b any) string {
			ta, ok := toTime(a)
			if !ok {
				return ""
			}
			tb, ok := toTime(b)
			if !ok {
				tb = time.Now()
			}
			return humanizeDuration(tb.Sub(ta))
		},
		// tz returns the display time zone name.
		"tz": func() string { return s.location().String() },
		// nl2br escapes text and converts newlines to <br>.
		"nl2br": func(text string) template.HTML {
			esc := template.HTMLEscapeString(text)
			esc = strings.ReplaceAll(esc, "\r\n", "\n")
			return template.HTML(strings.ReplaceAll(esc, "\n", "<br>"))
		},
		// containsID reports whether ids contains id.
		"containsID": func(ids []int64, id int64) bool {
			for _, v := range ids {
				if v == id {
					return true
				}
			}
			return false
		},
		// dict builds a map from key/value pairs for passing to partials.
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
		"add":  func(a, b int) int { return a + b },
		"join": strings.Join,
		// statusLabel / overallLabel give human readable status texts.
		"statusLabel":  func(st store.Status) string { return st.Label() },
		"overallLabel": store.OverallLabel,
		"impacts":      func() []store.Status { return store.Impacts },
		"incidentStatuses": func() []store.IncidentStatus {
			return store.IncidentStatuses
		},
	}
}

func toTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, !t.IsZero()
	case *time.Time:
		if t == nil || t.IsZero() {
			return time.Time{}, false
		}
		return *t, true
	}
	return time.Time{}, false
}

func (s *Server) formatTime(v any, layout string) string {
	t, ok := toTime(v)
	if !ok {
		return ""
	}
	return t.In(s.location()).Format(layout)
}

func humanizeAgo(d time.Duration) string {
	if d < 0 {
		return "in " + humanizeDuration(-d)
	}
	if d < time.Minute {
		return "just now"
	}
	return humanizeDuration(d) + " ago"
}

func humanizeDuration(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute"
	}
	mins := int(d.Minutes())
	days, hours, m := mins/(60*24), (mins/60)%24, mins%60
	switch {
	case days > 0 && hours > 0:
		return plural(days, "day") + " " + plural(hours, "hour")
	case days > 0:
		return plural(days, "day")
	case hours > 0 && m > 0:
		return plural(hours, "hour") + " " + plural(m, "minute")
	case hours > 0:
		return plural(hours, "hour")
	}
	return plural(m, "minute")
}
