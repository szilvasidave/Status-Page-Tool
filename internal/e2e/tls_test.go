package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/setup"
)

// writeTestCert writes a self-signed certificate for 127.0.0.1 and its key
// as PEM files into dir and returns their paths and a pool trusting it.
func writeTestCert(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "status page test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, pool
}

// TestTLS runs the server with tls_cert/tls_key from config.json: the
// setup and the sign-in work over HTTPS and the cookies are Secure.
func TestTLS(t *testing.T) {
	logs := &logBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("application log:\n%s", logs)
		}
	})
	dataDir := t.TempDir()
	addr := freeAddr(t)
	cfg, err := setup.Apply(context.Background(), dataDir, setup.Params{Driver: config.DriverSQLite, Listen: addr})
	if err != nil {
		t.Fatal(err)
	}
	var pool *x509.CertPool
	cfg.TLSCert, cfg.TLSKey, pool = writeTestCert(t, t.TempDir())
	if err := cfg.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	b := newBrowser(t, "https")
	b.client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	app := startAppURL(t, dataDir, addr, "https://"+addr, b.client, logs)

	p := b.get(app.URL + "/setup").expectStatus(200).expectText("Create the administrator")
	f := p.form("/setup")
	if want := suggestedBaseURL(app.URL); f.value("base_url") != want {
		t.Errorf("suggested base URL %q, want %q", f.value("base_url"), want)
	}
	p = f.set("username", "tls-admin").set("password", adminPW).set("password_confirm", adminPW).set("site_name", "TLS Status").post()
	p.expectRedirect("/admin/settings")
	secure := map[string]bool{}
	for _, c := range p.Header.Values("Set-Cookie") {
		if cookie, err := http.ParseSetCookie(c); err == nil {
			secure[cookie.Name] = cookie.Secure
		}
	}
	if !secure[auth.SessionCookie] {
		t.Errorf("the session cookie is not Secure over HTTPS: %q", p.Header.Values("Set-Cookie"))
	}
	p.follow().expectStatus(200).expectFlash("success", "Setup complete")
}
