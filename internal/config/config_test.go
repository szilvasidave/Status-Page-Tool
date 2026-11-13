package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(Path(dir), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadBOM: config.json saved by Notepad or Windows PowerShell 5 starts
// with a UTF-8 byte order mark.
func TestLoadBOM(t *testing.T) {
	dir := t.TempDir()
	key := NewSecretKey()
	writeConfig(t, dir, "\xef\xbb\xbf"+`{"listen": ":9090", "db_driver": "sqlite", "secret_key": "`+key+`", "log_level": "debug"}`)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9090" || c.SecretKey != key || c.LogLevel != "debug" {
		t.Fatalf("unexpected config %+v", c)
	}
}

// TestSetupListen: the installer's setup-listen.txt, possibly saved with a
// byte order mark and a line break; anything but host:port is ignored.
func TestSetupListen(t *testing.T) {
	dir := t.TempDir()
	if got := SetupListen(dir); got != "" {
		t.Errorf("no file: %q", got)
	}
	for content, want := range map[string]string{
		"\xef\xbb\xbf:9090\r\n": ":9090",
		"127.0.0.1:9091":        "127.0.0.1:9091",
		"[::1]:9092":            "[::1]:9092",
		"9090":                  "",
		":http":                 "",
		":0":                    "",
		":70000":                "",
		"":                      "",
	} {
		if err := os.WriteFile(filepath.Join(dir, SetupListenFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := SetupListen(dir); got != want {
			t.Errorf("SetupListen(%q) = %q, want %q", content, got, want)
		}
	}
}

func TestLoadTLS(t *testing.T) {
	dir := t.TempDir()
	key := NewSecretKey()

	// Only one of the two files: an error instead of silently serving HTTP.
	writeConfig(t, dir, `{"db_driver": "sqlite", "secret_key": "`+key+`", "tls_cert": "status.pem"}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "tls_cert and tls_key") {
		t.Fatalf("half TLS configuration: %v", err)
	}

	// Relative paths are relative to the data directory, absolute ones are
	// kept.
	abs := filepath.Join(t.TempDir(), "key.pem")
	writeConfig(t, dir, `{"db_driver": "sqlite", "secret_key": "`+key+`", "tls_cert": "status.pem", "tls_key": `+jsonString(abs)+`}`)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cert, k := c.TLSFiles(dir)
	if cert != filepath.Join(dir, "status.pem") || k != abs {
		t.Fatalf("TLSFiles = %q, %q", cert, k)
	}
	if cert, k := (&Config{}).TLSFiles(dir); cert != "" || k != "" {
		t.Fatalf("TLSFiles without TLS = %q, %q", cert, k)
	}
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}

func TestEnsureDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "data")
	if err := EnsureDataDir(dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("data directory not created: %v", err)
	}
	if err := EnsureDataDir(dir); err != nil {
		t.Fatalf("existing directory: %v", err)
	}
}

// TestIcaclsArgs checks the Windows permissions of the data directory: only
// SYSTEM, Administrators and the current user, nothing inherited from
// ProgramData (where every user may read).
func TestIcaclsArgs(t *testing.T) {
	dir := `C:\ProgramData\StatusPage`
	want := []string{dir, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "*S-1-5-21-1-2-3-1001:(OI)(CI)F", "/Q"}
	if got := icaclsArgs(dir, "S-1-5-21-1-2-3-1001"); !reflect.DeepEqual(got, want) {
		t.Errorf("user: %q", got)
	}
	// The service runs as SYSTEM: no duplicate entry.
	want = []string{dir, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "/Q"}
	if got := icaclsArgs(dir, "S-1-5-18"); !reflect.DeepEqual(got, want) {
		t.Errorf("SYSTEM: %q", got)
	}
}
