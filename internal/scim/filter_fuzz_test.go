package scim

import (
	"net/http"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FuzzParseFilter: no filter makes the parser panic, and the values of an
// accepted filter are valid UTF-8 without control characters (they are
// compared in the database; PostgreSQL rejects NUL and invalid UTF-8).
func FuzzParseFilter(f *testing.F) {
	for _, s := range []string{
		`userName eq "jane@contoso.com"`,
		`externalId eq "jdoe" and emails[type eq "work"].value eq "jane@contoso.com"`,
		`id eq "1" and manager eq "2"`,
		`id eq 1 and members.value eq 2`,
		`displayName eq x and externalId eq "a \"b\" c"`,
		`members[value eq "1"]`,
		"0\"\\",
		`userName eq "x" and a"\`,
		`userName eq "x\u0000"`,
		"userName eq x\xff",
	} {
		f.Add(s)
	}
	valid := func(v string) bool { return utf8.ValidString(v) && !strings.ContainsFunc(v, unicode.IsControl) }
	f.Fuzz(func(t *testing.T, s string) {
		conds, err := parseFilter(s)
		if err != nil {
			return
		}
		for _, c := range conds {
			if !valid(c.value) {
				t.Fatalf("parseFilter(%q) accepted the value %q", s, c.value)
			}
		}
		if q, _, err := userQuery(conds); err == nil && !(valid(q.UserName) && valid(q.ExternalID) && valid(q.Email)) {
			t.Fatalf("userQuery(%q) = %+v", s, q)
		}
		if q, _, err := groupQuery(conds); err == nil && !(valid(q.DisplayName) && valid(q.ExternalID)) {
			t.Fatalf("groupQuery(%q) = %+v", s, q)
		}
	})
}

// TestMalformedFilters: broken filters and values that cannot be stored are
// answered with 400 invalidFilter, not with a server error (or a panic).
func TestMalformedFilters(t *testing.T) {
	forEachDB(t, func(t *testing.T, e *env) {
		for _, path := range []string{
			filterPath("/scim/v2/Users", "0\"\\"),
			filterPath("/scim/v2/Users", `userName eq "x" and a"\`),
			filterPath("/scim/v2/Groups", "0\"\\"),
			filterPath("/scim/v2/Users", `userName eq "x\u0000"`),
			filterPath("/scim/v2/Users", `externalId eq "a\u0000b"`),
			filterPath("/scim/v2/Users", `emails.value eq "a\u0000@b.c"`),
			filterPath("/scim/v2/Users", `emails[type eq "wo\u0000rk"].value eq "a@b.c"`),
			"/scim/v2/Users?filter=userName%20eq%20x%FF",
			"/scim/v2/Users?filter=userName%20eq%20x%00",
			filterPath("/scim/v2/Groups", `displayName eq "x\u0000"`),
			filterPath("/scim/v2/Groups", `externalId eq "x\u0000"`),
			"/scim/v2/Groups?filter=displayName%20eq%20x%FF",
		} {
			t.Run(path, func(t *testing.T) {
				e.do(http.MethodGet, path, "").expectError(t, http.StatusBadRequest, "invalidFilter")
			})
		}
	})
}
