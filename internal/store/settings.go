package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/secret"
	"github.com/szilvasidave/status-page-tool/internal/theme"
)

// settingsTTL is how long GetSettings serves the settings from memory.
const settingsTTL = 10 * time.Second

// settingField maps one Settings field to its key in the settings table.
type settingField struct {
	key    string
	secret bool // stored encrypted
	own    bool // written only by its own method (SetSCIMToken, SetBrandImage, …), never by SaveSettings
	encode func(st *Settings) string
	decode func(st *Settings, v string)
}

func strField(key string, p func(*Settings) *string) settingField {
	return settingField{
		key:    key,
		encode: func(st *Settings) string { return *p(st) },
		decode: func(st *Settings, v string) { *p(st) = v },
	}
}

func secretField(key string, p func(*Settings) *string) settingField {
	f := strField(key, p)
	f.secret = true
	return f
}

// ownField marks a setting that has its own writer: SaveSettings leaves it
// alone, so that saving the settings form never writes back an older value
// (e.g. a provisioning token or a logo that was replaced or deleted
// meanwhile).
func ownField(f settingField) settingField {
	f.own = true
	return f
}

func boolField(key string, p func(*Settings) *bool) settingField {
	return settingField{
		key:    key,
		encode: func(st *Settings) string { return strconv.FormatBool(*p(st)) },
		decode: func(st *Settings, v string) {
			if b, err := strconv.ParseBool(v); err == nil {
				*p(st) = b
			}
		},
	}
}

func timeField(key string, p func(*Settings) *time.Time) settingField {
	return settingField{
		key: key,
		encode: func(st *Settings) string {
			if t := *p(st); !t.IsZero() {
				return strconv.FormatInt(t.Unix(), 10)
			}
			return ""
		},
		decode: func(st *Settings, v string) {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				*p(st) = time.Unix(n, 0).UTC()
			}
		},
	}
}

func intField(key string, p func(*Settings) *int) settingField {
	return settingField{
		key:    key,
		encode: func(st *Settings) string { return strconv.Itoa(*p(st)) },
		decode: func(st *Settings, v string) {
			if n, err := strconv.Atoi(v); err == nil {
				*p(st) = n
			}
		},
	}
}

// settingFields lists every stored setting. Invalid stored values (e.g. a
// bool that does not parse) are ignored and the default is used instead.
var settingFields = []settingField{
	strField("site_name", func(s *Settings) *string { return &s.SiteName }),
	strField("base_url", func(s *Settings) *string { return &s.BaseURL }),
	boolField("public_access", func(s *Settings) *bool { return &s.PublicAccess }),
	strField("timezone", func(s *Settings) *string { return &s.Timezone }),

	boolField("oidc_enabled", func(s *Settings) *bool { return &s.OIDCEnabled }),
	strField("oidc_issuer", func(s *Settings) *string { return &s.OIDCIssuer }),
	strField("oidc_client_id", func(s *Settings) *string { return &s.OIDCClientID }),
	secretField("oidc_client_secret", func(s *Settings) *string { return &s.OIDCClientSecret }),
	strField("oidc_scopes", func(s *Settings) *string { return &s.OIDCScopes }),
	strField("oidc_groups_claim", func(s *Settings) *string { return &s.OIDCGroupsClaim }),
	strField("oidc_button_label", func(s *Settings) *string { return &s.OIDCButtonLabel }),

	boolField("local_login_enabled", func(s *Settings) *bool { return &s.LocalLoginEnabled }),

	strField("mail_method", func(s *Settings) *string { return &s.MailMethod }),
	strField("graph_sender", func(s *Settings) *string { return &s.GraphSender }),
	strField("smtp_host", func(s *Settings) *string { return &s.SMTPHost }),
	intField("smtp_port", func(s *Settings) *int { return &s.SMTPPort }),
	strField("smtp_security", func(s *Settings) *string { return &s.SMTPSecurity }),
	strField("smtp_username", func(s *Settings) *string { return &s.SMTPUsername }),
	secretField("smtp_password", func(s *Settings) *string { return &s.SMTPPassword }),
	strField("smtp_from", func(s *Settings) *string { return &s.SMTPFrom }),

	ownField(strField("scim_token_hash", func(s *Settings) *string { return &s.SCIMTokenHash })),
	ownField(timeField("scim_token_created_at", func(s *Settings) *time.Time { return &s.SCIMTokenCreatedAt })),

	strField("theme_mode", func(s *Settings) *string { return &s.ThemeMode }),
	strField("theme_accent_light", func(s *Settings) *string { return &s.ThemeAccentLight }),
	strField("theme_accent_dark", func(s *Settings) *string { return &s.ThemeAccentDark }),
	strField("theme_header_light", func(s *Settings) *string { return &s.ThemeHeaderLight }),
	strField("theme_header_dark", func(s *Settings) *string { return &s.ThemeHeaderDark }),
	boolField("theme_show_site_name", func(s *Settings) *bool { return &s.ThemeShowSiteName }),
	ownField(strField(brandSettingKeys[BrandLogoLight], func(s *Settings) *string { return &s.LogoLight })),
	ownField(strField(brandSettingKeys[BrandLogoDark], func(s *Settings) *string { return &s.LogoDark })),
	ownField(strField(brandSettingKeys[BrandFavicon], func(s *Settings) *string { return &s.Favicon })),
}

// normalizeTheme puts the branding settings into their canonical form:
// colours "#rrggbb" or "" (invalid colours become the default) and a known
// appearance mode (else theme.ModeAuto).
func normalizeTheme(st *Settings) {
	for _, c := range []*string{&st.ThemeAccentLight, &st.ThemeAccentDark, &st.ThemeHeaderLight, &st.ThemeHeaderDark} {
		*c, _ = theme.NormalizeHex(*c)
	}
	if !theme.ValidMode(st.ThemeMode) {
		st.ThemeMode = theme.ModeAuto
	}
}

// trimBaseURL removes surrounding spaces and trailing slashes.
func trimBaseURL(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }

// GetSettings returns the settings, using DefaultSettings() for keys that are
// not stored. Secret values are decrypted. The result may be cached in memory
// for a few seconds; SaveSettings invalidates the cache. The returned value
// is a copy that the caller may modify.
//
// A secret that cannot be decrypted (e.g. because the secret key in
// config.json was replaced) is returned as "" so that the rest of the
// settings keep working; an administrator then has to enter it again.
func (s *Store) GetSettings(ctx context.Context) (*Settings, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if s.settingsCache == nil || time.Since(s.settingsLoaded) > settingsTTL {
		st, err := s.loadSettings(ctx)
		if err != nil {
			return nil, err
		}
		s.settingsCache = st
		s.settingsLoaded = time.Now()
	}
	c := *s.settingsCache
	return &c, nil
}

func (s *Store) loadSettings(ctx context.Context) (*Settings, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	defer rows.Close()
	stored := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("load settings: %w", err)
		}
		stored[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}

	st := DefaultSettings()
	for _, f := range settingFields {
		v, ok := stored[f.key]
		if !ok {
			continue
		}
		if f.secret {
			plain, err := secret.Decrypt(s.key, v)
			if err != nil {
				plain = ""
			}
			v = plain
		}
		f.decode(&st, v)
	}
	st.BaseURL = trimBaseURL(st.BaseURL)
	if _, ok := stored["mail_method"]; !ok && st.SMTPHost != "" {
		// Installed before Microsoft Graph was added: keep using SMTP.
		st.MailMethod = MailSMTP
	}
	if st.MailMethod != MailSMTP {
		st.MailMethod = MailGraph
	}
	normalizeTheme(&st)
	return &st, nil
}

// SaveSettings stores all fields of st (secrets encrypted with the key) in
// one transaction and invalidates the settings cache. A trailing "/" is
// removed from BaseURL; the branding colours and mode are normalised like
// GetSettings does (callers validate them first). The provisioning token
// (SCIMTokenHash, SCIMTokenCreatedAt) and the versions of the brand images
// (LogoLight, LogoDark, Favicon) are not stored: only SetSCIMToken,
// SetBrandImage and DeleteBrandImage change them.
func (s *Store) SaveSettings(ctx context.Context, st *Settings) error {
	if st == nil {
		return fmt.Errorf("save settings: no settings given")
	}
	c := *st
	c.BaseURL = trimBaseURL(c.BaseURL)
	normalizeTheme(&c)
	values := make([]string, len(settingFields))
	for i, f := range settingFields {
		v := f.encode(&c)
		if f.secret {
			enc, err := secret.Encrypt(s.key, v)
			if err != nil {
				return fmt.Errorf("save settings: encrypt %s: %w", f.key, err)
			}
			v = enc
		}
		values[i] = v
	}
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		for i, f := range settingFields {
			if f.own {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO settings (key, value) VALUES (?, ?)
				 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, f.key, values[i]); err != nil {
				return fmt.Errorf("%s: %w", f.key, err)
			}
		}
		return nil
	})
	s.invalidateSettings()
	if err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	return nil
}

// invalidateSettings drops the cached settings, so that the next
// GetSettings reads them again.
func (s *Store) invalidateSettings() {
	s.settingsMu.Lock()
	s.settingsCache = nil
	s.settingsMu.Unlock()
}
