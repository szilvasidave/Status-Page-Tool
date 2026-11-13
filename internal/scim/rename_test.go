package scim

import (
	"net/http"
	"testing"
)

// TestRenameDerivedAddress: Microsoft Entra ID sends no e-mail address for
// users without the mail attribute (null attributes are skipped). The
// address derived from the user name then follows a rename of the user
// name (PATCH in both styles); an address that was sent does not.
func TestRenameDerivedAddress(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		email := func(id string) string {
			t.Helper()
			u, err := e.store.SCIMUser(ctx, mustID(t, id))
			if err != nil {
				t.Fatal(err)
			}
			return u.Email
		}
		const patch = `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":`

		// One operation per attribute (default).
		anna := e.do(http.MethodPost, "/scim/v2/Users", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],
			"externalId":"anna.schmidt","userName":"anna.schmidt@contoso.com","active":true,
			"displayName":"Anna Schmidt","name":{"givenName":"Anna","familyName":"Schmidt"}}`).expect(t, http.StatusCreated).str("id")
		if got := email(anna); got != "anna.schmidt@contoso.com" {
			t.Fatalf("derived address %q", got)
		}
		r := e.do(http.MethodPatch, "/scim/v2/Users/"+anna, patch+`[
			{"op":"Replace","path":"userName","value":"Anna.Meyer@contoso.com"},
			{"op":"Replace","path":"displayName","value":"Anna Meyer"},
			{"op":"Replace","path":"name.familyName","value":"Meyer"},
			{"op":"Replace","path":"externalId","value":"anna.meyer"}]}`).expect(t, http.StatusOK)
		if got := email(anna); got != "anna.meyer@contoso.com" {
			t.Errorf("address after the rename: %q", got)
		}
		if emails, _ := r.body["emails"].([]any); len(emails) != 1 || emails[0].(map[string]any)["value"] != "anna.meyer@contoso.com" {
			t.Errorf("response after the rename: %s", r.raw)
		}

		// Operations without path (aadOptscim062020).
		tom := e.do(http.MethodPost, "/scim/v2/Users", `{"userName":"tom.old@contoso.com","displayName":"Tom"}`).expect(t, http.StatusCreated).str("id")
		e.do(http.MethodPatch, "/scim/v2/Users/"+tom, patch+`[{"op":"replace","value":{"userName":"tom.new@contoso.com","displayName":"Tom New"}}]}`).
			expect(t, http.StatusOK)
		if got := email(tom); got != "tom.new@contoso.com" {
			t.Errorf("address after the rename without path: %q", got)
		}

		// An address that was sent stays, also when it equals the old user
		// name.
		max := e.do(http.MethodPost, "/scim/v2/Users", `{"userName":"max.old@contoso.com",
			"emails":[{"type":"work","primary":true,"value":"max.old@contoso.com"}]}`).expect(t, http.StatusCreated).str("id")
		e.do(http.MethodPatch, "/scim/v2/Users/"+max, patch+`[{"op":"Replace","path":"userName","value":"max.new@contoso.com"}]}`).expect(t, http.StatusOK)
		if got := email(max); got != "max.old@contoso.com" {
			t.Errorf("sent address after the rename: %q", got)
		}
		e.do(http.MethodPatch, "/scim/v2/Users/"+tom, patch+`[{"op":"Replace","path":"emails[type eq \"work\"].value","value":"tom@mail.contoso.com"}]}`).
			expect(t, http.StatusOK)
		e.do(http.MethodPatch, "/scim/v2/Users/"+tom, patch+`[{"op":"Replace","path":"userName","value":"tom.newer@contoso.com"}]}`).expect(t, http.StatusOK)
		if got := email(tom); got != "tom@mail.contoso.com" {
			t.Errorf("address sent later, after a rename: %q", got)
		}

		// Without the sent address it is derived again.
		e.do(http.MethodPatch, "/scim/v2/Users/"+tom, patch+`[{"op":"Remove","path":"emails[type eq \"work\"].value"}]}`).expect(t, http.StatusOK)
		e.do(http.MethodPatch, "/scim/v2/Users/"+tom, patch+`[{"op":"Replace","path":"userName","value":"tom.newest@contoso.com"}]}`).expect(t, http.StatusOK)
		if got := email(tom); got != "tom.newest@contoso.com" {
			t.Errorf("address after removing the sent one and a rename: %q", got)
		}
	})
}
