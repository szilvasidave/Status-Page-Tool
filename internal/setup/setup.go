// Package setup performs the first-run configuration: it checks the database
// connection, creates the tables and writes config.json. It is shared by the
// "statuspage setup" command (used by the Windows installer) and the web
// setup wizard.
//
// All errors returned by this package are meant to be shown to end users.
package setup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
)

// DefaultSQLiteFile is the SQLite database file name inside the data
// directory used when no path is given.
const DefaultSQLiteFile = "statuspage.db"

// DefaultPostgresPort is used when no PostgreSQL port is given.
const DefaultPostgresPort = 5432

// SSLModes are the accepted PostgreSQL SSL modes.
var SSLModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// Params are the settings collected by the installer or the setup wizard.
type Params struct {
	Driver     string                `json:"driver"`                // "sqlite" or "postgres"
	SQLitePath string                `json:"sqlite_path,omitempty"` // optional; default <data>/statuspage.db
	Postgres   config.PostgresParams `json:"postgres"`
	// Listen is the web server address. Empty keeps the address of an
	// existing config.json, or ":8080" for a new one.
	Listen string `json:"listen,omitempty"`
}

// normalized returns a copy of p with whitespace trimmed and defaults
// applied.
func (p Params) normalized() Params {
	p.Driver = strings.ToLower(strings.TrimSpace(p.Driver))
	if p.Driver == "postgresql" || p.Driver == "pgsql" {
		p.Driver = config.DriverPostgres
	}
	p.SQLitePath = strings.TrimSpace(p.SQLitePath)
	p.Listen = strings.TrimSpace(p.Listen)
	if p.Listen == "" {
		p.Listen = config.DefaultListen
	} else if _, err := strconv.Atoi(p.Listen); err == nil {
		p.Listen = ":" + p.Listen // a bare port number
	}

	pg := &p.Postgres
	pg.Host = strings.TrimSpace(pg.Host)
	if strings.Contains(pg.Host, ":") && !strings.HasPrefix(pg.Host, "[") {
		pg.Host = "[" + pg.Host + "]" // IPv6 address
	}
	pg.Database = strings.TrimSpace(pg.Database)
	pg.User = strings.TrimSpace(pg.User)
	pg.SSLMode = strings.ToLower(strings.TrimSpace(pg.SSLMode))
	if pg.Port == 0 {
		pg.Port = DefaultPostgresPort
	}
	if pg.SSLMode == "" {
		pg.SSLMode = "prefer"
	}
	return p
}

// Validate checks that p is complete. It does not connect to anything.
func (p Params) Validate() error {
	p = p.normalized()
	if err := validateListen(p.Listen); err != nil {
		return err
	}
	switch p.Driver {
	case config.DriverSQLite:
		return nil
	case config.DriverPostgres:
		pg := p.Postgres
		switch {
		case pg.Host == "":
			return errors.New("please enter the PostgreSQL server name")
		case pg.Port < 1 || pg.Port > 65535:
			return fmt.Errorf("the PostgreSQL port must be a number between 1 and 65535 (got %d)", pg.Port)
		case pg.Database == "":
			return errors.New("please enter the name of the PostgreSQL database")
		case pg.User == "":
			return errors.New("please enter the PostgreSQL user name")
		case !slices.Contains(SSLModes, pg.SSLMode):
			return fmt.Errorf("unknown PostgreSQL SSL mode %q (use one of: %s)", pg.SSLMode, strings.Join(SSLModes, ", "))
		}
		return nil
	case "":
		return errors.New("please choose a database type (SQLite or PostgreSQL)")
	default:
		return fmt.Errorf("unsupported database type %q (use \"sqlite\" or \"postgres\")", p.Driver)
	}
}

func validateListen(listen string) error {
	_, port, err := net.SplitHostPort(listen)
	if err == nil {
		var n int
		n, err = strconv.Atoi(port)
		if err == nil && (n < 1 || n > 65535) {
			err = errors.New("out of range")
		}
	}
	if err != nil {
		return fmt.Errorf("invalid listen address %q: use a port such as \":8080\" or an address and port such as \"127.0.0.1:8080\"", listen)
	}
	return nil
}

// Check validates p and tests the database connection: for PostgreSQL it
// connects and verifies that tables can be created, for SQLite it verifies
// that the database folder exists or can be created and is writable (and that
// an existing database file can be opened). It writes nothing.
func Check(ctx context.Context, dataDir string, p Params) error {
	if err := p.Validate(); err != nil {
		return err
	}
	p = p.normalized()
	if p.Driver == config.DriverSQLite {
		return checkSQLite(ctx, sqlitePath(dataDir, p.SQLitePath))
	}
	d, err := openPostgres(ctx, p.Postgres)
	if err != nil {
		return err
	}
	defer d.Close()
	return checkPostgresSchema(ctx, d, p.Postgres)
}

// Apply checks p, creates or upgrades all tables and writes config.json in
// dataDir. A new secret key is generated unless config.json already exists,
// in which case its secret key, TLS and log level settings (and its listen
// address, unless p.Listen is set) are kept, so that encrypted settings stay
// readable. Returns the written configuration.
func Apply(ctx context.Context, dataDir string, p Params) (*config.Config, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	keepListen := strings.TrimSpace(p.Listen) == ""
	p = p.normalized()

	old, err := existingConfig(dataDir)
	if err != nil {
		return nil, err
	}
	cfg := &config.Config{Listen: p.Listen, DBDriver: p.Driver, SecretKey: config.NewSecretKey()}
	if old != nil {
		if _, err := old.Key(); err == nil {
			cfg.SecretKey = old.SecretKey
		}
		cfg.TLSCert, cfg.TLSKey, cfg.LogLevel = old.TLSCert, old.TLSKey, old.LogLevel
		if keepListen && old.Listen != "" {
			cfg.Listen = old.Listen
		}
	}
	if err := config.EnsureDataDir(dataDir); err != nil {
		return nil, err
	}

	var d *db.DB
	if p.Driver == config.DriverSQLite {
		path := sqlitePath(dataDir, p.SQLitePath)
		if err := checkSQLite(ctx, path); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("could not create the folder %s: %v", filepath.Dir(path), pathErr(err))
		}
		if d, err = db.Open(ctx, config.DriverSQLite, path); err != nil {
			return nil, fmt.Errorf("could not create the SQLite database %s: %v", path, err)
		}
		cfg.DBDSN = sqliteDSN(dataDir, path)
	} else {
		if d, err = openPostgres(ctx, p.Postgres); err != nil {
			return nil, err
		}
		if err := checkPostgresSchema(ctx, d, p.Postgres); err != nil {
			d.Close()
			return nil, err
		}
		cfg.DBDSN = p.Postgres.DSN()
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("could not create the database tables: %v", err)
	}
	if err := cfg.Save(dataDir); err != nil {
		return nil, fmt.Errorf("could not write the configuration file %s: %v", config.Path(dataDir), pathErr(err))
	}
	// The installer's hint for setup mode is no longer needed.
	_ = os.Remove(filepath.Join(dataDir, config.SetupListenFile))
	return cfg, nil
}

// existingConfig reads config.json without validating it (nil if missing).
func existingConfig(dataDir string) (*config.Config, error) {
	path := config.Path(dataDir)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read the existing configuration file %s: %v", path, pathErr(err))
	}
	var c config.Config
	if err := json.Unmarshal(config.TrimBOM(b), &c); err != nil {
		return nil, fmt.Errorf("the existing configuration file %s is damaged (%v); fix or delete it and try again", path, err)
	}
	return &c, nil
}

// ------------------------------------------------------------------ SQLite

// sqlitePath resolves the database file path; relative paths are relative to
// the data directory.
func sqlitePath(dataDir, p string) string {
	switch {
	case p == "":
		return filepath.Join(dataDir, DefaultSQLiteFile)
	case filepath.IsAbs(p):
		return filepath.Clean(p)
	default:
		return filepath.Join(dataDir, p)
	}
}

// sqliteDSN returns the value stored in config.json: the path relative to the
// data directory when the file is inside it, otherwise the absolute path.
func sqliteDSN(dataDir, path string) string {
	absDir, err1 := filepath.Abs(dataDir)
	absPath, err2 := filepath.Abs(path)
	if err1 != nil || err2 != nil {
		return path
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return absPath
	}
	return rel
}

func checkSQLite(ctx context.Context, path string) error {
	fi, err := os.Stat(path)
	switch {
	case err == nil && fi.IsDir():
		return fmt.Errorf("%s is a folder, not a database file", path)
	case err == nil:
		// An existing database (e.g. re-running the setup): make sure it
		// really is a SQLite database we can read.
		d, err := db.Open(ctx, config.DriverSQLite, path)
		if err == nil {
			var n int
			err = d.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&n)
			d.Close()
		}
		if err != nil {
			return fmt.Errorf("could not open the existing SQLite database %s: %v", path, err)
		}
	case !notExist(err):
		return fmt.Errorf("cannot access %s: %v", path, pathErr(err))
	}
	return checkWritableDir(filepath.Dir(path))
}

// checkWritableDir verifies that dir exists and is writable or, if it does
// not exist yet, that its nearest existing parent folder is writable.
func checkWritableDir(dir string) error {
	existing := dir
	for {
		fi, err := os.Stat(existing)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("cannot use the folder %s: %s is a file, not a folder", dir, existing)
			}
			break
		}
		if !notExist(err) {
			return fmt.Errorf("cannot access the folder %s: %v", existing, pathErr(err))
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return fmt.Errorf("the folder %s cannot be created", dir)
		}
		existing = parent
	}
	f, err := os.CreateTemp(existing, ".statuspage-write-test-*")
	if err != nil {
		return fmt.Errorf("the folder %s is not writable: %v", existing, pathErr(err))
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}

// notExist reports whether err means that a path does not exist, including
// "not a directory" errors for paths below a file.
func notExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// pathErr strips the operation and path from file system errors, which are
// usually already part of our message.
func pathErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// ------------------------------------------------------------------ PostgreSQL

func openPostgres(ctx context.Context, pg config.PostgresParams) (*db.DB, error) {
	d, err := db.Open(ctx, config.DriverPostgres, pg.DSN())
	if err != nil {
		return nil, postgresError(pg, err)
	}
	return d, nil
}

// postgresError turns a connection error into a short, readable message.
func postgresError(pg config.PostgresParams, err error) error {
	addr := pg.Host + ":" + strconv.Itoa(pg.Port)
	prefix := "could not connect to PostgreSQL at " + addr + ": "

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "3D000": // invalid_catalog_name
			return fmt.Errorf("the database %q does not exist on the PostgreSQL server at %s. Please create it first, for example: CREATE DATABASE %s OWNER %s;",
				pg.Database, addr, quoteIdent(pg.Database), quoteIdent(pg.User))
		case "28000": // invalid_authorization_specification, e.g. pg_hba.conf
			return errors.New(prefix + pgErr.Message + " (the server's pg_hba.conf must allow connections from this computer)")
		}
		return errors.New(prefix + pgErr.Message)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return fmt.Errorf("%sthe server name %q was not found", prefix, strings.Trim(pg.Host, "[]"))
		}
		return fmt.Errorf("%scould not resolve the server name %q: %v", prefix, strings.Trim(pg.Host, "[]"), dnsErr.Err)
	}
	var netErr net.Error
	if pgconn.Timeout(err) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return errors.New(prefix + "the connection timed out (check the server name, the port and firewalls)")
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		msg := opErr.Err.Error()
		if strings.Contains(strings.ToLower(msg), "refused") {
			return errors.New(prefix + "connection refused (is PostgreSQL running and listening on this address and port?)")
		}
		return errors.New(prefix + msg)
	}

	// Fall back to the first line of the driver's message without the
	// "failed to connect to `user=... database=...`" prefix.
	msg := err.Error()
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) && errors.Unwrap(connErr) != nil {
		msg = errors.Unwrap(connErr).Error()
	}
	msg, _, _ = strings.Cut(strings.TrimSpace(msg), "\n")
	if strings.Contains(msg, "x509:") {
		msg += ` (the server certificate could not be verified; use SSL mode "require" to encrypt without verifying the certificate)`
	}
	return errors.New(prefix + msg)
}

// checkPostgresSchema verifies that the user may create tables, unless the
// tables are already up to date.
func checkPostgresSchema(ctx context.Context, d *db.DB, pg config.PostgresParams) error {
	var schema sql.NullString
	var canCreate sql.NullBool
	err := d.QueryRowContext(ctx, `SELECT current_schema(), has_schema_privilege(current_schema(), 'CREATE')`).Scan(&schema, &canCreate)
	if err != nil {
		return fmt.Errorf("could not check the permissions of user %q in database %q: %v", pg.User, pg.Database, err)
	}
	if !schema.Valid {
		return fmt.Errorf("the PostgreSQL user %q has no schema to create tables in (check the search_path setting of the user and the database %q)", pg.User, pg.Database)
	}
	if canCreate.Bool || schemaUpToDate(ctx, d) {
		return nil
	}
	return fmt.Errorf("the PostgreSQL user %q is not allowed to create tables in schema %q of database %q. Make the user the owner of the database (ALTER DATABASE %s OWNER TO %s;) or grant the permission (GRANT CREATE ON SCHEMA %s TO %s;)",
		pg.User, schema.String, pg.Database, quoteIdent(pg.Database), quoteIdent(pg.User), quoteIdent(schema.String), quoteIdent(pg.User))
}

func schemaUpToDate(ctx context.Context, d *db.DB) bool {
	var exists bool
	if err := d.QueryRowContext(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		return false
	}
	var v int
	err := d.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return err == nil && v >= db.SchemaVersion()
}

var plainIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// quoteIdent quotes a PostgreSQL identifier for the SQL examples in error
// messages when needed.
func quoteIdent(s string) string {
	if plainIdent.MatchString(s) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
