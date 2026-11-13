package scim

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentRequests sends requests at the same time, as the Microsoft
// Entra provisioning service does: users created in parallel (each one
// several times), members added to the same group in parallel, and queries.
// Nothing may fail with a server error, every user must exist once, and no
// membership may get lost.
func TestConcurrentRequests(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		const n = 16
		group := e.do(http.MethodPost, "/scim/v2/Groups", `{"displayName":"Everybody","externalId":"everybody"}`).
			expect(t, http.StatusCreated).str("id")

		type result struct {
			what   string
			status int
			id     string
			raw    string
		}
		results := make(chan result, 6*n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			user := fmt.Sprintf(`{"userName":"user%d@contoso.com","externalId":"u%d","displayName":"User %d",
				"emails":[{"value":"user%d@contoso.com","type":"work","primary":true}]}`, i, i, i, i)
			for attempt := 0; attempt < 3; attempt++ { // the same user three times
				wg.Add(1)
				go func() {
					defer wg.Done()
					r := e.do(http.MethodPost, "/scim/v2/Users", user)
					results <- result{"create", r.status, r.str("id"), r.raw}
				}()
			}
			wg.Add(2)
			go func() {
				defer wg.Done()
				r := e.do(http.MethodGet, filterPath("/scim/v2/Users", fmt.Sprintf(`userName eq "user%d@contoso.com"`, i)), "")
				results <- result{"query", r.status, "", r.raw}
			}()
			go func() {
				defer wg.Done()
				r := e.do(http.MethodGet, "/scim/v2/Groups/"+group, "")
				results <- result{"get group", r.status, "", r.raw}
			}()
		}
		wg.Wait()

		var ids []string
		for len(results) > 0 {
			r := <-results
			switch {
			case r.status >= 500:
				t.Errorf("%s: %d %s", r.what, r.status, r.raw)
			case r.what == "create" && r.status == http.StatusCreated:
				ids = append(ids, r.id)
			case r.what == "create" && r.status != http.StatusConflict:
				t.Errorf("create: %d %s", r.status, r.raw)
			case r.what != "create" && r.status != http.StatusOK:
				t.Errorf("%s: %d %s", r.what, r.status, r.raw)
			}
		}
		if len(ids) != n {
			t.Fatalf("%d users created, want %d (each user exactly once)", len(ids), n)
		}
		if r := e.do(http.MethodGet, "/scim/v2/Users", "").expect(t, http.StatusOK); r.total() != n {
			t.Fatalf("%d users stored, want %d", r.total(), n)
		}

		// Add every user to the group with its own request, all at once,
		// while the group is renamed.
		for i, id := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				body := `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
					"Operations":[{"op":"Add","path":"members","value":[{"value":"` + id + `"}]}]}`
				if i%4 == 0 {
					body = strings.Replace(body, `]}`, `]},{"op":"Replace","path":"displayName","value":"Everybody `+fmt.Sprint(i)+`"}`, 1)
				}
				r := e.do(http.MethodPatch, "/scim/v2/Groups/"+group, body)
				results <- result{"add member", r.status, id, r.raw}
			}()
		}
		wg.Wait()
		for len(results) > 0 {
			if r := <-results; r.status != http.StatusNoContent {
				t.Errorf("%s %s: %d %s", r.what, r.id, r.status, r.raw)
			}
		}
		r := e.do(http.MethodGet, "/scim/v2/Groups/"+group, "").expect(t, http.StatusOK)
		if members, _ := r.body["members"].([]any); len(members) != n {
			t.Fatalf("%d members, want %d: %s", len(members), n, r.raw)
		}

		// Remove them again in parallel (the compliant style).
		for _, id := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := e.do(http.MethodPatch, "/scim/v2/Groups/"+group,
					`{"Operations":[{"op":"remove","path":"members[value eq \"`+id+`\"]"}]}`)
				results <- result{"remove member", r.status, id, r.raw}
			}()
		}
		wg.Wait()
		for len(results) > 0 {
			if r := <-results; r.status != http.StatusNoContent {
				t.Errorf("%s %s: %d %s", r.what, r.id, r.status, r.raw)
			}
		}
		r = e.do(http.MethodGet, "/scim/v2/Groups/"+group, "").expect(t, http.StatusOK)
		if members, _ := r.body["members"].([]any); len(members) != 0 {
			t.Fatalf("%d members left: %s", len(members), r.raw)
		}
	})
}
