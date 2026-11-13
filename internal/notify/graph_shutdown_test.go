package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/msgraph"
	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakegraph"
)

// TestGraphShutdownDuringTokenRequest: when the service stops while the
// worker waits for an access token that does not come (e.g. a firewall
// drops the connections to Microsoft Entra ID), Run returns at once instead
// of after the 30 s timeout (the Windows service manager allows about 20 s).
// The e-mail keeps its attempts.
func TestGraphShutdownDuringTokenRequest(t *testing.T) {
	fg := fakegraph.Start(t)
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		select { // no answer
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // before srv.Close

	e := newNotifyEnv(t)
	e.configureGraph(fg)
	msgraph.DefaultEndpoints = msgraph.Endpoints{Login: srv.URL, Graph: srv.URL} // restored by fakegraph
	e.enqueue(1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.n.Run(ctx)
		close(done)
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("no token request")
	}
	cancel() // shutdown
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s after shutdown")
	}
	if n := e.outbox()[0]; n.Status != store.NotificationPending || n.Attempts != 0 || n.LastError != "" {
		t.Errorf("e-mail after shutdown = %+v, want pending without a failed attempt", n)
	}
}
