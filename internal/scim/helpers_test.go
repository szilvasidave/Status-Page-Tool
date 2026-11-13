package scim

// Test harness: the handler on a local HTTP server, backed by SQLite and,
// when STATUSPAGE_TEST_PG_DSN is set, by PostgreSQL (the public schema of
// that database is dropped and recreated).

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

var ctx = context.Background()

// env is a running SCIM endpoint with a token.
type env struct {
	t     *testing.T
	store *store.Store
	srv   *httptest.Server
	token string
}

// forEachDB runs fn against SQLite and, if configured, PostgreSQL.
func forEachDB(t *testing.T, fn func(t *testing.T, e *env)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		d, err := db.Open(ctx, config.DriverSQLite, filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		fn(t, newEnv(t, d))
	})
	if dsn := os.Getenv("STATUSPAGE_TEST_PG_DSN"); dsn != "" {
		t.Run("postgres", func(t *testing.T) {
			d, err := db.Open(ctx, config.DriverPostgres, dsn)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.ExecContext(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
				t.Fatal(err)
			}
			fn(t, newEnv(t, d))
		})
	}
}

func newEnv(t *testing.T, d *db.DB) *env {
	t.Helper()
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	st := store.New(d, []byte("0123456789abcdef0123456789abcdef"))
	settings, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.BaseURL = "https://status.example.com"
	if err := st.SaveSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	token, hash := NewToken()
	if err := st.SetSCIMToken(ctx, hash, time.Now()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/scim/", New(st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{t: t, store: st, srv: srv, token: token}
}

// response is a decoded response.
type response struct {
	status int
	header http.Header
	raw    string
	body   map[string]any // decoded JSON object (nil without body)
}

// do sends a request like Microsoft Entra ID: bearer token, JSON body with
// Content-Type application/scim+json.
func (e *env) do(method, path, body string) response {
	e.t.Helper()
	return e.doWith(method, path, body, map[string]string{"Authorization": "Bearer " + e.token})
}

// doWith sends a request with the given headers (and Content-Type
// application/scim+json; charset=utf-8 when there is a body and no other is
// given).
func (e *env) doWith(method, path, body string, header map[string]string) response {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/scim+json; charset=utf-8")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	r := response{status: resp.StatusCode, header: resp.Header, raw: string(b)}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &r.body); err != nil {
			e.t.Fatalf("%s %s: response is not a JSON object: %v\n%s", method, path, err, b)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/scim+json" {
			e.t.Errorf("%s %s: Content-Type %q, want application/scim+json", method, path, ct)
		}
	}
	return r
}

// expect fails the test unless the response has the status.
func (r response) expect(t *testing.T, status int) response {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d:\n%s", r.status, status, r.raw)
	}
	return r
}

// expectError checks a SCIM error response (RFC 7644 section 3.12).
func (r response) expectError(t *testing.T, status int, scimType string) response {
	t.Helper()
	r.expect(t, status)
	if s, _ := r.body["schemas"].([]any); len(s) != 1 || s[0] != "urn:ietf:params:scim:api:messages:2.0:Error" {
		t.Errorf("error schemas: %v", r.body["schemas"])
	}
	if got, _ := r.body["status"].(string); got != strconv.Itoa(status) {
		t.Errorf("error status %#v, want the string %q", r.body["status"], strconv.Itoa(status))
	}
	if got, _ := r.body["scimType"].(string); got != scimType {
		t.Errorf("scimType %q, want %q (%s)", got, scimType, r.raw)
	}
	if d, _ := r.body["detail"].(string); d == "" {
		t.Errorf("error without detail: %s", r.raw)
	}
	return r
}

// str returns a string attribute of the response body ("a.b" for nested).
func (r response) str(path string) string {
	var v any = r.body
	for _, k := range strings.Split(path, ".") {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	s, _ := v.(string)
	return s
}

// resources returns the Resources of a ListResponse.
func (r response) resources(t *testing.T) []map[string]any {
	t.Helper()
	if s, _ := r.body["schemas"].([]any); len(s) != 1 || s[0] != "urn:ietf:params:scim:api:messages:2.0:ListResponse" {
		t.Fatalf("not a ListResponse: %s", r.raw)
	}
	list, ok := r.body["Resources"].([]any)
	if !ok {
		t.Fatalf("ListResponse without Resources array: %s", r.raw)
	}
	out := make([]map[string]any, len(list))
	for i, x := range list {
		out[i], _ = x.(map[string]any)
	}
	return out
}

// total returns totalResults of a ListResponse.
func (r response) total() int {
	n, _ := r.body["totalResults"].(float64)
	return int(n)
}

// mustID parses a SCIM id.
func mustID(t *testing.T, id string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		t.Fatalf("id %q is not a number", id)
	}
	return n
}
