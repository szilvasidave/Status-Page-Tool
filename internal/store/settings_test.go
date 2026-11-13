package store

import (
	"strings"
	"testing"
	"time"
)

func TestSettingsDefaults(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		st, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "settings", *st, DefaultSettings())
	})
}

func fullSettings() Settings {
	return Settings{
		SiteName:             "IT Status",
		BaseURL:              "https://status.example.com",
		PublicAccess:         false,
		Timezone:             "Europe/Budapest",
		OIDCEnabled:          true,
		OIDCIssuer:           "https://login.example.com/tenant/v2.0",
		OIDCClientID:         "client-id",
		OIDCClientSecret:     "very secret",
		OIDCScopes:           "openid email",
		OIDCGroupsClaim:      "roles",
		OIDCButtonLabel:      "Sign in with Example",
		DirectorySyncEnabled: true,
		SMTPHost:             "smtp.example.com",
		SMTPPort:             465,
		SMTPSecurity:         "tls",
		SMTPUsername:         "mailer",
		SMTPPassword:         "p@ss word",
		SMTPFrom:             "IT Status <status@example.com>",
		LocalLoginEnabled:    true,
		MailMethod:           MailSMTP,
		GraphSender:          "status@example.com",
		SCIMTokenHash:        "0123abcd",
		SCIMTokenCreatedAt:   time.Unix(1700000000, 0).UTC(),
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		want := fullSettings()
		in := want
		in.BaseURL = " https://status.example.com// "
		check(t, s.SaveSettings(ctx, &in))
		equal(t, "input not modified", in.BaseURL, " https://status.example.com// ")

		got, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "settings", *got, want)

		// stored representation
		raw := map[string]string{}
		rows, err := s.DB().QueryContext(ctx, `SELECT key, value FROM settings`)
		check(t, err)
		for rows.Next() {
			var k, v string
			check(t, rows.Scan(&k, &v))
			raw[k] = v
		}
		rows.Close()
		equal(t, "number of keys", len(raw), len(settingFields))
		equal(t, "bool", raw["public_access"], "false")
		equal(t, "bool", raw["oidc_enabled"], "true")
		equal(t, "int", raw["smtp_port"], "465")
		equal(t, "base_url", raw["base_url"], "https://status.example.com")
		for _, k := range []string{"oidc_client_secret", "smtp_password"} {
			if !strings.HasPrefix(raw[k], "enc:v1:") || strings.Contains(raw[k], "secret") || strings.Contains(raw[k], "word") {
				t.Fatalf("%s is not stored encrypted: %q", k, raw[k])
			}
		}

		// saving again updates the existing rows; empty secrets stay empty
		in = want
		in.SiteName = "Changed"
		in.SMTPPassword = ""
		in.PublicAccess = true
		check(t, s.SaveSettings(ctx, &in))
		got, err = s.GetSettings(ctx)
		check(t, err)
		equal(t, "after second save", *got, in)
	})
}

func TestSettingsCache(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		st, err := s.GetSettings(ctx)
		check(t, err)
		st.SiteName = "mutated by caller"
		again, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "copy returned", again.SiteName, "Status")

		// a change behind the store's back is not visible while cached
		mustExec(t, s, `INSERT INTO settings (key, value) VALUES ('site_name', 'Direct')`)
		cached, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "cached", cached.SiteName, "Status")

		// ... but after the TTL
		s.settingsMu.Lock()
		s.settingsLoaded = time.Now().Add(-settingsTTL - time.Second)
		s.settingsMu.Unlock()
		fresh, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "reloaded", fresh.SiteName, "Direct")

		// SaveSettings invalidates the cache immediately
		fresh.SiteName = "Saved"
		check(t, s.SaveSettings(ctx, fresh))
		saved, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "after save", saved.SiteName, "Saved")
	})
}

func TestSettingsPartialAndInvalid(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		mustExec(t, s, `INSERT INTO settings (key, value) VALUES ('site_name', 'Only name'), ('public_access', 'maybe'),
			('smtp_port', 'abc'), ('oidc_client_secret', 'plain-text-secret'), ('smtp_password', 'enc:v1:not-valid'),
			('base_url', 'http://x.example/'), ('unknown_key', 'ignored')`)
		st, err := s.GetSettings(ctx)
		check(t, err)
		want := DefaultSettings()
		want.SiteName = "Only name"
		want.OIDCClientSecret = "plain-text-secret" // unencrypted values are read as-is
		want.SMTPPassword = ""                      // cannot be decrypted
		want.BaseURL = "http://x.example"
		equal(t, "settings", *st, want)
	})
}

func TestSettingsWrongKey(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		in := fullSettings()
		check(t, s.SaveSettings(ctx, &in))
		other := New(s.DB(), []byte("another key, also thirty-two by."))
		st, err := other.GetSettings(ctx)
		check(t, err)
		equal(t, "secret cleared", st.SMTPPassword, "")
		equal(t, "secret cleared", st.OIDCClientSecret, "")
		equal(t, "other values kept", st.SiteName, in.SiteName)

		bad := New(s.DB(), []byte("short"))
		wantError(t, "save with invalid key", bad.SaveSettings(ctx, &in))
		wantError(t, "save nil", s.SaveSettings(ctx, nil))
	})
}

// TestSettingsMailMethodUpgrade: installations from before Microsoft Graph
// mail (no mail_method stored) keep sending with their SMTP server; new
// installations default to Microsoft Graph.
func TestSettingsMailMethodUpgrade(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		st, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "default method", st.MailMethod, MailGraph)

		_, err = s.DB().ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('smtp_host', 'smtp.example.com')`)
		check(t, err)
		s.settingsMu.Lock()
		s.settingsCache = nil
		s.settingsMu.Unlock()
		st, err = s.GetSettings(ctx)
		check(t, err)
		equal(t, "method of an SMTP installation", st.MailMethod, MailSMTP)

		st.MailMethod = "bogus"
		check(t, s.SaveSettings(ctx, st))
		st, err = s.GetSettings(ctx)
		check(t, err)
		equal(t, "unknown method", st.MailMethod, MailGraph)
	})
}
