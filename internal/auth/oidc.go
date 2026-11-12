package auth

import (
	"context"
	"errors"
	"net/http"
)

// ErrOIDCNotConfigured is returned when single sign-on is not set up.
var ErrOIDCNotConfigured = errors.New("single sign-on is not configured")

// OIDCCallbackPath is the redirect URI path registered at the identity
// provider: <BaseURL>/auth/oidc/callback.
const OIDCCallbackPath = "/auth/oidc/callback"

// oidcClient caches the discovered provider and oauth2 config for the
// current settings. TODO(oidc): implement.
type oidcClient struct{}

// OIDCEnabled reports whether single sign-on is enabled and configured.
func (m *Manager) OIDCEnabled(ctx context.Context) bool {
	st, err := m.store.GetSettings(ctx)
	return err == nil && st.OIDCConfigured()
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
	return ErrOIDCNotConfigured
}

// FinishOIDC handles the redirect back from the identity provider: verifies
// state, exchanges the code, verifies the ID token, updates the user and its
// groups, starts a session and returns the local path to redirect to.
func (m *Manager) FinishOIDC(w http.ResponseWriter, r *http.Request) (string, error) {
	return "", ErrOIDCNotConfigured
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
