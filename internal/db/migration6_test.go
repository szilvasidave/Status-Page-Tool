package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/config"
)

// Databases migrated by branch builds of migration 6 are repaired: the build
// of commit 500cb22 kept the rows of cleared alerts (integration_alerts.firing
// false) and had no reported_start; the builds up to commit 0447b5c had no
// incidents.automatic_notify. Migrating again changes nothing.
func TestMigration6BranchBuildRepair(t *testing.T) {
	run := func(t *testing.T, d *DB, build string) {
		ctx := context.Background()
		all := migrations
		migrations = all[:5]
		err := d.Migrate(ctx)
		migrations = all
		if err != nil {
			t.Fatal(err)
		}
		cut := func(s, old, new string) string {
			t.Helper()
			out := strings.Replace(s, old, new, 1)
			if out == s {
				t.Fatalf("migration 6 does not contain %q", old)
			}
			return out
		}
		old6 := cut(migrations[5], "ALTER TABLE incidents ADD COLUMN automatic_notify {{BOOL}} NOT NULL DEFAULT {{FALSE}};\n", "")
		if build == "500cb22" {
			old6 = cut(old6, "\treported_start BIGINT,\n", "")
			old6 = cut(old6, "\tsuppressed     {{BOOL}}", "\tfiring         {{BOOL}} NOT NULL,\n\tsuppressed     {{BOOL}}")
		}
		for _, s := range splitStatements(d.dialect(old6)) {
			if _, err := d.ExecContext(ctx, s); err != nil {
				t.Fatalf("%v\n%s", err, s)
			}
		}
		exec := func(q string, args ...any) {
			t.Helper()
			if _, err := d.ExecContext(ctx, d.dialect(q), args...); err != nil {
				t.Fatalf("%v\n%s", err, q)
			}
		}
		exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (6, ?)`, Now())
		exec(`INSERT INTO services (name, created_at) VALUES ('VPN', ?)`, Now())
		exec(`INSERT INTO incidents (title, status, started_at, created_at, updated_at, automatic) VALUES ('VPN down', 'investigating', ?, ?, ?, {{TRUE}})`, Now(), Now(), Now())
		exec(`INSERT INTO integrations (name, provider, token_hash, token_created_at, created_at) VALUES ('Prometheus', 'alertmanager', 'h', ?, ?)`, Now(), Now())
		if build == "500cb22" {
			exec(`INSERT INTO integration_alerts (integration_id, alert_key, service_id, incident_id, firing, impact, started_at, updated_at)
			      VALUES (1, 'firing', 1, 1, {{TRUE}}, 'major_outage', ?, ?), (1, 'cleared', 1, 1, {{FALSE}}, 'major_outage', ?, ?)`,
				Now(), Now(), Now(), Now())
		} else {
			exec(`INSERT INTO integration_alerts (integration_id, alert_key, service_id, incident_id, impact, reported_start, started_at, updated_at)
			      VALUES (1, 'firing', 1, 1, 'major_outage', ?, ?, ?)`, Now(), Now(), Now())
		}

		for range 2 {
			if err := d.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
		}
		checkSchemaVersion(t, d)
		for _, c := range []struct {
			table, column string
			want          bool
		}{
			{"integration_alerts", "firing", false},
			{"integration_alerts", "reported_start", true},
			{"incidents", "automatic_notify", true},
		} {
			if has, err := d.hasColumn(ctx, c.table, c.column); err != nil || has != c.want {
				t.Errorf("%s.%s: exists %v (%v), want %v", c.table, c.column, has, err, c.want)
			}
		}
		var keys string
		if err := d.QueryRowContext(ctx, `SELECT `+map[bool]string{true: "STRING_AGG(alert_key, ',')", false: "GROUP_CONCAT(alert_key)"}[d.IsPostgres()]+
			` FROM integration_alerts`).Scan(&keys); err != nil {
			t.Fatal(err)
		}
		if keys != "firing" {
			t.Errorf("alerts after the repair: %q, want only the firing one", keys)
		}
		// The current code's statements work on the repaired tables.
		exec(`INSERT INTO integration_alerts (integration_id, alert_key, service_id, incident_id, suppressed, impact, title, reported_start, started_at, updated_at)
		      VALUES (1, 'new', 1, 1, {{FALSE}}, 'degraded', '', NULL, ?, ?)`, Now(), Now())
		exec(`INSERT INTO incidents (title, status, started_at, created_at, updated_at, automatic, automatic_notify) VALUES ('x', 'investigating', ?, ?, ?, {{TRUE}}, {{TRUE}})`,
			Now(), Now(), Now())
		var notify bool
		if err := d.QueryRowContext(ctx, `SELECT automatic_notify FROM incidents WHERE id = 1`).Scan(&notify); err != nil || notify {
			t.Errorf("automatic_notify of an incident from before: %v, %v; want false", notify, err)
		}
	}
	for _, build := range []string{"500cb22", "0447b5c"} {
		t.Run(build, func(t *testing.T) {
			t.Run("sqlite", func(t *testing.T) {
				d, err := Open(context.Background(), config.DriverSQLite, filepath.Join(t.TempDir(), "t.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				run(t, d, build)
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
				run(t, d, build)
			})
		})
	}
}
