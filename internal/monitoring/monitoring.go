// Package monitoring connects external monitoring tools to the status page:
// each integration (Admin → Monitoring) has a webhook URL, and the tool
// sends its alerts there. The alerts open, update and resolve incidents of
// the services they name (store.ApplyAlerts), so that the status of a
// service follows the monitoring without anybody updating it by hand.
//
// The package has three parts:
//
//   - Providers (providers.go): one entry per monitoring tool, with the
//     function that reads its requests (Parse) and the setup instructions
//     shown to administrators. Tools whose request body can be configured
//     (Zabbix, PRTG, Datadog, UptimeRobot, Nagios, scripts, …) send the
//     generic format (generic.go; PRTG and Datadog with a few rules of
//     their own, prtg.go and datadog.go); tools with a fixed format have
//     their own parser (alertmanager.go for Prometheus Alertmanager and
//     Grafana, uptimekuma.go, azure.go). To support another tool, add an
//     entry to providers – with a parser of its own only if it cannot send
//     the generic format – and a test with a request the tool really sends.
//   - Normalisation (this file): status words, severities and impacts of
//     the tools mapped to the status page's, and the limits applied to
//     every alert before it reaches the store.
//   - The handler (handler.go) for POST /hooks/{token}, which authenticates
//     the request by its token, records it for the integration's request
//     log, applies the alerts and sends the e-mails; and the worker that
//     opens the incidents of alerts that fired during maintenance once it
//     has ended.
package monitoring

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/szilvasidave/status-page-tool/internal/secret"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// Path is the path of the webhook URLs: <Base URL>/hooks/<token>, or
// <Base URL>/hooks with the header "Authorization: Bearer <token>".
const Path = "/hooks"

// Limits applied to every request and alert.
const (
	MaxBodyBytes     = 1 << 20 // request body
	MaxAlerts        = 1000    // alerts per request; more are ignored
	maxTitleLength   = 200     // characters, like incident titles
	maxMessageLength = 2000    // characters of an alert's description
	maxKeyLength     = 200     // longer keys are replaced by their SHA-256
	maxTargets       = 10      // values that may name the service
	maxTargetLength  = 200     // characters, like the names of services in monitoring tools
	maxEchoLength    = 80      // characters of a value quoted in a note or an error (echo)
)

// Request is a webhook request as a Provider reads it.
type Request struct {
	Body        []byte
	ContentType string     // media type in lower case, without parameters ("" if none)
	Query       url.Values // query parameters of the URL (the generic format reads them)
}

// Alert is an alert read from a request, before normalisation.
type Alert struct {
	// Key identifies the alert within the integration, so that the
	// resolution of an alert clears it (e.g. the Alertmanager fingerprint,
	// the Uptime Kuma monitor id). Required.
	Key    string
	Firing bool // false: resolved
	// Targets are values that may name the affected service (its name or
	// one of its names in monitoring tools), most specific first: the first
	// one that names a service decides (store.MatchServices).
	Targets  []string
	Title    string
	Message  string
	Severity string       // as the tool sends it; see SeverityImpact
	Impact   store.Status // asked for explicitly (e.g. a label statuspage_impact); "" = none
	StartsAt time.Time    // when the problem began, if the tool says so
}

// Batch is what a Provider read from one request.
type Batch struct {
	Alerts []Alert
	// Test reports a test message of the tool: it is recorded in the
	// integration's request log and changes nothing.
	Test bool
	// Notes are remarks for the request log, e.g. about alerts that were
	// skipped.
	Notes []string
}

// ParseError is a request that a Provider cannot read; its message is shown
// to the sender (HTTP 400) and in the integration's request log.
type ParseError struct{ msg string }

func (e *ParseError) Error() string { return e.msg }

func parseErrorf(format string, args ...any) error {
	return &ParseError{msg: fmt.Sprintf(format, args...)}
}

// NewToken returns a new random webhook token (32 bytes, base64url) and the
// hash to store (store.CreateIntegration, store.SetIntegrationToken).
func NewToken() (token, hash string) {
	token = secret.RandomToken(32)
	return token, HashToken(token)
}

// HashToken returns the stored form of a token: SHA-256, hex.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ------------------------------------------------------------------ status words

// state is what a status word of a tool means.
type state int

const (
	stateUnknown  state = iota // neither (e.g. pending, unknown, no data): ignored
	stateFiring                // a problem
	stateResolved              // the problem is over
	stateTest                  // a test message
)

// statusWords maps the first word of a status, in lower case, to its state
// and the severity it implies ("" if none). They cover the status texts of
// common tools: Nagios/Icinga (OK, WARNING, CRITICAL, UNKNOWN, UP, DOWN,
// UNREACHABLE), PRTG (Up, Down, Warning, Unusual, Paused), Datadog
// (Triggered, Re-Triggered, Warn, Re-Warn, Recovered, No Data, Renotify),
// UptimeRobot (Up, Down), Zabbix scripts (problem, resolved).
var statusWords = map[string]struct {
	state    state
	severity string
}{
	// problems
	"firing": {stateFiring, ""}, "fire": {stateFiring, ""}, "fired": {stateFiring, ""},
	"problem": {stateFiring, ""}, "down": {stateFiring, ""}, "alert": {stateFiring, ""},
	"alerting": {stateFiring, ""}, "alarm": {stateFiring, ""}, "triggered": {stateFiring, ""},
	"re-triggered": {stateFiring, ""}, "retriggered": {stateFiring, ""}, "renotify": {stateFiring, ""},
	"open": {stateFiring, ""}, "opened": {stateFiring, ""}, "active": {stateFiring, ""},
	"fail": {stateFiring, ""}, "failed": {stateFiring, ""}, "failure": {stateFiring, ""},
	"failing": {stateFiring, ""}, "unreachable": {stateFiring, ""}, "unavailable": {stateFiring, ""},
	"outage": {stateFiring, ""}, "nok": {stateFiring, ""}, "bad": {stateFiring, ""},
	"critical": {stateFiring, "critical"}, "crit": {stateFiring, "critical"},
	"error": {stateFiring, "error"}, "err": {stateFiring, "error"},
	"warning": {stateFiring, "warning"}, "warn": {stateFiring, "warning"}, "re-warn": {stateFiring, "warning"},
	"rewarn": {stateFiring, "warning"}, "degraded": {stateFiring, "warning"}, "unusual": {stateFiring, "warning"},
	// recoveries
	"resolved": {stateResolved, ""}, "resolve": {stateResolved, ""}, "ok": {stateResolved, ""},
	"okay": {stateResolved, ""}, "up": {stateResolved, ""}, "recovery": {stateResolved, ""},
	"recovered": {stateResolved, ""}, "recover": {stateResolved, ""}, "clear": {stateResolved, ""},
	"cleared": {stateResolved, ""}, "closed": {stateResolved, ""}, "close": {stateResolved, ""},
	"normal": {stateResolved, ""}, "operational": {stateResolved, ""}, "healthy": {stateResolved, ""},
	"good": {stateResolved, ""}, "fixed": {stateResolved, ""}, "inactive": {stateResolved, ""},
	"success": {stateResolved, ""}, "succeeded": {stateResolved, ""}, "pass": {stateResolved, ""},
	"passed": {stateResolved, ""}, "passing": {stateResolved, ""},
	// tests
	"test": {stateTest, ""}, "testing": {stateTest, ""},
}

// parseStatus reads a status text such as "firing", "Down", "CRITICAL",
// "Down (Partial)" or PRTG's "Down ended (now: Up)". It returns the state
// and the severity the text implies ("" if none; "partial" for a partial
// outage).
func parseStatus(s string) (state, string) {
	s = strings.ToLower(strings.TrimSpace(s))
	// PRTG: "Down ended (now: Up)", "Warning ended (now: Down)".
	if i := strings.LastIndex(s, "(now:"); i >= 0 {
		s = strings.TrimSpace(strings.TrimRight(s[i+len("(now:"):], ") "))
	}
	word := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && r != '-' })
	if len(word) == 0 {
		return stateUnknown, ""
	}
	w, ok := statusWords[word[0]]
	if !ok {
		return stateUnknown, ""
	}
	if w.state == stateFiring && strings.Contains(s, "partial") {
		return stateFiring, "partial"
	}
	return w.state, w.severity
}

// ------------------------------------------------------------------ severities and impacts

// severityImpacts maps severities, normalised by severityKey, to impacts.
// They cover Prometheus/Grafana labels (critical, error, warning, info),
// Zabbix (Disaster, High, Average, Warning, Information, Not classified),
// Azure Monitor (Sev0–Sev4), priorities (P1–P5) and syslog levels.
var severityImpacts = map[string]store.Status{
	"critical": store.StatusMajorOutage, "crit": store.StatusMajorOutage, "fatal": store.StatusMajorOutage,
	"emergency": store.StatusMajorOutage, "emerg": store.StatusMajorOutage, "disaster": store.StatusMajorOutage,
	"high": store.StatusMajorOutage, "page": store.StatusMajorOutage, "major": store.StatusMajorOutage,
	"sev0": store.StatusMajorOutage, "p1": store.StatusMajorOutage,

	"error": store.StatusPartialOutage, "err": store.StatusPartialOutage, "average": store.StatusPartialOutage,
	"partial": store.StatusPartialOutage, "sev1": store.StatusPartialOutage, "p2": store.StatusPartialOutage,

	"warning": store.StatusDegraded, "warn": store.StatusDegraded, "minor": store.StatusDegraded,
	"medium": store.StatusDegraded, "moderate": store.StatusDegraded, "low": store.StatusDegraded,
	"info": store.StatusDegraded, "information": store.StatusDegraded, "informational": store.StatusDegraded,
	"notice": store.StatusDegraded, "notclassified": store.StatusDegraded, "verbose": store.StatusDegraded,
	"debug": store.StatusDegraded, "none": store.StatusDegraded, "sev2": store.StatusDegraded,
	"sev3": store.StatusDegraded, "sev4": store.StatusDegraded, "p3": store.StatusDegraded,
	"p4": store.StatusDegraded, "p5": store.StatusDegraded,
}

// severityKey normalises a severity: lower case without spaces, hyphens and
// underscores ("Not classified" → "notclassified", "SEV-1" → "sev1").
func severityKey(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '-' || r == '_' {
			return -1
		}
		return unicode.ToLower(r)
	}, s)
}

// SeverityImpact returns the impact of a severity of a monitoring tool:
// critical (and its equivalents) is a major outage, error a partial outage,
// warning and lower degraded performance. ok is false for an unknown or
// empty severity.
func SeverityImpact(severity string) (impact store.Status, ok bool) {
	impact, ok = severityImpacts[severityKey(severity)]
	return impact, ok
}

// ParseImpact reads an impact given explicitly, e.g. in a label
// statuspage_impact: degraded, partial_outage or major_outage (case, spaces
// and hyphens do not matter; "degraded performance", "partial" and "major"
// are understood too). ok is false for anything else.
func ParseImpact(s string) (impact store.Status, ok bool) {
	switch severityKey(s) {
	case "degraded", "degradedperformance":
		return store.StatusDegraded, true
	case "partialoutage", "partial":
		return store.StatusPartialOutage, true
	case "majoroutage", "major":
		return store.StatusMajorOutage, true
	}
	return "", false
}

// impactOf returns the impact of a firing alert: the impact the alert asks
// for, else the integration's fixed impact (fixed, "" if none), else the
// impact of its severity, else a major outage (e.g. an uptime monitor that
// reports a service down).
func impactOf(a *Alert, fixed store.Status) store.Status {
	switch {
	case store.ValidImpact(a.Impact):
		return a.Impact
	case store.ValidImpact(fixed):
		return fixed
	}
	if impact, ok := SeverityImpact(a.Severity); ok {
		return impact
	}
	return store.StatusMajorOutage
}

// ------------------------------------------------------------------ normalisation

// input returns the alert as the store takes it, with the limits applied:
// a title of one line, a message without control characters, a short key
// and at most maxTargets targets.
func (a *Alert) input(fixed store.Status) store.AlertInput {
	in := store.AlertInput{
		Key:       oneLine(a.Key),
		Firing:    a.Firing,
		Title:     clip(oneLine(a.Title), maxTitleLength),
		Message:   clip(cleanText(a.Message), maxMessageLength),
		StartedAt: a.StartsAt,
	}
	if len(in.Key) > maxKeyLength {
		in.Key = HashToken(in.Key)
	}
	if in.Firing {
		in.Impact = impactOf(a, fixed)
	}
	for _, t := range a.Targets {
		t = oneLine(t)
		if t == "" || len([]rune(t)) > maxTargetLength || containsFold(in.Targets, t) {
			continue
		}
		if in.Targets = append(in.Targets, t); len(in.Targets) == maxTargets {
			break
		}
	}
	return in
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// oneLine returns s as valid UTF-8 with every run of white space and
// control characters as one space.
func oneLine(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
}

// cleanText returns s as valid UTF-8 without control characters other than
// line breaks and tabs, with Windows line breaks as "\n" and without
// surrounding white space. (PostgreSQL stores neither invalid UTF-8 nor NUL
// characters, which form fields can carry.)
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// clip shortens s to at most n characters, the last one "…".
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// echo returns a value of a request as it is quoted in a note or an error
// (the request log shows them as lines of their own): one line of at most
// maxEchoLength characters.
func echo(s string) string { return clip(oneLine(s), maxEchoLength) }

// ------------------------------------------------------------------ JSON helpers

// text is a JSON value read as text: strings as they are, numbers and
// booleans as written, null as "". Objects and arrays are an error.
type text string

func (t *text) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || string(b) == "null":
		*t = ""
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*t = text(s)
	case b[0] == '{' || b[0] == '[':
		return errors.New("expected text, a number or true/false, not an object or a list")
	default:
		*t = text(b)
	}
	return nil
}

// texts is a JSON value read as a list of texts: a list, or one value.
type texts []string

func (t *texts) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '[' {
		var list []text
		if err := json.Unmarshal(b, &list); err != nil {
			return err
		}
		*t = make(texts, len(list))
		for i, v := range list {
			(*t)[i] = string(v)
		}
		return nil
	}
	var one text
	if err := json.Unmarshal(b, &one); err != nil {
		return err
	}
	*t = nil
	if one != "" {
		*t = texts{string(one)}
	}
	return nil
}

// jsonError describes a JSON decoding error for the sender.
func jsonError(err error) string {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Sprintf("the request is not valid JSON (at byte %d: %s)", syntax.Offset, echo(syntax.Error()))
	case errors.As(err, &typeErr) && typeErr.Field != "":
		// Value can hold the number that did not fit, as it was sent.
		return fmt.Sprintf("unexpected JSON value for “%s” (%s)", echo(typeErr.Field), echo(typeErr.Value))
	}
	return "the request is not valid JSON: " + clip(oneLine(err.Error()), 2*maxEchoLength)
}

// timeLayouts are the layouts of the times parseTime reads besides Unix
// times. A time without a zone is UTC; fractions of a second are read
// after the seconds whether the layout has them or not.
var timeLayouts = []string{
	time.RFC3339,                // 2026-11-16T10:02:00Z, 2026-11-16T10:02:00.123+01:00
	"2006-01-02T15:04:05",       // 2026-11-16T10:02:00
	"2006-01-02 15:04:05Z07:00", // 2026-11-16 10:02:00+01:00
	"2006-01-02 15:04:05",       // 2026-11-16 10:02:00, 2026-11-16 10:02:00.123 (Uptime Kuma)
}

// parseTime reads a time: RFC 3339, "YYYY-MM-DD HH:MM:SS" (UTC unless a
// zone follows), or Unix seconds or milliseconds (also as text). It returns
// the zero time for anything else and for times before 2001 (e.g.
// Alertmanager's "0001-01-01T00:00:00Z", or 0 for "never").
func parseTime(s string) time.Time {
	t, _ := readTime(s)
	return t
}

// readTime is parseTime that also reports whether s is a time at all: ok
// is false for text in none of the formats, not for an empty one or a time
// before 2001.
func readTime(s string) (t time.Time, ok bool) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return time.Time{}, true
	case strings.Trim(s, "0123456789.") == "":
		n, err := strconv.ParseFloat(s, 64)
		switch {
		case err != nil:
			return time.Time{}, false
		case n < 1e11: // seconds (1e11 s is the year 5138)
			sec, frac := math.Modf(n)
			t = time.Unix(int64(sec), int64(frac*1e9)).UTC()
		case n < 1e14: // milliseconds
			t = time.UnixMilli(int64(n)).UTC()
		default:
			return time.Time{}, false
		}
	default:
		for _, layout := range timeLayouts {
			if parsed, err := time.Parse(layout, s); err == nil {
				t, ok = parsed, true
				break
			}
		}
		if !ok {
			return time.Time{}, false
		}
	}
	if t.Year() < 2001 {
		return time.Time{}, true
	}
	return t.UTC(), true
}

// lowerKeys returns m with lower-case keys (labels, tags and properties are
// looked up regardless of case).
func lowerKeys[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}
