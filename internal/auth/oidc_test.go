package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// ===================================================================== mock IdP

const (
	mockClientID     = "status-page-client"
	mockClientSecret = "client-secret-123"
	testBaseURL      = "https://status.example.com"
)

// mockIdP is a minimal in-process OpenID provider: discovery, JWKS,
// authorization endpoint (signs the user in immediately), token endpoint
// (checks client credentials, redirect URI and the PKCE verifier) and
// userinfo endpoint.
type mockIdP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey // published in the JWKS

	mu            sync.Mutex
	claims        map[string]any       // ID token claims of the next sign-in
	userinfo      map[string]any       // userinfo response (sub is added); nil = 404
	noUserinfo    bool                 // leave userinfo_endpoint out of discovery
	tamper        func(map[string]any) // modifies the ID token claims
	signKey       *rsa.PrivateKey      // signs ID tokens instead of key
	codes         map[string]mockGrant
	tokens        map[string]map[string]any // access token -> userinfo claims
	discoveryHits int
	userinfoHits  int
}

type mockGrant struct {
	clientID, redirectURI, challenge, nonce string
	claims                                  map[string]any
}

func newMockIdP(t *testing.T) *mockIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &mockIdP{t: t, key: key, codes: map[string]mockGrant{}, tokens: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /userinfo", p.userinfoEndpoint)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *mockIdP) issuer() string { return p.srv.URL }

// setUser sets the claims of the next sign-in (sub is required).
func (p *mockIdP) setUser(claims map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claims = claims
}

func (p *mockIdP) setUserinfo(claims map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.userinfo = claims
}

func (p *mockIdP) setTamper(f func(map[string]any)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tamper = f
}

func (p *mockIdP) hits() (discovery, userinfo int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.discoveryHits, p.userinfoHits
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *mockIdP) discovery(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.discoveryHits++
	noUserinfo := p.noUserinfo
	p.mu.Unlock()
	doc := map[string]any{
		"issuer":                                p.issuer(),
		"authorization_endpoint":                p.issuer() + "/authorize",
		"token_endpoint":                        p.issuer() + "/token",
		"jwks_uri":                              p.issuer() + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	}
	if !noUserinfo {
		doc["userinfo_endpoint"] = p.issuer() + "/userinfo"
	}
	writeJSON(w, http.StatusOK, doc)
}

func (p *mockIdP) jwks(w http.ResponseWriter, r *http.Request) {
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}}}
	writeJSON(w, http.StatusOK, set)
}

// authorize checks the request like a real provider and immediately
// redirects back with a code, as if the user had signed in.
func (p *mockIdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case q.Get("response_type") != "code":
		http.Error(w, "bad response_type", http.StatusBadRequest)
		return
	case q.Get("client_id") != mockClientID:
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	case q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
		http.Error(w, "PKCE S256 required", http.StatusBadRequest)
		return
	case !slices.Contains(strings.Fields(q.Get("scope")), "openid"):
		http.Error(w, "openid scope missing", http.StatusBadRequest)
		return
	case q.Get("state") == "" || q.Get("nonce") == "":
		http.Error(w, "state and nonce required", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	code := "code-" + randomString()
	p.codes[code] = mockGrant{
		clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"),
		challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), claims: maps.Clone(p.claims),
	}
	p.mu.Unlock()
	back, _ := url.Parse(q.Get("redirect_uri"))
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (p *mockIdP) token(w http.ResponseWriter, r *http.Request) {
	tokenError := func(code, desc string) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": desc})
	}
	if err := r.ParseForm(); err != nil {
		tokenError("invalid_request", err.Error())
		return
	}
	id, sec, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		sec, _ = url.QueryUnescape(sec)
	} else {
		id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != mockClientID || sec != mockClientSecret {
		tokenError("invalid_client", "Invalid client secret provided.")
		return
	}
	p.mu.Lock()
	g, found := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	tamper := p.tamper
	userinfo := p.userinfo
	p.mu.Unlock()
	switch {
	case r.PostForm.Get("grant_type") != "authorization_code":
		tokenError("unsupported_grant_type", "")
		return
	case !found:
		tokenError("invalid_grant", "The code is invalid or was already used.")
		return
	case r.PostForm.Get("redirect_uri") != g.redirectURI:
		tokenError("invalid_grant", "redirect_uri mismatch")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		tokenError("invalid_grant", "PKCE verification failed")
		return
	}

	now := time.Now()
	claims := map[string]any{
		"iss": p.issuer(), "aud": mockClientID, "nonce": g.nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	maps.Copy(claims, g.claims)
	if tamper != nil {
		tamper(claims)
	}
	access := "at-" + randomString()
	if userinfo != nil {
		ui := maps.Clone(userinfo)
		if _, ok := ui["sub"]; !ok {
			ui["sub"] = claims["sub"]
		}
		p.mu.Lock()
		p.tokens[access] = ui
		p.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 3600,
		"id_token": p.sign(claims),
	})
}

func (p *mockIdP) userinfoEndpoint(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.userinfoHits++
	claims, ok := p.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	p.mu.Unlock()
	if !ok {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, claims)
}

// sign returns a signed JWT with the given claims.
func (p *mockIdP) sign(claims map[string]any) string {
	p.t.Helper()
	p.mu.Lock()
	key := p.key
	if p.signKey != nil {
		key = p.signKey
	}
	p.mu.Unlock()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "test-key"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		p.t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := signer.Sign(payload)
	if err != nil {
		p.t.Fatal(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		p.t.Fatal(err)
	}
	return s
}

func randomString() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ===================================================================== test env

type oidcEnv struct {
	t      *testing.T
	idp    *mockIdP
	store  *store.Store
	m      *Manager
	logBuf *syncBuffer
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newTestStore returns a store backed by a fresh SQLite database.
func newTestStore(t *testing.T) (*store.Store, []byte) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, config.DriverSQLite, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{SecretKey: config.NewSecretKey()}
	key, err := cfg.Key()
	if err != nil {
		t.Fatal(err)
	}
	return store.New(d, key), key
}

func newOIDCEnv(t *testing.T) *oidcEnv {
	t.Helper()
	st, key := newTestStore(t)
	buf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := &oidcEnv{t: t, idp: newMockIdP(t), store: st, m: New(st, key, log), logBuf: buf}
	e.saveSettings(func(s *store.Settings) {})
	return e
}

// saveSettings stores working OIDC settings, modified by f.
func (e *oidcEnv) saveSettings(f func(*store.Settings)) {
	e.t.Helper()
	s := store.DefaultSettings()
	s.BaseURL = testBaseURL
	s.OIDCEnabled = true
	s.OIDCIssuer = e.idp.issuer()
	s.OIDCClientID = mockClientID
	s.OIDCClientSecret = mockClientSecret
	f(&s)
	if err := e.store.SaveSettings(context.Background(), &s); err != nil {
		e.t.Fatal(err)
	}
}

// loginResult is the outcome of a complete sign-in.
type loginResult struct {
	returnTo string
	err      error
	cookies  []*http.Cookie // cookies set by FinishOIDC
}

// start runs StartOIDC and returns the state cookie and the authorization
// URL it redirects to.
func (e *oidcEnv) start(returnTo string) (*http.Cookie, *url.URL) {
	e.t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, testBaseURL+"/auth/oidc/login", nil)
	if err := e.m.StartOIDC(rec, req, returnTo); err != nil {
		e.t.Fatalf("StartOIDC: %v", err)
	}
	if rec.Code != http.StatusFound {
		e.t.Fatalf("StartOIDC status = %d, want 302", rec.Code)
	}
	var state *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == OIDCCookie {
			state = c
		}
	}
	if state == nil {
		e.t.Fatal("StartOIDC did not set the state cookie")
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	return state, loc
}

// authorize follows the authorization URL at the mock IdP and returns the
// callback URL it redirects back to.
func (e *oidcEnv) authorize(authURL *url.URL) *url.URL {
	e.t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authURL.String())
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		b, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("authorize: status %d: %s", resp.StatusCode, b)
	}
	cb, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	return cb
}

// finish calls FinishOIDC with the callback URL and the given cookies.
func (e *oidcEnv) finish(callback *url.URL, cookies ...*http.Cookie) loginResult {
	e.t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, callback.String(), nil)
	for _, c := range cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	returnTo, err := e.m.FinishOIDC(rec, req)
	return loginResult{returnTo: returnTo, err: err, cookies: rec.Result().Cookies()}
}

// login runs a complete sign-in with the claims currently set at the IdP.
func (e *oidcEnv) login(returnTo string) loginResult {
	e.t.Helper()
	state, authURL := e.start(returnTo)
	return e.finish(e.authorize(authURL), state)
}

// mustLogin runs a sign-in that must succeed and returns the signed-in user.
func (e *oidcEnv) mustLogin(claims map[string]any) *store.User {
	e.t.Helper()
	e.idp.setUser(claims)
	res := e.login("/")
	if res.err != nil {
		e.t.Fatalf("login failed: %v", res.err)
	}
	return e.sessionUser(res)
}

// sessionUser returns the user of the session cookie set by FinishOIDC.
func (e *oidcEnv) sessionUser(res loginResult) *store.User {
	e.t.Helper()
	for _, c := range res.cookies {
		if c.Name == SessionCookie && c.Value != "" {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(c)
			id := e.m.loadIdentity(req)
			if id == nil {
				e.t.Fatal("session cookie does not belong to a valid session")
			}
			return id.User
		}
	}
	e.t.Fatal("no session cookie set")
	return nil
}

func (e *oidcEnv) groupsOf(userID int64) []string {
	e.t.Helper()
	gs, err := e.store.UserGroups(context.Background(), userID)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, g := range gs {
		out = append(out, g.ExternalID)
	}
	slices.Sort(out)
	return out
}

func (e *oidcEnv) userCount() int {
	e.t.Helper()
	us, err := e.store.ListUsers(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return len(us)
}

// expectFailure asserts that a sign-in failed with an error containing want
// and that no session was started.
func expectFailure(t *testing.T, res loginResult, want string) {
	t.Helper()
	if res.err == nil {
		t.Fatalf("sign-in succeeded, want error containing %q", want)
	}
	t.Logf("sign-in error: %v", res.err)
	if !strings.Contains(res.err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", res.err, want)
	}
	for _, c := range res.cookies {
		if c.Name == SessionCookie && c.Value != "" {
			t.Error("a session cookie was set for a failed sign-in")
		}
	}
}

// ===================================================================== tests

func TestOIDCHappyPath(t *testing.T) {
	e := newOIDCEnv(t)
	e.idp.setUser(map[string]any{
		"sub": "user-1", "email": "Alice@Example.com", "name": "Alice Example",
		"oid": "11111111-2222", "groups": []string{"g-admins", "g-staff"},
	})
	state, authURL := e.start("/incidents/5?x=1")

	// The authorization request carries the expected parameters.
	q := authURL.Query()
	if got := q.Get("redirect_uri"); got != testBaseURL+OIDCCallbackPath {
		t.Errorf("redirect_uri = %q", got)
	}
	if got := q.Get("scope"); got != "openid profile email" {
		t.Errorf("scope = %q", got)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" || q.Get("state") == "" {
		t.Errorf("missing PKCE/nonce/state in %s", authURL)
	}
	if state.Path != "/auth/oidc" || !state.HttpOnly || !state.Secure || state.SameSite != http.SameSiteLaxMode || state.MaxAge != 600 {
		t.Errorf("state cookie attributes: %+v", state)
	}
	if strings.Contains(state.Value, q.Get("state")) {
		t.Error("state cookie is not encrypted")
	}

	res := e.finish(e.authorize(authURL), state)
	if res.err != nil {
		t.Fatalf("FinishOIDC: %v", res.err)
	}
	if res.returnTo != "/incidents/5?x=1" {
		t.Errorf("returnTo = %q", res.returnTo)
	}
	cleared := false
	for _, c := range res.cookies {
		if c.Name == OIDCCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("state cookie not cleared")
	}

	u := e.sessionUser(res)
	if u.OIDCSubject != "user-1" || u.Email != "alice@example.com" || u.Name != "Alice Example" || u.DirectoryID != "11111111-2222" {
		t.Errorf("user = %+v", u)
	}
	if u.LastLoginAt == nil {
		t.Error("last login not recorded")
	}
	if got := e.groupsOf(u.ID); !slices.Equal(got, []string{"g-admins", "g-staff"}) {
		t.Errorf("groups = %v", got)
	}

	// The browser drops the state cookie, so the callback cannot be replayed.
	expectFailure(t, e.finish(e.authorize(authURL)), "expired")
}

func TestOIDCSecondLoginReplacesGroups(t *testing.T) {
	e := newOIDCEnv(t)
	u1 := e.mustLogin(map[string]any{"sub": "user-1", "email": "bob@example.com", "groups": []string{"a", "b"}})
	if got := e.groupsOf(u1.ID); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("groups = %v", got)
	}
	u2 := e.mustLogin(map[string]any{"sub": "user-1", "email": "bob@example.com", "name": "Bob", "groups": []string{"b", "c"}})
	if u2.ID != u1.ID || e.userCount() != 1 {
		t.Fatalf("second login created another user (%d vs %d)", u2.ID, u1.ID)
	}
	if u2.Name != "Bob" {
		t.Errorf("name not updated: %q", u2.Name)
	}
	if got := e.groupsOf(u1.ID); !slices.Equal(got, []string{"b", "c"}) {
		t.Errorf("groups after second login = %v, want [b c]", got)
	}
	// An empty list removes all memberships.
	e.mustLogin(map[string]any{"sub": "user-1", "groups": []string{}})
	if got := e.groupsOf(u1.ID); len(got) != 0 {
		t.Errorf("groups after empty claim = %v", got)
	}
}

func TestOIDCGroupsAsSingleString(t *testing.T) {
	e := newOIDCEnv(t)
	u := e.mustLogin(map[string]any{"sub": "user-1", "groups": "only-group"})
	if got := e.groupsOf(u.ID); !slices.Equal(got, []string{"only-group"}) {
		t.Errorf("groups = %v", got)
	}
}

func TestOIDCCustomGroupsClaim(t *testing.T) {
	e := newOIDCEnv(t)
	e.saveSettings(func(s *store.Settings) { s.OIDCGroupsClaim = "roles" })
	u := e.mustLogin(map[string]any{"sub": "user-1", "groups": []string{"ignored"}, "roles": []string{"r1"}})
	if got := e.groupsOf(u.ID); !slices.Equal(got, []string{"r1"}) {
		t.Errorf("groups = %v", got)
	}
}

func TestOIDCMissingGroupsClaimKeepsMemberships(t *testing.T) {
	e := newOIDCEnv(t)
	u := e.mustLogin(map[string]any{"sub": "user-1", "groups": []string{"keep"}})

	// Userinfo without groups.
	e.idp.setUserinfo(map[string]any{"email": "x@example.com"})
	e.mustLogin(map[string]any{"sub": "user-1"})
	if got := e.groupsOf(u.ID); !slices.Equal(got, []string{"keep"}) {
		t.Errorf("groups = %v, want [keep]", got)
	}

	// Provider without userinfo endpoint.
	e.idp.mu.Lock()
	e.idp.noUserinfo = true
	e.idp.mu.Unlock()
	e.m.ResetOIDC()
	_, before := e.idp.hits()
	e.mustLogin(map[string]any{"sub": "user-1"})
	if _, after := e.idp.hits(); after != before {
		t.Error("userinfo called although the provider has no userinfo endpoint")
	}
	if got := e.groupsOf(u.ID); !slices.Equal(got, []string{"keep"}) {
		t.Errorf("groups = %v, want [keep]", got)
	}

	// A claim of the wrong type is ignored, too.
	e.mustLogin(map[string]any{"sub": "user-1", "groups": map[string]any{"x": 1}})
	if got := e.groupsOf(u.ID); !slices.Equal(got, []string{"keep"}) {
		t.Errorf("groups = %v, want [keep]", got)
	}
}

func TestOIDCUserinfoFallback(t *testing.T) {
	e := newOIDCEnv(t)
	e.idp.setUserinfo(map[string]any{"groups": []string{"from-userinfo"}, "email": "carol@example.com", "name": "Carol"})
	u := e.mustLogin(map[string]any{"sub": "user-1"})
	if got := e.groupsOf(u.ID); !slices.Equal(got, []string{"from-userinfo"}) {
		t.Errorf("groups = %v", got)
	}
	if u.Email != "carol@example.com" || u.Name != "Carol" {
		t.Errorf("missing e-mail/name not taken from userinfo: %+v", u)
	}
	if _, hits := e.idp.hits(); hits != 1 {
		t.Errorf("userinfo hits = %d, want 1", hits)
	}

	// Not called when the ID token has the groups.
	e.mustLogin(map[string]any{"sub": "user-1", "groups": []string{"x"}})
	if _, hits := e.idp.hits(); hits != 1 {
		t.Errorf("userinfo called although the ID token had groups")
	}

	// Userinfo for a different subject is ignored.
	e.idp.setUserinfo(map[string]any{"sub": "someone-else", "groups": []string{"evil"}})
	e.mustLogin(map[string]any{"sub": "user-1"})
	if got := e.groupsOf(u.ID); !slices.Equal(got, []string{"x"}) {
		t.Errorf("groups = %v, want [x]", got)
	}
}

func TestOIDCGroupOverage(t *testing.T) {
	e := newOIDCEnv(t)
	u := e.mustLogin(map[string]any{"sub": "user-1", "oid": "oid-1", "groups": []string{"g1"}})
	e.idp.setUserinfo(map[string]any{"groups": []string{"must-not-be-used"}})
	e.mustLogin(map[string]any{
		"sub": "user-1", "oid": "oid-1",
		"_claim_names":   map[string]any{"groups": "src1"},
		"_claim_sources": map[string]any{"src1": map[string]any{"endpoint": "https://graph.microsoft.com/v1.0/users/oid-1/getMemberObjects"}},
	})
	if got := e.groupsOf(u.ID); !slices.Equal(got, []string{"g1"}) {
		t.Errorf("groups after overage = %v, want [g1]", got)
	}
	if _, hits := e.idp.hits(); hits != 0 {
		t.Errorf("userinfo called on group overage")
	}
	if log := e.logBuf.String(); !strings.Contains(log, "overage") || !strings.Contains(log, "directory sync") {
		t.Errorf("no overage warning logged:\n%s", log)
	}
}

func TestOIDCRejectsInvalidResponses(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(map[string]any)
		want   string
	}{
		{"nonce mismatch", func(c map[string]any) { c["nonce"] = "other" }, "nonce mismatch"},
		{"wrong audience", func(c map[string]any) { c["aud"] = "another-client" }, "audience"},
		{"expired token", func(c map[string]any) {
			c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
			c["exp"] = time.Now().Add(-time.Hour).Unix()
		}, "expired"},
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://evil.example.com" }, "different provider"},
		{"missing subject", func(c map[string]any) { delete(c, "sub") }, "does not contain a subject"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newOIDCEnv(t)
			e.idp.setUser(map[string]any{"sub": "user-1", "email": "a@example.com"})
			e.idp.setTamper(tc.tamper)
			expectFailure(t, e.login("/"), tc.want)
			if n := e.userCount(); n != 0 {
				t.Errorf("%d users created", n)
			}
		})
	}
}

func TestOIDCBadSignature(t *testing.T) {
	e := newOIDCEnv(t)
	e.idp.setUser(map[string]any{"sub": "user-1"})
	// Sign the ID token with a key that is not in the JWKS.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e.idp.mu.Lock()
	e.idp.signKey = other
	e.idp.mu.Unlock()
	expectFailure(t, e.login("/"), "ID token from the identity provider is not valid")
}

func TestOIDCStateMismatch(t *testing.T) {
	e := newOIDCEnv(t)
	e.idp.setUser(map[string]any{"sub": "user-1"})
	state, authURL := e.start("/")
	cb := e.authorize(authURL)
	q := cb.Query()
	q.Set("state", "forged-state")
	cb.RawQuery = q.Encode()
	expectFailure(t, e.finish(cb, state), "state mismatch")

	// The state cookie of a different sign-in does not match either.
	other, _ := e.start("/")
	expectFailure(t, e.finish(e.authorize(authURL), other), "state mismatch")
}

func TestOIDCProviderError(t *testing.T) {
	e := newOIDCEnv(t)
	state, authURL := e.start("/")
	cb, _ := url.Parse(testBaseURL + OIDCCallbackPath)
	q := url.Values{}
	q.Set("state", authURL.Query().Get("state"))
	q.Set("error", "access_denied")
	q.Set("error_description", "AADSTS50105: The signed in user is not assigned to a role for the application.")
	cb.RawQuery = q.Encode()
	res := e.finish(cb, state)
	expectFailure(t, res, "refused the sign-in (access_denied): AADSTS50105")
}

func TestOIDCMissingOrExpiredStateCookie(t *testing.T) {
	e := newOIDCEnv(t)
	e.idp.setUser(map[string]any{"sub": "user-1"})

	// Missing cookie.
	_, authURL := e.start("/")
	expectFailure(t, e.finish(e.authorize(authURL)), "your sign-in session expired, please try again")

	// Expired cookie (valid encryption, expiry in the past).
	_, authURL = e.start("/")
	cb := e.authorize(authURL)
	value, err := e.m.sealOIDCState(oidcState{
		State: cb.Query().Get("state"), Nonce: "n", Verifier: "v", ReturnTo: "/",
		Expires: time.Now().Add(-time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	expectFailure(t, e.finish(cb, &http.Cookie{Name: OIDCCookie, Value: value}), "expired")

	// Garbage / cookie encrypted with another key.
	expectFailure(t, e.finish(cb, &http.Cookie{Name: OIDCCookie, Value: "not-a-valid-cookie"}), "expired")
	otherKey := make([]byte, 32)
	foreign := &Manager{key: otherKey}
	value, _ = foreign.sealOIDCState(oidcState{State: cb.Query().Get("state"), Expires: time.Now().Add(time.Minute).Unix()})
	expectFailure(t, e.finish(cb, &http.Cookie{Name: OIDCCookie, Value: value}), "expired")
}

func TestOIDCEmailVerifiedFalse(t *testing.T) {
	for _, verified := range []any{false, "false"} {
		e := newOIDCEnv(t)
		u := e.mustLogin(map[string]any{"sub": "user-1", "email": "mallory@example.com", "email_verified": verified, "name": "Mallory"})
		if u.Email != "" {
			t.Errorf("email_verified=%v: e-mail %q stored", verified, u.Email)
		}
	}
	e := newOIDCEnv(t)
	u := e.mustLogin(map[string]any{"sub": "user-2", "email": "ok@example.com", "email_verified": true})
	if u.Email != "ok@example.com" {
		t.Errorf("verified e-mail not stored: %q", u.Email)
	}
}

func TestOIDCReturnToIsLocal(t *testing.T) {
	e := newOIDCEnv(t)
	for _, rt := range []string{"https://evil.example.com/", "//evil.example.com", `/\evil.example.com`, "javascript:alert(1)", ""} {
		e.idp.setUser(map[string]any{"sub": "user-1"})
		res := e.login(rt)
		if res.err != nil {
			t.Fatalf("login: %v", res.err)
		}
		if res.returnTo != "/" {
			t.Errorf("returnTo %q → %q, want /", rt, res.returnTo)
		}
	}
}

func TestOIDCClientCache(t *testing.T) {
	e := newOIDCEnv(t)
	e.mustLogin(map[string]any{"sub": "user-1"})
	e.mustLogin(map[string]any{"sub": "user-1"})
	if d, _ := e.idp.hits(); d != 1 {
		t.Errorf("discovery hits = %d, want 1 (cached)", d)
	}

	e.m.ResetOIDC()
	e.mustLogin(map[string]any{"sub": "user-1"})
	if d, _ := e.idp.hits(); d != 2 {
		t.Errorf("discovery hits after ResetOIDC = %d, want 2", d)
	}

	// Changed settings rebuild the client: the new scopes are used.
	e.saveSettings(func(s *store.Settings) { s.OIDCScopes = "profile, email offline_access" })
	_, authURL := e.start("/")
	if got := authURL.Query().Get("scope"); got != "openid profile email offline_access" {
		t.Errorf("scope = %q", got)
	}
	if d, _ := e.idp.hits(); d != 3 {
		t.Errorf("discovery hits after settings change = %d, want 3", d)
	}

	// A wrong client secret is reported readably.
	e.saveSettings(func(s *store.Settings) { s.OIDCClientSecret = "wrong" })
	e.idp.setUser(map[string]any{"sub": "user-1"})
	expectFailure(t, e.login("/"), "check the client ID and client secret")
}

func TestOIDCDiscoveryFailureNotCached(t *testing.T) {
	e := newOIDCEnv(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	e.saveSettings(func(s *store.Settings) { s.OIDCIssuer = deadURL })
	rec := httptest.NewRecorder()
	err := e.m.StartOIDC(rec, httptest.NewRequest(http.MethodGet, testBaseURL+"/auth/oidc/login", nil), "/")
	if err == nil || !strings.Contains(err.Error(), "could not reach the identity provider at "+deadURL) {
		t.Fatalf("err = %v", err)
	}
	if e.m.oidc != nil {
		t.Error("failed discovery was cached")
	}

	// Issuer that differs from the discovery document.
	e.saveSettings(func(s *store.Settings) { s.OIDCIssuer = e.idp.issuer() + "/tenant" })
	mux := http.NewServeMux()
	other := httptest.NewServer(mux)
	defer other.Close()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"issuer": "https://login.example.com/{tenantid}/v2.0"})
	})
	e.saveSettings(func(s *store.Settings) { s.OIDCIssuer = other.URL })
	err = e.m.StartOIDC(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, testBaseURL+"/auth/oidc/login", nil), "/")
	if err == nil || !strings.Contains(err.Error(), "reports a different issuer") {
		t.Fatalf("err = %v", err)
	}

	// Fixed settings work again.
	e.saveSettings(func(s *store.Settings) {})
	e.mustLogin(map[string]any{"sub": "user-1"})
}

func TestOIDCTrailingSlashIssuer(t *testing.T) {
	e := newOIDCEnv(t)
	e.saveSettings(func(s *store.Settings) { s.OIDCIssuer = e.idp.issuer() + "/" })
	e.mustLogin(map[string]any{"sub": "user-1"})
}

func TestOIDCNotConfigured(t *testing.T) {
	e := newOIDCEnv(t)
	e.saveSettings(func(s *store.Settings) { s.OIDCEnabled = false })
	if e.m.OIDCEnabled(context.Background()) {
		t.Error("OIDCEnabled = true")
	}
	err := e.m.StartOIDC(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil), "/")
	if !errors.Is(err, ErrOIDCNotConfigured) {
		t.Errorf("StartOIDC err = %v", err)
	}
	e.saveSettings(func(s *store.Settings) {})
	if !e.m.OIDCEnabled(context.Background()) {
		t.Error("OIDCEnabled = false")
	}
}

func TestTestOIDC(t *testing.T) {
	e := newOIDCEnv(t)
	ctx := context.Background()
	if err := e.m.TestOIDC(ctx); err != nil {
		t.Errorf("TestOIDC: %v", err)
	}
	e.saveSettings(func(s *store.Settings) { s.OIDCEnabled = false })
	if err := e.m.TestOIDC(ctx); err != nil {
		t.Errorf("TestOIDC with SSO disabled: %v", err)
	}
	e.saveSettings(func(s *store.Settings) { s.OIDCIssuer = "" })
	if err := e.m.TestOIDC(ctx); err == nil || !strings.Contains(err.Error(), "issuer URL") {
		t.Errorf("TestOIDC without issuer: %v", err)
	}
	e.saveSettings(func(s *store.Settings) { s.OIDCIssuer = "not a url" })
	if err := e.m.TestOIDC(ctx); err == nil || !strings.Contains(err.Error(), "not a valid") {
		t.Errorf("TestOIDC with invalid issuer: %v", err)
	}
	e.saveSettings(func(s *store.Settings) { s.OIDCIssuer = e.idp.issuer() + "/nothing-here" })
	if err := e.m.TestOIDC(ctx); err == nil || !strings.Contains(err.Error(), "could not reach") {
		t.Errorf("TestOIDC with wrong issuer: %v", err)
	}
}

func TestCallbackURL(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://status.local:8080/auth/oidc/login", nil)
	if got := CallbackURL(r, ""); got != "http://status.local:8080/auth/oidc/callback" {
		t.Errorf("got %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := CallbackURL(r, ""); got != "https://status.local:8080/auth/oidc/callback" {
		t.Errorf("got %q", got)
	}
	if got := CallbackURL(r, "https://status.example.com"); got != "https://status.example.com/auth/oidc/callback" {
		t.Errorf("got %q", got)
	}
}

func TestOIDCUserFromClaims(t *testing.T) {
	cases := []struct {
		claims map[string]any
		want   store.OIDCUser
	}{
		{map[string]any{"email": "a@example.com", "name": "A"}, store.OIDCUser{Email: "a@example.com", Name: "A"}},
		{map[string]any{"preferred_username": "b@example.com"}, store.OIDCUser{Email: "b@example.com", Name: "b@example.com"}},
		{map[string]any{"preferred_username": "bob", "upn": "bob@corp.example.com", "given_name": "Bob", "family_name": "Builder"},
			store.OIDCUser{Email: "bob@corp.example.com", Name: "Bob Builder"}},
		{map[string]any{"preferred_username": "carol", "given_name": "Carol"}, store.OIDCUser{Name: "Carol"}},
		{map[string]any{"email": "not-an-email", "oid": "oid-9"}, store.OIDCUser{DirectoryID: "oid-9"}},
		{map[string]any{"email": "d@example.com", "email_verified": false}, store.OIDCUser{}},
		{map[string]any{"preferred_username": "d@example.com", "email_verified": "FALSE"}, store.OIDCUser{Name: "d@example.com"}},
	}
	for _, tc := range cases {
		if got := oidcUserFromClaims(tc.claims); got != tc.want {
			t.Errorf("%v → %+v, want %+v", tc.claims, got, tc.want)
		}
	}
}

func TestOIDCScopes(t *testing.T) {
	cases := map[string]string{
		"":                        "openid",
		"profile email":           "openid profile email",
		"openid profile":          "openid profile",
		"email, profile,openid ":  "openid email profile",
		"  profile\temail email ": "openid profile email",
	}
	for in, want := range cases {
		if got := strings.Join(oidcScopes(in), " "); got != want {
			t.Errorf("oidcScopes(%q) = %q, want %q", in, got, want)
		}
	}
}
