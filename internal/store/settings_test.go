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
		SiteName:           "IT Status",
		BaseURL:            "https://status.example.com",
		PublicAccess:       false,
		Timezone:           "Europe/Budapest",
		OIDCEnabled:        true,
		OIDCIssuer:         "https://login.example.com/tenant/v2.0",
		OIDCClientID:       "client-id",
		OIDCClientSecret:   "very secret",
		OIDCScopes:         "openid email",
		OIDCGroupsClaim:    "roles",
		OIDCButtonLabel:    "Sign in with Example",
		SMTPHost:           "smtp.example.com",
		SMTPPort:           465,
		SMTPSecurity:       "tls",
		SMTPUsername:       "mailer",
		SMTPPassword:       "p@ss word",
		SMTPFrom:           "IT Status <status@example.com>",
		LocalLoginEnabled:  true,
		MailMethod:         MailSMTP,
		GraphSender:        "status@example.com",
		SCIMTokenHash:      "0123abcd",
		SCIMTokenCreatedAt: time.Unix(1700000000, 0).UTC(),
		ThemeMode:          "dark",
		ThemeAccentLight:   "#ffd400",
		ThemeAccentDark:    "#002855",
		ThemeHeaderLight:   "#101820",
		ThemeHeaderDark:    "",
		ThemeShowSiteName:  false,
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		want := fullSettings()
		in := want
		in.BaseURL = " https://status.example.com// "
		in.ThemeAccentLight = " #FFD400 "
		in.ThemeHeaderLight = "101820"
		check(t, s.SetSCIMToken(ctx, want.SCIMTokenHash, want.SCIMTokenCreatedAt)) // not stored by SaveSettings
		for name, p := range map[string]*string{BrandLogoLight: &want.LogoLight, BrandLogoDark: &want.LogoDark, BrandFavicon: &want.Favicon} {
			v, err := s.SetBrandImage(ctx, name, "image/png", []byte(name)) // not stored by SaveSettings either
			check(t, err)
			*p = v
		}
		in.LogoLight = "stale version"
		check(t, s.SaveSettings(ctx, &in))
		equal(t, "input not modified", in.BaseURL, " https://status.example.com// ")
		equal(t, "input not modified", in.ThemeAccentLight, " #FFD400 ")

		got, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "settings", *got, want)

		// stored representation
		raw := rawSettings(t, s)
		equal(t, "number of keys", len(raw), len(settingFields))
		equal(t, "bool", raw["public_access"], "false")
		equal(t, "bool", raw["oidc_enabled"], "true")
		equal(t, "int", raw["smtp_port"], "465")
		equal(t, "base_url", raw["base_url"], "https://status.example.com")
		equal(t, "colour", raw["theme_accent_light"], "#ffd400")
		equal(t, "colour", raw["theme_header_light"], "#101820")
		equal(t, "mode", raw["theme_mode"], "dark")
		equal(t, "bool", raw["theme_show_site_name"], "false")
		equal(t, "logo version", raw["brand_logo_light"], want.LogoLight)
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

// rawSettings returns the stored settings as they are in the table.
func rawSettings(t *testing.T, s *Store) map[string]string {
	t.Helper()
	raw := map[string]string{}
	rows, err := s.DB().QueryContext(ctx, `SELECT key, value FROM settings`)
	check(t, err)
	defer rows.Close()
	for rows.Next() {
		var k, v string
		check(t, rows.Scan(&k, &v))
		raw[k] = v
	}
	check(t, rows.Err())
	return raw
}

// themeKeys are the keys that SaveTheme writes.
var themeKeys = map[string]bool{
	"theme_mode": true, "theme_accent_light": true, "theme_accent_dark": true,
	"theme_header_light": true, "theme_header_dark": true, "theme_show_site_name": true,
}

// TestSaveTheme: SaveTheme writes only the branding settings, so that
// saving the branding with an older copy of the settings does not undo
// changes made meanwhile (e.g. the local login switched off by the command
// line, a new site name or SMTP password, a replaced logo).
func TestSaveTheme(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		n := 0
		for _, f := range settingFields {
			if f.theme {
				n++
				if !themeKeys[f.key] || f.own || f.secret {
					t.Errorf("unexpected branding field %s", f.key)
				}
			}
		}
		equal(t, "number of branding fields", n, len(themeKeys))

		// Into an empty table: only the branding keys are written.
		before := rawSettings(t, s)
		in := fullSettings()
		check(t, s.SaveTheme(ctx, &in))
		after := rawSettings(t, s)
		for k, v := range after {
			if old, ok := before[k]; !themeKeys[k] && (!ok || old != v) {
				t.Errorf("SaveTheme wrote %s = %q", k, v)
			}
		}
		for k := range themeKeys {
			if _, ok := after[k]; !ok {
				t.Errorf("SaveTheme did not write %s", k)
			}
		}
		got, err := s.GetSettings(ctx)
		check(t, err)
		want := DefaultSettings()
		want.ThemeMode, want.ThemeAccentLight, want.ThemeAccentDark = in.ThemeMode, in.ThemeAccentLight, in.ThemeAccentDark
		want.ThemeHeaderLight, want.ThemeHeaderDark, want.ThemeShowSiteName = in.ThemeHeaderLight, in.ThemeHeaderDark, in.ThemeShowSiteName
		equal(t, "settings after the first SaveTheme", *got, want)

		// A stale copy, read before other settings were changed.
		stale, err := s.GetSettings(ctx)
		check(t, err)
		fresh := fullSettings()
		fresh.SiteName = "Fresh name"
		fresh.LocalLoginEnabled = false // e.g. switched off by the command line
		fresh.SMTPPassword = "fresh secret"
		check(t, s.SaveSettings(ctx, &fresh))
		check(t, s.SetSCIMToken(ctx, fresh.SCIMTokenHash, fresh.SCIMTokenCreatedAt))
		fresh.LogoLight, err = s.SetBrandImage(ctx, BrandLogoLight, "image/png", []byte("logo"))
		check(t, err)
		before = rawSettings(t, s)
		_, err = s.GetSettings(ctx) // cached now
		check(t, err)

		stale.ThemeMode = "light"
		stale.ThemeAccentLight = " #ABC "
		stale.ThemeAccentDark = "red" // invalid: the default
		stale.ThemeHeaderLight = ""
		stale.ThemeHeaderDark = " 0A0B0C "
		stale.ThemeShowSiteName = true
		stale.LogoLight = "stale version"
		check(t, s.SaveTheme(ctx, stale))
		equal(t, "input not modified", stale.ThemeAccentLight, " #ABC ")

		got, err = s.GetSettings(ctx) // the cache was invalidated
		check(t, err)
		want = fresh
		want.ThemeMode, want.ThemeAccentLight, want.ThemeAccentDark = "light", "#aabbcc", ""
		want.ThemeHeaderLight, want.ThemeHeaderDark, want.ThemeShowSiteName = "", "#0a0b0c", true
		equal(t, "settings after saving a stale copy", *got, want)

		// Nothing but the branding keys was written: the encrypted SMTP
		// password is byte for byte the same (it was not encrypted again).
		after = rawSettings(t, s)
		equal(t, "number of keys", len(after), len(before))
		for k, v := range before {
			if !themeKeys[k] {
				equal(t, "stored "+k, after[k], v)
			}
		}
		equal(t, "stored mode", after["theme_mode"], "light")
		equal(t, "stored colour", after["theme_accent_light"], "#aabbcc")
		equal(t, "stored colour", after["theme_accent_dark"], "")
		equal(t, "stored colour", after["theme_header_dark"], "#0a0b0c")
		equal(t, "stored bool", after["theme_show_site_name"], "true")

		// An unknown mode becomes the default, like with SaveSettings.
		stale.ThemeMode = "sepia"
		check(t, s.SaveTheme(ctx, stale))
		got, err = s.GetSettings(ctx)
		check(t, err)
		equal(t, "unknown mode", got.ThemeMode, "auto")

		wantError(t, "save nil", s.SaveTheme(ctx, nil))
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
			('base_url', 'http://x.example/'), ('unknown_key', 'ignored'),
			('theme_mode', 'sepia'), ('theme_accent_light', '#ABC'), ('theme_accent_dark', 'red'),
			('theme_header_light', '#12345'), ('theme_header_dark', ' 0A0B0C '), ('theme_show_site_name', 'perhaps')`)
		st, err := s.GetSettings(ctx)
		check(t, err)
		want := DefaultSettings()
		want.SiteName = "Only name"
		want.OIDCClientSecret = "plain-text-secret" // unencrypted values are read as-is
		want.SMTPPassword = ""                      // cannot be decrypted
		want.BaseURL = "http://x.example"
		want.ThemeAccentLight = "#aabbcc" // normalised
		want.ThemeHeaderDark = "#0a0b0c"
		equal(t, "settings", *st, want) // unknown mode → auto, invalid colours → default
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
