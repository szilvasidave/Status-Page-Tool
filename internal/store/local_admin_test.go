package store

import (
	"errors"
	"testing"
	"time"
)

func TestUpdateLocalAdmin(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		admin, err := s.CreateLocalAdmin(ctx, "admin", "Administrator", "hash1")
		check(t, err)
		other, err := s.CreateLocalAdmin(ctx, "backup", "", "hash2")
		check(t, err)
		sso := mustUser(t, s, "sub-1", "ann@example.com", "Ann")

		// The user name is stored in lower case; sign-in finds it in any case.
		check(t, s.UpdateLocalAdmin(ctx, admin.ID, "  IT-Admin ", " Ada Admin "))
		got, err := s.GetUserByUsername(ctx, "it-ADMIN")
		check(t, err)
		equal(t, "user name", got.Username, "it-admin")
		equal(t, "display name", got.Name, "Ada Admin")
		equal(t, "id", got.ID, admin.ID)
		equal(t, "password kept", got.PasswordHash, "hash1")
		_, err = s.GetUserByUsername(ctx, "admin")
		wantNotFound(t, "old user name", err)

		// Another local account's name is taken, regardless of case; the own
		// name (in another case) is not. An empty display name is allowed.
		if err := s.UpdateLocalAdmin(ctx, admin.ID, "BACKUP", "x"); !errors.Is(err, ErrUsernameTaken) {
			t.Fatalf("taken user name: %v", err)
		}
		check(t, s.UpdateLocalAdmin(ctx, admin.ID, "IT-ADMIN", ""))
		got, err = s.GetUser(ctx, admin.ID)
		check(t, err)
		equal(t, "after own name", [2]string{got.Username, got.Name}, [2]string{"it-admin", ""})
		got, err = s.GetUser(ctx, other.ID)
		check(t, err)
		equal(t, "other account unchanged", got.Username, "backup")

		// Only local administrator accounts can be changed.
		wantNotFound(t, "single sign-on user", s.UpdateLocalAdmin(ctx, sso.ID, "ann", "Ann"))
		wantNotFound(t, "unknown user", s.UpdateLocalAdmin(ctx, 999, "nobody", ""))
		wantError(t, "empty user name", s.UpdateLocalAdmin(ctx, admin.ID, " ", ""))
		got, err = s.GetUser(ctx, sso.ID)
		check(t, err)
		equal(t, "single sign-on user unchanged", [2]string{got.Username, got.Name}, [2]string{"", "Ann"})
	})
}

func TestCheckLocalLoginOff(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		st := DefaultSettings()
		want := func(what string, wantErr error) {
			t.Helper()
			if err := s.CheckLocalLoginOff(ctx, &st); !errors.Is(err, wantErr) {
				t.Fatalf("%s: got %v, want %v", what, err, wantErr)
			}
		}
		want("defaults", ErrSSONotConfigured)
		st.OIDCIssuer, st.OIDCClientID, st.OIDCClientSecret = "https://idp.example.com", "client", "secret"
		want("single sign-on configured but not enabled", ErrSSONotConfigured)
		st.OIDCEnabled, st.OIDCClientSecret = true, ""
		want("no client secret", ErrSSONotConfigured)
		st.OIDCClientSecret = "secret"
		want("no groups", ErrNoAdmin)

		g := mustGroup(t, s, "editors", "Editors")
		check(t, s.UpdateGroup(ctx, g.ID, g.Name, RoleEditor))
		want("only an editor group", ErrNoAdmin)
		check(t, s.UpdateGroup(ctx, g.ID, g.Name, RoleAdmin))
		want("administrator group", nil)
	})
}

func TestSetLocalLoginEnabled(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		admin, err := s.CreateLocalAdmin(ctx, "admin", "", "hash")
		check(t, err)
		sso := mustUser(t, s, "sub-1", "ann@example.com", "Ann")
		session := func(id string, userID int64) {
			t.Helper()
			check(t, s.CreateSession(ctx, &Session{ID: id, UserID: userID, ExpiresAt: time.Now().Add(time.Hour)}))
		}
		exists := func(id string) bool {
			t.Helper()
			_, err := s.GetSession(ctx, id, time.Now())
			if err != nil && !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
			return err == nil
		}
		enabled := func() bool {
			t.Helper()
			st, err := s.GetSettings(ctx)
			check(t, err)
			return st.LocalLoginEnabled
		}
		set := func(on, wantChanged bool) {
			t.Helper()
			changed, err := s.SetLocalLoginEnabled(ctx, on)
			check(t, err)
			equal(t, "changed", changed, wantChanged)
			equal(t, "enabled", enabled(), on) // the settings cache is refreshed
		}

		equal(t, "default", enabled(), true)
		set(true, false)
		session("admin-1", admin.ID)
		session("sso-1", sso.ID)

		// Turning it off signs out the local administrator only.
		set(false, true)
		equal(t, "sessions after turning off", [2]bool{exists("admin-1"), exists("sso-1")}, [2]bool{false, true})
		set(false, false)

		// Sessions from before do not come back when it is turned on again.
		session("admin-2", admin.ID)
		set(true, true)
		equal(t, "admin session after turning on", exists("admin-2"), false)

		// The other settings are kept, and SaveSettings stores the flag too.
		st, err := s.GetSettings(ctx)
		check(t, err)
		st.SiteName, st.LocalLoginEnabled = "Renamed", false
		check(t, s.SaveSettings(ctx, st))
		set(false, false)
		set(true, true)
		st, err = s.GetSettings(ctx)
		check(t, err)
		equal(t, "site name kept", st.SiteName, "Renamed")
	})
}
