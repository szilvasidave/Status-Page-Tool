package db

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/config"
)

// TestDSN returns the PostgreSQL DSN for tests (STATUSPAGE_TEST_PG_DSN), or
// "" to skip PostgreSQL tests.
func testPGDSN() string { return os.Getenv("STATUSPAGE_TEST_PG_DSN") }

func TestMigrateSQLite(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, config.DriverSQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx); err != nil { // idempotent
		t.Fatal(err)
	}
	var fk int
	if err := d.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign keys not enabled: %v %d", err, fk)
	}
	checkSchemaVersion(t, d)
	checkBrandImages(t, d)
}

func TestMigratePostgres(t *testing.T) {
	dsn := testPGDSN()
	if dsn == "" {
		t.Skip("STATUSPAGE_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	d, err := Open(ctx, config.DriverPostgres, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// start from a clean schema
	if _, err := d.ExecContext(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	checkSchemaVersion(t, d)
	checkBrandImages(t, d)
}

// checkSchemaVersion checks that all migrations were recorded.
func checkSchemaVersion(t *testing.T, d *DB) {
	t.Helper()
	var v, n int
	if err := d.QueryRowContext(context.Background(), `SELECT MAX(version), COUNT(*) FROM schema_migrations`).Scan(&v, &n); err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion() || n != SchemaVersion() || SchemaVersion() < 4 {
		t.Fatalf("schema version %d with %d migrations recorded, SchemaVersion %d", v, n, SchemaVersion())
	}
}

// checkBrandImages stores and reads back binary data in brand_images
// (migration 4, {{BLOB}}): every byte value must survive.
func checkBrandImages(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()
	data := make([]byte, 512)
	for i := range data {
		data[i] = byte(i)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO brand_images (name, content_type, data, version, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"logo-light", "image/png", data, "0123456789abcdef", Now()); err != nil {
		t.Fatal(err)
	}
	var got []byte
	if err := d.QueryRowContext(ctx, `SELECT data FROM brand_images WHERE name = ?`, "logo-light").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("binary data changed: %d bytes, want %d", len(got), len(data))
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO brand_images (name, content_type, data, version, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"logo-light", "image/png", data, "x", Now()); err == nil {
		t.Fatal("brand image names must be unique")
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO brand_images (name, content_type, data, version, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"favicon", "image/png", nil, "x", Now()); err == nil {
		t.Fatal("brand image data must not be NULL")
	}
}

// TestDialect checks that every placeholder of every migration is replaced
// for both databases.
func TestDialect(t *testing.T) {
	for _, driver := range []string{config.DriverSQLite, config.DriverPostgres} {
		d := &DB{Driver: driver}
		for i, m := range migrations {
			if sql := d.dialect(m); strings.Contains(sql, "{{") || strings.Contains(sql, "}}") {
				t.Errorf("%s: migration %d has a placeholder left:\n%s", driver, i+1, sql)
			}
		}
	}
	if got := (&DB{Driver: config.DriverPostgres}).dialect("data {{BLOB}} NOT NULL"); got != "data BYTEA NOT NULL" {
		t.Errorf("PostgreSQL blob: %q", got)
	}
	if got := (&DB{Driver: config.DriverSQLite}).dialect("data {{BLOB}} NOT NULL"); got != "data BLOB NOT NULL" {
		t.Errorf("SQLite blob: %q", got)
	}
}

func TestRebind(t *testing.T) {
	got := rebind(config.DriverPostgres, "SELECT ? WHERE a = '?' AND b = ?")
	if got != "SELECT $1 WHERE a = '?' AND b = $2" {
		t.Fatal(got)
	}
	if rebind(config.DriverSQLite, "a = ?") != "a = ?" {
		t.Fatal("sqlite must not rebind")
	}
}
