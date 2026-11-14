package msgraph_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/msgraph"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakegraph"
)

// objectID returns the i-th test object ID.
func objectID(i int) string { return fmt.Sprintf("%08x-0000-4000-8000-000000000000", i) }

// GroupNames asks for at most 15 IDs per request (the limit of the "in"
// operator), skips what is not a GUID and repeated IDs, and leaves out the
// groups Microsoft Entra ID does not know.
func TestGroupNames(t *testing.T) {
	fg := fakegraph.Start(t)
	c := newClient(t, fg)
	var ids []string
	for i := 1; i <= 40; i++ {
		ids = append(ids, objectID(i))
		if i%10 != 0 { // every tenth group was deleted
			fg.SetGroup(objectID(i), fmt.Sprintf("Group %d", i))
		}
	}
	ids = append(ids, strings.ToUpper(objectID(1)), "it-admins", "x' or 1 eq 1 or id eq 'y", "")

	names, err := c.GroupNames(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 36 {
		t.Errorf("%d names, want 36: %v", len(names), names)
	}
	if names[objectID(1)] != "Group 1" || names[objectID(39)] != "Group 39" {
		t.Errorf("names = %v", names)
	}
	if _, found := names[objectID(10)]; found {
		t.Error("a deleted group has a name")
	}
	if requests, asked := fg.GroupRequests(); requests != 3 || asked != 40 {
		t.Errorf("%d requests for %d IDs, want 3 for 40", requests, asked)
	}

	// Nothing to look up: no request.
	names, err = c.GroupNames(context.Background(), []string{"it-admins"})
	if err != nil || len(names) != 0 {
		t.Errorf("GroupNames(non-GUID) = %v, %v", names, err)
	}
	if requests, _ := fg.GroupRequests(); requests != 3 {
		t.Errorf("%d requests, want no further one", requests)
	}
}

func TestGroupNamesErrors(t *testing.T) {
	fg := fakegraph.Start(t)
	c := newClient(t, fg)
	var ids []string
	for i := 1; i <= 20; i++ {
		fg.SetGroup(objectID(i), "g")
		ids = append(ids, objectID(i))
	}

	// No permission to read groups.
	fg.FailGroupsNext(fakegraph.GroupsDenied())
	names, err := c.GroupNames(context.Background(), ids)
	var ge *msgraph.Error
	if !errors.As(err, &ge) || ge.StatusCode != http.StatusForbidden || ge.Code != "Authorization_RequestDenied" || names != nil {
		t.Errorf("GroupNames = %v, %v; want HTTP 403 Authorization_RequestDenied and no names", names, err)
	}

	// Wrong client secret: the access token request fails.
	bad, err := msgraph.New(fg.Issuer(), fg.ClientID, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.GroupNames(context.Background(), ids); err == nil || !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Errorf("error with a wrong secret = %v", err)
	}
}

// The request as sent: the filter with quoted IDs and spaces as %20, the
// selected properties; next page links are followed on the Graph server
// only, and a failure of a later request returns no names.
func TestGroupNamesRequest(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
		answer   func(w http.ResponseWriter, r *http.Request, n int)
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token_type":"Bearer","expires_in":3600,"access_token":"t"}`)
			return
		}
		mu.Lock()
		requests = append(requests, r.RequestURI)
		n := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		answer(w, r, n)
	}))
	t.Cleanup(srv.Close)
	useEndpoints(t, srv.URL, srv.URL)
	c, err := msgraph.New(testIssuer, "client", "secret")
	if err != nil {
		t.Fatal(err)
	}
	a, b := objectID(1), objectID(2)

	answer = func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			fmt.Fprintf(w, `{"value":[{"id":%q,"displayName":" IT Ops "}],"@odata.nextLink":%q}`, strings.ToUpper(a), srv.URL+"/v1.0/groups?page=2")
			return
		}
		fmt.Fprintf(w, `{"value":[{"id":%q,"displayName":"Staff"}]}`, b)
	}
	names, err := c.GroupNames(context.Background(), []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[a] != "IT Ops" || names[b] != "Staff" {
		t.Errorf("names = %q", names)
	}
	want := "/v1.0/groups?%24filter=id%20in%20%28%27" + a + "%27%2C%20%27" + b + "%27%29&%24select=id%2CdisplayName"
	mu.Lock()
	if len(requests) != 2 || requests[0] != want || requests[1] != "/v1.0/groups?page=2" {
		t.Errorf("requests = %q, want [%q /v1.0/groups?page=2]", requests, want)
	}
	requests = nil
	mu.Unlock()

	// A next page link to another server is not followed: the access token
	// must not go there. The second link looks like one to the Graph server
	// (user info before the host).
	var foreignHits int
	var foreignMu sync.Mutex
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignMu.Lock()
		foreignHits++
		foreignMu.Unlock()
		t.Errorf("request to the foreign server: %s, Authorization %q", r.URL, r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"value":[]}`)
	}))
	defer foreign.Close()
	for _, link := range []string{
		foreign.URL + "/v1.0/groups?page=2",
		srv.URL + "@" + strings.TrimPrefix(foreign.URL, "http://") + "/v1.0/groups?page=2",
		"https://evil.example/v1.0/groups?page=2",
	} {
		answer = func(w http.ResponseWriter, r *http.Request, n int) {
			fmt.Fprintf(w, `{"value":[],"@odata.nextLink":%q}`, link)
		}
		names, err := c.GroupNames(context.Background(), []string{a})
		if err == nil || names != nil || !strings.Contains(err.Error(), "next page link to another server") {
			t.Errorf("GroupNames with the next link %s = %v, %v; want the guard's error", link, names, err)
		}
	}
	foreignMu.Lock()
	if foreignHits != 0 {
		t.Errorf("%d requests to the foreign server", foreignHits)
	}
	foreignMu.Unlock()

	// The second of two requests fails: no names.
	mu.Lock()
	requests = nil
	mu.Unlock()
	var ids []string
	for i := 1; i <= 16; i++ {
		ids = append(ids, objectID(i))
	}
	answer = func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			fmt.Fprintf(w, `{"value":[{"id":%q,"displayName":"x"}]}`, a)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"code":"ServiceUnavailable","message":"Try again later."}}`)
	}
	names, err = c.GroupNames(context.Background(), ids)
	var ge *msgraph.Error
	if !errors.As(err, &ge) || ge.StatusCode != http.StatusServiceUnavailable || names != nil {
		t.Errorf("GroupNames = %v, %v; want HTTP 503 and no names", names, err)
	}
}
