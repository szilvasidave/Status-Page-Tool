package msgraph

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A timeout of the access token request names Microsoft Entra ID, one of
// the sendMail request Microsoft Graph. Nothing is logged (oauth2.Transport
// logged a deprecation warning when the client timeout fired).
func TestTimeouts(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok/oauth2/v2.0/token" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token_type":"Bearer","expires_in":3600,"access_token":"t"}`)
			return
		}
		select { // the other token endpoint and Microsoft Graph do not answer
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // before srv.Close

	var logged bytes.Buffer
	out := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(out) })

	mail := Mail{To: "a@contoso.example", HTML: "x"}
	tests := []struct{ tenant, want string }{
		{"hang", "Microsoft Entra ID did not respond within 30s to the access token request"},
		{"ok", "Microsoft Graph did not respond within 30s"},
	}
	for _, tc := range tests {
		c := newClient(srv.URL+"/"+tc.tenant+"/oauth2/v2.0/token", srv.URL, "client", "secret")
		c.http.Timeout = 100 * time.Millisecond
		if err := c.SendMail(context.Background(), "status@contoso.example", mail); err == nil || err.Error() != tc.want {
			t.Errorf("%s: error = %v, want %q", tc.tenant, err, tc.want)
		}
	}
	log.SetOutput(out) // waits for a write in progress
	if s := logged.String(); strings.TrimSpace(s) != "" {
		t.Errorf("logged: %s", s)
	}
}
