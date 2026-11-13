package auth

// Sign-in with SCIM provisioning: provisioned users are found by their user
// name, keep what provisioning set, and cannot sign in when deactivated; the
// middleware leaves /scim/ alone.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

func TestOIDCSignInProvisionedUser(t *testing.T) {
	ctx := context.Background()
	e := newOIDCEnv(t)
	const subscribers = "aaaaaaaa-0000-4000-8000-000000000001"
	p, _, err := e.store.CreateSCIMUser(ctx, store.SCIMUserData{UserName: "jane.doe@contoso.com", Email: "jane@contoso.com", Name: "Jane Doe", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.CreateSCIMGroup(ctx, store.SCIMGroupData{DisplayName: "Subscribers", ExternalID: subscribers, Members: []int64{p.ID}}); err != nil {
		t.Fatal(err)
	}

	// Another subject, no e-mail: the user name (preferred_username) finds
	// the provisioned user. The claim does not contain the provisioned
	// group; the membership stays.
	u := e.mustLogin(map[string]any{"sub": "pairwise-subject", "oid": "oid-jane", "preferred_username": "Jane.Doe@contoso.com",
		"name": "J. D.", "groups": []string{"claim-group"}})
	if u.ID != p.ID || e.userCount() != 1 {
		t.Fatalf("signed in as user %d (%d users), want the provisioned user %d", u.ID, e.userCount(), p.ID)
	}
	if u.Name != "Jane Doe" || u.Email != "jane@contoso.com" || u.PrincipalName != "jane.doe@contoso.com" || u.OIDCSubject != "pairwise-subject" {
		t.Errorf("user after sign-in: %+v", u)
	}
	if got := e.groupsOf(u.ID); !slices.Equal(got, []string{subscribers, "claim-group"}) {
		t.Errorf("groups = %v", got)
	}

	// Entra ID v1.0 tokens have the user name in upn.
	o := e.mustLogin(map[string]any{"sub": "other", "upn": "otto@contoso.com"})
	if o.PrincipalName != "otto@contoso.com" {
		t.Errorf("user name from upn: %+v", o)
	}
}

func TestOIDCSignInDeactivatedUser(t *testing.T) {
	ctx := context.Background()
	e := newOIDCEnv(t)
	p, _, err := e.store.CreateSCIMUser(ctx, store.SCIMUserData{UserName: "max@contoso.com", Email: "max@contoso.com", Active: false})
	if err != nil {
		t.Fatal(err)
	}
	e.idp.setUser(map[string]any{"sub": "sub-max", "preferred_username": "max@contoso.com", "groups": []string{"g"}})
	expectFailure(t, e.login("/"), "your account has been deactivated by your organisation's user provisioning")
	got, err := e.store.GetUser(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OIDCSubject != "" || got.LastLoginAt != nil || len(e.groupsOf(p.ID)) != 0 {
		t.Errorf("the refused sign-in changed the user: %+v, groups %v", got, e.groupsOf(p.ID))
	}
	if !strings.Contains(e.logBuf.String(), "deactivated by provisioning") {
		t.Errorf("refused sign-in not logged:\n%s", e.logBuf.String())
	}
}

// TestMiddlewareSCIMPassThrough: /scim/ requests get no cookies, no session
// and no CSRF check (the SCIM handler authenticates them itself).
func TestMiddlewareSCIMPassThrough(t *testing.T) {
	st, key := newTestStore(t)
	m := New(st, key, nil)
	called := false
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if FromContext(r.Context()) != nil || CSRFToken(r) != "" {
			t.Error("a SCIM request got an identity or a CSRF token")
		}
		w.WriteHeader(http.StatusCreated)
	}))
	req := httptest.NewRequest(http.MethodPost, "/scim/v2/Users", strings.NewReader(`{"userName":"x"}`))
	req.Header.Set("Origin", "https://evil.example") // no browser: no origin check either
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusCreated || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("called %v, status %d, cookies %v", called, rec.Code, rec.Result().Cookies())
	}

	// Other paths keep the CSRF protection.
	called = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scim", strings.NewReader("x")))
	if called || rec.Code != http.StatusForbidden {
		t.Fatalf("POST /scim without CSRF token: called %v, status %d", called, rec.Code)
	}
}
