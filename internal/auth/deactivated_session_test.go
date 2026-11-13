package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// A session of a deactivated user does not work and is deleted. Provisioning
// deletes the sessions a user has when it deactivates them, but a sign-in
// that is just finishing creates its session right afterwards.
func TestSessionOfDeactivatedUser(t *testing.T) {
	ctx := context.Background()
	st, key := newTestStore(t)
	m := New(st, key, nil)
	startSession := func(userID int64) *http.Cookie {
		t.Helper()
		rec := httptest.NewRecorder()
		if err := m.StartSession(ctx, rec, httptest.NewRequest(http.MethodGet, "/", nil), userID); err != nil {
			t.Fatal(err)
		}
		return rec.Result().Cookies()[0]
	}
	identity := func(c *http.Cookie) *Identity {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(c)
		return m.loadIdentity(r)
	}

	ann, _, err := st.CreateSCIMUser(ctx, store.SCIMUserData{UserName: "ann@contoso.com", Email: "ann@contoso.com", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if id := identity(startSession(ann.ID)); id == nil || id.User.ID != ann.ID {
		t.Fatalf("active user: %+v", id)
	}

	// The sign-in of a user that provisioning deactivated meanwhile.
	gone, _, err := st.CreateSCIMUser(ctx, store.SCIMUserData{UserName: "max@contoso.com", Email: "max@contoso.com", Active: false})
	if err != nil {
		t.Fatal(err)
	}
	c := startSession(gone.ID)
	if id := identity(c); id != nil {
		t.Fatalf("the deactivated user is signed in with role %q", id.Role)
	}
	if _, err := st.GetSession(ctx, hashToken(c.Value), time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the session of the deactivated user was not deleted: %v", err)
	}
}
