// Package fakegraph is a fake Microsoft Entra ID token endpoint and
// Microsoft Graph sendMail endpoint for tests.
//
// The token endpoint (POST /{tenant}/oauth2/v2.0/token) checks the client
// credentials request and answers like Entra ID (e.g. AADSTS7000215 for a
// wrong client secret). The sendMail endpoint
// (POST /v1.0/users/{sender}/sendMail, or /v1.0/users('{sender}')/sendMail,
// which a sender that begins with "$" needs) checks the bearer token, the
// sender mailbox in the path and the JSON body: one recipient, a subject, an
// HTML or text body and "saveToSentItems": false. Accepted messages are
// kept; failures (throttling, access denied, …) can be injected.
//
//	fg := fakegraph.Start(t) // also points internal/msgraph to the fake server
//	// configure the application with the issuer fg.Issuer(), fg.ClientID,
//	// fg.ClientSecret and the sender mailbox fg.Sender
//	msgs := fg.WaitFor(2, 10*time.Second)
//
// Tests that use it must not run in parallel (msgraph.DefaultEndpoints is
// global). It is only meant to be used by tests.
package fakegraph

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/msgraph"
)

// Message is an e-mail accepted by the fake server.
type Message struct {
	Sender      string // mailbox from the request path (/users/{sender}/sendMail or /users('{sender}')/sendMail)
	To          string // the only recipient
	Subject     string
	ContentType string // "HTML" or "Text"
	Content     string // the body
}

// Failure is an error response of the sendMail endpoint.
type Failure struct {
	Status     int
	Code       string // Microsoft Graph error code, e.g. "ErrorAccessDenied"
	Message    string
	RetryAfter string // value of the Retry-After header; "" for none
}

// Throttled is the answer of Microsoft Graph to too many requests (HTTP 429)
// with the given Retry-After header ("" for none).
func Throttled(retryAfter string) Failure {
	return Failure{Status: http.StatusTooManyRequests, Code: "ApplicationThrottled",
		Message: "Application is over its MailboxConcurrency limit.", RetryAfter: retryAfter}
}

// Unavailable is a temporary server error: status 503 or 504.
func Unavailable(status int, retryAfter string) Failure {
	return Failure{Status: status, Code: "ServiceUnavailable",
		Message: "The service is temporarily unavailable. Try again later.", RetryAfter: retryAfter}
}

// AccessDenied is the answer when the app has no Mail.Send permission for
// the mailbox (HTTP 403).
func AccessDenied() Failure {
	return Failure{Status: http.StatusForbidden, Code: "ErrorAccessDenied", Message: "Access is denied. Check credentials and try again."}
}

// NoMailbox is the answer for a user without an Exchange Online mailbox
// (HTTP 404). An unknown sender gets HTTP 404 ErrorInvalidUser anyway.
func NoMailbox() Failure {
	return Failure{Status: http.StatusNotFound, Code: "MailboxNotEnabledForRESTAPI", Message: "REST API is not yet supported for this mailbox."}
}

// InvalidRecipient is the answer for a recipient Exchange Online cannot
// resolve (HTTP 400).
func InvalidRecipient(addr string) Failure {
	return Failure{Status: http.StatusBadRequest, Code: "ErrorInvalidRecipients",
		Message: "At least one recipient is not valid., Recipient '" + addr + "' is not resolved. All recipients must be resolved before a message can be submitted."}
}

// ServerError is an unexpected server error (HTTP 500, 502, …).
func ServerError(status int) Failure {
	return Failure{Status: status, Code: "UnknownError", Message: http.StatusText(status)}
}

// Server is a running fake.
type Server struct {
	// Tenant, ClientID and ClientSecret of the app registration that may get
	// tokens, and Sender, a mailbox that exists. Change them before the
	// first request.
	Tenant       string
	ClientID     string
	ClientSecret string
	Sender       string

	t   testing.TB
	srv *httptest.Server

	mu         sync.Mutex
	mailboxes  map[string]bool // lower-case addresses of further mailboxes
	tokens     map[string]bool // valid access tokens
	tokenCalls int
	requests   int // sendMail requests with a valid access token
	msgs       []Message
	next       []Failure                // answers for the next requests
	recipients map[string]Failure       // answers for messages to these recipients (lower case)
	respond    func(m Message) *Failure // decides about the other requests; nil accepts
	received   chan struct{}            // signalled after every accepted message
}

// Start starts the server on a random local port and points the Microsoft
// Graph client (msgraph.DefaultEndpoints) to it, until the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		Tenant:       "11111111-2222-3333-4444-555555555555",
		ClientID:     "status-page-app",
		ClientSecret: "fake-graph-secret",
		Sender:       "status@contoso.example",
		t:            t,
		mailboxes:    map[string]bool{},
		tokens:       map[string]bool{},
		recipients:   map[string]Failure{},
		received:     make(chan struct{}, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", s.token)
	mux.HandleFunc("POST /v1.0/users/{sender}/sendMail", s.sendMail)
	mux.HandleFunc("POST /v1.0/{user}/sendMail", s.sendMail) // users('{sender}')
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fakegraph: unexpected request %s %s", r.Method, r.URL)
		graphError(w, Failure{Status: http.StatusBadRequest, Code: "BadRequest", Message: "Unsupported request."})
	})
	s.srv = httptest.NewServer(mux)
	old := msgraph.DefaultEndpoints
	msgraph.DefaultEndpoints = msgraph.Endpoints{Login: s.srv.URL, Graph: s.srv.URL}
	t.Cleanup(func() {
		s.srv.Close()
		msgraph.DefaultEndpoints = old
	})
	return s
}

// URL returns the base URL of the fake server.
func (s *Server) URL() string { return s.srv.URL }

// Issuer returns the OIDC issuer of the tenant, as configured for single
// sign-on with Microsoft Entra ID.
func (s *Server) Issuer() string {
	return "https://login.microsoftonline.com/" + s.Tenant + "/v2.0"
}

// AddMailbox adds a further mailbox that may send.
func (s *Server) AddMailbox(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailboxes[strings.ToLower(addr)] = true
}

// RevokeTokens makes the issued access tokens invalid (HTTP 401).
func (s *Server) RevokeTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.tokens)
}

// TokenRequests returns the number of successful token requests.
func (s *Server) TokenRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenCalls
}

// Requests returns the number of sendMail requests with a valid access
// token (accepted or failed).
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// FailNext makes the next len(f) valid sendMail requests fail with f, in
// order.
func (s *Server) FailNext(f ...Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = append(s.next, f...)
}

// FailRecipient makes every message to addr fail with f.
func (s *Server) FailRecipient(addr string, f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recipients[strings.ToLower(addr)] = f
}

// Respond sets a function that decides about the valid requests that are
// not answered by FailNext or FailRecipient: nil accepts the message.
// Respond(nil) accepts all of them again. The function is called without
// holding the server's lock.
func (s *Server) Respond(f func(m Message) *Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = f
}

// SetLimit makes the server accept only n messages in total; further
// requests fail with f (e.g. Throttled("60")). n < 0 removes the limit.
func (s *Server) SetLimit(n int, f Failure) {
	if n < 0 {
		s.Respond(nil)
		return
	}
	s.Respond(func(Message) *Failure {
		if len(s.Messages()) >= n {
			return &f
		}
		return nil
	})
}

// Messages returns the accepted messages.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

// Reset forgets the accepted messages.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = nil
}

// WaitFor waits until at least n messages were accepted and returns them.
// It fails the test after timeout.
func (s *Server) WaitFor(n int, timeout time.Duration) []Message {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if msgs := s.Messages(); len(msgs) >= n {
			return msgs
		}
		left := time.Until(deadline)
		if left <= 0 {
			s.t.Fatalf("fakegraph: %d messages accepted within %s, want %d", len(s.Messages()), timeout, n)
		}
		select {
		case <-s.received:
		case <-time.After(min(left, 50*time.Millisecond)):
		}
	}
}

// ------------------------------------------------------------------ token

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request", "AADSTS900144: The request body must contain the following parameter: 'grant_type'.")
		return
	}
	if r.PathValue("tenant") != s.Tenant {
		tokenError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("AADSTS90002: Tenant '%s' not found. Check to make sure you have the correct tenant ID and are signing into the correct cloud.", r.PathValue("tenant")))
		return
	}
	if got := r.PostForm.Get("grant_type"); got != "client_credentials" {
		s.t.Errorf("fakegraph: token request grant_type = %q, want client_credentials", got)
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "AADSTS70003: The app requested an unsupported grant type.")
		return
	}
	if got := r.PostForm.Get("scope"); got != "https://graph.microsoft.com/.default" {
		s.t.Errorf("fakegraph: token request scope = %q, want https://graph.microsoft.com/.default", got)
		tokenError(w, http.StatusBadRequest, "invalid_scope", "AADSTS1002012: The provided value for scope is not valid.")
		return
	}
	if r.PostForm.Get("client_id") != s.ClientID {
		tokenError(w, http.StatusBadRequest, "unauthorized_client",
			fmt.Sprintf("AADSTS700016: Application with identifier '%s' was not found in the directory.", r.PostForm.Get("client_id")))
		return
	}
	if r.PostForm.Get("client_secret") != s.ClientSecret {
		tokenError(w, http.StatusUnauthorized, "invalid_client",
			"AADSTS7000215: Invalid client secret provided. Ensure the secret being sent in the request is the client secret value, not the client secret ID.")
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := "fake-token-" + hex.EncodeToString(b)
	s.mu.Lock()
	s.tokens[tok] = true
	s.tokenCalls++
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"token_type": "Bearer", "expires_in": 3599, "ext_expires_in": 3599, "access_token": tok})
}

// tokenError answers like Entra ID, which appends trace and correlation IDs
// to the description.
func tokenError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]any{
		"error":             code,
		"error_description": desc + " Trace ID: 820764c5-3324-4806-9a10-5cd27f276f00 Correlation ID: 887e951e-b711-4c69-8218-8210514c2d82 Timestamp: 2026-01-01 00:00:00Z",
	})
}

// ------------------------------------------------------------------ sendMail

// sendMailBody is the expected JSON body; unknown fields are rejected.
type sendMailBody struct {
	Message *struct {
		Subject *string `json:"subject"`
		Body    *struct {
			ContentType string `json:"contentType"`
			Content     string `json:"content"`
		} `json:"body"`
		ToRecipients []struct {
			EmailAddress struct {
				Address string `json:"address"`
			} `json:"emailAddress"`
		} `json:"toRecipients"`
	} `json:"message"`
	SaveToSentItems *bool `json:"saveToSentItems"`
}

func (s *Server) sendMail(w http.ResponseWriter, r *http.Request) {
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	valid := ok && s.tokens[auth]
	s.mu.Unlock()
	if !valid {
		graphError(w, Failure{Status: http.StatusUnauthorized, Code: "InvalidAuthenticationToken", Message: "Access token validation failure. Invalid audience."})
		return
	}

	sender, problem := senderOf(r)
	if problem != "" {
		s.t.Errorf("fakegraph: invalid sendMail request: %s", problem)
		graphError(w, Failure{Status: http.StatusBadRequest, Code: "BadRequest", Message: problem})
		return
	}
	m, problem := s.parse(r)
	if problem != "" {
		s.t.Errorf("fakegraph: invalid sendMail request: %s", problem)
		graphError(w, Failure{Status: http.StatusBadRequest, Code: "RequestBodyRead", Message: problem})
		return
	}
	m.Sender = sender

	// The answer: an unknown sender mailbox, an injected failure, an
	// invalid recipient, the Respond function, else accepted.
	var f *Failure
	s.mu.Lock()
	s.requests++
	rf, recipientFails := s.recipients[strings.ToLower(m.To)]
	switch {
	case !strings.EqualFold(m.Sender, s.Sender) && !s.mailboxes[strings.ToLower(m.Sender)]:
		f = &Failure{Status: http.StatusNotFound, Code: "ErrorInvalidUser", Message: "The requested user '" + m.Sender + "' is invalid."}
	case len(s.next) > 0:
		next := s.next[0]
		s.next = s.next[1:]
		f = &next
	case recipientFails:
		f = &rf
	case !validAddress(m.To):
		inv := InvalidRecipient(m.To)
		f = &inv
	}
	respond := s.respond
	s.mu.Unlock()
	if f == nil && respond != nil {
		f = respond(m)
	}
	if f != nil {
		graphError(w, *f)
		return
	}

	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.mu.Unlock()
	select {
	case s.received <- struct{}{}:
	default:
	}
	w.WriteHeader(http.StatusAccepted)
}

// senderOf returns the sender mailbox of the request path, or a description
// of what is wrong with the path: /users/{sender}, or the key syntax
// /users('{sender}') with quotes doubled, which a sender that begins with
// "$" needs (Microsoft Graph answers /users/$… with HTTP 400, as OData
// reserves the "$" prefix).
func senderOf(r *http.Request) (string, string) {
	if sender := r.PathValue("sender"); sender != "" {
		if strings.HasPrefix(sender, "$") {
			return "", fmt.Sprintf("/users/%s: a sender that begins with $ must be addressed as /users('%s')", sender, sender)
		}
		return sender, ""
	}
	key, prefixed := strings.CutPrefix(r.PathValue("user"), "users('")
	key, quoted := strings.CutSuffix(key, "')")
	if !prefixed || !quoted {
		return "", fmt.Sprintf("unsupported path segment %q, want users/{sender} or users('{sender}')", r.PathValue("user"))
	}
	return strings.ReplaceAll(key, "''", "'"), ""
}

// parse checks the request body and returns the message without its
// sender, or a description of what is wrong with the body.
func (s *Server) parse(r *http.Request) (Message, string) {
	var m Message
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return m, fmt.Sprintf("Content-Type %q, want application/json", r.Header.Get("Content-Type"))
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return m, "reading the body: " + err.Error()
	}
	var body sendMailBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return m, "body " + string(raw) + ": " + err.Error()
	}
	switch {
	case body.Message == nil:
		return m, `"message" is missing`
	case body.Message.Subject == nil:
		return m, `"message.subject" is missing`
	case body.Message.Body == nil:
		return m, `"message.body" is missing`
	case body.Message.Body.ContentType != "HTML" && body.Message.Body.ContentType != "Text":
		return m, fmt.Sprintf(`"message.body.contentType" is %q, want "HTML" or "Text"`, body.Message.Body.ContentType)
	case len(body.Message.ToRecipients) != 1:
		return m, fmt.Sprintf(`%d entries in "message.toRecipients", want one recipient per e-mail`, len(body.Message.ToRecipients))
	case body.SaveToSentItems == nil || *body.SaveToSentItems:
		return m, `"saveToSentItems" must be false`
	}
	m.To = body.Message.ToRecipients[0].EmailAddress.Address
	m.Subject = *body.Message.Subject
	m.ContentType = body.Message.Body.ContentType
	m.Content = body.Message.Body.Content
	return m, ""
}

// validAddress reports whether Exchange Online could resolve addr: a plain
// e-mail address.
func validAddress(addr string) bool {
	a, err := mail.ParseAddress(addr)
	return err == nil && a.Address == addr
}

func graphError(w http.ResponseWriter, f Failure) {
	if f.RetryAfter != "" {
		w.Header().Set("Retry-After", f.RetryAfter)
	}
	writeJSON(w, f.Status, map[string]any{"error": map[string]any{
		"code":    f.Code,
		"message": f.Message,
		"innerError": map[string]any{
			"date":       time.Now().UTC().Format("2006-01-02T15:04:05"),
			"request-id": "00000000-0000-0000-0000-000000000000",
		},
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
