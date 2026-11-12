// Package auth implements sessions, CSRF protection, local (break-glass)
// administrator login and OpenID Connect single sign-on.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/szilvasidave/status-page-tool/internal/secret"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

const (
	SessionCookie   = "sp_session"
	CSRFCookie      = "sp_csrf"
	CSRFField       = "csrf_token"
	CSRFHeader      = "X-CSRF-Token"
	SessionLifetime = 12 * time.Hour
	// MaxFormBytes limits the size of request bodies.
	MaxFormBytes = 1 << 20
)

// ErrInvalidCredentials is returned for a wrong username or password.
var ErrInvalidCredentials = errors.New("invalid username or password")

// ErrTooManyAttempts is returned when an address failed to log in too often.
var ErrTooManyAttempts = errors.New("too many failed login attempts, please wait a few minutes")

// Identity is the signed-in user of a request.
type Identity struct {
	User      *store.User
	Role      store.Role
	SessionID string
}

// Has reports whether the identity has at least role r. Safe on nil.
func (i *Identity) Has(r store.Role) bool { return i != nil && i.Role >= r }

type ctxKey int

const (
	identityKey ctxKey = iota
	csrfKey
)

// WithIdentity returns a context carrying id (used by the middleware and tests).
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// FromContext returns the signed-in identity or nil for anonymous requests.
func FromContext(ctx context.Context) *Identity {
	id, _ := ctx.Value(identityKey).(*Identity)
	return id
}

// CSRFToken returns the CSRF token to embed in forms (hidden input named
// CSRFField) for the current request.
func CSRFToken(r *http.Request) string {
	t, _ := r.Context().Value(csrfKey).(string)
	return t
}

// WithCSRFToken returns a context carrying a CSRF token (tests).
func WithCSRFToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, csrfKey, token)
}

// Manager handles sessions and logins.
type Manager struct {
	store *store.Store
	key   []byte
	log   *slog.Logger

	limiter *loginLimiter

	oidcMu sync.Mutex
	oidc   *oidcClient // cached OIDC client, see oidc.go
}

// New creates a Manager. key is the 32 byte secret key from the config.
func New(st *store.Store, key []byte, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{store: st, key: key, log: log, limiter: newLoginLimiter()}
}

// Middleware loads the session, puts the Identity and CSRF token into the
// request context and rejects state-changing requests (POST, PUT, PATCH,
// DELETE) without a valid CSRF token or with a foreign Origin.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		token := ""
		if c, err := r.Cookie(CSRFCookie); err == nil && len(c.Value) >= 20 {
			token = c.Value
		} else {
			token = secret.RandomToken(24)
			http.SetCookie(w, &http.Cookie{
				Name: CSRFCookie, Value: token, Path: "/",
				HttpOnly: true, Secure: IsHTTPS(r), SameSite: http.SameSiteLaxMode,
			})
		}
		ctx = context.WithValue(ctx, csrfKey, token)

		if !strings.HasPrefix(r.URL.Path, "/static/") {
			if id := m.loadIdentity(r); id != nil {
				ctx = WithIdentity(ctx, id)
			}
		}
		r = r.WithContext(ctx)

		if !isSafeMethod(r.Method) {
			r.Body = http.MaxBytesReader(w, r.Body, MaxFormBytes)
			if !m.sameOrigin(r) {
				http.Error(w, "Forbidden: cross-origin request", http.StatusForbidden)
				return
			}
			sent := r.Header.Get(CSRFHeader)
			if sent == "" {
				sent = r.PostFormValue(CSRFField)
			}
			if sent == "" || subtle.ConstantTimeCompare([]byte(sent), []byte(token)) != 1 {
				http.Error(w, "Forbidden: invalid or missing CSRF token. Please reload the page and try again.", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameOrigin checks the Origin (or Referer) header, if the browser sent one,
// against the request host and the configured base URL.
func (m *Manager) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return true // non-browser client; the CSRF token still applies
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if st, err := m.store.GetSettings(r.Context()); err == nil && st.BaseURL != "" {
		if b, err := url.Parse(st.BaseURL); err == nil && strings.EqualFold(b.Host, u.Host) {
			return true
		}
	}
	return false
}

func (m *Manager) loadIdentity(r *http.Request) *Identity {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	ctx := r.Context()
	sess, err := m.store.GetSession(ctx, hashToken(c.Value), time.Now())
	if err != nil {
		return nil
	}
	user, err := m.store.GetUser(ctx, sess.UserID)
	if err != nil {
		return nil
	}
	role, err := m.store.UserRole(ctx, user.ID)
	if err != nil {
		m.log.Error("load user role", "user", user.ID, "err", err)
		return nil
	}
	return &Identity{User: user, Role: role, SessionID: sess.ID}
}

// StartSession creates a new session for userID and sets the session cookie.
func (m *Manager) StartSession(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64) error {
	// Drop a previous session of this browser (session fixation).
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		_ = m.store.DeleteSession(ctx, hashToken(c.Value))
	}
	token := secret.RandomToken(32)
	now := time.Now()
	sess := &store.Session{ID: hashToken(token), UserID: userID, CreatedAt: now, ExpiresAt: now.Add(SessionLifetime)}
	if err := m.store.CreateSession(ctx, sess); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/",
		MaxAge:   int(SessionLifetime.Seconds()),
		HttpOnly: true, Secure: IsHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// Logout deletes the current session and clears the cookie.
func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		if err := m.store.DeleteSession(r.Context(), hashToken(c.Value)); err != nil {
			m.log.Error("delete session", "err", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: IsHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
}

// LoginLocal verifies a local administrator's credentials and starts a
// session. Returns ErrInvalidCredentials or ErrTooManyAttempts on failure.
func (m *Manager) LoginLocal(w http.ResponseWriter, r *http.Request, username, password string) error {
	ip := ClientIP(r)
	if !m.limiter.allow(ip) {
		return ErrTooManyAttempts
	}
	ctx := r.Context()
	user, err := m.store.GetUserByUsername(ctx, strings.TrimSpace(username))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		CheckPassword(dummyHash, password) // keep timing similar
		m.limiter.fail(ip)
		return ErrInvalidCredentials
	}
	if user.PasswordHash == "" || !CheckPassword(user.PasswordHash, password) {
		m.limiter.fail(ip)
		return ErrInvalidCredentials
	}
	m.limiter.reset(ip)
	return m.StartSession(ctx, w, r, user.ID)
}

// HashPassword hashes a password with bcrypt.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword compares a bcrypt hash with a password.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// MinPasswordLength is enforced for local accounts.
const MinPasswordLength = 10

var dummyHash, _ = HashPassword("not-a-real-password-just-for-timing")

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// IsHTTPS reports whether the request reached us over HTTPS (directly or via
// a reverse proxy that sets X-Forwarded-Proto).
func IsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ClientIP returns the remote IP address of the request (without port).
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// SafeReturnTo returns p if it is a local absolute path ("/..."), else "/".
// Used to validate ?next= parameters and prevent open redirects.
func SafeReturnTo(p string) string {
	if p == "" || p[0] != '/' || strings.HasPrefix(p, "//") || strings.HasPrefix(p, `/\`) || strings.ContainsAny(p, "\r\n") {
		return "/"
	}
	return p
}

// ------------------------------------------------------------------ limiter

type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

const (
	limiterWindow = 15 * time.Minute
	limiterMax    = 10
)

func newLoginLimiter() *loginLimiter { return &loginLimiter{failures: map[string][]time.Time{}} }

func (l *loginLimiter) prune(ip string, now time.Time) []time.Time {
	list := l.failures[ip]
	i := 0
	for i < len(list) && now.Sub(list[i]) > limiterWindow {
		i++
	}
	list = list[i:]
	if len(list) == 0 {
		delete(l.failures, ip)
	} else {
		l.failures[ip] = list
	}
	return list
}

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(ip, time.Now())) < limiterMax
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.failures[ip] = append(l.prune(ip, now), now)
	if len(l.failures) > 10000 { // bound memory
		for k := range l.failures {
			l.prune(k, now)
		}
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}
