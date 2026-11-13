package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/config"
)

func saveConfig(t *testing.T, dir string, c config.Config) {
	t.Helper()
	c.DBDriver, c.SecretKey = config.DriverSQLite, config.NewSecretKey()
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}
}

func TestLocalURL(t *testing.T) {
	for _, tt := range []struct {
		cfg  *config.Config // nil: setup mode
		hint string         // content of setup-listen.txt
		want string
	}{
		{nil, "", "http://localhost:8080"},
		{nil, ":9090\r\n", "http://localhost:9090"},
		{nil, "junk", "http://localhost:8080"},
		{&config.Config{Listen: ":9091"}, ":9090", "http://localhost:9091"},
		{&config.Config{}, "", "http://localhost:8080"},
		{&config.Config{Listen: "0.0.0.0:9092"}, "", "http://localhost:9092"},
		{&config.Config{Listen: "127.0.0.1:9093"}, "", "http://127.0.0.1:9093"},
		{&config.Config{Listen: "[::1]:9094"}, "", "http://[::1]:9094"},
		{&config.Config{Listen: ":443", TLSCert: "c.pem", TLSKey: "k.pem"}, "", "https://localhost:443"},
	} {
		dir := t.TempDir()
		if tt.cfg != nil {
			saveConfig(t, dir, *tt.cfg)
		}
		if tt.hint != "" {
			if err := os.WriteFile(filepath.Join(dir, config.SetupListenFile), []byte(tt.hint), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := LocalURL(dir); err != nil || got != tt.want {
			t.Errorf("%+v, hint %q: LocalURL = %q, %v; want %q", tt.cfg, tt.hint, got, err, tt.want)
		}
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestSetupListenFile: after a failed database setup the Windows installer
// leaves the chosen port in setup-listen.txt, so that the setup wizard runs
// on the port the installer opened in the firewall and showed to the user.
func TestSetupListenFile(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if err := os.WriteFile(filepath.Join(dir, config.SetupListenFile), []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Options{DataDir: dir, Log: discardLog()}) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/setup")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /setup: %s", resp.Status)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the setup wizard does not answer on %s: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestTLSCertificateMissing: a wrong certificate path is reported at start
// (relative to the data directory), before the server says it is listening.
func TestTLSCertificateMissing(t *testing.T) {
	dir := t.TempDir()
	saveConfig(t, dir, config.Config{Listen: "127.0.0.1:0", TLSCert: "status.pem", TLSKey: "status-key.pem"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := Run(ctx, Options{DataDir: dir, Log: discardLog()})
	if err == nil || !strings.Contains(err.Error(), "could not load the TLS certificate") || !strings.Contains(err.Error(), filepath.Join(dir, "status.pem")) {
		t.Fatalf("Run = %v", err)
	}
}
