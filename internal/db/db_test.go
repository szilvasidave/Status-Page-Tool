package db

import (
	"context"
	"os"
	"path/filepath"
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
