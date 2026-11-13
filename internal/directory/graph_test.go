package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

const (
	testGroupID = "11111111-2222-3333-4444-555555555555"
	testSecret  = "s3cret~value"
)

// fakeGraph serves the token endpoint and the Microsoft Graph endpoints.
type fakeGraph struct {
	t   *testing.T
	srv *httptest.Server
	mux *http.ServeMux

	mu          sync.Mutex
	tokenCalls  int
	tokenStatus int    // != 0: token endpoint fails with this status
	tokenBody   string // body for a failing token endpoint
}

func newFakeGraph(t *testing.T) *fakeGraph {
	f := &fakeGraph{t: t, mux: http.NewServeMux()}
	f.mux.HandleFunc("POST /tenant/oauth2/v2.0/token", f.token)
	f.srv = httptest.NewServer(f.mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGraph) client() *GraphClient {
	c := newGraphClient(f.srv.URL+"/tenant/oauth2/v2.0/token", f.srv.URL, "client-id", testSecret)
	c.sleep = func(ctx context.Context, d time.Duration) error { return nil }
	return c
}

func (f *fakeGraph) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tokenCalls++
	status, body := f.tokenStatus, f.tokenBody
	f.mu.Unlock()
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
		return
	}
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("token request: %v", err)
	}
	want := map[string]string{
		"grant_type":    "client_credentials",
		"client_id":     "client-id",
		"client_secret": testSecret,
		"scope":         "https://graph.microsoft.com/.default",
	}
	for k, v := range want {
		if got := r.PostForm.Get(k); got != v {
			f.t.Errorf("token request %s = %q, want %q", k, got, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"token_type":"Bearer","expires_in":3600,"access_token":"test-token"}`)
}

// handle registers a Graph handler that checks the access token.
func (f *fakeGraph) handle(pattern string, h http.HandlerFunc) {
	f.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			f.t.Errorf("Authorization = %q", got)
		}
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func graphErrorBody(code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message}}
}

func TestNewGraphClient(t *testing.T) {
	tests := []struct {
		issuer    string
		wantToken string // "" = error wrapping ErrNotEnabled
	}{
		{"https://login.microsoftonline.com/9188040d-6c67-4c5b-b112-36a304b66dad/v2.0", "https://login.microsoftonline.com/9188040d-6c67-4c5b-b112-36a304b66dad/oauth2/v2.0/token"},
		{"https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0/", "https://login.microsoftonline.com/contoso.onmicrosoft.com/oauth2/v2.0/token"},
		{"https://sts.windows.net/9188040d-6c67-4c5b-b112-36a304b66dad/", "https://login.microsoftonline.com/9188040d-6c67-4c5b-b112-36a304b66dad/oauth2/v2.0/token"},
		{"https://sts.windows.net/9188040d-6c67-4c5b-b112-36a304b66dad", "https://login.microsoftonline.com/9188040d-6c67-4c5b-b112-36a304b66dad/oauth2/v2.0/token"},
		{"https://accounts.google.com", ""},
		{"https://login.microsoftonline.com/common/v2.0", ""},
		{"https://login.microsoftonline.com/tenant", ""},
		{"http://login.microsoftonline.com/tenant/v2.0", ""},
		{"https://keycloak.example.com/realms/it", ""},
		{"", ""},
	}
	for _, tc := range tests {
		c, err := NewGraphClient(tc.issuer, "client-id", "secret")
		if tc.wantToken == "" {
			if !errors.Is(err, ErrNotEnabled) {
				t.Errorf("%q: error = %v, want ErrNotEnabled", tc.issuer, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.issuer, err)
			continue
		}
		if c.tokenURL != tc.wantToken {
			t.Errorf("%q: token URL = %q, want %q", tc.issuer, c.tokenURL, tc.wantToken)
		}
		if c.graphURL != "https://graph.microsoft.com" {
			t.Errorf("graph URL = %q", c.graphURL)
		}
	}

	_, err := NewGraphClient("https://accounts.google.com", "id", "secret")
	if err == nil || !strings.Contains(err.Error(), "issuer is not a Microsoft Entra ID issuer") {
		t.Errorf("non-Entra error = %v", err)
	}
	_, err = NewGraphClient("https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0", "id", "")
	if !errors.Is(err, ErrNotEnabled) {
		t.Errorf("missing secret: error = %v, want ErrNotEnabled", err)
	}
}

func TestIsGUID(t *testing.T) {
	for s, want := range map[string]bool{
		"11111111-2222-3333-4444-555555555555":   true,
		"9188040D-6C67-4C5B-B112-36A304B66DAD":   true,
		"9188040d6c674c5bb11236a304b66dad":       false,
		"{9188040d-6c67-4c5b-b112-36a304b66dad}": false,
		"9188040d-6c67-4c5b-b112-36a304b66dag":   false,
		"9188040d-6c67-4c5b-b112_36a304b66dad":   false,
		"IT-Admins":                              false,
		"":                                       false,
	} {
		if got := IsGUID(s); got != want {
			t.Errorf("IsGUID(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestGroup(t *testing.T) {
	f := newFakeGraph(t)
	f.handle("GET /v1.0/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$select"); got != "id,displayName,description" {
			t.Errorf("$select = %q", got)
		}
		if r.PathValue("id") != testGroupID {
			writeJSON(w, 404, graphErrorBody("Request_ResourceNotFound", "Resource does not exist."))
			return
		}
		writeJSON(w, 200, map[string]any{"id": testGroupID, "displayName": "IT Admins", "description": nil})
	})
	c := f.client()

	g, err := c.Group(context.Background(), testGroupID)
	if err != nil {
		t.Fatal(err)
	}
	if g != (Group{ID: testGroupID, Name: "IT Admins"}) {
		t.Errorf("Group = %+v", g)
	}

	_, err = c.Group(context.Background(), "99999999-2222-3333-4444-555555555555")
	if !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("missing group: error = %v, want ErrGroupNotFound", err)
	}
	_, err = c.Group(context.Background(), "IT-Admins")
	if err == nil || errors.Is(err, ErrGroupNotFound) {
		t.Errorf("non-GUID id: error = %v", err)
	}

	// The token is fetched once and cached.
	if _, err := c.Group(context.Background(), testGroupID); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenCalls != 1 {
		t.Errorf("token requested %d times, want 1", f.tokenCalls)
	}
}

func TestGroupMembersPagingAndFiltering(t *testing.T) {
	f := newFakeGraph(t)
	f.handle("GET /v1.0/groups/{id}/transitiveMembers/microsoft.graph.user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("ConsistencyLevel"); got != "eventual" {
			t.Errorf("ConsistencyLevel = %q", got)
		}
		q := r.URL.Query()
		if q.Get("$skiptoken") == "page2" {
			writeJSON(w, 200, map[string]any{"value": []map[string]any{
				{"id": "u5", "displayName": "Eve", "mail": nil, "userPrincipalName": "eve@contoso.com", "accountEnabled": nil},
			}})
			return
		}
		if got := q.Get("$select"); got != "id,displayName,mail,userPrincipalName,accountEnabled" {
			t.Errorf("$select = %q", got)
		}
		if got := q.Get("$top"); got != "999" {
			t.Errorf("$top = %q", got)
		}
		writeJSON(w, 200, map[string]any{
			"@odata.nextLink": f.srv.URL + "/v1.0/groups/" + testGroupID + "/transitiveMembers/microsoft.graph.user?$skiptoken=page2",
			"value": []map[string]any{
				{"id": "u1", "displayName": "Alice", "mail": "Alice@Contoso.com", "userPrincipalName": "alice@contoso.onmicrosoft.com", "accountEnabled": true},
				{"id": "u2", "displayName": "Bob", "mail": nil, "userPrincipalName": "bob@contoso.com", "accountEnabled": true},
				{"id": "u3", "displayName": "Guest", "mail": nil, "userPrincipalName": "guest_example.com#EXT#@contoso.onmicrosoft.com", "accountEnabled": true},
				{"id": "u4", "displayName": "Disabled", "mail": "disabled@contoso.com", "userPrincipalName": "disabled@contoso.com", "accountEnabled": false},
				{"id": "u6", "displayName": "Guest With Mail", "mail": "jane@example.org", "userPrincipalName": "jane_example.org#EXT#@contoso.onmicrosoft.com", "accountEnabled": true},
				{"id": "u7", "displayName": "No Mail", "mail": "", "userPrincipalName": "service-account", "accountEnabled": true},
			},
		})
	})

	got, err := f.client().GroupMembers(context.Background(), testGroupID)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.DirectoryUser{
		{DirectoryID: "u1", Email: "alice@contoso.com", Name: "Alice"},
		{DirectoryID: "u2", Email: "bob@contoso.com", Name: "Bob"},
		{DirectoryID: "u3", Email: "", Name: "Guest"},
		{DirectoryID: "u6", Email: "jane@example.org", Name: "Guest With Mail"},
		{DirectoryID: "u7", Email: "", Name: "No Mail"},
		{DirectoryID: "u5", Email: "eve@contoso.com", Name: "Eve"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("members =\n%+v\nwant\n%+v", got, want)
	}
}

func TestGroupMembersRejectsForeignNextLink(t *testing.T) {
	f := newFakeGraph(t)
	f.handle("GET /v1.0/groups/{id}/transitiveMembers/microsoft.graph.user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"value": []any{}, "@odata.nextLink": "https://evil.example.com/steal"})
	})
	if _, err := f.client().GroupMembers(context.Background(), testGroupID); err == nil {
		t.Fatal("expected an error for a next link to another host")
	}
}

func TestGroupMembersNotFound(t *testing.T) {
	f := newFakeGraph(t)
	f.handle("GET /v1.0/groups/{id}/transitiveMembers/microsoft.graph.user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, graphErrorBody("Request_ResourceNotFound", "Resource '"+r.PathValue("id")+"' does not exist."))
	})
	_, err := f.client().GroupMembers(context.Background(), testGroupID)
	if !errors.Is(err, ErrGroupNotFound) || !strings.Contains(err.Error(), testGroupID) {
		t.Errorf("error = %v, want ErrGroupNotFound mentioning the id", err)
	}
}

func TestGraphErrors(t *testing.T) {
	f := newFakeGraph(t)
	f.handle("GET /v1.0/groups/{id}/transitiveMembers/microsoft.graph.user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, graphErrorBody("Authorization_RequestDenied", "Insufficient privileges to complete the operation."))
	})
	f.handle("GET /v1.0/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, "<html>oops</html>")
	})
	c := f.client()

	_, err := c.GroupMembers(context.Background(), testGroupID)
	if err == nil {
		t.Fatal("expected error")
	}
	var ge *graphError
	if !errors.As(err, &ge) || ge.StatusCode != 403 || ge.Code != "Authorization_RequestDenied" {
		t.Errorf("error = %#v", err)
	}
	for _, want := range []string{"HTTP 403", "Authorization_RequestDenied", "Insufficient privileges to complete the operation.", "GroupMember.Read.All and User.Read.All"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	_, err = c.Group(context.Background(), testGroupID)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "Internal Server Error") {
		t.Errorf("non-JSON error = %v", err)
	}
}

func TestTokenError(t *testing.T) {
	f := newFakeGraph(t)
	f.tokenStatus = 401
	f.tokenBody = `{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided.\r\nTrace ID: abc\r\nCorrelation ID: def","error_codes":[7000215]}`
	f.handle("GET /v1.0/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		t.Error("Graph called without a token")
	})
	_, err := f.client().Group(context.Background(), testGroupID)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "invalid_client: AADSTS7000215: Invalid client secret provided.") || strings.Contains(msg, "Trace ID") {
		t.Errorf("error = %q", msg)
	}
}

func TestRetry(t *testing.T) {
	f := newFakeGraph(t)
	var calls int
	f.handle("GET /v1.0/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.PathValue("id") {
		case testGroupID: // throttled once, then OK
			if calls == 1 {
				w.Header().Set("Retry-After", "7")
				writeJSON(w, 429, graphErrorBody("TooManyRequests", "Too many requests"))
				return
			}
			writeJSON(w, 200, map[string]any{"id": testGroupID, "displayName": "IT"})
		default: // always unavailable
			w.Header().Set("Retry-After", "3600")
			writeJSON(w, 503, graphErrorBody("serviceNotAvailable", "Service unavailable"))
		}
	})
	c := f.client()
	var waits []time.Duration
	c.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}

	if _, err := c.Group(context.Background(), testGroupID); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(waits) != 1 || waits[0] != 7*time.Second {
		t.Errorf("calls = %d, waits = %v; want 2 calls and one 7s wait", calls, waits)
	}

	calls, waits = 0, nil
	_, err := c.Group(context.Background(), "99999999-2222-3333-4444-555555555555")
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("error = %v", err)
	}
	if calls != 3 || len(waits) != 2 || waits[0] != graphMaxWait || waits[1] != graphMaxWait {
		t.Errorf("calls = %d, waits = %v; want 3 calls and 2 waits capped at %s", calls, waits, graphMaxWait)
	}

	// A cancelled context stops waiting.
	c.sleep = sleepContext
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = c.Group(ctx, "99999999-2222-3333-4444-555555555555")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("error = %v after %s", err, time.Since(start))
	}
}

func TestRetryAfter(t *testing.T) {
	if got := retryAfter("", 1); got != 2*time.Second {
		t.Errorf("default first wait = %s", got)
	}
	if got := retryAfter("5", 1); got != 5*time.Second {
		t.Errorf("seconds = %s", got)
	}
	if got := retryAfter("100000", 1); got != graphMaxWait {
		t.Errorf("capped = %s", got)
	}
	date := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfter(date, 1); got <= 8*time.Second || got > 10*time.Second {
		t.Errorf("http date = %s", got)
	}
}

func TestSearchGroups(t *testing.T) {
	f := newFakeGraph(t)
	f.handle("GET /v1.0/groups", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "+") {
			t.Errorf("raw query contains '+': %s", r.URL.RawQuery)
		}
		q := r.URL.Query()
		if got, want := q.Get("$filter"), "startswith(displayName,'O''Brien & Co #1+')"; got != want {
			t.Errorf("$filter = %q, want %q", got, want)
		}
		if got := q.Get("$select"); got != "id,displayName,description" {
			t.Errorf("$select = %q", got)
		}
		if got := q.Get("$top"); got != "5" {
			t.Errorf("$top = %q", got)
		}
		writeJSON(w, 200, map[string]any{"value": []map[string]any{
			{"id": "g2", "displayName": "o'brien & co #1+ support", "description": "Support"},
			{"id": "g1", "displayName": "O'Brien & Co #1+", "description": nil},
		}})
	})
	f.handle("GET /v1.0/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != testGroupID {
			writeJSON(w, 404, graphErrorBody("Request_ResourceNotFound", "not found"))
			return
		}
		writeJSON(w, 200, map[string]any{"id": testGroupID, "displayName": "IT Admins"})
	})
	c := f.client()

	got, err := c.SearchGroups(context.Background(), " O'Brien & Co #1+ ", 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []Group{{ID: "g1", Name: "O'Brien & Co #1+"}, {ID: "g2", Name: "o'brien & co #1+ support", Description: "Support"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("SearchGroups = %+v, want %+v", got, want)
	}

	got, err = c.SearchGroups(context.Background(), testGroupID, 5)
	if err != nil || len(got) != 1 || got[0].Name != "IT Admins" {
		t.Errorf("GUID search = %+v, %v", got, err)
	}
	got, err = c.SearchGroups(context.Background(), "99999999-2222-3333-4444-555555555555", 5)
	if err != nil || len(got) != 0 {
		t.Errorf("unknown GUID search = %+v, %v", got, err)
	}
	got, err = c.SearchGroups(context.Background(), "  ", 5)
	if err != nil || len(got) != 0 {
		t.Errorf("empty search = %+v, %v", got, err)
	}
}
