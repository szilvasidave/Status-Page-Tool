package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Tests of the provisioning token: writes of requests whose token was
// deleted or replaced meanwhile are refused, turning provisioning off waits
// for running writes, and saving the settings form never writes the token.

// TestSCIMWritesCheckToken: a provisioning write whose request was
// authenticated before the token was deleted or replaced fails with
// ErrProvisioningOff and changes nothing.
func TestSCIMWritesCheckToken(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		check(t, s.SetSCIMToken(ctx, "hash-1", time.Now()))
		u := mustSCIMUser(t, s, SCIMUserData{UserName: "u@contoso.com", Active: true})
		g := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "G", ExternalID: "g", Members: []int64{u.ID}})
		admins := mustGroup(t, s, "11111111-0000-4000-8000-000000000001", "Admins")
		check(t, s.UpdateGroup(ctx, admins.ID, "Admins", RoleAdmin))
		signedIn, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-bob", PrincipalName: "bob@contoso.com"})
		check(t, err)

		writes := map[string]func(c context.Context) error{
			"create user": func(c context.Context) error {
				_, _, err := s.CreateSCIMUser(c, SCIMUserData{UserName: "bob@contoso.com", Active: true})
				return err
			},
			"update user": func(c context.Context) error {
				_, err := s.UpdateSCIMUser(c, u.ID, replaceUser(SCIMUserData{UserName: "u@contoso.com", Active: false}))
				return err
			},
			"create group": func(c context.Context) error {
				_, _, err := s.CreateSCIMGroup(c, SCIMGroupData{DisplayName: "Admins", ExternalID: admins.ExternalID})
				return err
			},
			"update group": func(c context.Context) error {
				_, err := s.UpdateSCIMGroup(c, g.ID, func(d *SCIMGroupData) error { d.Members = nil; return nil })
				return err
			},
			"delete group": func(c context.Context) error { return s.DeleteSCIMGroup(c, g.ID) },
		}

		// The token was replaced: requests with the old one are refused,
		// requests with the new one work.
		check(t, s.SetSCIMToken(ctx, "hash-2", time.Now()))
		for name, write := range writes {
			if err := write(WithSCIMToken(ctx, "hash-1")); !errors.Is(err, ErrProvisioningOff) {
				t.Errorf("%s with the replaced token: %v, want ErrProvisioningOff", name, err)
			}
		}
		_, err = s.UpdateSCIMGroup(WithSCIMToken(ctx, "hash-2"), g.ID, func(*SCIMGroupData) error { return nil })
		check(t, err)

		// Provisioning was turned off: nothing is provisioned again.
		check(t, s.SetSCIMToken(ctx, "", time.Time{}))
		for name, write := range writes {
			if err := write(WithSCIMToken(ctx, "hash-2")); !errors.Is(err, ErrProvisioningOff) {
				t.Errorf("%s after turning provisioning off: %v, want ErrProvisioningOff", name, err)
			}
		}
		for _, id := range []int64{g.ID, admins.ID} {
			got, err := s.GetGroup(ctx, id)
			check(t, err)
			if got.Provisioned {
				t.Errorf("group %q provisioned with provisioning off", got.Name)
			}
		}
		for _, id := range []int64{u.ID, signedIn.ID} {
			got, err := s.GetUser(ctx, id)
			check(t, err)
			if got.Provisioned || !got.Active {
				t.Errorf("user %d changed with provisioning off: %+v", id, got)
			}
		}
		gs, err := s.GetGroup(ctx, g.ID)
		check(t, err)
		equal(t, "members kept", gs.MemberCount, 1)

		// Writes without a request token (tests, tools) are not checked.
		_, err = s.UpdateSCIMGroup(ctx, admins.ID, func(*SCIMGroupData) error { return nil })
		wantNotFound(t, "group that is not provisioned", err)
		_, _, err = s.CreateSCIMGroup(ctx, SCIMGroupData{DisplayName: "Unchecked"})
		check(t, err)
	})
}

// TestSCIMTurnOffWaitsForWrites: turning provisioning off while a
// provisioning write is running (between its read and its write) waits for
// it, so the write cannot leave the group provisioned afterwards.
func TestSCIMTurnOffWaitsForWrites(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		check(t, s.SetSCIMToken(ctx, "hash-1", time.Now()))
		bob, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: "s-bob", PrincipalName: "bob@contoso.com"})
		check(t, err)
		g := mustSCIMGroup(t, s, SCIMGroupData{DisplayName: "Admins", ExternalID: "admins", Members: []int64{bob.ID}})
		check(t, s.UpdateGroup(ctx, g.ID, "Admins", RoleAdmin))

		turnedOff := make(chan error, 1)
		var offErr error
		done := false
		_, err = s.UpdateSCIMGroup(WithSCIMToken(ctx, "hash-1"), g.ID, func(*SCIMGroupData) error {
			go func() { turnedOff <- s.SetSCIMToken(ctx, "", time.Time{}) }()
			select {
			case offErr = <-turnedOff:
				done = true
				t.Error("provisioning was turned off during a provisioning write")
			case <-time.After(300 * time.Millisecond):
			}
			return nil
		})
		check(t, err)
		if !done {
			select {
			case offErr = <-turnedOff:
			case <-time.After(15 * time.Second):
				t.Fatal("turning provisioning off does not finish")
			}
		}
		check(t, offErr)
		st, err := s.GetSettings(ctx)
		check(t, err)
		got, err := s.GetGroup(ctx, g.ID)
		check(t, err)
		equal(t, "provisioning off, group no longer provisioned", [2]bool{st.SCIMEnabled(), got.Provisioned}, [2]bool{false, false})

		// The groups claim manages the group again: Bob, who was removed
		// from it, loses the Administrator role at his next sign-in.
		check(t, s.SetUserGroups(ctx, bob.ID, nil))
		role, err := s.UserRole(ctx, bob.ID)
		check(t, err)
		equal(t, "role after sign-in", role, RoleViewer)
	})
}

// TestSaveSettingsKeepsSCIMToken: saving the settings with values read
// before the token was deleted or replaced keeps the current token.
func TestSaveSettingsKeepsSCIMToken(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		check(t, s.SetSCIMToken(ctx, "old-hash", time.Now()))
		form, err := s.GetSettings(ctx)
		check(t, err)

		check(t, s.SetSCIMToken(ctx, "", time.Time{}))
		form.SiteName = "Changed"
		check(t, s.SaveSettings(ctx, form))
		st, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "turned off, site name saved", [3]any{st.SCIMEnabled(), st.SCIMTokenCreatedAt.IsZero(), st.SiteName}, [3]any{false, true, "Changed"})

		created := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
		check(t, s.SetSCIMToken(ctx, "new-hash", created))
		check(t, s.SaveSettings(ctx, form)) // still holds "old-hash"
		st, err = s.GetSettings(ctx)
		check(t, err)
		equal(t, "replaced token kept", [2]any{st.SCIMTokenHash, st.SCIMTokenCreatedAt}, [2]any{"new-hash", created})
	})
}
