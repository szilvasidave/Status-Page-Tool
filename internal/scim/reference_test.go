package scim

import (
	"net/http"
	"testing"
)

// TestReferenceChecks: Microsoft Entra ID may check the value of a reference
// attribute before changing it (use-scim-to-provision-users-and-groups,
// "Query the value of a reference attribute"): the manager of a user, which
// is not stored (no user matches, so the change is sent and ignored), and
// the members of a group (answered with the real memberships).
func TestReferenceChecks(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		u1 := e.do(http.MethodPost, "/scim/v2/Users", `{"userName":"one@contoso.com"}`).expect(t, http.StatusCreated).str("id")
		u2 := e.do(http.MethodPost, "/scim/v2/Users", `{"userName":"two@contoso.com"}`).expect(t, http.StatusCreated).str("id")
		g := e.do(http.MethodPost, "/scim/v2/Groups", `{"displayName":"G","externalId":"g","members":[{"value":"`+u1+`"}]}`).
			expect(t, http.StatusCreated).str("id")

		for _, f := range []string{
			`id eq "` + u1 + `" and manager eq "` + u2 + `"`,
			`id eq ` + u1 + ` and manager eq ` + u2,
			`id eq "` + u1 + `" and manager.value eq "` + u2 + `"`,
			`id eq "` + u1 + `" and urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:manager eq "` + u2 + `"`,
		} {
			if r := e.do(http.MethodGet, filterPath("/scim/v2/Users", f, "attributes", "id"), "").expect(t, http.StatusOK); r.total() != 0 {
				t.Errorf("users %s: %s", f, r.raw)
			}
		}
		// Other attributes that are not stored stay unsupported.
		e.do(http.MethodGet, filterPath("/scim/v2/Users", `id eq "`+u1+`" and title eq "Boss"`), "").expectError(t, http.StatusBadRequest, "invalidFilter")

		groups := func(f string) int {
			t.Helper()
			return e.do(http.MethodGet, filterPath("/scim/v2/Groups", f, "excludedAttributes", "members"), "").expect(t, http.StatusOK).total()
		}
		for f, want := range map[string]int{
			`id eq "` + g + `" and members eq "` + u1 + `"`:                       1,
			`id eq "` + g + `" and members.value eq "` + u1 + `"`:                 1,
			`id eq ` + g + ` and members eq ` + u1:                                1,
			`urn:ietf:params:scim:schemas:core:2.0:Group:members eq "` + u1 + `"`: 1,
			`id eq "` + g + `" and members eq "` + u2 + `"`:                       0,
			`members eq "` + u1 + `" and members eq "` + u2 + `"`:                 0,
			`members eq "not-a-user"`:                                             0,
		} {
			if got := groups(f); got != want {
				t.Errorf("groups %s: %d, want %d", f, got, want)
			}
		}
		e.do(http.MethodPatch, "/scim/v2/Groups/"+g, `{"Operations":[{"op":"Add","path":"members","value":[{"value":"`+u2+`"}]}]}`).expect(t, http.StatusNoContent)
		if got := groups(`members eq "` + u1 + `" and members eq "` + u2 + `"`); got != 1 {
			t.Errorf("both members: %d", got)
		}
		e.do(http.MethodGet, filterPath("/scim/v2/Groups", `members[value eq "`+u1+`"]`), "").expectError(t, http.StatusBadRequest, "invalidFilter")
	})
}
