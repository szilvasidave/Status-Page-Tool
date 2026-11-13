package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// brandData returns n bytes with every byte value.
func brandData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func TestBrandImages(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		for _, name := range []string{BrandLogoLight, BrandLogoDark, BrandFavicon} {
			_, err := s.BrandImage(ctx, name)
			wantNotFound(t, "image before upload", err)
		}

		// Store: the version is the start of the SHA-256 and also a setting.
		data := brandData(300 << 10)
		sum := sha256.Sum256(data)
		v1, err := s.SetBrandImage(ctx, BrandLogoLight, "image/png", data)
		check(t, err)
		equal(t, "version", v1, hex.EncodeToString(sum[:])[:16])
		img, err := s.BrandImage(ctx, BrandLogoLight)
		check(t, err)
		if !bytes.Equal(img.Data, data) {
			t.Fatalf("image data changed: %d bytes, want %d", len(img.Data), len(data))
		}
		equal(t, "name", img.Name, BrandLogoLight)
		equal(t, "content type", img.ContentType, "image/png")
		equal(t, "version", img.Version, v1)
		if !recent(img.UpdatedAt) || img.UpdatedAt.Location().String() != "UTC" {
			t.Errorf("updated at %v", img.UpdatedAt)
		}
		st, err := s.GetSettings(ctx) // the cache was invalidated
		check(t, err)
		equal(t, "logo version setting", st.LogoLight, v1)
		equal(t, "other images", st.LogoDark+st.Favicon, "")

		// Replace: new content, type and version.
		svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)
		v2, err := s.SetBrandImage(ctx, BrandLogoLight, "image/svg+xml", svg)
		check(t, err)
		if v2 == v1 || len(v2) != 16 {
			t.Fatalf("version after replacing: %q (before %q)", v2, v1)
		}
		img, err = s.BrandImage(ctx, BrandLogoLight)
		check(t, err)
		equal(t, "replaced data", img.Data, svg)
		equal(t, "replaced type", img.ContentType, "image/svg+xml")
		equal(t, "replaced version", img.Version, v2)

		// The same content gives the same version.
		again, err := s.SetBrandImage(ctx, BrandFavicon, "image/svg+xml", svg)
		check(t, err)
		equal(t, "version of the same data", again, v2)

		dark, err := s.SetBrandImage(ctx, BrandLogoDark, "image/gif", []byte("GIF89a"))
		check(t, err)
		st, err = s.GetSettings(ctx)
		check(t, err)
		equal(t, "versions", []string{st.LogoLight, st.LogoDark, st.Favicon}, []string{v2, dark, v2})

		// Delete: the image and its version are gone; deleting again is fine.
		check(t, s.DeleteBrandImage(ctx, BrandLogoLight))
		_, err = s.BrandImage(ctx, BrandLogoLight)
		wantNotFound(t, "deleted image", err)
		check(t, s.DeleteBrandImage(ctx, BrandLogoLight))
		st, err = s.GetSettings(ctx)
		check(t, err)
		equal(t, "versions after delete", []string{st.LogoLight, st.LogoDark, st.Favicon}, []string{"", dark, v2})
		img, err = s.BrandImage(ctx, BrandFavicon)
		check(t, err)
		equal(t, "other image kept", img.Data, svg)
	})
}

func TestBrandImageErrors(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		_, err := s.SetBrandImage(ctx, "logo", "image/png", []byte("x"))
		wantError(t, "unknown name", err)
		_, err = s.SetBrandImage(ctx, BrandLogoLight, "", []byte("x"))
		wantError(t, "no content type", err)
		_, err = s.SetBrandImage(ctx, BrandLogoLight, "image/png", nil)
		wantError(t, "no data", err)
		wantError(t, "delete unknown name", s.DeleteBrandImage(ctx, "../settings"))
		_, err = s.BrandImage(ctx, "logo")
		wantNotFound(t, "unknown name", err)

		// Nothing was stored by the failed calls.
		st, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "settings", *st, DefaultSettings())
		var n int
		check(t, s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM brand_images`).Scan(&n))
		equal(t, "images", n, 0)
	})
}

// TestBrandVersionsNotSavedBySettings: saving an older copy of the settings
// (e.g. the branding form opened before another administrator replaced a
// logo) never brings back a removed or replaced image version.
func TestBrandVersionsNotSavedBySettings(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		old, err := s.GetSettings(ctx)
		check(t, err)
		v, err := s.SetBrandImage(ctx, BrandLogoLight, "image/png", []byte("logo"))
		check(t, err)
		_, err = s.SetBrandImage(ctx, BrandFavicon, "image/png", []byte("icon"))
		check(t, err)
		check(t, s.DeleteBrandImage(ctx, BrandFavicon))

		old.ThemeAccentLight = "#ffd400"
		old.Favicon = "0123456789abcdef" // stale
		check(t, s.SaveSettings(ctx, old))
		st, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "logo version", st.LogoLight, v)
		equal(t, "favicon version", st.Favicon, "")
		equal(t, "accent saved", st.ThemeAccentLight, "#ffd400")
	})
}

// TestThemeSettings: defaults, normalisation on save and ThemeColors.
func TestThemeSettings(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		st, err := s.GetSettings(ctx)
		check(t, err)
		equal(t, "default mode", st.ThemeMode, "auto")
		equal(t, "default show site name", st.ThemeShowSiteName, true)
		equal(t, "default colours", st.ThemeColors().Customized(), false)

		st.ThemeMode = "LIGHT"
		st.ThemeAccentLight = "1F5BD8"
		st.ThemeAccentDark = "not a colour"
		st.ThemeHeaderLight = "#fff"
		st.ThemeHeaderDark = ""
		st.ThemeShowSiteName = false
		check(t, s.SaveSettings(ctx, st))
		st, err = s.GetSettings(ctx)
		check(t, err)
		equal(t, "mode", st.ThemeMode, "auto") // unknown (case matters)
		c := st.ThemeColors()
		equal(t, "colours", []string{c.Mode, c.AccentLight, c.AccentDark, c.HeaderLight, c.HeaderDark},
			[]string{"auto", "#1f5bd8", "", "#ffffff", ""})
		equal(t, "show site name", st.ThemeShowSiteName, false)

		st.ThemeMode = "dark"
		check(t, s.SaveSettings(ctx, st))
		st, err = s.GetSettings(ctx)
		check(t, err)
		equal(t, "mode", st.ThemeColors().Mode, "dark")

		var nilSettings *Settings
		equal(t, "nil settings", nilSettings.ThemeColors().Mode, "auto")
	})
}

// TestBrandSettingKeys: every brand image has an own (not saved by
// SaveSettings) setting for its version.
func TestBrandSettingKeys(t *testing.T) {
	for name, key := range brandSettingKeys {
		found := false
		for _, f := range settingFields {
			if f.key == key {
				found = true
				if !f.own {
					t.Errorf("%s (%s) is saved by SaveSettings", key, name)
				}
			}
		}
		if !found || !strings.HasPrefix(key, "brand_") {
			t.Errorf("no setting %s for %s", key, name)
		}
	}
}
