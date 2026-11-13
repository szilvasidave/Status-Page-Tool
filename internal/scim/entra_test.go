package scim

// Replays of the requests Microsoft Entra ID sends, as documented in
// "Tutorial: Develop and plan provisioning for a SCIM endpoint in Microsoft
// Entra ID" (learn.microsoft.com/entra/identity/app-provisioning/
// use-scim-to-provision-users-and-groups) and "Known issues and resolutions
// with SCIM 2.0 protocol compliance" (…/application-provisioning-config-
// problem-scim-compatibility), with and without the aadOptscim062020 flag.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// filterPath returns path?filter=<filter> (plus extra query parameters),
// encoded like Entra ID does.
func filterPath(path, filter string, extra ...string) string {
	q := url.Values{"filter": {filter}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return path + "?" + q.Encode()
}

// Create User (request body exactly as documented).
const entraCreateUser = `{
    "schemas": [
        "urn:ietf:params:scim:schemas:core:2.0:User",
        "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"],
    "externalId": "0a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef",
    "userName": "Test_User_00aa00aa-bb11-cc22-dd33-44ee44ee44ee",
    "active": true,
    "emails": [{
        "primary": true,
        "type": "work",
        "value": "Test_User_11bb11bb-cc22-dd33-ee44-55ff55ff55ff@testuser.com"
    }],
    "meta": {
        "resourceType": "User"
    },
    "name": {
        "formatted": "givenName familyName",
        "familyName": "familyName",
        "givenName": "givenName"
    },
    "roles": []
}`

func TestEntraTestConnection(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		// "Test Connection" queries a random user; with the feature flag in
		// the Tenant URL the query string has an extra parameter.
		for _, path := range []string{
			filterPath("/scim/v2/Users", `userName eq "a1b2c3d4-0000-4000-8000-1234567890ab"`),
			"/scim/v2/Users?aadOptscim062020&filter=" + url.QueryEscape(`userName eq "a1b2c3d4-0000-4000-8000-1234567890ab"`),
			filterPath("/scim/v2/Groups", `displayName eq "a1b2c3d4"`, "excludedAttributes", "members"),
		} {
			r := e.do(http.MethodGet, path, "").expect(t, http.StatusOK)
			if r.total() != 0 || len(r.resources(t)) != 0 || r.raw == "" || !strings.Contains(r.raw, `"Resources":[]`) {
				t.Errorf("%s: want an empty ListResponse, got %s", path, r.raw)
			}
		}
	})
}

func TestEntraUserOperations(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		// Create User → 201 with id and meta.
		r := e.do(http.MethodPost, "/scim/v2/Users", entraCreateUser).expect(t, http.StatusCreated)
		id := r.str("id")
		if id == "" || r.str("userName") != "Test_User_00aa00aa-bb11-cc22-dd33-44ee44ee44ee" ||
			r.str("externalId") != "0a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef" || r.body["active"] != true {
			t.Fatalf("created user: %s", r.raw)
		}
		location := "https://status.example.com/scim/v2/Users/" + id
		if r.str("meta.resourceType") != "User" || r.str("meta.created") == "" || r.str("meta.lastModified") == "" ||
			r.str("meta.location") != location || r.header.Get("Location") != location {
			t.Errorf("meta / Location header: %s, %q", r.raw, r.header.Get("Location"))
		}
		emails, _ := r.body["emails"].([]any)
		if len(emails) != 1 || emails[0].(map[string]any)["value"] != "test_user_11bb11bb-cc22-dd33-ee44-55ff55ff55ff@testuser.com" {
			t.Errorf("emails: %v", r.body["emails"])
		}
		u, err := e.store.SCIMUser(ctx, mustID(t, id))
		if err != nil {
			t.Fatal(err)
		}
		if u.Name != "givenName familyName" || !u.Provisioned || !u.Active || u.Email != "test_user_11bb11bb-cc22-dd33-ee44-55ff55ff55ff@testuser.com" {
			t.Errorf("stored user: %+v", u)
		}

		// Create Duplicate User → 409.
		r = e.do(http.MethodPost, "/scim/v2/Users", entraCreateUser).expectError(t, http.StatusConflict, "uniqueness")
		if d := r.str("detail"); d != `A user with the user name "Test_User_00aa00aa-bb11-cc22-dd33-44ee44ee44ee" exists already.` {
			t.Errorf("detail of the conflict: %q", d)
		}

		// Get User / not found.
		e.do(http.MethodGet, "/scim/v2/Users/"+id, "").expect(t, http.StatusOK)
		e.do(http.MethodGet, "/scim/v2/Users/5171a35d82074e068ce2", "").expectError(t, http.StatusNotFound, "")
		e.do(http.MethodGet, "/scim/v2/Users/987654", "").expectError(t, http.StatusNotFound, "")

		// Get User by query, also by externalId (mailNickname), and zero
		// results.
		for _, f := range []string{
			`userName eq "Test_User_00aa00aa-bb11-cc22-dd33-44ee44ee44ee"`,
			`userName eq "test_user_00AA00AA-bb11-cc22-dd33-44ee44ee44ee"`, // userName is not case-sensitive
			`externalId eq "0a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef"`,
			`emails[type eq "work"].value eq "Test_User_11bb11bb-cc22-dd33-ee44-55ff55ff55ff@testuser.com"`,
			`id eq "` + id + `"`,
		} {
			r = e.do(http.MethodGet, filterPath("/scim/v2/Users", f), "").expect(t, http.StatusOK)
			if res := r.resources(t); r.total() != 1 || len(res) != 1 || res[0]["id"] != id {
				t.Errorf("filter %s: %s", f, r.raw)
			}
		}
		r = e.do(http.MethodGet, filterPath("/scim/v2/Users", `userName eq "non-existent user"`), "").expect(t, http.StatusOK)
		if r.total() != 0 || len(r.resources(t)) != 0 {
			t.Errorf("zero results: %s", r.raw)
		}

		// Update User [Multi-valued properties].
		r = e.do(http.MethodPatch, "/scim/v2/Users/"+id, `{
    "schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
    "Operations": [
            {
            "op": "Replace",
            "path": "emails[type eq \"work\"].value",
            "value": "updatedEmail@microsoft.com"
            },
            {
            "op": "Replace",
            "path": "name.familyName",
            "value": "updatedFamilyName"
            }
    ]
}`).expect(t, http.StatusOK)
		if emails, _ := r.body["emails"].([]any); len(emails) != 1 || emails[0].(map[string]any)["value"] != "updatedemail@microsoft.com" {
			t.Errorf("e-mail not updated: %s", r.raw)
		}

		// Update User [Single-valued properties]: the joining property.
		e.do(http.MethodPatch, "/scim/v2/Users/"+id, `{
    "schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
    "Operations": [{
        "op": "Replace",
        "path": "userName",
        "value": "5b50642d-79fc-4410-9e90-4c077cdd1a59@testuser.com"
    }]
}`).expect(t, http.StatusOK)
		r = e.do(http.MethodGet, filterPath("/scim/v2/Users", `userName eq "5b50642d-79fc-4410-9e90-4c077cdd1a59@testuser.com"`), "").expect(t, http.StatusOK)
		if r.total() != 1 {
			t.Errorf("user not found by the new user name: %s", r.raw)
		}

		// Disable User (aadOptscim062020: a boolean), still returned.
		r = e.do(http.MethodPatch, "/scim/v2/Users/"+id, `{
    "Operations": [
        {
            "op": "Replace",
            "path": "active",
            "value": false
        }
    ],
    "schemas": [
        "urn:ietf:params:scim:api:messages:2.0:PatchOp"
    ]
}`).expect(t, http.StatusOK)
		if r.body["active"] != false {
			t.Errorf("not disabled: %s", r.raw)
		}
		r = e.do(http.MethodGet, filterPath("/scim/v2/Users", `userName eq "5b50642d-79fc-4410-9e90-4c077cdd1a59@testuser.com"`), "").expect(t, http.StatusOK)
		if res := r.resources(t); len(res) != 1 || res[0]["active"] != false {
			t.Errorf("disabled user not returned with active=false: %s", r.raw)
		}

		// Without the flag Entra ID sends "True"/"False" strings.
		for _, v := range []struct {
			value string
			want  bool
		}{{`"True"`, true}, {`"False"`, false}, {`"true"`, true}} {
			r = e.do(http.MethodPatch, "/scim/v2/Users/"+id, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
"Operations":[{"op":"Replace","path":"active","value":`+v.value+`}]}`).expect(t, http.StatusOK)
			if r.body["active"] != v.want {
				t.Errorf("active %s: %s", v.value, r.raw)
			}
		}

		// Delete User → 204, then 404.
		e.do(http.MethodDelete, "/scim/v2/Users/"+id, "").expect(t, http.StatusNoContent)
		e.do(http.MethodGet, "/scim/v2/Users/"+id, "").expectError(t, http.StatusNotFound, "")
		e.do(http.MethodDelete, "/scim/v2/Users/"+id, "").expectError(t, http.StatusNotFound, "")
		e.do(http.MethodPatch, "/scim/v2/Users/"+id, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
"Operations":[{"op":"Replace","path":"active","value":true}]}`).expectError(t, http.StatusNotFound, "")
	})
}

// TestEntraUserAttributes: the provisioning example with null attributes,
// replacing several attributes with and without aadOptscim062020, and
// attributes the status page does not store.
func TestEntraUserAttributes(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		r := e.do(http.MethodPost, "/scim/v2/Users", `{
   "schemas":
   [
     "urn:ietf:params:scim:schemas:core:2.0:User",
     "urn:ietf:params:scim:schemas:extension:enterprise:2.0User"],
   "externalId":"jyoung",
   "userName":"jyoung@testuser.com",
   "active":true,
   "addresses":null,
   "displayName":"Joy Young",
   "emails": [
     {
       "type":"work",
       "value":"jyoung@Contoso.com",
       "primary":true}],
   "meta": {
     "resourceType":"User"},
    "name":{
     "familyName":"Young",
     "givenName":"Joy"},
   "phoneNumbers":null,
   "preferredLanguage":null,
   "title":null,
   "department":null,
   "manager":null}`).expect(t, http.StatusCreated)
		id := r.str("id")
		if r.str("displayName") != "Joy Young" || r.str("externalId") != "jyoung" {
			t.Fatalf("created: %s", r.raw)
		}
		e.do(http.MethodGet, filterPath("/scim/v2/Users", "externalId eq jyoung"), "").expect(t, http.StatusOK) // unquoted, as in the example

		// Replace several attributes, without the flag …
		r = e.do(http.MethodPatch, "/scim/v2/Users/"+id, `{
  "schemas": [
      "urn:ietf:params:scim:api:messages:2.0:PatchOp"
  ],
  "Operations": [
      {
          "op": "Replace",
          "path": "displayName",
          "value": "Pvlo"
      },
      {
          "op": "Replace",
          "path": "emails[type eq \"work\"].value",
          "value": "TestBcwqnm@test.microsoft.com"
      },
      {
          "op": "Replace",
          "path": "name.givenName",
          "value": "Gtfd"
      },
      {
          "op": "Replace",
          "path": "name.familyName",
          "value": "Pkqf"
      },
      {
          "op": "Replace",
          "path": "externalId",
          "value": "Eqpj"
      },
      {
          "op": "Replace",
          "path": "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:employeeNumber",
          "value": "Eqpj"
      }
  ]
}`).expect(t, http.StatusOK)
		if r.str("displayName") != "Pvlo" || r.str("externalId") != "Eqpj" || !strings.Contains(r.raw, "testbcwqnm@test.microsoft.com") {
			t.Errorf("replace without the flag: %s", r.raw)
		}

		// … and with it (a value object whose keys are paths).
		r = e.do(http.MethodPatch, "/scim/v2/Users/"+id, `{
  "schemas": [
      "urn:ietf:params:scim:api:messages:2.0:PatchOp"
  ],
  "Operations": [
      {
          "op": "replace",
          "path": "emails[type eq \"work\"].value",
          "value": "TestMhvaes@test.microsoft.com"
      },
      {
          "op": "replace",
          "value": {
              "displayName": "Bjfe",
              "name.givenName": "Kkom",
              "name.familyName": "Unua",
              "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:employeeNumber": "Aklq"
          }
      }
  ]
}`).expect(t, http.StatusOK)
		if r.str("displayName") != "Bjfe" || !strings.Contains(r.raw, "testmhvaes@test.microsoft.com") {
			t.Errorf("replace with the flag: %s", r.raw)
		}

		// Single-value attributes the status page does not store, both
		// styles, and the manager (enterprise extension): accepted, ignored.
		for _, body := range []string{
			`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Add","path":"nickName","value":"Babs"}]}`,
			`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"add","path":"nickName","value":"Babs"}]}`,
			`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Add","path":"manager","value":[{"$ref":"http://example.com/scim/Users/00aa00aa-bb11-cc22-dd33-44ee44ee44ee","value":"00aa00aa-bb11-cc22-dd33-44ee44ee44ee"}]}]}`,
			`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Add","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:manager","value":"42"}]}`,
			`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[
				{"op":"Replace","path":"title","value":"Engineer"},
				{"op":"Replace","path":"phoneNumbers[type eq \"work\"].value","value":"+1 555 0100"},
				{"op":"Replace","path":"addresses[type eq \"work\"].locality","value":"Seattle"},
				{"op":"Replace","path":"preferredLanguage","value":"en-US"},
				{"op":"Replace","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department","value":"IT"},
				{"op":"Remove","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:manager"}]}`,
		} {
			r = e.do(http.MethodPatch, "/scim/v2/Users/"+id, body).expect(t, http.StatusOK)
			if r.str("displayName") != "Bjfe" {
				t.Errorf("an ignored attribute changed the user: %s", r.raw)
			}
		}

		// Removing the address: the user name is an address, so it is used.
		r = e.do(http.MethodPatch, "/scim/v2/Users/"+id, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
"Operations":[{"op":"Remove","path":"emails[type eq \"work\"].value"}]}`).expect(t, http.StatusOK)
		u, err := e.store.SCIMUser(ctx, mustID(t, id))
		if err != nil {
			t.Fatal(err)
		}
		if u.Email != "jyoung@testuser.com" {
			t.Errorf("e-mail after removing the address: %q, want the user name", u.Email)
		}
	})
}

// Create Group (request body exactly as documented).
const entraCreateGroup = `{
    "schemas": ["urn:ietf:params:scim:schemas:core:2.0:Group", "http://schemas.microsoft.com/2006/11/ResourceManagement/ADSCIM/2.0/Group"],
    "externalId": "8aa1a0c0-c4c3-4bc0-b4a5-2ef676900159",
    "displayName": "displayName",
    "meta": {
        "resourceType": "Group"
    }
}`

func TestEntraGroupOperations(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		u1 := e.do(http.MethodPost, "/scim/v2/Users", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"one@contoso.com","active":true}`).
			expect(t, http.StatusCreated).str("id")
		u2 := e.do(http.MethodPost, "/scim/v2/Users", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"two@contoso.com","active":true}`).
			expect(t, http.StatusCreated).str("id")

		// Create Group: created with an empty members list.
		r := e.do(http.MethodPost, "/scim/v2/Groups", entraCreateGroup).expect(t, http.StatusCreated)
		id := r.str("id")
		if id == "" || r.str("displayName") != "displayName" || r.str("externalId") != "8aa1a0c0-c4c3-4bc0-b4a5-2ef676900159" ||
			!strings.Contains(r.raw, `"members":[]`) || r.str("meta.resourceType") != "Group" || r.str("meta.created") == "" ||
			r.header.Get("Location") != "https://status.example.com/scim/v2/Groups/"+id {
			t.Fatalf("created group: %s (Location %q)", r.raw, r.header.Get("Location"))
		}
		e.do(http.MethodPost, "/scim/v2/Groups", entraCreateGroup).expectError(t, http.StatusConflict, "uniqueness")

		// Get Group / by displayName, without members.
		r = e.do(http.MethodGet, "/scim/v2/Groups/"+id+"?excludedAttributes=members", "").expect(t, http.StatusOK)
		if _, has := r.body["members"]; has || r.str("displayName") != "displayName" {
			t.Errorf("get group without members: %s", r.raw)
		}
		r = e.do(http.MethodGet, filterPath("/scim/v2/Groups", `displayName eq "displayName"`, "excludedAttributes", "members"), "").expect(t, http.StatusOK)
		if res := r.resources(t); r.total() != 1 || len(res) != 1 || res[0]["id"] != id || res[0]["members"] != nil {
			t.Errorf("group by displayName: %s", r.raw)
		}
		r = e.do(http.MethodGet, filterPath("/scim/v2/Groups", `externalId eq "8AA1A0C0-C4C3-4BC0-B4A5-2EF676900159"`), "").expect(t, http.StatusOK)
		if r.total() != 1 {
			t.Errorf("group by externalId: %s", r.raw)
		}
		e.do(http.MethodGet, "/scim/v2/Groups/40734ae655284ad3abcc?excludedAttributes=members", "").expectError(t, http.StatusNotFound, "")

		// Update Group [Non-member attributes] → 204.
		e.do(http.MethodPatch, "/scim/v2/Groups/"+id, `{
    "schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
    "Operations": [{
        "op": "Replace",
        "path": "displayName",
        "value": "1879db59-3bdf-4490-ad68-ab880a269474updatedDisplayName"
    }]
}`).expect(t, http.StatusNoContent)
		r = e.do(http.MethodGet, filterPath("/scim/v2/Groups", `displayName eq "1879db59-3bdf-4490-ad68-ab880a269474updatedDisplayName"`), "").expect(t, http.StatusOK)
		if r.total() != 1 {
			t.Errorf("renamed group not found: %s", r.raw)
		}

		// Update Group [Add Members] → 204.
		addMembers := func(ids ...string) {
			t.Helper()
			var values []string
			for _, id := range ids {
				values = append(values, `{"$ref": null, "value": "`+id+`"}`)
			}
			e.do(http.MethodPatch, "/scim/v2/Groups/"+id, `{
    "schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
    "Operations": [{
        "op": "Add",
        "path": "members",
        "value": [`+strings.Join(values, ",")+`]
    }]
}`).expect(t, http.StatusNoContent)
		}
		members := func() []string {
			t.Helper()
			r := e.do(http.MethodGet, "/scim/v2/Groups/"+id, "").expect(t, http.StatusOK)
			list, ok := r.body["members"].([]any)
			if !ok {
				t.Fatalf("group without members attribute: %s", r.raw)
			}
			var out []string
			for _, m := range list {
				mm := m.(map[string]any)
				if mm["$ref"] != "https://status.example.com/scim/v2/Users/"+mm["value"].(string) {
					t.Errorf("member $ref: %v", mm)
				}
				out = append(out, mm["value"].(string))
			}
			return out
		}
		addMembers(u1)
		addMembers(u1, u2) // already a member: no error
		if got := members(); len(got) != 2 {
			t.Fatalf("members after adding: %v", got)
		}

		// Update Group [Remove Members], without the flag …
		e.do(http.MethodPatch, "/scim/v2/Groups/"+id, `{
  "schemas": [
      "urn:ietf:params:scim:api:messages:2.0:PatchOp"
  ],
  "Operations": [
      {
          "op": "Remove",
          "path": "members",
          "value": [
              {
                  "value": "`+u1+`"
              }
          ]
      }
  ]
}`).expect(t, http.StatusNoContent)
		if got := members(); len(got) != 1 || got[0] != u2 {
			t.Fatalf("members after removing %s: %v", u1, got)
		}
		// … and with it.
		e.do(http.MethodPatch, "/scim/v2/Groups/"+id, `{
  "schemas": [
      "urn:ietf:params:scim:api:messages:2.0:PatchOp"
  ],
  "Operations": [
      {
          "op": "remove",
          "path": "members[value eq \"`+u2+`\"]"
      }
  ]
}`).expect(t, http.StatusNoContent)
		if got := members(); len(got) != 0 {
			t.Fatalf("members after removing %s: %v", u2, got)
		}

		// Replace the members; several operations in one request.
		e.do(http.MethodPatch, "/scim/v2/Groups/"+id, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[
			{"op":"Replace","path":"members","value":[{"value":"`+u1+`"},{"value":"`+u2+`"}]},
			{"op":"Remove","path":"members","value":[{"value":"`+u1+`"}]},
			{"op":"Replace","value":{"id":"`+id+`","displayName":"Renamed again"}}]}`).expect(t, http.StatusNoContent)
		if got := members(); len(got) != 1 || got[0] != u2 {
			t.Fatalf("members after replace: %v", got)
		}
		g, err := e.store.SCIMGroup(ctx, mustID(t, id))
		if err != nil || g.Name != "Renamed again" || !g.Provisioned || g.LastSyncedAt == nil {
			t.Fatalf("stored group: %+v %v", g, err)
		}

		// Give the group a role and a service, then Delete Group: 204, the
		// group is gone for SCIM but keeps role and services.
		if err := e.store.UpdateGroup(ctx, g.ID, g.Name, store.RoleEditor); err != nil {
			t.Fatal(err)
		}
		svc := &store.Service{Name: "VPN", GroupIDs: []int64{g.ID}}
		if err := e.store.CreateService(ctx, svc); err != nil {
			t.Fatal(err)
		}
		e.do(http.MethodDelete, "/scim/v2/Groups/"+id, "").expect(t, http.StatusNoContent)
		e.do(http.MethodGet, "/scim/v2/Groups/"+id, "").expectError(t, http.StatusNotFound, "")
		e.do(http.MethodPatch, "/scim/v2/Groups/"+id, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
"Operations":[{"op":"Add","path":"members","value":[{"value":"`+u1+`"}]}]}`).expectError(t, http.StatusNotFound, "")
		kept, err := e.store.GetGroup(ctx, g.ID)
		if err != nil || kept.Role != store.RoleEditor || len(kept.ServiceIDs) != 1 || kept.MemberCount != 0 || kept.Provisioned {
			t.Fatalf("group after delete: %+v %v", kept, err)
		}

		// Assigning the group again in Entra ID restores it.
		r = e.do(http.MethodPost, "/scim/v2/Groups", entraCreateGroup).expect(t, http.StatusCreated)
		if r.str("id") != id {
			t.Errorf("the group was created anew (%s) instead of taken over (%s)", r.str("id"), id)
		}
		restored, _ := e.store.GetGroup(ctx, g.ID)
		if restored == nil || restored.Role != store.RoleEditor || !restored.Provisioned {
			t.Errorf("restored group: %+v", restored)
		}
	})
}
