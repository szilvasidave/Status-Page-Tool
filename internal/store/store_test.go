package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
)

// The tests run against SQLite (always) and against PostgreSQL when
// STATUSPAGE_TEST_PG_DSN is set, e.g.
//
//	STATUSPAGE_TEST_PG_DSN=postgres://user:pw@localhost:5432/test?sslmode=disable go test ./internal/store/
//
// The PostgreSQL database is wiped (DROP SCHEMA public CASCADE) before every
// test, so never point it at a database with real data.

var testKey = []byte("0123456789abcdef0123456789abcdef")

var ctx = context.Background()

// forEachDB runs fn as a subtest against every available database, each
// time with a freshly migrated, empty schema.
func forEachDB(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { fn(t, openSQLite(t)) })
	if dsn := os.Getenv("STATUSPAGE_TEST_PG_DSN"); dsn != "" {
		t.Run("postgres", func(t *testing.T) { fn(t, openPostgres(t, dsn)) })
	}
}

func openSQLite(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(ctx, config.DriverSQLite, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return New(d, testKey)
}

func openPostgres(t *testing.T, dsn string) *Store {
	t.Helper()
	d, err := db.Open(ctx, config.DriverPostgres, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.ExecContext(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return New(d, testKey)
}

// ------------------------------------------------------------------ fixtures

func mustService(t *testing.T, s *Store, name, category string, order int, groupIDs ...int64) *Service {
	t.Helper()
	svc := &Service{Name: name, Category: category, SortOrder: order, GroupIDs: groupIDs}
	if err := s.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	return svc
}

func mustGroup(t *testing.T, s *Store, externalID, name string) *Group {
	t.Helper()
	g, err := s.CreateGroup(ctx, externalID, name)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func mustUser(t *testing.T, s *Store, subject, email, name string) *User {
	t.Helper()
	u, err := s.UpsertOIDCUser(ctx, OIDCUser{Subject: subject, Email: email, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustExec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.DB().ExecContext(ctx, q, args...); err != nil {
		t.Fatal(err)
	}
}

// ------------------------------------------------------------------ asserts

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func equal[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got: %+v\nwant: %+v", what, got, want)
	}
}

func wantNotFound(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("%s: want ErrNotFound, got %v", what, err)
	}
}

func wantError(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want an error, got nil", what)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("%s: want a validation error, got ErrNotFound", what)
	}
}

// recent reports whether t is within a few seconds of now.
func recent(t time.Time) bool {
	d := time.Since(t)
	return d > -5*time.Second && d < 5*time.Second
}

func date(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

// ------------------------------------------------------------------ tests

func TestPing(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		check(t, s.Ping(ctx))
		if s.DB() == nil {
			t.Fatal("DB() returned nil")
		}
	})
}

func TestHelpers(t *testing.T) {
	equal(t, "uniqueIDs", uniqueIDs([]int64{3, 1, 3, 0, -1, 2, 1}), []int64{3, 1, 2})
	equal(t, "uniqueIDs(nil)", uniqueIDs(nil), []int64{})
	in, args := inList([]int64{1, 2, 3})
	equal(t, "inList", in, "?, ?, ?")
	equal(t, "inList args", args, []any{int64(1), int64(2), int64(3)})
	in, args = inList([]int64{7})
	equal(t, "inList single", in, "?")
	equal(t, "inList single args", args, []any{int64(7)})
	equal(t, "normEmail", normEmail("  Foo.Bar@Example.COM "), "foo.bar@example.com")
	equal(t, "nullString empty", nullString(""), any(nil))
	equal(t, "nullString", nullString("x"), any("x"))
	equal(t, "nullID 0", nullID(0), any(nil))
	equal(t, "nullID", nullID(5), any(int64(5)))
	ts := time.Date(2026, 1, 2, 3, 4, 5, 999, time.FixedZone("X", 3600))
	equal(t, "truncate", truncate(ts), time.Date(2026, 1, 2, 2, 4, 5, 0, time.UTC))
	equal(t, "timeArg nil", timeArg(nil), any(nil))
	equal(t, "timeArg", timeArg(&ts), any(ts.Unix()))
}

// TestBooleanColumns verifies that Go bool parameters and scans work for
// boolean columns on both databases (INTEGER on SQLite, BOOLEAN on
// PostgreSQL), including the schema defaults.
func TestBooleanColumns(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		mustExec(t, s, `INSERT INTO maintenances (title, starts_at, ends_at, created_at, updated_at) VALUES ('defaults', 1, 2, 0, 0)`)
		mustExec(t, s, `INSERT INTO maintenances (title, starts_at, ends_at, cancelled, notify, created_at, updated_at) VALUES ('params', 1, 2, ?, ?, 0, 0)`, true, false)
		rows, err := s.DB().QueryContext(ctx, `SELECT title, cancelled, notify, notified_start FROM maintenances ORDER BY id`)
		check(t, err)
		defer rows.Close()
		type row struct {
			title                    string
			cancelled, notify, start bool
		}
		var got []row
		for rows.Next() {
			var r row
			check(t, rows.Scan(&r.title, &r.cancelled, &r.notify, &r.start))
			got = append(got, r)
		}
		check(t, rows.Err())
		equal(t, "rows", got, []row{{"defaults", false, true, false}, {"params", true, false, false}})

		var n int
		check(t, s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenances WHERE cancelled = ? AND notify = ?`, true, false).Scan(&n))
		equal(t, "count", n, 1)
	})
}
