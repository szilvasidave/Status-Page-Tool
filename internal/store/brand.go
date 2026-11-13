package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

// Names of the brand images (Admin → Branding).
const (
	BrandLogoLight = "logo-light" // logo for light mode
	BrandLogoDark  = "logo-dark"  // logo for dark mode (optional)
	BrandFavicon   = "favicon"
)

// brandSettingKeys maps each brand image to the setting that holds its
// version (Settings.LogoLight, LogoDark, Favicon).
var brandSettingKeys = map[string]string{
	BrandLogoLight: "brand_logo_light",
	BrandLogoDark:  "brand_logo_dark",
	BrandFavicon:   "brand_favicon",
}

// BrandImage is an uploaded logo or favicon.
type BrandImage struct {
	Name        string // BrandLogoLight, BrandLogoDark or BrandFavicon
	ContentType string
	Data        []byte
	Version     string // first 16 hex characters of the SHA-256 of Data
	UpdatedAt   time.Time
}

// brandVersion returns the version of image data: the first 16 hex
// characters of its SHA-256.
func brandVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// setBrandVersion stores the version setting of a brand image.
func setBrandVersion(ctx context.Context, tx *db.Tx, name, version string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, brandSettingKeys[name], version)
	return err
}

// SetBrandImage stores (inserts or replaces) the brand image name and its
// version setting in one transaction, invalidates the settings cache and
// returns the version. Unknown names, an empty content type and empty data
// are errors; the content itself is not checked (the web layer does that
// with theme.CheckImage).
func (s *Store) SetBrandImage(ctx context.Context, name, contentType string, data []byte) (version string, err error) {
	if _, ok := brandSettingKeys[name]; !ok {
		return "", fmt.Errorf("store brand image: unknown image %q", name)
	}
	if contentType == "" || len(data) == 0 {
		return "", errors.New("store brand image: no content type or no data")
	}
	version = brandVersion(data)
	err = s.db.InTx(ctx, func(tx *db.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO brand_images (name, content_type, data, version, updated_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (name) DO UPDATE SET content_type = excluded.content_type, data = excluded.data,
			   version = excluded.version, updated_at = excluded.updated_at`,
			name, contentType, data, version, db.Now()); err != nil {
			return err
		}
		return setBrandVersion(ctx, tx, name, version)
	})
	s.invalidateSettings()
	if err != nil {
		return "", fmt.Errorf("store brand image %s: %w", name, err)
	}
	return version, nil
}

// DeleteBrandImage removes the brand image name and clears its version
// setting in one transaction, and invalidates the settings cache. Removing
// an image that does not exist is not an error; an unknown name is.
func (s *Store) DeleteBrandImage(ctx context.Context, name string) error {
	if _, ok := brandSettingKeys[name]; !ok {
		return fmt.Errorf("remove brand image: unknown image %q", name)
	}
	err := s.db.InTx(ctx, func(tx *db.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM brand_images WHERE name = ?`, name); err != nil {
			return err
		}
		return setBrandVersion(ctx, tx, name, "")
	})
	s.invalidateSettings()
	if err != nil {
		return fmt.Errorf("remove brand image %s: %w", name, err)
	}
	return nil
}

// BrandImage returns the brand image name, or ErrNotFound when none is
// stored (or the name is unknown).
func (s *Store) BrandImage(ctx context.Context, name string) (*BrandImage, error) {
	img := &BrandImage{}
	var updated int64
	err := s.db.QueryRowContext(ctx,
		`SELECT name, content_type, data, version, updated_at FROM brand_images WHERE name = ?`, name).
		Scan(&img.Name, &img.ContentType, &img.Data, &img.Version, &updated)
	if err != nil {
		if err = notFound(err); errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("load brand image %s: %w", name, err)
	}
	img.UpdatedAt = fromUnix(updated)
	return img, nil
}
