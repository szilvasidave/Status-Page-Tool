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
// over MaxBodyBytes. Each request of a known integration is recorded in its
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

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// Notifier sends the e-mails about incidents (implemented by
// notify.Notifier).
type Notifier interface {
	IncidentCreated(ctx context.Context, incidentID int64) (int, error)
	IncidentUpdated(ctx context.Context, incidentID int64, previousServiceIDs []int64) (int, error)
}

// Handler serves the webhook endpoint; Run is its worker. Create it with
// New.
type Handler struct {
	store    *store.Store
	notifier Notifier // nil: no e-mails
	log      *slog.Logger
	// releaseInterval is how often Run checks for alerts that waited for
	// the end of a maintenance.
	releaseInterval time.Duration
}

// New returns the webhook endpoint for the routes under Path.
func New(st *store.Store, n Notifier, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: st, notifier: n, log: log, releaseInterval: time.Minute}
}

// IsPath reports whether p is the path of a webhook URL (Path or below).
func IsPath(p string) bool { return p == Path || strings.HasPrefix(p, Path+"/") }

// RedactPath returns p with the token of a webhook URL replaced by "…", for
// logs.
func RedactPath(p string) string {
	if strings.HasPrefix(p, Path+"/") {
		return Path + "/…"
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
			Error: "This webhook URL is not valid: it was replaced, or its integration deleted. Copy the current URL from Admin → Monitoring of the status page."})
		return
	}

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
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
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
	switch {
	case b.Test:
		lines := append([]string{"Test message received: the connection works. Nothing was changed."}, b.Notes...)
		h.record(ctx, in, store.EventTest, lines, payload)
		writeJSON(w, http.StatusOK, response{Status: "test", Summary: lines})
		return
	case in.Paused:
		lines := append([]string{fmt.Sprintf("The integration is paused: %s received, nothing was changed.", plural(len(b.Alerts), "alert"))}, b.Notes...)
		h.record(ctx, in, store.EventPaused, lines, payload)
		writeJSON(w, http.StatusOK, response{Status: "paused", Alerts: len(b.Alerts), Summary: lines})
		return
	}

	inputs := make([]store.AlertInput, len(b.Alerts))
	for i := range b.Alerts {
		inputs[i] = b.Alerts[i].input(in.Impact)
	}
	res, err := h.store.ApplyAlerts(ctx, in, inputs)
	if err != nil {
		h.log.Error("monitoring request failed", "integration", in.Name, "err", err)
		writeJSON(w, http.StatusInternalServerError, response{Status: "error", Error: "Internal error of the status page; details are in its log file."})
		return
	}
	// The changes are made: queue their e-mails and record the request even
	// if the tool hangs up now.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	h.notify(ctx, res.Notices)
	outcome, lines := summarize(res)
	lines = append(lines, b.Notes...)
	if len(lines) == 0 {
		lines = []string{"The request contained no alerts."}
	}
	h.record(ctx, in, outcome, lines, payload)
	h.log.Info("monitoring alerts received", "integration", in.Name, "alerts", len(inputs), "outcome", outcome)
	writeJSON(w, http.StatusOK, response{Status: "ok", Alerts: len(inputs), Summary: lines})
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
	ev := &store.IntegrationEvent{IntegrationID: in.ID, Outcome: outcome, Summary: storable(strings.Join(lines, "\n")), Payload: storable(payload)}
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
		var err error
		if n.Created {
			_, err = h.notifier.IncidentCreated(ctx, n.IncidentID)
		} else {
			_, err = h.notifier.IncidentUpdated(ctx, n.IncidentID, nil)
		}
		if err != nil {
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
		name := "“" + a.Title + "”"
		if a.Title == "" {
			name = "Alert " + clip(a.Key, 60)
		}
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
				"“"+ar.Alert.Title+"” (firing since a maintenance that has ended): "+describe(sr))
		}
	}
	for _, id := range order {
		h.record(ctx, &store.Integration{ID: id}, store.EventChanged, byIntegration[id], "")
	}
	h.log.Info("monitoring: alerts that fired during maintenance opened incidents", "alerts", len(res.Alerts))
}
