// Package mockidp is a minimal in-process OpenID Connect provider for tests.
//
// It serves discovery, a JWKS with one RSA key, an authorization endpoint
// that signs the "current user" in immediately (no login form), a token
// endpoint that checks the client credentials, the redirect URI and the PKCE
// verifier (S256) and issues a signed ID token, and a userinfo endpoint.
//
//	idp := mockidp.New(t)
//	idp.SetUser(mockidp.User{Subject: "alice", Email: "alice@example.com", Groups: []string{"ops"}})
//	// configure the application with idp.Issuer(), idp.ClientID, idp.ClientSecret
//
// It is only meant to be used by tests.
package mockidp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// User is the identity the provider signs in. Empty string fields are left
// out of the ID token.
type User struct {
	Subject string // "sub", required
	Email   string
	Name    string
	OID     string // Microsoft Entra ID object id ("oid")
	// PreferredUsername is the "preferred_username" claim (Microsoft Entra
	// ID: the user principal name).
	PreferredUsername string
	// Groups is the "groups" claim: nil leaves the claim out, an empty
	// (non-nil) slice sends "groups": [].
	Groups []string
	// Roles is the "roles" claim (Microsoft Entra ID: the values of the
	// user's app roles); nil leaves the claim out.
	Roles []string
}

// claims returns the identity claims of u (without iss, aud, exp, …).
func (u User) claims() map[string]any {
	c := map[string]any{"sub": u.Subject}
	set := func(k, v string) {
		if v != "" {
			c[k] = v
		}
	}
	set("email", u.Email)
	set("name", u.Name)
	set("oid", u.OID)
	set("preferred_username", u.PreferredUsername)
	if u.Email != "" {
		c["email_verified"] = true
	}
	if u.Groups != nil {
		c["groups"] = slices.Clone(u.Groups)
	}
	if u.Roles != nil {
		c["roles"] = slices.Clone(u.Roles)
	}
	return c
}

// Provider is a running mock OpenID provider.
type Provider struct {
	// ClientID and ClientSecret are the credentials of the only registered
	// client. Change them only before the first sign-in.
	ClientID     string
	ClientSecret string

	t   testing.TB
	srv *httptest.Server
	key *rsa.PrivateKey

	mu          sync.Mutex
	user        *User
	redirectURI string // if set, the only accepted redirect URI
	codes       map[string]grant
	tokens      map[string]map[string]any // access token → userinfo claims
	logins      int                       // successful token requests
}

// grant is an issued authorization code.
type grant struct {
	redirectURI string
	challenge   string // PKCE S256 code challenge
	nonce       string
	claims      map[string]any
	expires     time.Time
}

// New starts a provider on a random local port; it is stopped when the test
// ends.
func New(t testing.TB) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{
		ClientID: "statuspage", ClientSecret: "mock-client-secret",
		t: t, key: key, codes: map[string]grant{}, tokens: map[string]map[string]any{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /userinfo", p.userinfo)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// Issuer returns the issuer URL (http://127.0.0.1:<port>).
func (p *Provider) Issuer() string { return p.srv.URL }

// SetUser sets the user that is signed in by the following authorization
// requests.
func (p *Provider) SetUser(u User) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.user = &u
}

// RequireRedirectURI makes the authorization endpoint reject every other
// redirect URI, like a provider with a registered redirect URI.
func (p *Provider) RequireRedirectURI(uri string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.redirectURI = uri
}

// Logins returns the number of successful token requests (completed
// sign-ins).
func (p *Provider) Logins() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logins
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *Provider) discovery(w http.ResponseWriter, r *http.Request) {
	iss := p.Issuer()
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                iss + "/authorize",
		"token_endpoint":                        iss + "/token",
		"userinfo_endpoint":                     iss + "/userinfo",
		"jwks_uri":                              iss + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &p.key.PublicKey, KeyID: "mock-key", Algorithm: string(jose.RS256), Use: "sig"},
	}})
}

// authorize validates the request like a real provider and redirects back
// with a code for the current user at once.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p.mu.Lock()
	user, wantRedirect := p.user, p.redirectURI
	p.mu.Unlock()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	problem := ""
	switch {
	case q.Get("redirect_uri") == "" || err != nil || !redirect.IsAbs():
		problem = "invalid redirect_uri"
	case wantRedirect != "" && q.Get("redirect_uri") != wantRedirect:
		problem = "redirect_uri " + q.Get("redirect_uri") + " is not registered (want " + wantRedirect + ")"
	case q.Get("client_id") != p.ClientID:
		problem = "unknown client_id"
	case q.Get("response_type") != "code":
		problem = "response_type must be code"
	case !slices.Contains(strings.Fields(q.Get("scope")), "openid"):
		problem = "scope must contain openid"
	case q.Get("state") == "" || q.Get("nonce") == "":
		problem = "state and nonce are required"
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		problem = "PKCE with S256 is required"
	case user == nil:
		problem = "no user set (call SetUser)"
	}
	if problem != "" {
		p.t.Logf("mockidp: authorization request rejected: %s", problem)
		http.Error(w, problem, http.StatusBadRequest)
		return
	}

	code := randomString()
	p.mu.Lock()
	p.codes[code] = grant{
		redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), nonce: q.Get("nonce"),
		claims: user.claims(), expires: time.Now().Add(time.Minute),
	}
	p.mu.Unlock()
	back := redirect.Query()
	back.Set("code", code)
	back.Set("state", q.Get("state"))
	redirect.RawQuery = back.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// token redeems an authorization code.
func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	fail := func(code, desc string) {
		p.t.Logf("mockidp: token request rejected: %s: %s", code, desc)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": desc})
	}
	if err := r.ParseForm(); err != nil {
		fail("invalid_request", err.Error())
		return
	}
	id, secret, basic := r.BasicAuth()
	if basic { // RFC 6749 2.3.1: form-encoded before base64
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != p.ClientID || secret != p.ClientSecret {
		w.Header().Set("WWW-Authenticate", `Basic realm="mockidp"`)
		p.t.Logf("mockidp: token request rejected: invalid client credentials")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client", "error_description": "invalid client credentials"})
		return
	}
	code := r.PostForm.Get("code")
	p.mu.Lock()
	g, found := p.codes[code]
	delete(p.codes, code) // codes are single-use
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case r.PostForm.Get("grant_type") != "authorization_code":
		fail("unsupported_grant_type", r.PostForm.Get("grant_type"))
		return
	case !found || time.Now().After(g.expires):
		fail("invalid_grant", "unknown, used or expired code")
		return
	case r.PostForm.Get("redirect_uri") != g.redirectURI:
		fail("invalid_grant", "redirect_uri differs from the authorization request")
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge:
		fail("invalid_grant", "PKCE code_verifier does not match the code_challenge")
		return
	}

	now := time.Now()
	claims := map[string]any{
		"iss": p.Issuer(), "aud": p.ClientID, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"nonce": g.nonce,
	}
	for k, v := range g.claims {
		claims[k] = v
	}
	idToken, err := p.sign(claims)
	if err != nil {
		fail("server_error", err.Error())
		return
	}
	access := randomString()
	p.mu.Lock()
	p.tokens[access] = g.claims
	p.logins++
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 3600, "id_token": idToken,
	})
}

// userinfo returns the user's claims for a valid access token.
func (p *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.mu.Lock()
	claims, found := p.tokens[token]
	p.mu.Unlock()
	if !ok || !found {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, "invalid access token", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, claims)
}

// sign returns claims as a JWT signed with the provider's key (RS256).
func (p *Provider) sign(claims map[string]any) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: p.key, KeyID: "mock-key"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return obj.CompactSerialize()
}

func randomString() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
