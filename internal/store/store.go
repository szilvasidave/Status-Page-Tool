// Package store contains all database access. Every SQL statement of the
// application lives in this package; other packages only use the methods
// of Store. See package db for the SQL portability rules (SQLite +
// PostgreSQL).
//
// Conventions used by all methods:
//   - a missing row is reported as ErrNotFound (also by Update*/Delete*
//     methods that are given an unknown id), other errors are wrapped with
//     context;
//   - times are stored as unix seconds and returned in UTC;
//   - names are ordered case-insensitively.
//
// The implementation is split by topic: settings.go, users.go, groups.go,
// services.go, incidents.go, maintenance.go, notifications.go, sessions.go,
// brand.go (logos and favicon).
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

// Store is safe for concurrent use.
type Store struct {
	db  *db.DB
	key []byte // secret key used to encrypt secret settings

	settingsMu     sync.Mutex
	settingsCache  *Settings // decoded settings, nil when not loaded
	settingsLoaded time.Time
}

// New creates a store. key is the 32 byte secret key from the config file.
func New(d *db.DB, key []byte) *Store { return &Store{db: d, key: key} }

// DB exposes the underlying database (used by tests and health checks).
func (s *Store) DB() *db.DB { return s.db }

// Ping checks database connectivity.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// ===================================================================== helpers

// scanner is implemented by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// queryAll runs a query and scans every row with scan.
func queryAll[T any](ctx context.Context, q db.Querier, scan func(scanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// notFound converts sql.ErrNoRows to ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// isConflict reports whether err is a PostgreSQL unique violation or
// deadlock caused by a concurrent transaction, so that retrying the
// transaction can succeed. (SQLite serialises writing transactions and never
// sees these races.)
func isConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "40P01")
}

// affected returns ErrNotFound when an UPDATE/DELETE did not touch any row.
func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// nowTime returns the current time with second precision (the precision of
// the database) in UTC.
func nowTime() time.Time { return time.Unix(db.Now(), 0).UTC() }

// fromUnix converts a stored timestamp to a UTC time.
func fromUnix(v int64) time.Time { return time.Unix(v, 0).UTC() }

// fromNullUnix converts a nullable stored timestamp.
func fromNullUnix(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromUnix(v.Int64)
	return &t
}

// truncate drops sub-second precision so that a struct written to the
// database equals the struct read back.
func truncate(t time.Time) time.Time { return fromUnix(t.Unix()) }

// timeArg returns a parameter for a nullable timestamp column.
func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

// nullString returns NULL for "" (used for UNIQUE nullable columns).
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullID returns NULL for id 0 (used for nullable foreign keys).
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// normEmail lower-cases and trims an e-mail address.
func normEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// uniqueIDs returns ids without duplicates and non-positive values, keeping
// the original order.
func uniqueIDs(ids []int64) []int64 {
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// inList returns "?, ?, ?" and the matching arguments for an IN (...) list.
// Callers must handle an empty ids slice themselves.
func inList(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", "), args
}

// displayNameSQL is the SQL expression for the display name of a user row
// with alias a (name, else e-mail, else username, else "").
func displayNameSQL(a string) string {
	return "COALESCE(NULLIF(" + a + ".name, ''), NULLIF(" + a + ".email, ''), " + a + ".username, '')"
}

// serviceOrderSQL orders services like the status page (alias sv).
const serviceOrderSQL = "LOWER(sv.category), sv.sort_order, LOWER(sv.name), sv.id"
