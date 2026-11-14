package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/szilvasidave/status-page-tool/internal/msgraph"
	"github.com/szilvasidave/status-page-tool/internal/secret"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// ErrOIDCNotConfigured is returned when single sign-on is not set up.
var ErrOIDCNotConfigured = errors.New("single sign-on is not configured")

// errOIDCExpired is returned by FinishOIDC when the state cookie is missing,
// invalid or expired.
var errOIDCExpired = errors.New("your sign-in session expired, please try again")

// OIDCCallbackPath is the redirect URI path registered at the identity
// provider: <BaseURL>/auth/oidc/callback.
const OIDCCallbackPath = "/auth/oidc/callback"

const (
	// OIDCCookie holds the encrypted state of a sign-in in progress.
	OIDCCookie = "sp_oidc"

	oidcCookiePath    = "/auth/oidc"
	oidcStateLifetime = 10 * time.Minute
	oidcTimeout       = 15 * time.Second
)

// oidcHTTPClient is used for all requests to the identity provider
// (discovery, keys, token exchange, userinfo).
var oidcHTTPClient = &http.Client{Timeout: oidcTimeout}

// oidcKey identifies the settings an oidcClient was built for.
type oidcKey struct {
	issuer, clientID, clientSecret, scopes, redirectURL string
}

// oidcClient caches the discovered provider and oauth2 config for the
// current settings. It is rebuilt when the settings change or after
// ResetOIDC; a failed discovery is not cached.
type oidcClient struct {
	key      oidcKey
	provider *oidc.Provider
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// oidcState is stored encrypted in the OIDCCookie between StartOIDC and
// FinishOIDC.
type oidcState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"` // PKCE code verifier
	ReturnTo string `json:"r"`
	Expires  int64  `json:"e"` // unix seconds
}

// ResetOIDC drops the cached provider so that changed settings take effect.
func (m *Manager) ResetOIDC() {
	m.oidcMu.Lock()
	m.oidc = nil
	m.oidcMu.Unlock()
}

// StartOIDC redirects the browser to the identity provider. returnTo is the
// local path to go to after a successful login.
func (m *Manager) StartOIDC(w http.ResponseWriter, r *http.Request, returnTo string) error {
	ctx := r.Context()
	st, err := m.store.GetSettings(ctx)
	if err != nil {
		return err
	}
	if !st.OIDCConfigured() {
		return ErrOIDCNotConfigured
	}
	c, err := m.oidcClientFor(ctx, st, CallbackURL(r, st.BaseURL))
	if err != nil {
		return err
	}

	s := oidcState{
		State:    secret.RandomToken(24),
		Nonce:    secret.RandomToken(24),
		Verifier: oauth2.GenerateVerifier(),
		ReturnTo: SafeReturnTo(returnTo),
		Expires:  time.Now().Add(oidcStateLifetime).Unix(),
	}
	value, err := m.sealOIDCState(s)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: OIDCCookie, Value: value, Path: oidcCookiePath,
		MaxAge:   int(oidcStateLifetime.Seconds()),
		HttpOnly: true, Secure: IsHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	target := c.oauth.AuthCodeURL(s.State, oidc.Nonce(s.Nonce), oauth2.S256ChallengeOption(s.Verifier))
	http.Redirect(w, r, target, http.StatusFound)
	return nil
}

// FinishOIDC handles the redirect back from the identity provider: verifies
// state, exchanges the code, verifies the ID token, updates the user (with
// its app roles) and its groups, starts a session and returns the local path
// to redirect to.
func (m *Manager) FinishOIDC(w http.ResponseWriter, r *http.Request) (string, error) {
	ctx := r.Context()
	saved, ok := m.readOIDCState(r)
	http.SetCookie(w, &http.Cookie{
		Name: OIDCCookie, Value: "", Path: oidcCookiePath, MaxAge: -1,
		HttpOnly: true, Secure: IsHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	if !ok {
		return "", errOIDCExpired
	}

	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(saved.State)) != 1 {
		return "", errors.New("the response of the identity provider does not belong to this sign-in (state mismatch), please try again")
	}
	if code := q.Get("error"); code != "" {
		msg := "the identity provider refused the sign-in (" + brief(code) + ")"
		if desc := q.Get("error_description"); desc != "" {
			msg += ": " + brief(desc)
		}
		return "", errors.New(msg)
	}
	code := q.Get("code")
	if code == "" {
		return "", errors.New("the identity provider did not return an authorization code")
	}

	st, err := m.store.GetSettings(ctx)
	if err != nil {
		return "", err
	}
	if !st.OIDCConfigured() {
		return "", ErrOIDCNotConfigured
	}
	c, err := m.oidcClientFor(ctx, st, CallbackURL(r, st.BaseURL))
	if err != nil {
		return "", err
	}

	// Redeem the code and verify the ID token.
	hctx := oidc.ClientContext(ctx, oidcHTTPClient)
	tok, err := c.oauth.Exchange(hctx, code, oauth2.VerifierOption(saved.Verifier))
	if err != nil {
		return "", exchangeError(err)
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return "", errors.New("the identity provider did not return an ID token")
	}
	idt, err := c.verifier.Verify(ctx, rawID)
	if err != nil {
		var expired *oidc.TokenExpiredError
		if errors.As(err, &expired) {
			return "", errors.New("the ID token from the identity provider has expired; check that the clock of this server is correct")
		}
		return "", fmt.Errorf("the ID token from the identity provider is not valid: %s", brief(err.Error()))
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(saved.Nonce)) != 1 {
		return "", errors.New("the ID token does not belong to this sign-in (nonce mismatch), please try again")
	}
	if strings.TrimSpace(idt.Subject) == "" {
		return "", errors.New("the ID token from the identity provider does not contain a subject (sub claim)")
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return "", fmt.Errorf("could not read the ID token: %s", brief(err.Error()))
	}

	user := oidcUserFromClaims(claims)
	user.Subject = idt.Subject

	groupsClaim := claimName(st.OIDCGroupsClaim, "groups")
	rolesClaim := claimName(st.OIDCRolesClaim, "roles")
	// Without a groups claim the user is in no groups (providers leave an
	// empty claim out), unless the groups are unknown: Entra ID signals that
	// they did not fit into the token (overage), or userinfo failed.
	groups, found := m.claimStrings(claims, groupsClaim, claimGroups)
	overage := groupOverage(claims, groupsClaim)
	// Without a roles claim the user has no app roles; also when they are
	// unknown because userinfo failed, as they grant roles (fail closed).
	// They are recorded even while roles from app roles are off, so that
	// turning it on uses those of the last sign-in, not older ones; only then
	// is userinfo read for them, and not with Entra ID (rolesFromUserinfo).
	roles, rolesFound := m.claimStrings(claims, rolesClaim, claimRoles)
	needGroups := !found && !overage
	needRoles := !rolesFound && st.OIDCAppRoles && rolesFromUserinfo(st.OIDCIssuer, c.provider.UserInfoEndpoint())
	infoFailed := false
	if (needGroups || needRoles) && c.provider.UserInfoEndpoint() != "" {
		info, err := m.userInfo(hctx, c, tok, idt.Subject)
		if err != nil {
			infoFailed = needGroups
			msg := "OIDC: could not read the userinfo endpoint; group memberships are left unchanged"
			switch {
			case needGroups && needRoles:
				msg += " and this sign-in gives no app roles"
			case needRoles:
				msg = "OIDC: could not read the userinfo endpoint for the roles claim; this sign-in gives no app roles"
			}
			m.log.Warn(msg, "subject", idt.Subject, "err", err)
		} else {
			if needGroups {
				groups, found = m.claimStrings(info, groupsClaim, claimGroups)
				overage = groupOverage(info, groupsClaim)
			}
			if !rolesFound {
				roles, _ = m.claimStrings(info, rolesClaim, claimRoles)
			}
			fromInfo := oidcUserFromClaims(info)
			if user.Email == "" {
				user.Email = fromInfo.Email
			}
			if user.Name == "" {
				user.Name = fromInfo.Name
			}
			if user.DirectoryID == "" {
				user.DirectoryID = fromInfo.DirectoryID
			}
			if user.PrincipalName == "" {
				user.PrincipalName = fromInfo.PrincipalName
			}
		}
	}

	user.AppRoles = roles
	u, err := m.store.UpsertOIDCUser(ctx, user)
	if errors.Is(err, store.ErrUserDeactivated) {
		m.log.Info("OIDC: sign-in refused: the user was deactivated by provisioning", "subject", idt.Subject,
			"user", user.PrincipalName, "ip", ClientIP(r))
		return "", err
	}
	if err != nil {
		return "", err
	}
	switch {
	case overage:
		m.log.Warn("OIDC: the identity provider did not include the groups in the token because the user is a member of too many groups (group overage); group memberships were not changed. With SCIM provisioning from Microsoft Entra ID the members of the provisioned groups are kept up to date regardless.",
			"user", u.DisplayName(), "claim", groupsClaim)
	case infoFailed:
		// Logged above.
	default:
		if !found {
			m.log.Info("OIDC: no groups claim in the token or userinfo; the user is in no groups",
				"user", u.DisplayName(), "claim", groupsClaim)
		}
		refs := make([]store.GroupRef, 0, len(groups))
		for _, g := range groups {
			refs = append(refs, store.GroupRef{ExternalID: g})
		}
		// The memberships of provisioned groups are left to provisioning.
		if err := m.store.SetUserGroups(ctx, u.ID, refs); err != nil {
			return "", err
		}
	}

	if err := m.StartSession(ctx, w, r, u.ID); err != nil {
		return "", err
	}
	m.log.Info("OIDC login", "user", u.DisplayName(), "email", u.Email, "groups", len(groups), "app_roles", u.AppRoles, "ip", ClientIP(r))
	return SafeReturnTo(saved.ReturnTo), nil
}

// CallbackURL returns the absolute redirect URI for the identity provider,
// based on the configured base URL (or the request host if not configured).
func CallbackURL(r *http.Request, baseURL string) string {
	if baseURL == "" {
		scheme := "http"
		if IsHTTPS(r) {
			scheme = "https"
		}
		baseURL = scheme + "://" + r.Host
	}
	return baseURL + OIDCCallbackPath
}

// ------------------------------------------------------------------ client

// oidcClientFor returns the cached client for the settings or discovers the
// provider and builds a new one.
func (m *Manager) oidcClientFor(ctx context.Context, st *store.Settings, redirectURL string) (*oidcClient, error) {
	k := oidcKey{
		issuer:       strings.TrimSpace(st.OIDCIssuer),
		clientID:     strings.TrimSpace(st.OIDCClientID),
		clientSecret: st.OIDCClientSecret,
		scopes:       strings.Join(oidcScopes(st.OIDCScopes), " "),
		redirectURL:  redirectURL,
	}
	m.oidcMu.Lock()
	c := m.oidc
	m.oidcMu.Unlock()
	if c != nil && c.key == k {
		return c, nil
	}

	// Discover outside the lock: it may take a while and must not block
	// ResetOIDC. Concurrent discoveries are harmless.
	p, err := discoverOIDC(ctx, k.issuer)
	if err != nil {
		return nil, err
	}
	c = &oidcClient{
		key:      k,
		provider: p,
		oauth: &oauth2.Config{
			ClientID:     k.clientID,
			ClientSecret: k.clientSecret,
			Endpoint:     p.Endpoint(),
			RedirectURL:  redirectURL,
			Scopes:       strings.Fields(k.scopes),
		},
		verifier: p.Verifier(&oidc.Config{ClientID: k.clientID}),
	}
	m.oidcMu.Lock()
	m.oidc = c
	m.oidcMu.Unlock()
	return c, nil
}

// discoverOIDC fetches the discovery document of issuer.
func discoverOIDC(ctx context.Context, issuer string) (*oidc.Provider, error) {
	if u, err := url.Parse(issuer); issuer == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("the issuer URL %q is not a valid http(s) URL", issuer)
	}
	ctx, cancel := context.WithTimeout(ctx, oidcTimeout)
	defer cancel()
	ctx = oidc.ClientContext(ctx, oidcHTTPClient)

	p, err := oidc.NewProvider(ctx, issuer)
	var mismatch *oidc.IssuerMismatchError
	if errors.As(err, &mismatch) {
		if strings.TrimRight(mismatch.Discovered, "/") != strings.TrimRight(issuer, "/") {
			return nil, fmt.Errorf("the identity provider at %s reports a different issuer (%q); enter the issuer URL exactly as the provider reports it (Microsoft Entra ID: https://login.microsoftonline.com/<tenant ID>/v2.0)",
				issuer, mismatch.Discovered)
		}
		// Only a trailing slash differs: use the provider's spelling.
		p, err = oidc.NewProvider(ctx, mismatch.Discovered)
	}
	if err != nil {
		return nil, fmt.Errorf("could not reach the identity provider at %s: %s", issuer, brief(err.Error()))
	}
	return p, nil
}

// oidcScopes parses the configured scopes (separated by spaces or commas)
// and makes sure "openid" is included.
func oidcScopes(s string) []string {
	out := []string{oidc.ScopeOpenID}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// userInfo fetches the userinfo claims with the access token. The claims are
// only used if they belong to the subject of the ID token.
func (m *Manager) userInfo(ctx context.Context, c *oidcClient, tok *oauth2.Token, subject string) (map[string]any, error) {
	info, err := c.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		return nil, errors.New(brief(err.Error()))
	}
	var claims map[string]any
	if err := info.Claims(&claims); err != nil {
		return nil, errors.New(brief(err.Error()))
	}
	if info.Subject != subject {
		return nil, fmt.Errorf("the userinfo subject %q differs from the ID token subject", info.Subject)
	}
	return claims, nil
}

// exchangeError turns an error of the token request into a readable error.
func exchangeError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode != "" {
		msg := re.ErrorCode
		if re.ErrorDescription != "" {
			msg += ": " + re.ErrorDescription
		}
		hint := ""
		if re.ErrorCode == "invalid_client" || re.ErrorCode == "unauthorized_client" {
			hint = "; check the client ID and client secret in the settings"
		}
		return fmt.Errorf("the identity provider did not accept the sign-in (%s)%s", brief(msg), hint)
	}
	return fmt.Errorf("could not complete the sign-in with the identity provider: %s", brief(err.Error()))
}

// brief returns a short single-line message (without the "oidc: " prefixes
// of the library), suitable for showing to users.
func brief(s string) string {
	s = strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "oidc: ", "")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return s
}

// ------------------------------------------------------------------ state cookie

func (m *Manager) sealOIDCState(s oidcState) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sealed, err := secret.Seal(m.key, b)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// readOIDCState decrypts the state cookie. ok is false if it is missing,
// invalid or expired.
func (m *Manager) readOIDCState(r *http.Request) (s oidcState, ok bool) {
	c, err := r.Cookie(OIDCCookie)
	if err != nil || c.Value == "" {
		return s, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return s, false
	}
	b, err := secret.Open(m.key, raw)
	if err != nil {
		return s, false
	}
	if err := json.Unmarshal(b, &s); err != nil || s.State == "" || time.Now().Unix() > s.Expires {
		return oidcState{}, false
	}
	return s, true
}

// ------------------------------------------------------------------ claims

// oidcUserFromClaims extracts e-mail, name, directory id and user name from
// ID token or userinfo claims (Subject is not set).
func oidcUserFromClaims(c map[string]any) store.OIDCUser {
	str := func(k string) string {
		s, _ := c[k].(string)
		return strings.TrimSpace(s)
	}
	var u store.OIDCUser
	for _, k := range []string{"email", "preferred_username", "upn"} {
		if v := str(k); looksLikeEmail(v) {
			u.Email = v
			break
		}
	}
	if isFalse(c["email_verified"]) {
		u.Email = "" // never trust an unverified address: it is used to match accounts
	}
	u.Name = str("name")
	if u.Name == "" {
		u.Name = strings.TrimSpace(str("given_name") + " " + str("family_name"))
	}
	if u.Name == "" {
		u.Name = str("preferred_username")
	}
	u.DirectoryID = str("oid") // Microsoft Entra ID object id
	// The user name, as provisioning (SCIM userName) knows it: Entra ID puts
	// the user principal name into preferred_username (v2.0) or upn (v1.0).
	u.PrincipalName = str("preferred_username")
	if u.PrincipalName == "" {
		u.PrincipalName = str("upn")
	}
	return u
}

// looksLikeEmail is a deliberately loose check for "local@domain.tld".
func looksLikeEmail(s string) bool {
	local, domain, ok := strings.Cut(s, "@")
	return ok && local != "" && strings.Contains(domain, ".") && !strings.ContainsAny(domain, "@") &&
		!strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".") &&
		!strings.ContainsFunc(s, unicode.IsSpace)
}

// isFalse reports whether a claim is the boolean false (or the string
// "false", as sent by some providers).
func isFalse(v any) bool {
	switch b := v.(type) {
	case bool:
		return !b
	case string:
		return strings.EqualFold(strings.TrimSpace(b), "false")
	}
	return false
}

// rolesFromUserinfo reports whether the roles claim is read from the
// userinfo endpoint when the ID token has none. Not with Microsoft Entra ID:
// its ID token holds all app roles of the user (a missing claim means none),
// and its userinfo endpoint, on Microsoft Graph, never returns them; asking
// it would only delay every sign-in of a user without app roles.
func rolesFromUserinfo(issuer, userinfoEndpoint string) bool {
	if _, err := msgraph.Tenant(issuer); err == nil {
		return false
	}
	u, err := url.Parse(userinfoEndpoint)
	return err != nil || !strings.EqualFold(u.Hostname(), "graph.microsoft.com")
}

// claimName returns the configured claim name, or def if none is set.
func claimName(configured, def string) string {
	if name := strings.TrimSpace(configured); name != "" {
		return name
	}
	return def
}

// What a claim read by claimStrings holds.
const (
	claimGroups = "groups"
	claimRoles  = "roles"
)

// claimStrings returns the values of a claim that is an array of strings or
// a single string. found is false if the claim is absent. A claim of another
// type is logged (what: claimGroups or claimRoles) and counts as an empty
// list.
func (m *Manager) claimStrings(c map[string]any, name, what string) (values []string, found bool) {
	switch v := c[name].(type) {
	case nil:
		return nil, false
	case string:
		if v = strings.TrimSpace(v); v != "" {
			values = append(values, v)
		}
		return values, true
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				values = append(values, strings.TrimSpace(s))
			}
		}
		return values, true
	default:
		consequence := "the user is treated as a member of no groups"
		if what == claimRoles {
			consequence = "the user is treated as having no app roles"
		}
		m.log.Warn("OIDC: the "+what+" claim is neither a list of strings nor a string; "+consequence,
			"claim", name, "type", fmt.Sprintf("%T", v))
		return nil, true
	}
}

// groupOverage reports whether Microsoft Entra ID left out the groups claim
// because the user is a member of too many groups (the claim is then
// referenced in "_claim_names" or signalled by "hasgroups").
func groupOverage(c map[string]any, name string) bool {
	if names, ok := c["_claim_names"].(map[string]any); ok {
		if _, ok := names[name]; ok {
			return true
		}
	}
	has, _ := c["hasgroups"].(bool)
	return has
}
