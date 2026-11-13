package setup

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
)

func TestValidate(t *testing.T) {
	pg := config.PostgresParams{Host: "db1", Database: "statuspage", User: "sp"}
	tests := []struct {
		name    string
		p       Params
		wantErr string
	}{
		{"sqlite defaults", Params{Driver: "sqlite"}, ""},
		{"sqlite upper case", Params{Driver: " SQLite "}, ""},
		{"postgres defaults", Params{Driver: "postgres", Postgres: pg}, ""},
		{"postgresql alias", Params{Driver: "postgresql", Postgres: pg}, ""},
		{"bare port", Params{Driver: "sqlite", Listen: "9090"}, ""},
		{"ip and port", Params{Driver: "sqlite", Listen: "127.0.0.1:9090"}, ""},
		{"no driver", Params{}, "choose a database type"},
		{"bad driver", Params{Driver: "mysql"}, `unsupported database type "mysql"`},
		{"bad listen", Params{Driver: "sqlite", Listen: "localhost"}, "invalid listen address"},
		{"port out of range", Params{Driver: "sqlite", Listen: ":70000"}, "invalid listen address"},
		{"no host", Params{Driver: "postgres", Postgres: config.PostgresParams{Database: "x", User: "y"}}, "server name"},
		{"no database", Params{Driver: "postgres", Postgres: config.PostgresParams{Host: "h", User: "y"}}, "name of the PostgreSQL database"},
		{"no user", Params{Driver: "postgres", Postgres: config.PostgresParams{Host: "h", Database: "x"}}, "user name"},
		{"bad pg port", Params{Driver: "postgres", Postgres: config.PostgresParams{Host: "h", Database: "x", User: "y", Port: 99999}}, "between 1 and 65535"},
		{"bad sslmode", Params{Driver: "postgres", Postgres: config.PostgresParams{Host: "h", Database: "x", User: "y", SSLMode: "on"}}, "SSL mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.p.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got error %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizedIPv6(t *testing.T) {
	p := Params{Driver: "postgres", Postgres: config.PostgresParams{Host: "::1"}}.normalized()
	if p.Postgres.Host != "[::1]" || p.Postgres.Port != 5432 || p.Postgres.SSLMode != "prefer" || p.Listen != ":8080" {
		t.Fatalf("unexpected normalisation: %+v", p)
	}
}

func TestCheckSQLiteWritesNothing(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "a", "b")
	if err := Check(context.Background(), dataDir, Params{Driver: "sqlite"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Check wrote %d entries into %s", len(entries), root)
	}
}

func TestCheckSQLiteErrors(t *testing.T) {
	dir := t.TempDir()

	// The database path is a folder.
	if err := os.Mkdir(filepath.Join(dir, "folder.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := Check(context.Background(), dir, Params{Driver: "sqlite", SQLitePath: "folder.db"})
	if err == nil || !strings.Contains(err.Error(), "is a folder") {
		t.Fatalf("got %v", err)
	}

	// An existing file that is not a database.
	if err := os.WriteFile(filepath.Join(dir, "junk.db"), []byte(strings.Repeat("this is not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Check(context.Background(), dir, Params{Driver: "sqlite", SQLitePath: "junk.db"})
	if err == nil || !strings.Contains(err.Error(), "could not open the existing SQLite database") {
		t.Fatalf("got %v", err)
	}

	// A parent "folder" that is a file.
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = Check(context.Background(), dir, Params{Driver: "sqlite", SQLitePath: "file/sub/x.db"})
	if err == nil || !strings.Contains(err.Error(), "is a file, not a folder") {
		t.Fatalf("got %v", err)
	}

	// A read-only folder.
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return // permissions are not enforced
	}
	ro := filepath.Join(dir, "readonly")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	err = Check(context.Background(), ro, Params{Driver: "sqlite"})
	if err == nil || !strings.Contains(err.Error(), "is not writable") {
		t.Fatalf("got %v", err)
	}
}

// TestApplySetupListenFile: "statuspage setup" without -listen, run after the
// installer's database setup failed, keeps the installer's port (which setup
// mode listens on and the firewall rule allows) instead of :8080 and removes
// the hint file; -listen takes precedence.
func TestApplySetupListenFile(t *testing.T) {
	for _, tt := range []struct{ hint, listen, want string }{
		{"\xef\xbb\xbf:9090\r\n", "", ":9090"},
		{"junk", "", config.DefaultListen},
		{":9090", ":9095", ":9095"},
	} {
		dataDir := t.TempDir()
		hint := filepath.Join(dataDir, config.SetupListenFile)
		if err := os.WriteFile(hint, []byte(tt.hint), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Apply(context.Background(), dataDir, Params{Driver: "sqlite", Listen: tt.listen})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Listen != tt.want {
			t.Errorf("hint %q, -listen %q: listen = %q, want %q", tt.hint, tt.listen, cfg.Listen, tt.want)
		}
		if _, err := os.Stat(hint); !os.IsNotExist(err) {
			t.Errorf("hint %q: %s not removed: %v", tt.hint, config.SetupListenFile, err)
		}
	}
}

func TestApplySQLite(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")

	cfg, err := Apply(ctx, dataDir, Params{Driver: "sqlite", Listen: "9090"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBDriver != config.DriverSQLite || cfg.DBDSN != DefaultSQLiteFile || cfg.Listen != ":9090" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	loaded, err := config.Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if *loaded != *cfg {
		t.Fatalf("loaded config differs:\n%+v\n%+v", loaded, cfg)
	}
	assertMigrated(t, loaded, dataDir)

	// Re-running (e.g. for a new database password) keeps the secret key,
	// the TLS and log settings and the listen address, also when the file
	// was saved with a byte order mark by Notepad. The installer's
	// setup-mode hint is removed.
	loaded.TLSCert, loaded.TLSKey, loaded.LogLevel = "cert.pem", "key.pem", "debug"
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(config.Path(dataDir))
	if err := os.WriteFile(config.Path(dataDir), append([]byte("\xef\xbb\xbf"), b...), 0o600); err != nil {
		t.Fatal(err)
	}
	hint := filepath.Join(dataDir, config.SetupListenFile)
	if err := os.WriteFile(hint, []byte(":9191"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Apply(ctx, dataDir, Params{Driver: "sqlite"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.SecretKey != cfg.SecretKey || cfg2.TLSCert != "cert.pem" || cfg2.TLSKey != "key.pem" || cfg2.LogLevel != "debug" || cfg2.Listen != ":9090" {
		t.Fatalf("settings not kept: %+v", cfg2)
	}
	if _, err := os.Stat(hint); !os.IsNotExist(err) {
		t.Fatalf("%s not removed: %v", config.SetupListenFile, err)
	}
	// An explicit listen address replaces the old one.
	if cfg2, err = Apply(ctx, dataDir, Params{Driver: "sqlite", Listen: ":9092"}); err != nil || cfg2.Listen != ":9092" {
		t.Fatalf("listen = %q, %v", cfg2.Listen, err)
	}

	// A damaged config file is reported, not overwritten.
	if err := os.WriteFile(config.Path(dataDir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, dataDir, Params{Driver: "sqlite"}); err == nil || !strings.Contains(err.Error(), "is damaged") {
		t.Fatalf("got %v", err)
	}
}

func TestApplySQLitePaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")

	// Inside the data directory: stored relative.
	cfg, err := Apply(ctx, dataDir, Params{Driver: "sqlite", SQLitePath: filepath.Join(dataDir, "db", "sp.db")})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("db", "sp.db"); cfg.DBDSN != want {
		t.Fatalf("DBDSN = %q, want %q", cfg.DBDSN, want)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "db", "sp.db")); err != nil {
		t.Fatal(err)
	}

	// Outside the data directory: stored absolute.
	outside := filepath.Join(root, "elsewhere", "sp.db")
	cfg, err = Apply(ctx, dataDir, Params{Driver: "sqlite", SQLitePath: outside})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBDSN != outside {
		t.Fatalf("DBDSN = %q, want %q", cfg.DBDSN, outside)
	}
	assertMigrated(t, cfg, dataDir)
}

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{"statuspage": "statuspage", "Status": `"Status"`, `a"b`: `"a""b"`, "my-db": `"my-db"`} {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
}

func assertMigrated(t *testing.T, cfg *config.Config, dataDir string) {
	t.Helper()
	ctx := context.Background()
	d, err := db.OpenConfig(ctx, cfg, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var v int
	if err := d.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != db.SchemaVersion() {
		t.Fatalf("schema version %d, want %d", v, db.SchemaVersion())
	}
}

// ------------------------------------------------------------------ PostgreSQL

// testPostgres returns the PostgreSQL test parameters from
// STATUSPAGE_TEST_PG_DSN (after resetting the schema) or skips the test.
func testPostgres(t *testing.T) config.PostgresParams {
	t.Helper()
	dsn := os.Getenv("STATUSPAGE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("STATUSPAGE_TEST_PG_DSN not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	port, _ := strconv.Atoi(u.Port())
	pg := config.PostgresParams{
		Host: u.Hostname(), Port: port, Database: strings.TrimPrefix(u.Path, "/"),
		User: u.User.Username(), Password: pw, SSLMode: u.Query().Get("sslmode"),
	}
	d, err := db.Open(context.Background(), config.DriverPostgres, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.ExecContext(context.Background(), "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	return pg
}

func TestPostgres(t *testing.T) {
	pg := testPostgres(t)
	ctx := context.Background()
	dataDir := t.TempDir()
	p := Params{Driver: "postgres", Postgres: pg}

	if err := Check(ctx, dataDir, p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(config.Path(dataDir)); err == nil {
		t.Fatal("Check wrote config.json")
	}
	d, err := openPostgres(ctx, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if schemaUpToDate(ctx, d) {
		t.Fatal("empty schema reported as up to date")
	}

	cfg, err := Apply(ctx, dataDir, p)
	if err != nil {
		t.Fatal(err)
	}
	if !schemaUpToDate(ctx, d) {
		t.Fatal("schema not reported as up to date after Apply")
	}
	if cfg.DBDriver != config.DriverPostgres || cfg.DBDSN != pg.DSN() {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	loaded, err := config.Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	assertMigrated(t, loaded, dataDir)

	// Second run: tables exist already, key is kept.
	cfg2, err := Apply(ctx, dataDir, p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.SecretKey != cfg.SecretKey {
		t.Fatal("secret key changed")
	}
	if err := Check(ctx, dataDir, p); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresErrors(t *testing.T) {
	pg := testPostgres(t)
	ctx := context.Background()
	addr := pg.Host + ":" + strconv.Itoa(pg.Port)

	wrongPW := pg
	wrongPW.Password = "definitely-wrong"
	err := Check(ctx, t.TempDir(), Params{Driver: "postgres", Postgres: wrongPW})
	want := "could not connect to PostgreSQL at " + addr + ": password authentication failed for user \"" + pg.User + "\""
	if err == nil || err.Error() != want {
		t.Fatalf("got  %v\nwant %s", err, want)
	}

	noDB := pg
	noDB.Database = "statuspage_does_not_exist"
	err = Check(ctx, t.TempDir(), Params{Driver: "postgres", Postgres: noDB})
	if err == nil || !strings.Contains(err.Error(), `the database "statuspage_does_not_exist" does not exist`) ||
		!strings.Contains(err.Error(), "CREATE DATABASE statuspage_does_not_exist OWNER") {
		t.Fatalf("got %v", err)
	}

	refused := pg
	refused.Host, refused.Port = "127.0.0.1", 1
	err = Check(ctx, t.TempDir(), Params{Driver: "postgres", Postgres: refused})
	if err == nil || !strings.HasPrefix(err.Error(), "could not connect to PostgreSQL at 127.0.0.1:1: connection refused") {
		t.Fatalf("got %v", err)
	}

	unknownHost := pg
	unknownHost.Host = "statuspage-test.invalid"
	err = Check(ctx, t.TempDir(), Params{Driver: "postgres", Postgres: unknownHost})
	if err == nil || !strings.HasPrefix(err.Error(), "could not connect to PostgreSQL at statuspage-test.invalid:") {
		t.Fatalf("got %v", err)
	}
	t.Logf("unknown host: %v", err)
}
