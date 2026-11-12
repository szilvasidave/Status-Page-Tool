package mockidp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// login runs the authorization code flow with PKCE like a relying party and
// returns the verified ID token claims.
func login(t *testing.T, p *Provider, verifierOverride string) (map[string]any, error) {
	t.Helper()
	ctx := context.Background()
	provider, err := oidc.NewProvider(ctx, p.Issuer())
	if err != nil {
		t.Fatal(err)
	}
	conf := &oauth2.Config{
		ClientID: p.ClientID, ClientSecret: p.ClientSecret, Endpoint: provider.Endpoint(),
		RedirectURL: "http://127.0.0.1:1/callback", Scopes: []string{oidc.ScopeOpenID, "email"},
	}
	verifier := oauth2.GenerateVerifier()
	authURL := conf.AuthCodeURL("the-state", oidc.Nonce("the-nonce"), oauth2.S256ChallengeOption(verifier))
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: status %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("state") != "the-state" || loc.Path != "/callback" {
		t.Fatalf("authorize redirect: %s", loc)
	}
	if verifierOverride != "" {
		verifier = verifierOverride
	}
	tok, err := conf.Exchange(ctx, loc.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, err
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := provider.Verifier(&oidc.Config{ClientID: p.ClientID}).Verify(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if idt.Nonce != "the-nonce" {
		t.Fatalf("nonce = %q", idt.Nonce)
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		t.Fatal(err)
	}
	info, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		t.Fatal(err)
	}
	if info.Subject != idt.Subject {
		t.Fatalf("userinfo subject %q, ID token subject %q", info.Subject, idt.Subject)
	}
	return claims, nil
}

func TestFlow(t *testing.T) {
	p := New(t)
	p.SetUser(User{Subject: "u1", Email: "u1@example.com", Name: "User One", OID: "oid-1", Groups: []string{"a", "b"}})
	c, err := login(t, p, "")
	if err != nil {
		t.Fatal(err)
	}
	if c["sub"] != "u1" || c["email"] != "u1@example.com" || c["name"] != "User One" || c["oid"] != "oid-1" {
		t.Fatalf("claims: %v", c)
	}
	if g, _ := c["groups"].([]any); len(g) != 2 || g[0] != "a" || g[1] != "b" {
		t.Fatalf("groups: %v", c["groups"])
	}

	// Empty groups are sent as [], nil groups leave the claim out.
	p.SetUser(User{Subject: "u2", Groups: []string{}})
	c, _ = login(t, p, "")
	if g, ok := c["groups"].([]any); !ok || len(g) != 0 {
		t.Fatalf("empty groups: %#v", c["groups"])
	}
	p.SetUser(User{Subject: "u3"})
	c, _ = login(t, p, "")
	if _, ok := c["groups"]; ok {
		t.Fatalf("groups claim present: %v", c)
	}
	if p.Logins() != 3 {
		t.Fatalf("logins = %d", p.Logins())
	}
}

func TestRejectsWrongVerifierAndSecret(t *testing.T) {
	p := New(t)
	p.SetUser(User{Subject: "u1"})
	if _, err := login(t, p, oauth2.GenerateVerifier()); err == nil {
		t.Fatal("wrong PKCE verifier accepted")
	}

	ctx := context.Background()
	provider, err := oidc.NewProvider(ctx, p.Issuer())
	if err != nil {
		t.Fatal(err)
	}
	conf := &oauth2.Config{ClientID: p.ClientID, ClientSecret: "wrong", Endpoint: provider.Endpoint(), RedirectURL: "http://127.0.0.1:1/cb"}
	_, err = conf.Exchange(ctx, "no-such-code")
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) || re.ErrorCode != "invalid_client" {
		t.Fatalf("wrong client secret: err = %v, want invalid_client", err)
	}
}

func TestRequireRedirectURI(t *testing.T) {
	p := New(t)
	p.SetUser(User{Subject: "u1"})
	p.RequireRedirectURI("https://status.example.com/auth/oidc/callback")
	resp, err := http.Get(p.Issuer() + "/authorize?" + url.Values{
		"client_id": {p.ClientID}, "response_type": {"code"}, "scope": {"openid"}, "state": {"s"}, "nonce": {"n"},
		"code_challenge": {"c"}, "code_challenge_method": {"S256"}, "redirect_uri": {"https://evil.example.com/cb"},
	}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}
