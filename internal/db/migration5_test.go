package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/config"
)

// Migration 5 clears the group names that only repeat the external id (the
// default of earlier versions), except those of provisioned groups, so that
// groups show their directory names; it adds users.app_roles.
func TestMigration5(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		d, err := Open(context.Background(), config.DriverSQLite, filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		checkMigration5(t, d)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := testPGDSN()
		if dsn == "" {
			t.Skip("STATUSPAGE_TEST_PG_DSN not set")
		}
		d, err := Open(context.Background(), config.DriverPostgres, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if _, err := d.ExecContext(context.Background(), "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
			t.Fatal(err)
		}
		checkMigration5(t, d)
	})
}

func checkMigration5(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()
	all := migrations
	migrations = all[:4]
	err := d.Migrate(ctx)
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	const guid = "8f3a1c2d-0000-4000-8000-00000000abcd"
	for _, g := range []struct {
		ext, name   string
		provisioned bool
	}{
		{guid, guid, false},             // recorded from a sign-in: cleared
		{"ops", "Ops Team", false},      // named by an administrator: kept
		{"kc-group", "kc-group", false}, // a Keycloak group name: cleared (shown the same)
		{"prov", "prov", true},          // named so by provisioning: kept
	} {
		if _, err := d.ExecContext(ctx, `INSERT INTO idp_groups (external_id, name, role, created_at, provisioned) VALUES (?, ?, '', ?, ?)`,
			g.ext, g.name, Now(), g.provisioned); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO users (email, name, oidc_subject, created_at) VALUES ('a@example.com', 'A', 'sub-a', ?)`, Now()); err != nil {
		t.Fatal(err)
	}

	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	checkSchemaVersion(t, d)
	want := map[string]string{guid: "", "ops": "Ops Team", "kc-group": "", "prov": "prov"}
	rows, err := d.QueryContext(ctx, `SELECT external_id, name, directory_name, COALESCE(directory_checked_at, directory_found_at) FROM idp_groups`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ext, name, dir string
		var checked sql.NullInt64
		if err := rows.Scan(&ext, &name, &dir, &checked); err != nil {
			t.Fatal(err)
		}
		n++
		if name != want[ext] || dir != "" || checked.Valid {
			t.Errorf("group %s: name %q, directory name %q, checked %v; want name %q", ext, name, dir, checked, want[ext])
		}
	}
	if err := rows.Err(); err != nil || n != len(want) {
		t.Fatalf("%d groups, %v", n, err)
	}
	var roles string
	if err := d.QueryRowContext(ctx, `SELECT app_roles FROM users WHERE oidc_subject = 'sub-a'`).Scan(&roles); err != nil || roles != "" {
		t.Errorf("app_roles = %q, %v; want empty", roles, err)
	}
}

// A database migrated by the build of commit c22fd87, whose migration 5 had
// no idp_groups.directory_found_at yet, gets the column (columnRepairs);
// migrating again changes nothing.
func TestMigration5BranchBuildRepair(t *testing.T) {
	run := func(t *testing.T, d *DB) {
		ctx := context.Background()
		all := migrations
		migrations = all[:4]
		err := d.Migrate(ctx)
		migrations = all
		if err != nil {
			t.Fatal(err)
		}
		old5 := strings.Replace(migrations[4], "ALTER TABLE idp_groups ADD COLUMN directory_found_at BIGINT;\n", "", 1)
		if old5 == migrations[4] {
			t.Fatal("migration 5 does not add directory_found_at")
		}
		for _, s := range splitStatements(d.dialect(old5)) {
			if _, err := d.ExecContext(ctx, s); err != nil {
				t.Fatalf("%v\n%s", err, s)
			}
		}
		if _, err := d.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (5, ?)`, Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := d.ExecContext(ctx, `SELECT directory_found_at FROM idp_groups`); err == nil {
			t.Fatal("the column exists before the repair")
		}
		for range 2 {
			if err := d.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := d.ExecContext(ctx, `SELECT directory_found_at FROM idp_groups`); err != nil {
				t.Fatalf("after Migrate: %v", err)
			}
		}
		checkSchemaVersion(t, d)
	}
	t.Run("sqlite", func(t *testing.T) {
		d, err := Open(context.Background(), config.DriverSQLite, filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		run(t, d)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := testPGDSN()
		if dsn == "" {
			t.Skip("STATUSPAGE_TEST_PG_DSN not set")
		}
		d, err := Open(context.Background(), config.DriverPostgres, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if _, err := d.ExecContext(context.Background(), "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
			t.Fatal(err)
		}
		run(t, d)
	})
}
