// Package db opens the SQLite or PostgreSQL database and applies the schema.
//
// All SQL in this project is written with "?" placeholders and passed through
// DB.Rebind (or the Exec/Query/QueryRow wrappers below, which rebind
// automatically) so that the same statements work on both databases.
//
// Portability rules used throughout the code base:
//   - timestamps are stored as BIGINT unix seconds (UTC)
//   - booleans are BOOLEAN on PostgreSQL and INTEGER 0/1 on SQLite; always
//     pass Go bool values as parameters and scan into Go bool - never write
//     boolean literals in SQL
//   - new rows return their id with "INSERT ... RETURNING id"
//   - upserts use "INSERT ... ON CONFLICT (...) DO UPDATE/NOTHING"
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
	_ "modernc.org/sqlite"             // registers "sqlite"

	"github.com/szilvasidave/status-page-tool/internal/config"
)

type DB struct {
	*sql.DB
	Driver string // config.DriverSQLite or config.DriverPostgres
}

// Querier is implemented by *DB and *Tx.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Open opens and pings the database. For SQLite dsn is a file path.
func Open(ctx context.Context, driver, dsn string) (*DB, error) {
	var (
		sqlDB *sql.DB
		err   error
	)
	switch driver {
	case config.DriverSQLite:
		// Plain path (no "file:" prefix) so Windows paths work; the
		// query parameters are still applied by the driver.
		sqlDB, err = sql.Open("sqlite", dsn+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_txlock=immediate")
		if err == nil {
			sqlDB.SetMaxOpenConns(8)
		}
	case config.DriverPostgres:
		sqlDB, err = sql.Open("pgx", dsn)
		if err == nil {
			sqlDB.SetMaxOpenConns(10)
			sqlDB.SetConnMaxIdleTime(5 * time.Minute)
		}
	default:
		return nil, fmt.Errorf("unsupported database driver %q", driver)
	}
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return &DB{DB: sqlDB, Driver: driver}, nil
}

// OpenConfig opens the database described by cfg.
func OpenConfig(ctx context.Context, cfg *config.Config, dataDir string) (*DB, error) {
	dsn := cfg.DBDSN
	if cfg.DBDriver == config.DriverSQLite {
		dsn = cfg.SQLitePath(dataDir)
	}
	return Open(ctx, cfg.DBDriver, dsn)
}

// IsPostgres reports whether the database is PostgreSQL.
func (d *DB) IsPostgres() bool { return d.Driver == config.DriverPostgres }

// Rebind converts "?" placeholders to "$1, $2, ..." for PostgreSQL.
func (d *DB) Rebind(q string) string { return rebind(d.Driver, q) }

func rebind(driver, q string) string {
	if driver != config.DriverPostgres || !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 10)
	n := 0
	inStr := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == '\'' {
			inStr = !inStr
		}
		if c == '?' && !inStr {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ExecContext rebinds and executes.
func (d *DB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.DB.ExecContext(ctx, d.Rebind(q), args...)
}

// QueryContext rebinds and queries.
func (d *DB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return d.DB.QueryContext(ctx, d.Rebind(q), args...)
}

// QueryRowContext rebinds and queries a single row.
func (d *DB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return d.DB.QueryRowContext(ctx, d.Rebind(q), args...)
}

// Tx wraps sql.Tx with automatic rebinding.
type Tx struct {
	*sql.Tx
	driver string
}

func (t *Tx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, rebind(t.driver, q), args...)
}

func (t *Tx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(ctx, rebind(t.driver, q), args...)
}

func (t *Tx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, rebind(t.driver, q), args...)
}

// InTx runs fn inside a transaction, committing on success and rolling back
// on error or panic. Use only the provided Tx inside fn (never the outer DB),
// otherwise SQLite may wait on its own lock.
func (d *DB) InTx(ctx context.Context, fn func(tx *Tx) error) (err error) {
	sqlTx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	tx := &Tx{Tx: sqlTx, driver: d.Driver}
	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = sqlTx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return sqlTx.Commit()
}

// Now returns the current time as unix seconds, the format used for all
// timestamp columns.
func Now() int64 { return time.Now().Unix() }
