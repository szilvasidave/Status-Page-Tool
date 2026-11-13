// Package config handles the small bootstrap configuration file (config.json)
// that lives in the data directory. It only contains what is needed before the
// database can be reached: listen address, optional TLS certificate, database
// connection, secret key and log level. Everything else is stored in the
// database and edited in the web UI.
package config

import (
	"bytes"
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
	// Optional TLS certificate and key (PEM files, absolute or relative to
	// the data directory). When both are set the server serves HTTPS
	// directly; setting only one of them is an error.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`
	// LogLevel is debug, info (default), warn or error. The environment
	// variable STATUSPAGE_LOG_LEVEL overrides it.
	LogLevel string `json:"log_level,omitempty"`
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
	if err := json.Unmarshal(TrimBOM(b), &c); err != nil {
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
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, errors.New("config: tls_cert and tls_key must be set together (or both left empty)")
	}
	return &c, nil
}

// TrimBOM removes a UTF-8 byte order mark, which Notepad and Windows
// PowerShell 5 write when saving a file as UTF-8.
func TrimBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
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
	return resolve(dataDir, c.DBDSN)
}

// TLSFiles returns the TLS certificate and key paths, resolved relative to
// dataDir; both are empty when TLS is not configured.
func (c *Config) TLSFiles(dataDir string) (cert, key string) {
	if c.TLSCert == "" || c.TLSKey == "" {
		return "", ""
	}
	return resolve(dataDir, c.TLSCert), resolve(dataDir, c.TLSKey)
}

// resolve returns p, or p relative to dataDir if it is not absolute.
func resolve(dataDir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dataDir, p)
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
	SSLMode  string `json:"sslmode"` // disable, allow, prefer, require, verify-ca, verify-full
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

// SetupListenFile is the name of an optional file in the data directory with
// the listen address (e.g. ":9090") to use while the server runs in setup
// mode. The Windows installer writes it when its database setup failed, so
// that the web setup runs on the port chosen in the installer. It is deleted
// when the setup is complete.
const SetupListenFile = "setup-listen.txt"
