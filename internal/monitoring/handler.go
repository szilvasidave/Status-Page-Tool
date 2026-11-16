package monitoring

// The webhook endpoint (POST /hooks/{token}, or POST /hooks with
// "Authorization: Bearer <token>") and the worker that opens the incidents
// of alerts that fired during maintenance.
//
// The token identifies the integration; only its SHA-256 is stored. The
// endpoint uses no cookies, sessions or CSRF tokens (the auth middleware
// passes /hooks through), and the request log of the web server shows the
// path without the token. Answers are JSON: 200 for every request that was
// read (also when an alert names no service or the integration is paused,
// so that the tool does not repeat it), 400 for a request that cannot be
// read, 401 for an unknown token, 405 for other methods, 413 for a body
// over MaxBodyBytes, 503 when the status page was too busy to apply it, 500
// for an error. Each request of a known integration is recorded in its
// request log (store.AddIntegrationEvent).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// Notifier sends the e-mails about incidents (implemented by
// notify.Notifier).
type Notifier interface {
	// IncidentUpdateNotice e-mails the groups of an incident's services
	// about one of its updates; created marks the update that opened it.
	IncidentUpdateNotice(ctx context.Context, incidentID, updateID int64, created bool) (int, error)
}

// defaultApplyTimeout is how long a request may wait for its turn and take
// to apply its alerts. The tool may hang up meanwhile: what it sent is
// applied anyway (it may not send it again).
const defaultApplyTimeout = 5 * time.Minute

// Handler serves the webhook endpoint; Run is its worker. Create it with
// New.
type Handler struct {
	store    *store.Store
	notifier Notifier // nil: no e-mails
	log      *slog.Logger
	// releaseInterval is how often Run checks for alerts that waited for
	// the end of a maintenance.
	releaseInterval time.Duration
	// applyTimeout is how long a request may wait for its turn and take to
	// apply its alerts (defaultApplyTimeout).
	applyTimeout time.Duration
}

// New returns the webhook endpoint for the routes under Path.
func New(st *store.Store, n Notifier, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: st, notifier: n, log: log, releaseInterval: time.Minute, applyTimeout: defaultApplyTimeout}
}

// IsPath reports whether p is the path of a webhook URL (Path or below).
func IsPath(p string) bool { return p == Path || strings.HasPrefix(p, Path+"/") }

// RedactPath returns p for logs, with everything after a path segment that
// starts with "hooks" (in any case) replaced by "…": a webhook token never
// reaches a log, also not from a path that the routes do not take as a
// webhook, e.g. //hooks/<token>, /HOOKS/<token> or /prefix/hooks/<token>
// behind a misconfigured proxy.
func RedactPath(p string) string {
	segments := strings.Split(p, "/")
	for i, seg := range segments[:max(len(segments)-1, 0)] {
		if strings.HasPrefix(strings.ToLower(seg), "hooks") {
			return strings.Join(segments[:i+1], "/") + "/…"
		}
	}
	return p
}

// URL returns the webhook URL of a token: the base URL setting, or the
// address of the request if it is not set, followed by Path and the token
// ("" for the address without a token).
func URL(r *http.Request, baseURL, token string) string {
	if baseURL = strings.TrimRight(baseURL, "/"); baseURL == "" {
		scheme := "http"
		if auth.IsHTTPS(r) {
			scheme = "https"
		}
		baseURL = scheme + "://" + r.Host
	}
	if token == "" {
		return baseURL + Path
	}
	return baseURL + Path + "/" + token
}

// response is the JSON answer of the endpoint.
type response struct {
	Status  string   `json:"status"` // ok, test, paused or error
	Alerts  int      `json:"alerts,omitempty"`
	Summary []string `json:"summary,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// ServeHTTP authenticates and applies one webhook request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, response{Status: "error",
			Error: "This is the address to which monitoring tools send their alerts, with POST. Set it up in Admin → Monitoring of the status page."})
		return
	}
	ctx := r.Context()
	token := tokenOf(r)
	if token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="Status Page monitoring"`)
		writeJSON(w, http.StatusUnauthorized, response{Status: "error",
			Error: "No token: send the alerts to the webhook URL of an integration (Admin → Monitoring of the status page), or send its token in the header Authorization: Bearer <token>."})
		return
	}
	in, err := h.store.IntegrationByTokenHash(ctx, HashToken(token))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.log.Error("monitoring request failed", "err", err)
			writeJSON(w, http.StatusInternalServerError, response{Status: "error", Error: "Internal error of the status page; details are in its log file."})
			return
		}
		h.log.Warn("monitoring request rejected: unknown webhook token", "ip", auth.ClientIP(r))
		w.Header().Set("WWW-Authenticate", `Bearer realm="Status Page monitoring"`)
		writeJSON(w, http.StatusUnauthorized, response{Status: "error",
			Error: "This webhook URL is not valid: it was replaced, or its integration deleted. Enter the integration's current URL in the tool; if you no longer have it, generate a new one in Admin → Monitoring of the status page (it is shown only once), or add the integration again."})
		return
	}

	// The answer may come after the server's WriteTimeout (a minute,
	// internal/app): the request may wait for its turn until applyTimeout,
	// and recording it and queueing its e-mails take 30 seconds at most.
	// Extended now, as HTTP/2 ends the stream when the WriteTimeout passes.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(h.applyTimeout + time.Minute))

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			msg := fmt.Sprintf("The request is larger than %d MB.", MaxBodyBytes>>20)
			h.record(ctx, in, store.EventInvalid, []string{msg}, "")
			writeJSON(w, http.StatusRequestEntityTooLarge, response{Status: "error", Error: msg})
			return
		}
		h.log.Warn("monitoring request: reading the body failed", "integration", in.Name, "err", err)
		writeJSON(w, http.StatusBadRequest, response{Status: "error", Error: "The request body could not be read."})
		return
	}
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")) // a UTF-8 byte order mark (e.g. Windows PowerShell 5)
	mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	body = toUTF8(body, params["charset"])
	req := &Request{Body: body, ContentType: strings.ToLower(mediaType), Query: r.URL.Query()}
	payload := string(body)
	if payload == "" && r.URL.RawQuery != "" {
		payload = "?" + r.URL.RawQuery
	}

	p := Lookup(in.Provider)
	if p == nil {
		p = Lookup(Generic)
	}
	b, err := p.Parse(req)
	if err != nil {
		msg := err.Error()
		var pe *ParseError
		if !errors.As(err, &pe) {
			msg = "the request could not be read: " + msg
		}
		msg = capitalize(msg) + "."
		h.record(ctx, in, store.EventInvalid, []string{msg}, payload)
		h.log.Info("monitoring request not understood", "integration", in.Name, "provider", p.ID, "err", msg)
		writeJSON(w, http.StatusBadRequest, response{Status: "error", Error: msg})
		return
	}
	if b.Unread > 0 {
		// First, so that the lines of the alerts read do not crowd it out.
		b.Notes = append([]string{fmt.Sprintf("Only the first %d of %d alerts were read: the others were ignored (send at most %d per request).",
			MaxAlerts, MaxAlerts+b.Unread, MaxAlerts)}, b.Notes...)
	}
	if b.Test {
		lines := capLines(append([]string{"Test message received: the connection works. Nothing was changed."}, b.Notes...))
		h.record(ctx, in, store.EventTest, lines, payload)
		writeJSON(w, http.StatusOK, response{Status: "test", Summary: lines})
		return
	}

	// A paused integration applies only resolved alerts: they can only end
	// what its alerts started, and an incident must not stay open because
	// its recovery came while the integration was paused.
	var (
		inputs []store.AlertInput
		head   []string
		status = "ok"
	)
	for i := range b.Alerts {
		if !in.Paused || !b.Alerts[i].Firing {
			inputs = append(inputs, b.Alerts[i].input(in.Impact))
		}
	}
	if in.Paused {
		status = "paused"
		if len(inputs) == 0 {
			lines := capLines(append([]string{fmt.Sprintf("The integration is paused: %s received, nothing was changed.", plural(len(b.Alerts), "alert"))}, b.Notes...))
			h.record(ctx, in, store.EventPaused, lines, payload)
			writeJSON(w, http.StatusOK, response{Status: status, Alerts: len(b.Alerts), Summary: lines})
			return
		}
		head = []string{"The integration is paused: resolved alerts still clear."}
		if ignored := len(b.Alerts) - len(inputs); ignored > 0 {
			head = []string{fmt.Sprintf("The integration is paused: %s ignored; resolved alerts still clear.", plural(ignored, "firing alert"))}
		}
	}

	// From here on the tool may hang up: what it sent is applied, its
	// e-mails queued and the request recorded anyway.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.applyTimeout)
	defer cancel()
	res, err := h.store.ApplyAlerts(actx, in, inputs)
	ctx, cancelRest := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelRest()
	if err != nil {
		code, msg := http.StatusInternalServerError, "Internal error of the status page; details are in its log file."
		line := "The alerts were not applied because of an internal error; details are in the log file of the status page."
		if errors.Is(err, store.ErrBusy) || actx.Err() != nil {
			// Out of time, waiting behind other alerts.
			code, msg = http.StatusServiceUnavailable, "The status page is busy: send the alerts again later."
			line = "The alerts were not applied: the status page was busy applying other alerts for too long."
			w.Header().Set("Retry-After", "60")
		}
		h.log.Error("monitoring request failed", "integration", in.Name, "err", err)
		h.record(ctx, in, store.EventFailed, capLines(append([]string{line}, b.Notes...)), payload)
		writeJSON(w, code, response{Status: "error", Error: msg})
		return
	}
	h.notify(ctx, res.Notices)
	outcome, lines := summarize(res)
	if in.Paused && outcome != store.EventChanged {
		outcome = store.EventPaused
	}
	notes := b.Notes
	if b.Unread > 0 {
		head, notes = append(head, notes[0]), notes[1:]
	}
	lines = capLines(append(append(head, lines...), notes...))
	if len(lines) == 0 {
		lines = []string{"The request contained no alerts."}
	}
	h.record(ctx, in, outcome, lines, payload)
	h.log.Info("monitoring alerts received", "integration", in.Name, "alerts", len(b.Alerts), "outcome", outcome)
	writeJSON(w, http.StatusOK, response{Status: status, Alerts: len(b.Alerts), Summary: lines})
}

// toUTF8 returns a request body as UTF-8: decoded from the charset of its
// Content-Type if that is Windows-1252 or one of its subsets (ISO-8859-1,
// US-ASCII), else as it is if it is valid UTF-8, else – without a charset –
// as Windows-1252, which Windows PowerShell 5.1 and other Windows tools send
// without saying so.
func toUTF8(body []byte, charset string) []byte {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "windows-1252", "cp1252", "x-cp1252", "iso-8859-1", "iso8859-1", "latin1", "l1", "us-ascii", "ascii":
		return decodeWindows1252(body)
	case "":
		if !utf8.Valid(body) {
			return decodeWindows1252(body)
		}
	}
	return body
}

// windows1252 are the characters of the bytes 0x80–0x9F in Windows-1252
// (the bytes it leaves undefined stand for themselves, as in browsers).
var windows1252 = [32]rune{
	'€', '\u0081', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '\u008D', 'Ž', '\u008F',
	'\u0090', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '\u009D', 'ž', 'Ÿ',
}

func decodeWindows1252(b []byte) []byte {
	out := make([]byte, 0, len(b)+len(b)/4)
	for _, c := range b {
		switch {
		case c < 0x80:
			out = append(out, c)
		case c < 0xA0:
			out = utf8.AppendRune(out, windows1252[c-0x80])
		default:
			out = utf8.AppendRune(out, rune(c))
		}
	}
	return out
}

// ServeUnavailable answers webhook requests while the status page runs in
// setup mode (no configuration): 503, so that tools that retry send the
// alerts again later.
func ServeUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "300")
	writeJSON(w, http.StatusServiceUnavailable, response{Status: "error",
		Error: "The status page is not set up (setup mode): send the alerts again later."})
}

// maxSummaryLines is the number of lines of a request's summary that are
// kept (in the request log and the answer).
const maxSummaryLines = 100

// capLines returns at most maxSummaryLines lines, the last one saying how
// many were left out.
func capLines(lines []string) []string {
	if len(lines) <= maxSummaryLines {
		return lines
	}
	return append(lines[:maxSummaryLines-1:maxSummaryLines-1], fmt.Sprintf("… and %d more lines.", len(lines)-(maxSummaryLines-1)))
}

// tokenOf returns the token of a request: the last part of the path
// /hooks/<token>, else the bearer token of the Authorization header.
func tokenOf(r *http.Request) string {
	if t, ok := strings.CutPrefix(r.URL.Path, Path+"/"); ok && t != "" && !strings.Contains(t, "/") {
		return t
	}
	if v := r.Header.Get("Authorization"); len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

// record adds a request to the integration's request log; a failure is
// only logged (the alerts were applied).
func (h *Handler) record(ctx context.Context, in *store.Integration, outcome string, lines []string, payload string) {
	h.addEvent(ctx, in, &store.IntegrationEvent{Outcome: outcome, Summary: strings.Join(lines, "\n"), Payload: payload})
}

// addEvent adds an entry to the integration's request log; a failure is
// only logged.
func (h *Handler) addEvent(ctx context.Context, in *store.Integration, ev *store.IntegrationEvent) {
	ev.IntegrationID = in.ID
	ev.Summary, ev.Payload = storable(ev.Summary), storable(ev.Payload)
	if err := h.store.AddIntegrationEvent(ctx, ev); err != nil {
		h.log.Warn("could not record a monitoring request", "integration", in.Name, "err", err)
	}
}

// storable returns s as text that every database stores: valid UTF-8
// without NUL characters.
func storable(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "")
}

// notify sends the e-mails of the incidents monitoring opened or resolved.
// A failure is logged: the changes are made, only the e-mails are missing.
func (h *Handler) notify(ctx context.Context, notices []store.IncidentNotice) {
	if h.notifier == nil {
		return
	}
	for _, n := range notices {
		if _, err := h.notifier.IncidentUpdateNotice(ctx, n.IncidentID, n.UpdateID, n.Created); err != nil {
			h.log.Warn("could not queue the e-mails about an incident of monitoring", "incident", n.IncidentID, "err", err)
		}
	}
}

// summarize describes what the alerts did, one line per alert and service,
// and returns the outcome: unmatched if a firing alert named no service,
// else changed if an incident was opened, updated or resolved, else
// unchanged.
func summarize(res *store.ApplyResult) (outcome string, lines []string) {
	outcome = store.EventUnchanged
	unmatched := false
	for _, ar := range res.Alerts {
		a := ar.Alert
		name := alertName(a)
		if !a.Firing {
			name += " (resolved)"
		} else {
			name += " (firing)"
		}
		switch {
		case a.Key == "":
			lines = append(lines, name+": no key, ignored.")
			continue
		case a.Firing && ar.Matched == "":
			unmatched = true
			if len(a.Targets) == 0 {
				lines = append(lines, name+": names no service.")
			} else {
				lines = append(lines, name+": no service is named "+quoteList(a.Targets)+
					". Add one of these names to a service's names in monitoring tools (Admin → Services).")
			}
			continue
		case !a.Firing && len(ar.Services) == 0:
			lines = append(lines, name+": was not firing, nothing to do.")
			continue
		}
		for _, sr := range ar.Services {
			lines = append(lines, name+": "+describe(sr))
			switch sr.Action {
			case store.AlertOpened, store.AlertJoined, store.AlertCleared, store.AlertResolved:
				outcome = store.EventChanged
			}
		}
	}
	if unmatched {
		outcome = store.EventUnmatched
	}
	return outcome, lines
}

// alertName names an alert in the request log: its title, or its key.
func alertName(a *store.AlertInput) string {
	if a.Title == "" {
		return "Alert " + clip(a.Key, 60)
	}
	return "“" + a.Title + "”"
}

// describe says what happened for one service of an alert.
func describe(sr store.AlertServiceResult) string {
	inc := fmt.Sprintf("incident #%d", sr.IncidentID)
	switch sr.Action {
	case store.AlertOpened:
		return "opened " + inc + " of " + sr.ServiceName + "."
	case store.AlertJoined:
		return "added to the open " + inc + " of " + sr.ServiceName + "."
	case store.AlertRepeated:
		return "still firing (" + inc + " of " + sr.ServiceName + ")."
	case store.AlertSuppressed:
		return sr.ServiceName + " is in maintenance: if the alert still fires when the maintenance has ended, it opens an incident."
	case store.AlertDismissed:
		if sr.IncidentID == 0 {
			return "still firing for " + sr.ServiceName + "; its incident was deleted, so it opens none until it has cleared."
		}
		return "still firing for " + sr.ServiceName + "; " + inc + " was resolved by an editor, so it opens none until it has cleared."
	case store.AlertCleared:
		return "cleared in " + inc + " of " + sr.ServiceName + "; other alerts of it still fire."
	case store.AlertResolved:
		return "resolved " + inc + " of " + sr.ServiceName + "."
	case store.AlertEnded:
		return "cleared for " + sr.ServiceName + " (no open incident)."
	}
	return string(sr.Action) + "."
}

func quoteList(values []string) string {
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = "“" + v + "”"
	}
	return strings.Join(q, ", ")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func writeJSON(w http.ResponseWriter, status int, v response) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// ------------------------------------------------------------------ worker

// Run opens, every minute, the incidents of alerts that fired during
// maintenance and still fire after it ended (store.ReleaseSuppressedAlerts),
// until ctx is cancelled.
func (h *Handler) Run(ctx context.Context) {
	t := time.NewTicker(h.releaseInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.release(ctx)
		}
	}
}

// release opens the incidents of the alerts that waited for the end of a
// maintenance, sends their e-mails and records them in the request logs.
func (h *Handler) release(ctx context.Context) {
	res, err := h.store.ReleaseSuppressedAlerts(ctx)
	if err != nil {
		if ctx.Err() == nil {
			h.log.Warn("monitoring: could not open the incidents of alerts after maintenance", "err", err)
		}
		return
	}
	if len(res.Alerts) == 0 {
		return
	}
	h.notify(ctx, res.Notices)
	byIntegration := map[int64][]string{}
	var order []int64
	for _, ar := range res.Alerts {
		if _, ok := byIntegration[ar.IntegrationID]; !ok {
			order = append(order, ar.IntegrationID)
		}
		for _, sr := range ar.Services {
			byIntegration[ar.IntegrationID] = append(byIntegration[ar.IntegrationID],
				alertName(ar.Alert)+" (firing since a maintenance that has ended): "+describe(sr))
		}
	}
	for _, id := range order {
		h.addEvent(ctx, &store.Integration{ID: id}, &store.IntegrationEvent{Outcome: store.EventChanged,
			Summary: strings.Join(capLines(byIntegration[id]), "\n"), Internal: true})
	}
	h.log.Info("monitoring: alerts that fired during maintenance opened incidents", "alerts", len(res.Alerts))
}

// Clear treats a firing alert of the integration as resolved, as if the
// tool had reported its recovery (store.ClearIntegrationAlert), on behalf
// of the administrator by: it sends the e-mails, records what it did in
// the request log and returns that. Returns store.ErrNotFound if the alert
// is not firing.
func (h *Handler) Clear(ctx context.Context, in *store.Integration, key, by string) ([]string, error) {
	res, err := h.store.ClearIntegrationAlert(ctx, in, key)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	h.notify(ctx, res.Notices)
	var lines []string
	outcome := store.EventUnchanged // e.g. its incident was resolved by an editor
	for _, ar := range res.Alerts {
		for _, sr := range ar.Services {
			lines = append(lines, alertName(ar.Alert)+" (cleared by "+by+"): "+describe(sr))
			if sr.Action == store.AlertCleared || sr.Action == store.AlertResolved {
				outcome = store.EventChanged
			}
		}
	}
	lines = capLines(lines)
	h.addEvent(ctx, in, &store.IntegrationEvent{Outcome: outcome, Summary: strings.Join(lines, "\n"), Internal: true})
	h.log.Info("monitoring: alert cleared by an administrator", "integration", in.Name, "by", by)
	return lines, nil
}
