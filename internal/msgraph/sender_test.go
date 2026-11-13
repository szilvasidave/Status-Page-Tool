package msgraph_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/msgraph"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakegraph"
)

// A sender that begins with "$" is addressed as /users('…'): Microsoft
// Graph answers /users/$… with HTTP 400 (OData reserves the "$" prefix).
// Other senders keep /users/{sender}.
func TestSenderPath(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token_type":"Bearer","expires_in":3600,"access_token":"t"}`)
			return
		}
		mu.Lock()
		paths = append(paths, r.RequestURI) // as sent
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	useEndpoints(t, srv.URL, srv.URL)
	c, err := msgraph.New(testIssuer, "client", "secret")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ sender, path string }{
		{"status@contoso.example", "/v1.0/users/status@contoso.example/sendMail"},
		{"a#b$c@contoso.example", "/v1.0/users/a%23b$c@contoso.example/sendMail"},
		{"$status@contoso.example", "/v1.0/users('$status@contoso.example')/sendMail"},
		{"$o'brien#1@contoso.example", "/v1.0/users('$o%27%27brien%231@contoso.example')/sendMail"},
	}
	for _, tc := range tests {
		if err := c.SendMail(context.Background(), tc.sender, msgraph.Mail{To: "a@contoso.example", HTML: "x"}); err != nil {
			t.Errorf("%s: %v", tc.sender, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i, tc := range tests {
		if i >= len(paths) || paths[i] != tc.path {
			t.Errorf("%s: request paths %q, want %q", tc.sender, paths, tc.path)
		}
	}
}

// The fake server answers like Microsoft Graph: it rejects /users/$… (and
// reports it as a test error) and accepts /users('$…').
func TestDollarSender(t *testing.T) {
	fg := fakegraph.Start(t)
	fg.AddMailbox("$status@contoso.example")
	fg.AddMailbox("$o'brien@contoso.example")
	c := newClient(t, fg)
	for _, sender := range []string{"$status@contoso.example", "$o'brien@contoso.example"} {
		if err := c.SendMail(context.Background(), sender, msgraph.Mail{To: "a@contoso.example", Subject: "Hi", HTML: "x"}); err != nil {
			t.Errorf("%s: %v", sender, err)
		}
	}
	msgs := fg.Messages()
	if len(msgs) != 2 || msgs[0].Sender != "$status@contoso.example" || msgs[1].Sender != "$o'brien@contoso.example" {
		t.Errorf("messages = %+v", msgs)
	}
}
