package msgraph_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/msgraph"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakegraph"
)

func TestTenant(t *testing.T) {
	tests := []struct {
		issuer string
		want   string // "" = error
	}{
		{"https://login.microsoftonline.com/9188040d-6c67-4c5b-b112-36a304b66dad/v2.0", "9188040d-6c67-4c5b-b112-36a304b66dad"},
		{" https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0/ ", "contoso.onmicrosoft.com"},
		{"https://LOGIN.microsoftonline.com/9188040d-6c67-4c5b-b112-36a304b66dad/v2.0", "9188040d-6c67-4c5b-b112-36a304b66dad"},
		{"https://sts.windows.net/9188040d-6c67-4c5b-b112-36a304b66dad/", "9188040d-6c67-4c5b-b112-36a304b66dad"},
		{"https://sts.windows.net/9188040d-6c67-4c5b-b112-36a304b66dad", "9188040d-6c67-4c5b-b112-36a304b66dad"},
		{"https://login.microsoftonline.com/common/v2.0", ""},
		{"https://login.microsoftonline.com/organizations/v2.0", ""},
		{"https://login.microsoftonline.com/tenant", ""},
		{"https://login.microsoftonline.com/tenant/v2.0?x=1", ""},
		{"http://login.microsoftonline.com/tenant/v2.0", ""},
		{"https://keycloak.example.com/realms/it", ""},
		{"https://accounts.google.com", ""},
		{"", ""},
	}
	for _, tc := range tests {
		got, err := msgraph.Tenant(tc.issuer)
		if tc.want == "" {
			if err == nil {
				t.Errorf("Tenant(%q) = %q, want an error", tc.issuer, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("Tenant(%q) = %q, %v; want %q", tc.issuer, got, err, tc.want)
		}
	}
	if _, err := msgraph.Tenant("https://keycloak.example.com/realms/it"); err == nil ||
		err.Error() != "the issuer is not a Microsoft Entra ID issuer (https://login.microsoftonline.com/<tenant ID>/v2.0)" {
		t.Errorf("error for another issuer = %v", err)
	}
	if _, err := msgraph.Tenant("https://login.microsoftonline.com/common/v2.0"); err == nil || !strings.Contains(err.Error(), `not "common"`) {
		t.Errorf("error for the common endpoint = %v", err)
	}
}

func TestNewRequiresCredentials(t *testing.T) {
	const issuer = "https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0"
	for _, c := range [][2]string{{"", "secret"}, {" ", "secret"}, {"client", ""}} {
		if _, err := msgraph.New(issuer, c[0], c[1]); err == nil || !strings.Contains(err.Error(), "client ID and client secret") {
			t.Errorf("New(%q, %q): error = %v", c[0], c[1], err)
		}
	}
	if _, err := msgraph.New("https://keycloak.example.com/realms/it", "client", "secret"); err == nil {
		t.Error("New with another issuer: no error")
	}
}

func newClient(t *testing.T, fg *fakegraph.Server) *msgraph.Client {
	t.Helper()
	c, err := msgraph.New(fg.Issuer(), fg.ClientID, fg.ClientSecret)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSendMail(t *testing.T) {
	fg := fakegraph.Start(t)
	fg.AddMailbox("IT Status+Ops/Team@contoso.example")
	c := newClient(t, fg)
	ctx := context.Background()

	html := `<p>VPN <b>down</b> & "slow" – ünïcode</p>`
	if err := c.SendMail(ctx, fg.Sender, msgraph.Mail{To: "alice@contoso.example", Subject: "[Status] VPN down", HTML: html, Text: "VPN down"}); err != nil {
		t.Fatal(err)
	}
	// Without HTML the text is sent; a sender with special characters is
	// escaped in the path.
	if err := c.SendMail(ctx, "IT Status+Ops/Team@contoso.example", msgraph.Mail{To: "bob@contoso.example", Subject: "Plain", Text: "Line 1\nLine 2"}); err != nil {
		t.Fatal(err)
	}
	got := fg.Messages()
	want := []fakegraph.Message{
		{Sender: fg.Sender, To: "alice@contoso.example", Subject: "[Status] VPN down", ContentType: "HTML", Content: html},
		{Sender: "IT Status+Ops/Team@contoso.example", To: "bob@contoso.example", Subject: "Plain", ContentType: "Text", Content: "Line 1\nLine 2"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("messages =\n%+v\nwant\n%+v", got, want)
	}
	// The access token is fetched once and reused.
	if n := fg.TokenRequests(); n != 1 {
		t.Errorf("%d token requests, want 1", n)
	}
}

func TestSendMailErrors(t *testing.T) {
	fg := fakegraph.Start(t)
	c := newClient(t, fg)
	ctx := context.Background()
	mail := msgraph.Mail{To: "alice@contoso.example", Subject: "Hi", HTML: "<p>Hi</p>"}

	expect := func(name string, err error, status int, code string, retryAfter time.Duration, texts ...string) {
		t.Helper()
		var ge *msgraph.Error
		if !errors.As(err, &ge) || ge.StatusCode != status || ge.Code != code || ge.RetryAfter != retryAfter {
			t.Errorf("%s: error = %#v, want HTTP %d %s, Retry-After %s", name, err, status, code, retryAfter)
			return
		}
		for _, s := range texts {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("%s: error %q does not contain %q", name, err, s)
			}
		}
	}

	fg.FailNext(fakegraph.AccessDenied())
	expect("access denied", c.SendMail(ctx, fg.Sender, mail), 403, "ErrorAccessDenied", 0,
		"Microsoft Graph returned HTTP 403 (ErrorAccessDenied): Access is denied. Check credentials and try again.")

	fg.FailNext(fakegraph.Throttled("7"))
	expect("throttled", c.SendMail(ctx, fg.Sender, mail), 429, "ApplicationThrottled", 7*time.Second)

	date := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	fg.FailNext(fakegraph.Unavailable(503, date))
	err := c.SendMail(ctx, fg.Sender, mail)
	var ge *msgraph.Error
	if !errors.As(err, &ge) || ge.StatusCode != 503 || ge.RetryAfter < 59*time.Minute || ge.RetryAfter > time.Hour {
		t.Errorf("Retry-After as HTTP date: %#v", err)
	}

	expect("unknown sender", c.SendMail(ctx, "nobody@contoso.example", mail), 404, "ErrorInvalidUser", 0,
		"The requested user 'nobody@contoso.example' is invalid.")
	expect("invalid recipient", c.SendMail(ctx, fg.Sender, msgraph.Mail{To: "not an address", Subject: "x", HTML: "x"}), 400, "ErrorInvalidRecipients", 0,
		"Recipient 'not an address' is not resolved")

	fg.RevokeTokens()
	expect("revoked token", c.SendMail(ctx, fg.Sender, mail), 401, "InvalidAuthenticationToken", 0)

	if len(fg.Messages()) != 0 {
		t.Errorf("messages accepted: %+v", fg.Messages())
	}

	// A cancelled context.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.SendMail(cctx, fg.Sender, mail); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: error = %v", err)
	}
}

func TestSendMailNonJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token_type":"Bearer","expires_in":3600,"access_token":"t"}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "<html>proxy error</html>")
	}))
	t.Cleanup(srv.Close)
	useEndpoints(t, srv.URL, srv.URL)
	c, err := msgraph.New("https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0", "client", "secret")
	if err != nil {
		t.Fatal(err)
	}
	err = c.SendMail(context.Background(), "status@contoso.example", msgraph.Mail{To: "a@contoso.example", HTML: "x"})
	if err == nil || err.Error() != "Microsoft Graph returned HTTP 502: Bad Gateway" {
		t.Errorf("error = %v", err)
	}
}

func useEndpoints(t *testing.T, login, graph string) {
	t.Helper()
	old := msgraph.DefaultEndpoints
	msgraph.DefaultEndpoints = msgraph.Endpoints{Login: login, Graph: graph}
	t.Cleanup(func() { msgraph.DefaultEndpoints = old })
}

func TestTokenErrors(t *testing.T) {
	fg := fakegraph.Start(t)
	mail := msgraph.Mail{To: "alice@contoso.example", HTML: "x"}

	c, err := msgraph.New(fg.Issuer(), fg.ClientID, "wrong-secret")
	if err != nil {
		t.Fatal(err)
	}
	err = c.SendMail(context.Background(), fg.Sender, mail)
	if err == nil || !strings.HasPrefix(err.Error(), "could not get an access token from Microsoft Entra ID (check the issuer, client ID and client secret): invalid_client: AADSTS7000215: Invalid client secret provided.") ||
		strings.Contains(err.Error(), "Trace ID") {
		t.Errorf("wrong secret: error = %v", err)
	}

	c, err = msgraph.New("https://login.microsoftonline.com/other.onmicrosoft.com/v2.0", fg.ClientID, fg.ClientSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SendMail(context.Background(), fg.Sender, mail); err == nil || !strings.HasSuffix(err.Error(), "AADSTS90002: Tenant 'other.onmicrosoft.com' not found. Check to make sure you have the correct tenant ID and are signing into the correct cloud.") {
		t.Errorf("unknown tenant: error = %v", err)
	}
	if n := fg.Requests(); n != 0 {
		t.Errorf("%d sendMail requests without a token", n)
	}

	// Trace IDs on further lines are left out, too.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid_client","error_description":"AADSTS7000222: The provided client secret keys are expired.\r\nTrace ID: abc\r\nCorrelation ID: def"}`)
	}))
	t.Cleanup(srv.Close)
	useEndpoints(t, srv.URL, srv.URL)
	if c, err = msgraph.New("https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0", "client", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendMail(context.Background(), "status@contoso.example", mail); err == nil || !strings.HasSuffix(err.Error(), "invalid_client: AADSTS7000222: The provided client secret keys are expired.") {
		t.Errorf("expired secret: error = %v", err)
	}
}

func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listens on its address any more
	useEndpoints(t, srv.URL, srv.URL)
	c, err := msgraph.New("https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0", "client", "secret")
	if err != nil {
		t.Fatal(err)
	}
	// The access token is requested first.
	err = c.SendMail(context.Background(), "status@contoso.example", msgraph.Mail{To: "a@contoso.example", HTML: "x"})
	if err == nil || !strings.HasPrefix(err.Error(), "could not reach Microsoft Entra ID to get an access token: ") {
		t.Errorf("error = %v", err)
	}
}

// The access token is never sent to another host: redirects are not
// followed.
func TestNoRedirects(t *testing.T) {
	leaked := make(chan string, 1)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked <- r.Header.Get("Authorization")
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token_type":"Bearer","expires_in":3600,"access_token":"secret-token"}`)
			return
		}
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	useEndpoints(t, srv.URL, srv.URL)
	c, err := msgraph.New("https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0", "client", "secret")
	if err != nil {
		t.Fatal(err)
	}
	err = c.SendMail(context.Background(), "status@contoso.example", msgraph.Mail{To: "a@contoso.example", HTML: "x"})
	var ge *msgraph.Error
	if !errors.As(err, &ge) || ge.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("error = %v, want the redirect as an error", err)
	}
	select {
	case auth := <-leaked:
		t.Errorf("the redirect was followed (Authorization %q)", auth)
	default:
	}
}
