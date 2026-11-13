package scim

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// sendWithChange sends a request whose headers arrive first; change runs
// once the endpoint has authenticated the request and reads its body (the
// server answers "100 Continue" then), before the body is sent.
func (e *env) sendWithChange(method, path, body string, change func()) *http.Response {
	e.t.Helper()
	conn, err := net.Dial("tcp", e.srv.Listener.Addr().String())
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: scim.test\r\nAuthorization: Bearer %s\r\nContent-Type: application/scim+json\r\n"+
		"Content-Length: %d\r\nExpect: 100-continue\r\n\r\n", method, path, e.token, len(body))
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(status, "HTTP/1.1 100 ") {
		e.t.Fatalf("no 100 Continue: %q %v", status, err)
	}
	if _, err := br.ReadString('\n'); err != nil {
		e.t.Fatal(err)
	}
	change()
	if _, err := conn.Write([]byte(body)); err != nil {
		e.t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// TestTokenChangedDuringRequest: a request that was authenticated before
// provisioning was turned off (or the token replaced) changes nothing. In
// particular a group that provisioning would take over is not left
// provisioned with provisioning off, which would freeze its members.
func TestTokenChangedDuringRequest(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		const adminsID = "11111111-0000-4000-8000-000000000001"
		admins, err := e.store.CreateGroup(ctx, adminsID, "Admins")
		if err != nil {
			t.Fatal(err)
		}
		if err := e.store.UpdateGroup(ctx, admins.ID, "Admins", store.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		user := e.do(http.MethodPost, "/scim/v2/Users", `{"userName":"bob@contoso.com","active":true}`).expect(t, http.StatusCreated).str("id")

		// Replaced token: the request with the old one fails.
		resp := e.sendWithChange(http.MethodPatch, "/scim/v2/Users/"+user, `{"Operations":[{"op":"Replace","path":"active","value":false}]}`, func() {
			_, hash := NewToken()
			if err := e.store.SetSCIMToken(ctx, hash, time.Now()); err != nil {
				t.Fatal(err)
			}
		})
		if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Fatalf("PATCH with the replaced token: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
		if u, err := e.store.SCIMUser(ctx, mustID(t, user)); err != nil || !u.Active {
			t.Fatalf("user changed with the replaced token: %+v %v", u, err)
		}

		// Turned off: the group with the Administrator role is not taken over.
		token, hash := NewToken()
		if err := e.store.SetSCIMToken(ctx, hash, time.Now()); err != nil {
			t.Fatal(err)
		}
		e.token = token
		resp = e.sendWithChange(http.MethodPost, "/scim/v2/Groups", `{"displayName":"Admins","externalId":"`+adminsID+`","members":[{"value":"`+user+`"}]}`, func() {
			if err := e.store.SetSCIMToken(ctx, "", time.Time{}); err != nil {
				t.Fatal(err)
			}
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("POST after turning provisioning off: %d", resp.StatusCode)
		}
		g, err := e.store.GetGroup(ctx, admins.ID)
		if err != nil {
			t.Fatal(err)
		}
		if g.Provisioned || g.MemberCount != 0 {
			t.Fatalf("group after turning provisioning off: %+v", g)
		}
	})
}
