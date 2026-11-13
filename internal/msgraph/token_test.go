package msgraph_test

// The access token: requested with the caller's context, cached until
// shortly before it expires, and its errors name Microsoft Entra ID rather
// than Microsoft Graph.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/msgraph"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakegraph"
)

const testIssuer = "https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0"

// A token request that gets no answer ends when the caller's context is
// cancelled (e.g. at shutdown), not only after the 30 s timeout.
func TestTokenRequestUsesContext(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		select { // Microsoft Entra ID does not answer
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // before srv.Close
	useEndpoints(t, srv.URL, srv.URL)
	c, err := msgraph.New(testIssuer, "client", "secret")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- c.SendMail(ctx, "status@contoso.example", msgraph.Mail{To: "a@contoso.example", HTML: "x"})
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("no token request")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SendMail did not return within 5s after the context was cancelled")
	}
}

// The access token is reused until it expires within 10 seconds; then a new
// one is requested.
func TestTokenRenewal(t *testing.T) {
	var mu sync.Mutex
	var tokens int
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") {
			tokens++
			w.Header().Set("Content-Type", "application/json")
			expires := 3600
			if tokens == 2 {
				expires = 5 // expires within 10 seconds: renewed before the next request
			}
			fmt.Fprintf(w, `{"token_type":"Bearer","expires_in":%d,"access_token":"token-%d"}`, expires, tokens)
			return
		}
		auth = append(auth, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	useEndpoints(t, srv.URL, srv.URL)
	send := func(c *msgraph.Client) {
		t.Helper()
		if err := c.SendMail(context.Background(), "status@contoso.example", msgraph.Mail{To: "a@contoso.example", HTML: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	c, err := msgraph.New(testIssuer, "client", "secret")
	if err != nil {
		t.Fatal(err)
	}
	send(c)
	send(c)
	// A new client (e.g. for a test e-mail) gets a new token.
	if c, err = msgraph.New(testIssuer, "client", "secret"); err != nil {
		t.Fatal(err)
	}
	send(c)
	send(c)
	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer token-1", "Bearer token-1", "Bearer token-2", "Bearer token-3"}
	if fmt.Sprint(auth) != fmt.Sprint(want) || tokens != 3 {
		t.Errorf("Authorization headers %q after %d token requests, want %q after 3", auth, tokens, want)
	}
}

// A client can be used concurrently (see go test -race).
func TestConcurrentSendMail(t *testing.T) {
	fg := fakegraph.Start(t)
	c := newClient(t, fg)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if err := c.SendMail(context.Background(), fg.Sender, msgraph.Mail{To: fmt.Sprintf("user%d@contoso.example", i), Subject: "Hi", HTML: "x"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := len(fg.Messages()); n != 8 {
		t.Errorf("%d messages accepted, want 8", n)
	}
}

// When Microsoft Entra ID (the token endpoint) cannot be reached, the error
// says so, not that Microsoft Graph cannot be reached, and the other way
// round: an administrator checks the right host.
func TestUnreachableEndpoint(t *testing.T) {
	fg := fakegraph.Start(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // nothing listens on its address any more
	mail := msgraph.Mail{To: "a@contoso.example", HTML: "x"}

	useEndpoints(t, dead.URL, fg.URL())
	err := newClient(t, fg).SendMail(context.Background(), fg.Sender, mail)
	if err == nil || !strings.HasPrefix(err.Error(), "could not reach Microsoft Entra ID to get an access token: ") || strings.Contains(err.Error(), "Microsoft Graph") {
		t.Errorf("token endpoint unreachable: error = %v", err)
	}
	if n := fg.Requests(); n != 0 {
		t.Errorf("%d sendMail requests without an access token", n)
	}

	useEndpoints(t, fg.URL(), dead.URL)
	err = newClient(t, fg).SendMail(context.Background(), fg.Sender, mail)
	if err == nil || !strings.HasPrefix(err.Error(), "could not reach Microsoft Graph: ") || strings.Contains(err.Error(), "Entra") {
		t.Errorf("Microsoft Graph unreachable: error = %v", err)
	}
	if n := fg.TokenRequests(); n != 1 {
		t.Errorf("%d token requests, want 1", n)
	}
}
