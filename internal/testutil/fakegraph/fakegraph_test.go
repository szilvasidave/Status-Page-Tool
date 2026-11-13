package fakegraph

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/msgraph"
)

// recorder collects the errors the fake reports instead of failing the test.
type recorder struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	errs := r.errs
	r.errs = nil
	return errs
}

func post(t *testing.T, rawURL, contentType, token, body string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func getToken(t *testing.T, s *Server) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {s.ClientID}, "client_secret": {s.ClientSecret},
		"scope": {"https://graph.microsoft.com/.default"}}
	status, _, body := post(t, s.URL()+"/"+s.Tenant+"/oauth2/v2.0/token", "application/x-www-form-urlencoded", "", form.Encode())
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if status != 200 || json.Unmarshal([]byte(body), &tok) != nil || tok.AccessToken == "" {
		t.Fatalf("token: %d %s", status, body)
	}
	return tok.AccessToken
}

const validBody = `{"message":{"subject":"Hi","body":{"contentType":"HTML","content":"<p>Hi</p>"},` +
	`"toRecipients":[{"emailAddress":{"address":"alice@contoso.example"}}]},"saveToSentItems":false}`

func TestFakeGraphChecks(t *testing.T) {
	rec := &recorder{TB: t}
	s := Start(rec)
	if msgraph.DefaultEndpoints.Graph != s.URL() || msgraph.DefaultEndpoints.Login != s.URL() {
		t.Errorf("msgraph endpoints = %+v, want the fake server", msgraph.DefaultEndpoints)
	}
	sendURL := s.URL() + "/v1.0/users/" + url.PathEscape(s.Sender) + "/sendMail"

	// No or an unknown token.
	if status, _, _ := post(t, sendURL, "application/json", "", validBody); status != 401 {
		t.Errorf("without a token: %d", status)
	}
	if status, _, _ := post(t, sendURL, "application/json", "made-up", validBody); status != 401 {
		t.Errorf("unknown token: %d", status)
	}
	tok := getToken(t, s)
	if status, _, body := post(t, sendURL, "application/json; charset=utf-8", tok, validBody); status != 202 || body != "" {
		t.Errorf("valid request: %d %s", status, body)
	}
	if got := s.Messages(); len(got) != 1 || got[0] != (Message{Sender: s.Sender, To: "alice@contoso.example", Subject: "Hi", ContentType: "HTML", Content: "<p>Hi</p>"}) {
		t.Errorf("messages = %+v", got)
	}
	if errs := rec.take(); len(errs) != 0 {
		t.Errorf("valid requests reported: %q", errs)
	}

	// Requests the real service would reject as malformed are reported.
	bad := map[string]string{
		"no saveToSentItems":   strings.Replace(validBody, `,"saveToSentItems":false`, "", 1),
		"saveToSentItems true": strings.Replace(validBody, `"saveToSentItems":false`, `"saveToSentItems":true`, 1),
		"two recipients": strings.Replace(validBody, `[{"emailAddress":{"address":"alice@contoso.example"}}]`,
			`[{"emailAddress":{"address":"a@contoso.example"}},{"emailAddress":{"address":"b@contoso.example"}}]`, 1),
		"unknown field":     strings.Replace(validBody, `"subject":"Hi"`, `"subject":"Hi","importance":"high"`, 1),
		"wrong body type":   strings.Replace(validBody, `"HTML"`, `"Markdown"`, 1),
		"no subject":        strings.Replace(validBody, `"subject":"Hi",`, "", 1),
		"not JSON":          "subject=Hi",
		"no message object": `{"saveToSentItems":false}`,
	}
	for name, body := range bad {
		if status, _, _ := post(t, sendURL, "application/json", tok, body); status != 400 {
			t.Errorf("%s: %d, want 400", name, status)
		}
		if errs := rec.take(); len(errs) != 1 {
			t.Errorf("%s: reported %q, want one error", name, errs)
		}
	}
	if status, _, _ := post(t, sendURL, "text/plain", tok, validBody); status != 400 || len(rec.take()) != 1 {
		t.Errorf("text/plain body: %d", status)
	}

	// A wrong grant type is reported, wrong credentials are answered like
	// Entra ID.
	form := url.Values{"grant_type": {"password"}, "client_id": {s.ClientID}, "client_secret": {s.ClientSecret}, "scope": {"https://graph.microsoft.com/.default"}}
	if status, _, _ := post(t, s.URL()+"/"+s.Tenant+"/oauth2/v2.0/token", "application/x-www-form-urlencoded", "", form.Encode()); status != 400 || len(rec.take()) != 1 {
		t.Errorf("password grant: %d", status)
	}
	form.Set("grant_type", "client_credentials")
	form.Set("client_secret", "wrong")
	if status, _, body := post(t, s.URL()+"/"+s.Tenant+"/oauth2/v2.0/token", "application/x-www-form-urlencoded", "", form.Encode()); status != 401 || !strings.Contains(body, "AADSTS7000215") {
		t.Errorf("wrong secret: %d %s", status, body)
	}
	if errs := rec.take(); len(errs) != 0 {
		t.Errorf("wrong secret reported as a test error: %q", errs)
	}
}

func TestFakeGraphFailures(t *testing.T) {
	s := Start(t)
	tok := getToken(t, s)
	send := func(to string) (int, http.Header, string) {
		t.Helper()
		body := strings.Replace(validBody, "alice@contoso.example", to, 1)
		return post(t, s.URL()+"/v1.0/users/"+url.PathEscape(s.Sender)+"/sendMail", "application/json", tok, body)
	}

	s.FailNext(Throttled("30"), AccessDenied())
	s.FailRecipient("Bob@Contoso.example", InvalidRecipient("bob@contoso.example"))
	if status, h, body := send("a@contoso.example"); status != 429 || h.Get("Retry-After") != "30" || !strings.Contains(body, `"code":"ApplicationThrottled"`) {
		t.Errorf("first: %d %v %s", status, h, body)
	}
	if status, _, body := send("a@contoso.example"); status != 403 || !strings.Contains(body, "ErrorAccessDenied") {
		t.Errorf("second: %d %s", status, body)
	}
	if status, _, _ := send("a@contoso.example"); status != 202 {
		t.Errorf("third: %d", status)
	}
	if status, _, body := send("bob@contoso.example"); status != 400 || !strings.Contains(body, "ErrorInvalidRecipients") {
		t.Errorf("failing recipient: %d %s", status, body)
	}
	if status, _, body := send("not an address"); status != 400 || !strings.Contains(body, "ErrorInvalidRecipients") {
		t.Errorf("invalid address: %d %s", status, body)
	}

	// Two messages in total, then throttled.
	s.SetLimit(2, Throttled(""))
	if status, _, _ := send("c@contoso.example"); status != 202 {
		t.Errorf("below the limit: %d", status)
	}
	if status, h, _ := send("d@contoso.example"); status != 429 || h.Get("Retry-After") != "" {
		t.Errorf("at the limit: %d", status)
	}
	s.SetLimit(-1, Failure{})
	if status, _, _ := send("d@contoso.example"); status != 202 {
		t.Errorf("limit removed: %d", status)
	}
	if got := s.WaitFor(3, time.Second); len(got) != 3 || got[2].To != "d@contoso.example" {
		t.Errorf("messages = %+v", got)
	}
	if n := s.Requests(); n != 8 {
		t.Errorf("%d requests counted, want 8", n)
	}

	// An unknown sender mailbox.
	body := strings.Replace(validBody, "alice@contoso.example", "e@contoso.example", 1)
	status, _, resp := post(t, s.URL()+"/v1.0/users/ghost@contoso.example/sendMail", "application/json", tok, body)
	if status != 404 || !strings.Contains(resp, "ErrorInvalidUser") {
		t.Errorf("unknown sender: %d %s", status, resp)
	}
}
