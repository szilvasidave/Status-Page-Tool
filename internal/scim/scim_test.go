package scim

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

func TestAuthentication(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		users := "/scim/v2/Users"
		for name, header := range map[string]map[string]string{
			"no token":     nil,
			"wrong token":  {"Authorization": "Bearer " + strings.Repeat("x", 43)},
			"basic auth":   {"Authorization": "Basic " + e.token},
			"empty bearer": {"Authorization": "Bearer "},
			"token prefix": {"Authorization": "Bearer " + e.token[:20]},
			"cookie only":  {"Cookie": "sp_session=" + e.token},
		} {
			t.Run(name, func(t *testing.T) {
				r := e.doWith(http.MethodGet, users, "", header).expectError(t, http.StatusUnauthorized, "")
				if !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Bearer") {
					t.Errorf("WWW-Authenticate %q", r.header.Get("WWW-Authenticate"))
				}
				// Writes are refused, too.
				e.doWith(http.MethodPost, users, `{"userName":"x@contoso.com"}`, header).expectError(t, http.StatusUnauthorized, "")
			})
		}
		if n, _, _ := e.store.SCIMUsers(ctx, store.SCIMUserQuery{}, 0, 10); len(n) != 0 {
			t.Fatal("an unauthenticated request created a user")
		}
		// The scheme is not case-sensitive.
		e.doWith(http.MethodGet, users, "", map[string]string{"Authorization": "bearer " + e.token}).expect(t, http.StatusOK)

		// A new token replaces the old one; without a token provisioning is off.
		token, hash := NewToken()
		if token == e.token || len(token) != 43 || len(hash) != 64 || hash != HashToken(token) {
			t.Fatalf("token %q hash %q", token, hash)
		}
		if err := e.store.SetSCIMToken(ctx, hash, time.Now()); err != nil {
			t.Fatal(err)
		}
		e.do(http.MethodGet, users, "").expectError(t, http.StatusUnauthorized, "")
		e.doWith(http.MethodGet, users, "", map[string]string{"Authorization": "Bearer " + token}).expect(t, http.StatusOK)
		if err := e.store.SetSCIMToken(ctx, "", time.Time{}); err != nil {
			t.Fatal(err)
		}
		r := e.doWith(http.MethodGet, users, "", map[string]string{"Authorization": "Bearer " + token}).expectError(t, http.StatusUnauthorized, "")
		if !strings.Contains(r.str("detail"), "turned off") {
			t.Errorf("detail: %s", r.raw)
		}
	})
}

func TestDiscovery(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		r := e.do(http.MethodGet, "/scim/v2/ServiceProviderConfig", "").expect(t, http.StatusOK)
		for key, want := range map[string]bool{"patch": true, "bulk": false, "filter": true, "changePassword": false, "sort": false, "etag": false} {
			m, _ := r.body[key].(map[string]any)
			if m["supported"] != want {
				t.Errorf("%s.supported = %v, want %v", key, m["supported"], want)
			}
		}
		if f, _ := r.body["filter"].(map[string]any); f["maxResults"] != float64(500) {
			t.Errorf("filter.maxResults: %v", f)
		}
		if s, _ := r.body["authenticationSchemes"].([]any); len(s) != 1 || s[0].(map[string]any)["type"] != "oauthbearertoken" {
			t.Errorf("authenticationSchemes: %v", r.body["authenticationSchemes"])
		}
		if !strings.Contains(r.raw, `"schemas":["urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"]`) || strings.Contains(r.raw, "null") {
			t.Errorf("ServiceProviderConfig: %s", r.raw)
		}

		r = e.do(http.MethodGet, "/scim/v2/ResourceTypes", "").expect(t, http.StatusOK)
		if res := r.resources(t); len(res) != 2 || res[0]["endpoint"] != "/Users" || res[1]["endpoint"] != "/Groups" {
			t.Errorf("ResourceTypes: %s", r.raw)
		}
		if r = e.do(http.MethodGet, "/scim/v2/ResourceTypes/Group", "").expect(t, http.StatusOK); r.str("schema") != schemaGroup {
			t.Errorf("ResourceTypes/Group: %s", r.raw)
		}

		r = e.do(http.MethodGet, "/scim/v2/Schemas", "").expect(t, http.StatusOK)
		res := r.resources(t)
		if len(res) != 2 || res[0]["id"] != schemaUser || res[1]["id"] != schemaGroup || strings.Contains(r.raw, "null") {
			t.Fatalf("Schemas: %s", r.raw)
		}
		attrs, _ := res[0]["attributes"].([]any)
		if len(attrs) == 0 || attrs[0].(map[string]any)["name"] != "userName" || attrs[0].(map[string]any)["required"] != true {
			t.Errorf("User schema attributes: %v", attrs)
		}
		e.do(http.MethodGet, "/scim/v2/Schemas/"+schemaUser, "").expect(t, http.StatusOK)
		e.do(http.MethodGet, "/scim/v2/Schemas/urn:example:unknown", "").expectError(t, http.StatusNotFound, "")
		e.do(http.MethodPost, "/scim/v2/Schemas", `{}`).expectError(t, http.StatusMethodNotAllowed, "")
	})
}

func TestRequestErrors(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		id := e.do(http.MethodPost, "/scim/v2/Users", `{"userName":"a@contoso.com"}`).expect(t, http.StatusCreated).str("id")
		patch := func(ops string) string {
			return `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[` + ops + `]}`
		}
		cases := []struct {
			name, method, path, body string
			status                   int
			scimType                 string
		}{
			{"invalid JSON", "POST", "/scim/v2/Users", `{"userName":`, 400, "invalidSyntax"},
			{"not an object", "POST", "/scim/v2/Users", `["a"]`, 400, "invalidSyntax"},
			{"no userName", "POST", "/scim/v2/Users", `{"emails":[{"value":"x@contoso.com"}]}`, 400, "invalidValue"},
			{"userName not a string", "POST", "/scim/v2/Users", `{"userName":{"a":1}}`, 400, "invalidValue"},
			{"userName too long", "POST", "/scim/v2/Users", `{"userName":"` + strings.Repeat("ő", 257) + `"}`, 400, "invalidValue"},
			{"control character", "POST", "/scim/v2/Users", `{"userName":"a\u0000b"}`, 400, "invalidValue"},
			{"invalid address", "POST", "/scim/v2/Users", `{"userName":"x","emails":[{"value":"not an address","type":"work"}]}`, 400, "invalidValue"},
			{"active not boolean", "PATCH", "/scim/v2/Users/" + id, patch(`{"op":"Replace","path":"active","value":"maybe"}`), 400, "invalidValue"},
			{"no Operations", "PATCH", "/scim/v2/Users/" + id, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"]}`, 400, "invalidSyntax"},
			{"unknown op", "PATCH", "/scim/v2/Users/" + id, patch(`{"op":"Move","path":"userName","value":"x"}`), 400, "invalidSyntax"},
			{"remove without path", "PATCH", "/scim/v2/Users/" + id, patch(`{"op":"Remove"}`), 400, "noTarget"},
			{"remove userName", "PATCH", "/scim/v2/Users/" + id, patch(`{"op":"Remove","path":"userName"}`), 400, "invalidValue"},
			{"bad path", "PATCH", "/scim/v2/Users/" + id, patch(`{"op":"Replace","path":"emails[type eq \"work\".value","value":"x"}`), 400, "invalidPath"},
			{"no-path value not an object", "PATCH", "/scim/v2/Users/" + id, patch(`{"op":"Replace","value":"x"}`), 400, "invalidValue"},
			{"no displayName", "POST", "/scim/v2/Groups", `{"externalId":"g"}`, 400, "invalidValue"},
			{"members not a list", "POST", "/scim/v2/Groups", `{"displayName":"G","members":"1"}`, 400, "invalidValue"},
			{"filter operator", "GET", filterPath("/scim/v2/Users", `userName co "a"`), "", 400, "invalidFilter"},
			{"filter or", "GET", filterPath("/scim/v2/Users", `userName eq "a" or userName eq "b"`), "", 400, "invalidFilter"},
			{"filter not", "GET", filterPath("/scim/v2/Users", `not (userName eq "a")`), "", 400, "invalidFilter"},
			{"filter grouping", "GET", filterPath("/scim/v2/Users", `(userName eq "a")`), "", 400, "invalidFilter"},
			{"filter present", "GET", filterPath("/scim/v2/Users", `userName pr`), "", 400, "invalidFilter"},
			{"filter attribute", "GET", filterPath("/scim/v2/Users", `title eq "Boss"`), "", 400, "invalidFilter"},
			{"filter trailing and", "GET", filterPath("/scim/v2/Users", `userName eq "a" and`), "", 400, "invalidFilter"},
			{"filter quote", "GET", filterPath("/scim/v2/Users", `userName eq "a`), "", 400, "invalidFilter"},
			{"group filter attribute", "GET", filterPath("/scim/v2/Groups", `members.value eq "1"`), "", 400, "invalidFilter"},
			{"count", "GET", "/scim/v2/Users?count=many", "", 400, "invalidValue"},
			{"method", "PATCH", "/scim/v2/Users", `{}`, 405, ""},
			{"method on a user", "POST", "/scim/v2/Users/" + id, `{}`, 405, ""},
			{"unknown resource", "GET", "/scim/v2/Devices", "", 404, ""},
			{"root", "GET", "/scim/v2", "", 404, ""},
			{"wrong base path", "GET", "/scim/Users", "", 404, ""},
			{"nested path", "GET", "/scim/v2/Users/" + id + "/groups", "", 404, ""},
			{"id that is not ours", "GET", "/scim/v2/Users/a0a0a0a0-bbbb-cccc-dddd-e1e1e1e1e1e1", "", 404, ""},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				e.do(c.method, c.path, c.body).expectError(t, c.status, c.scimType)
			})
		}
		r := e.do(http.MethodPatch, "/scim/v2/Users", `{}`)
		if r.header.Get("Allow") != "GET, POST" {
			t.Errorf("Allow: %q", r.header.Get("Allow"))
		}
		if r := e.do(http.MethodGet, "/scim/Users", ""); !strings.Contains(r.str("detail"), "https://status.example.com/scim/v2") {
			t.Errorf("the wrong base path does not name the Tenant URL: %s", r.raw)
		}

		// Content types: application/scim+json and application/json (any
		// charset); nothing else.
		for ct, want := range map[string]int{"application/json": 201, "application/scim+json": 201, "application/json; charset=UTF-8": 201,
			"text/plain": 415, "application/x-www-form-urlencoded": 415} {
			body := fmt.Sprintf(`{"userName":"ct-%d@contoso.com"}`, len(ct))
			r := e.doWith(http.MethodPost, "/scim/v2/Users", body, map[string]string{"Authorization": "Bearer " + e.token, "Content-Type": ct})
			if r.status != want {
				t.Errorf("Content-Type %q: %d, want %d", ct, r.status, want)
			}
		}
		big := `{"userName":"big@contoso.com","title":"` + strings.Repeat("x", maxBodyBytes) + `"}`
		e.do(http.MethodPost, "/scim/v2/Users", big).expectError(t, http.StatusRequestEntityTooLarge, "")
	})
}

func TestUserFiltersAndPaging(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		var ids []string
		for i := 1; i <= 5; i++ {
			ids = append(ids, e.do(http.MethodPost, "/scim/v2/Users", fmt.Sprintf(
				`{"userName":"user%d@contoso.com","externalId":"u%d","emails":[{"value":"user%d@contoso.com","type":"work","primary":true}]}`, i, i, i)).
				expect(t, http.StatusCreated).str("id"))
		}
		list := func(path string) ([]string, response) {
			t.Helper()
			r := e.do(http.MethodGet, path, "").expect(t, http.StatusOK)
			var got []string
			for _, res := range r.resources(t) {
				got = append(got, res["id"].(string))
			}
			return got, r
		}
		got, r := list("/scim/v2/Users")
		if !reflect.DeepEqual(got, ids) || r.total() != 5 || r.body["startIndex"] != float64(1) || r.body["itemsPerPage"] != float64(5) {
			t.Errorf("all users: %v %s", got, r.raw)
		}
		got, r = list("/scim/v2/Users?startIndex=2&count=2")
		if !reflect.DeepEqual(got, ids[1:3]) || r.total() != 5 || r.body["startIndex"] != float64(2) || r.body["itemsPerPage"] != float64(2) {
			t.Errorf("page 2: %v %s", got, r.raw)
		}
		got, r = list("/scim/v2/Users?startIndex=5&count=10")
		if !reflect.DeepEqual(got, ids[4:]) {
			t.Errorf("last page: %v", got)
		}
		got, r = list("/scim/v2/Users?startIndex=9")
		if len(got) != 0 || r.total() != 5 {
			t.Errorf("after the end: %v %s", got, r.raw)
		}
		got, r = list("/scim/v2/Users?count=0")
		if len(got) != 0 || r.total() != 5 {
			t.Errorf("count=0: %v %s", got, r.raw)
		}
		got, _ = list("/scim/v2/Users?startIndex=0&count=1000")
		if len(got) != 5 {
			t.Errorf("startIndex 0, count 1000: %v", got)
		}

		for filter, want := range map[string][]string{
			`userName eq "USER2@contoso.com"`:                                            ids[1:2],
			`UserName EQ "user2@contoso.com"`:                                            ids[1:2],
			`externalId eq "u3"`:                                                         ids[2:3],
			`externalId eq "U3"`:                                                         nil, // case-sensitive
			`emails.value eq "user4@contoso.com"`:                                        ids[3:4],
			`emails[type eq "work"].value eq "user4@contoso.com"`:                        ids[3:4],
			`emails[type eq "work" and primary eq true].value eq "user4@contoso.com"`:    ids[3:4],
			`emails[type eq "home"].value eq "user4@contoso.com"`:                        nil,
			`emails eq "user5@contoso.com"`:                                              ids[4:5],
			`userName eq "user1@contoso.com" and externalId eq "u1"`:                     ids[0:1],
			`userName eq "user1@contoso.com" and externalId eq "u2"`:                     nil,
			`userName eq "user1@contoso.com" and userName eq "user2@contoso.com"`:        nil,
			`id eq "` + ids[2] + `"`:                                                     ids[2:3],
			`id eq "not-a-number"`:                                                       nil,
			`urn:ietf:params:scim:schemas:core:2.0:User:userName eq "user1@contoso.com"`: ids[0:1],
			`userName eq ""`:                                                             nil,
		} {
			got, _ := list(filterPath("/scim/v2/Users", filter))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("filter %s: %v, want %v", filter, got, want)
			}
		}
	})
}

// TestLocalAdministratorInvisible: provisioning never lists, matches or
// changes local administrator accounts.
func TestLocalAdministratorInvisible(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		admin, err := e.store.CreateLocalAdmin(ctx, "admin", "Administrator", "hash")
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprint(admin.ID)
		r := e.do(http.MethodGet, "/scim/v2/Users", "").expect(t, http.StatusOK)
		if r.total() != 0 {
			t.Errorf("the local administrator is listed: %s", r.raw)
		}
		for _, f := range []string{`userName eq "admin"`, `id eq "` + id + `"`} {
			if r := e.do(http.MethodGet, filterPath("/scim/v2/Users", f), "").expect(t, http.StatusOK); r.total() != 0 {
				t.Errorf("filter %s finds the local administrator", f)
			}
		}
		e.do(http.MethodGet, "/scim/v2/Users/"+id, "").expectError(t, http.StatusNotFound, "")
		e.do(http.MethodPut, "/scim/v2/Users/"+id, `{"userName":"admin"}`).expectError(t, http.StatusNotFound, "")
		e.do(http.MethodPatch, "/scim/v2/Users/"+id, `{"Operations":[{"op":"Replace","path":"active","value":false}]}`).expectError(t, http.StatusNotFound, "")
		e.do(http.MethodDelete, "/scim/v2/Users/"+id, "").expectError(t, http.StatusNotFound, "")

		// Creating a user with the same name makes a new user; adding the
		// administrator's id to a group is ignored.
		other := e.do(http.MethodPost, "/scim/v2/Users", `{"userName":"admin"}`).expect(t, http.StatusCreated).str("id")
		if other == id {
			t.Fatal("the local administrator was taken over")
		}
		e.do(http.MethodPost, "/scim/v2/Groups", `{"displayName":"G","members":[{"value":"`+id+`"},{"value":"`+other+`"}]}`).expect(t, http.StatusCreated)
		got, err := e.store.GetUser(ctx, admin.ID)
		if err != nil || !reflect.DeepEqual(*got, *admin) {
			t.Fatalf("the local administrator changed: %+v %v", got, err)
		}
		if groups, _ := e.store.UserGroups(ctx, admin.ID); len(groups) != 0 {
			t.Errorf("the local administrator was added to %d group(s)", len(groups))
		}
	})
}

// TestTakeOverSignedInUser: a user who signed in before is taken over
// instead of creating a second user, by user name or e-mail address.
func TestTakeOverSignedInUser(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		alice, err := e.store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "sub-alice", PrincipalName: "Alice@contoso.com", Email: "alice@contoso.com"})
		if err != nil {
			t.Fatal(err)
		}
		bob, err := e.store.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "sub-bob", Email: "bob@contoso.com"})
		if err != nil {
			t.Fatal(err)
		}
		// Entra ID finds a user who signed in by the user name and updates it.
		r := e.do(http.MethodGet, filterPath("/scim/v2/Users", `userName eq "alice@contoso.com"`), "").expect(t, http.StatusOK)
		if res := r.resources(t); len(res) != 1 || res[0]["id"] != fmt.Sprint(alice.ID) {
			t.Fatalf("signed-in user not found by user name: %s", r.raw)
		}
		e.do(http.MethodPatch, fmt.Sprint("/scim/v2/Users/", alice.ID),
			`{"Operations":[{"op":"Replace","path":"displayName","value":"Alice Example"}]}`).expect(t, http.StatusOK)
		a, _ := e.store.GetUser(ctx, alice.ID)
		if !a.Provisioned || a.Name != "Alice Example" || a.OIDCSubject != "sub-alice" {
			t.Errorf("alice after PATCH: %+v", a)
		}
		// A user without user name is taken over by the e-mail address.
		r = e.do(http.MethodPost, "/scim/v2/Users", `{"userName":"robert@contoso.com","emails":[{"value":"Bob@contoso.com","type":"work"}],"displayName":"Robert"}`).
			expect(t, http.StatusCreated)
		if r.str("id") != fmt.Sprint(bob.ID) || r.str("userName") != "robert@contoso.com" {
			t.Errorf("bob not taken over: %s", r.raw)
		}
		if users, _, _ := e.store.SCIMUsers(ctx, store.SCIMUserQuery{}, 0, 10); len(users) != 2 {
			t.Errorf("%d users, want 2", len(users))
		}
	})
}

func TestPutReplaces(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		u := e.do(http.MethodPost, "/scim/v2/Users", `{"userName":"a@contoso.com","externalId":"a","displayName":"A","emails":[{"value":"a@contoso.com"}]}`).
			expect(t, http.StatusCreated).str("id")
		r := e.do(http.MethodPut, "/scim/v2/Users/"+u, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"b@contoso.com",
"name":{"givenName":"Bea","familyName":"Bauer"},"active":false}`).expect(t, http.StatusOK)
		if r.str("userName") != "b@contoso.com" || r.str("externalId") != "" || r.str("displayName") != "Bea Bauer" || r.body["active"] != false ||
			!strings.Contains(r.raw, `"value":"b@contoso.com"`) {
			t.Errorf("PUT user: %s", r.raw)
		}
		e.do(http.MethodPut, "/scim/v2/Users/987654", `{"userName":"x"}`).expectError(t, http.StatusNotFound, "")

		v := e.do(http.MethodPost, "/scim/v2/Users", `{"userName":"v@contoso.com"}`).expect(t, http.StatusCreated).str("id")
		g := e.do(http.MethodPost, "/scim/v2/Groups", `{"displayName":"G","externalId":"g-1","members":[{"value":"`+u+`"}]}`).
			expect(t, http.StatusCreated).str("id")
		r = e.do(http.MethodPut, "/scim/v2/Groups/"+g, `{"displayName":"G2","members":[{"value":"`+v+`"}]}`).expect(t, http.StatusOK)
		if r.str("displayName") != "G2" || r.str("externalId") != "g-1" || !strings.Contains(r.raw, `"value":"`+v+`"`) || strings.Contains(r.raw, `"value":"`+u+`"`) {
			t.Errorf("PUT group: %s", r.raw)
		}
		// Display names are unique among provisioned groups.
		e.do(http.MethodPost, "/scim/v2/Groups", `{"displayName":"Other"}`).expect(t, http.StatusCreated)
		e.do(http.MethodPatch, "/scim/v2/Groups/"+g, `{"Operations":[{"op":"Replace","path":"displayName","value":"other"}]}`).
			expectError(t, http.StatusConflict, "uniqueness")
	})
}

func TestUserAddress(t *testing.T) {
	cases := []struct {
		state userState
		want  string
	}{
		{userState{emails: []email{{value: "Home@x.com", typ: "home"}, {value: "Work@x.com", typ: "work"}}}, "work@x.com"},
		{userState{emails: []email{{value: "a@x.com", typ: "home"}, {value: "p@x.com", typ: "other", primary: true}}}, "p@x.com"},
		{userState{emails: []email{{value: "first@x.com"}, {value: "second@x.com"}}}, "first@x.com"},
		{userState{userName: "Jane@Contoso.com"}, "jane@contoso.com"},
		{userState{userName: "jane_x.com#EXT#@contoso.onmicrosoft.com"}, ""},
		{userState{userName: "jdoe"}, ""},
	}
	for _, c := range cases {
		if got := c.state.address(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.state, got, c.want)
		}
	}
}

func TestParseFilterAndPath(t *testing.T) {
	conds, err := parseFilter(`userName eq "a \"quoted\" b" AND emails[type eq "work"].value Eq "x@y.z" and active eq true`)
	if err != nil {
		t.Fatal(err)
	}
	if len(conds) != 3 || conds[0].value != `a "quoted" b` || !conds[0].str ||
		conds[1].path.attr != "emails" || conds[1].path.sub != "value" || len(conds[1].path.filter) != 1 ||
		conds[1].path.filter[0].value != "work" || conds[2].value != "true" || conds[2].str {
		t.Fatalf("parsed: %+v", conds)
	}
	for in, want := range map[string]path{
		"userName":       {attr: "username"},
		"name.givenName": {attr: "name", sub: "givenname"},
		"urn:ietf:params:scim:schemas:core:2.0:User:name.familyName":                {attr: "name", sub: "familyname"},
		"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:employeeNumber": {attr: "urn:ietf:params:scim:schemas:extension:enterprise:2.0:user:employeenumber", ext: true},
	} {
		got, err := parsePath(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parsePath(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	p, err := parsePath(`members[value eq "1]2"]`)
	if err != nil || p.attr != "members" || p.filter[0].value != "1]2" {
		t.Errorf("bracket in a quoted value: %+v %v", p, err)
	}
	for _, bad := range []string{"", "a.b.c", "emails[type eq \"work\"", "emails[type eq \"work\"]x", "a b"} {
		if _, err := parsePath(bad); err == nil {
			t.Errorf("parsePath(%q) accepted", bad)
		}
	}
}
