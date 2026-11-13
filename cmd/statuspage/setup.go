package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/setup"
)

const setupUsage = `statuspage setup [-data DIR] (-params-file FILE | -db sqlite|postgres [options]) [-check-only] [-result-file FILE]

Checks the database connection, creates the tables and writes config.json.
With -check-only only the connection is tested and nothing is written.
Running it again (e.g. for a new database password) keeps the secret key, the
TLS and log settings and, unless -listen is given, the listen address.
Exit code 0 on success, 1 on error, 2 for an invalid command line.

Examples:
  statuspage setup -db sqlite
  statuspage setup -db postgres -pg-host db1 -pg-database statuspage -pg-user statuspage
        (password from -pg-password or the environment variable STATUSPAGE_PG_PASSWORD)`

// dbFlags are the flags that describe the database; they cannot be combined
// with -params-file.
var dbFlags = []string{"db", "sqlite-path", "pg-host", "pg-port", "pg-database", "pg-user", "pg-password", "pg-sslmode", "listen"}

// cmdSetup configures the database. It is also called by the Windows
// installer, which reads the result from -result-file.
func cmdSetup(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("setup", setupUsage, stderr)
	dataDir := dataDirFlag(fs)
	paramsFile := fs.String("params-file", "", "read the settings from this JSON file instead of the flags below")
	dbType := fs.String("db", "", "database type: sqlite or postgres")
	sqlitePath := fs.String("sqlite-path", "", "SQLite database file (default: statuspage.db in the data directory)")
	pgHost := fs.String("pg-host", "localhost", "PostgreSQL server name or address")
	pgPort := fs.Int("pg-port", setup.DefaultPostgresPort, "PostgreSQL port")
	pgDatabase := fs.String("pg-database", "statuspage", "PostgreSQL database (must exist)")
	pgUser := fs.String("pg-user", "statuspage", "PostgreSQL user")
	pgPassword := fs.String("pg-password", "", "PostgreSQL password (default: env STATUSPAGE_PG_PASSWORD)")
	pgSSLMode := fs.String("pg-sslmode", "prefer", "PostgreSQL SSL mode: "+strings.Join(setup.SSLModes, ", "))
	listen := fs.String("listen", "", "address the web server listens on, e.g. :9090 (default: the address in an existing config.json, otherwise "+config.DefaultListen+")")
	checkOnly := fs.Bool("check-only", false, "only test the settings, write nothing")
	resultFile := fs.String("result-file", "", `write "OK" or the error message to this file (used by the installer)`)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	err := doSetup(fs, *dataDir, *paramsFile, *checkOnly, setup.Params{
		Driver:     *dbType,
		SQLitePath: *sqlitePath,
		Postgres: config.PostgresParams{
			Host: *pgHost, Port: *pgPort, Database: *pgDatabase,
			User: *pgUser, Password: *pgPassword, SSLMode: *pgSSLMode,
		},
		Listen: *listen,
	}, stdout)

	if *resultFile != "" {
		result := "OK"
		if err != nil {
			result = err.Error()
		}
		if werr := os.WriteFile(*resultFile, []byte(result), 0o600); werr != nil {
			fmt.Fprintln(stderr, "error: could not write the result file:", werr)
		}
	}
	return err
}

func doSetup(fs *flag.FlagSet, dataDir, paramsFile string, checkOnly bool, p setup.Params, stdout io.Writer) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if paramsFile != "" {
		var conflicting []string
		for _, name := range dbFlags {
			if set[name] {
				conflicting = append(conflicting, "-"+name)
			}
		}
		if len(conflicting) > 0 {
			sort.Strings(conflicting)
			return fmt.Errorf("-params-file cannot be combined with %s", strings.Join(conflicting, ", "))
		}
		var err error
		if p, err = readParams(paramsFile); err != nil {
			return err
		}
	} else if !set["db"] {
		return errors.New("please choose the database with -db sqlite or -db postgres (or use -params-file)")
	}
	if p.Postgres.Password == "" {
		p.Postgres.Password = os.Getenv("STATUSPAGE_PG_PASSWORD")
	}

	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if checkOnly {
		if err := setup.Check(ctx, dir, p); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "OK: the database settings work.")
		return nil
	}
	cfg, err := setup.Apply(ctx, dir, p)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "OK: the %s database is ready and the configuration was written to %s.\n", cfg.DBDriver, config.Path(dir))
	return nil
}

// readParams reads the JSON settings file written by the installer. A UTF-8
// byte order mark is ignored.
func readParams(path string) (setup.Params, error) {
	var p setup.Params
	b, err := os.ReadFile(path)
	if err != nil {
		return p, fmt.Errorf("could not read the settings file: %w", err)
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("the settings file %s is not valid JSON: %v", path, err)
	}
	return p, nil
}
