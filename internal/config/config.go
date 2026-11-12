// Package config handles the small bootstrap configuration file (config.json)
// that lives in the data directory. It only contains what is needed before the
// database can be reached: listen address, database connection and the secret
// key. Everything else is stored in the database and edited in the web UI.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

const FileName = "config.json"

// DefaultListen is used when the config file does not specify a listen address.
const DefaultListen = ":8080"

// Database drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// ErrNotConfigured is returned by Load when no config file exists yet.
var ErrNotConfigured = errors.New("status page is not configured yet")

type Config struct {
	// Listen is the TCP address the HTTP server listens on, e.g. ":8080".
	Listen string `json:"listen"`
	// DBDriver is "sqlite" or "postgres".
	DBDriver string `json:"db_driver"`
	// DBDSN is the SQLite file path (absolute, or relative to the data
	// directory) or a PostgreSQL connection URL.
	DBDSN string `json:"db_dsn"`
	// SecretKey is a base64 encoded 32 byte key used to encrypt secrets that
	// are stored in the database (OIDC client secret, SMTP password) and
	// short-lived cookies. Generated automatically.
	SecretKey string `json:"secret_key"`
	// Optional TLS certificate and key (PEM files). When both are set the
	// server serves HTTPS directly.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`
}

// DefaultDataDir returns the platform specific default data directory:
// %ProgramData%\StatusPage on Windows, otherwise a "data" directory next to
// the executable.
func DefaultDataDir() string {
	if v := os.Getenv("STATUSPAGE_DATA"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, "StatusPage")
	}
	exe, err := os.Executable()
	if err != nil {
		return "data"
	}
	return filepath.Join(filepath.Dir(exe), "data")
}

// Path returns the config file path inside dataDir.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

// Exists reports whether a config file exists in dataDir.
func Exists(dataDir string) bool {
	_, err := os.Stat(Path(dataDir))
	return err == nil
}

// Load reads config.json from dataDir. Returns ErrNotConfigured if missing.
func Load(dataDir string) (*Config, error) {
	b, err := os.ReadFile(Path(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotConfigured
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Path(dataDir), err)
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.DBDriver != DriverSQLite && c.DBDriver != DriverPostgres {
		return nil, fmt.Errorf("config: unsupported db_driver %q", c.DBDriver)
	}
	if _, err := c.Key(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes the config atomically to dataDir with restrictive permissions.
func (c *Config) Save(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(dataDir))
}

// Key decodes the secret key.
func (c *Config) Key() ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(c.SecretKey)
	if err != nil || len(k) != 32 {
		return nil, errors.New("config: secret_key must be 32 bytes, base64 encoded")
	}
	return k, nil
}

// SQLitePath resolves the SQLite file path relative to dataDir.
func (c *Config) SQLitePath(dataDir string) string {
	if c.DBDSN == "" {
		return filepath.Join(dataDir, "statuspage.db")
	}
	if filepath.IsAbs(c.DBDSN) {
		return c.DBDSN
	}
	return filepath.Join(dataDir, c.DBDSN)
}

// NewSecretKey generates a new random base64 encoded 32 byte key.
func NewSecretKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// PostgresParams are the individual connection settings collected by the
// installer / setup wizard.
type PostgresParams struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"password"`
	SSLMode  string `json:"sslmode"` // disable, require, verify-ca, verify-full
}

// DSN builds a PostgreSQL connection URL with proper escaping.
func (p PostgresParams) DSN() string {
	host := p.Host
	if host == "" {
		host = "localhost"
	}
	port := p.Port
	if port == 0 {
		port = 5432
	}
	ssl := p.SSLMode
	if ssl == "" {
		ssl = "prefer"
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(p.User, p.Password),
		Host:     host + ":" + strconv.Itoa(port),
		Path:     "/" + p.Database,
		RawQuery: url.Values{"sslmode": {ssl}}.Encode(),
	}
	return u.String()
}

// SetupTokenFile is the name of the file in the data directory that contains
// the one-time setup token. It is deleted when the setup is complete.
const SetupTokenFile = "setup-token.txt"
