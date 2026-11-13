package db

import (
	"context"
	"database/sql"
	"path/filepath"
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
	rows, err := d.QueryContext(ctx, `SELECT external_id, name, directory_name, directory_checked_at FROM idp_groups`)
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
