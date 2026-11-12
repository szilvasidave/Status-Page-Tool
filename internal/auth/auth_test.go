package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// authTestManager returns a Manager with a fresh database and a local
// administrator "admin" with password "correct-password".
func authTestManager(t *testing.T) (*Manager, *store.User) {
	t.Helper()
	st, key := newTestStore(t)
	h, err := HashPassword("correct-password")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateLocalAdmin(context.Background(), "admin", "Administrator", h)
	if err != nil {
		t.Fatal(err)
	}
	return New(st, key, slog.New(slog.NewTextHandler(io.Discard, nil))), u
}

// csrfServer serves a handler behind the middleware that reports the
// identity and CSRF token of the request.
func csrfServer(t *testing.T, m *Manager) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := "anonymous"
		if id := FromContext(r.Context()); id != nil {
			user = id.User.DisplayName() + "/" + id.Role.String()
		}
		_, _ = io.WriteString(w, "ok "+user+" token="+CSRFToken(r))
	})))
	t.Cleanup(srv.Close)
	return srv
}

func authDo(t *testing.T, req *http.Request) (int, string, *http.Response) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp
}

func TestCSRFMiddleware(t *testing.T) {
	m, _ := authTestManager(t)
	srv := csrfServer(t, m)

	// A GET sets the CSRF cookie and exposes the token to the handler.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	status, body, resp := authDo(t, req)
	var token string
	for _, c := range resp.Cookies() {
		if c.Name == CSRFCookie {
			token = c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
				t.Errorf("CSRF cookie attributes: %+v", c)
			}
		}
	}
	if status != http.StatusOK || token == "" || !strings.Contains(body, "token="+token) {
		t.Fatalf("GET: status %d, body %q, token %q", status, body, token)
	}

	post := func(form url.Values, header map[string]string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/x", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: CSRFCookie, Value: token})
		for k, v := range header {
			req.Header.Set(k, v)
		}
		status, body, _ := authDo(t, req)
		return status, body
	}

	cases := []struct {
		name   string
		form   url.Values
		header map[string]string
		want   int
	}{
		{"no token", url.Values{"a": {"1"}}, nil, http.StatusForbidden},
		{"wrong token", url.Values{CSRFField: {"wrong-token-wrong-token"}}, nil, http.StatusForbidden},
		{"valid token", url.Values{CSRFField: {token}}, map[string]string{"Origin": srv.URL}, http.StatusOK},
		{"valid header token", nil, map[string]string{CSRFHeader: token}, http.StatusOK},
		{"foreign origin", url.Values{CSRFField: {token}}, map[string]string{"Origin": "https://evil.example.com"}, http.StatusForbidden},
		{"foreign referer", url.Values{CSRFField: {token}}, map[string]string{"Referer": "https://evil.example.com/page"}, http.StatusForbidden},
		{"same-origin referer", url.Values{CSRFField: {token}}, map[string]string{"Referer": srv.URL + "/page"}, http.StatusOK},
		{"base URL origin", url.Values{CSRFField: {token}}, map[string]string{"Origin": "https://status.example.com"}, http.StatusOK},
	}
	st := store.DefaultSettings()
	st.BaseURL = "https://status.example.com"
	if err := m.store.SaveSettings(context.Background(), &st); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		if status, body := post(tc.form, tc.header); status != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, status, tc.want, strings.TrimSpace(body))
		}
	}

	// A POST without any CSRF cookie is rejected even if it carries a token.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/x", strings.NewReader(url.Values{CSRFField: {token}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if status, _, _ := authDo(t, req); status != http.StatusForbidden {
		t.Errorf("POST without CSRF cookie: status %d", status)
	}
}

func TestLocalLoginAndSession(t *testing.T) {
	m, _ := authTestManager(t)
	srv := csrfServer(t, m)
	ctx := context.Background()

	login := func(user, pw string) (*httptest.ResponseRecorder, error) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		return rec, m.LoginLocal(rec, req, user, pw)
	}

	if _, err := login("admin", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong password: err = %v", err)
	}
	if _, err := login("nobody", "correct-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("unknown user: err = %v", err)
	}
	rec, err := login(" ADMIN ", "correct-password")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			session = c
		}
	}
	if session == nil || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie: %+v", session)
	}

	// The middleware resolves the session to the administrator.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.AddCookie(session)
	if _, body, _ := authDo(t, req); !strings.Contains(body, "ok Administrator/admin") {
		t.Errorf("identity: %q", body)
	}

	// Logout deletes the session.
	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/logout", nil)
	r.AddCookie(session)
	m.Logout(rec, r)
	if _, err := m.store.GetSession(ctx, hashToken(session.Value), time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("session still exists after logout: %v", err)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.AddCookie(session)
	if _, body, _ := authDo(t, req); !strings.Contains(body, "ok anonymous") {
		t.Errorf("identity after logout: %q", body)
	}
}

func TestStartSessionReplacesPreviousSession(t *testing.T) {
	m, admin := authTestManager(t)
	ctx := context.Background()
	rec := httptest.NewRecorder()
	if err := m.StartSession(ctx, rec, httptest.NewRequest(http.MethodGet, "/", nil), admin.ID); err != nil {
		t.Fatal(err)
	}
	first := rec.Result().Cookies()[0]

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(first)
	if err := m.StartSession(ctx, rec, req, admin.ID); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(first)
	if m.loadIdentity(r) != nil {
		t.Error("previous session still valid (session fixation)")
	}
}

func TestLoginLimiter(t *testing.T) {
	m, _ := authTestManager(t)
	try := func(ip, pw string) error {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = ip + ":5555"
		return m.LoginLocal(httptest.NewRecorder(), req, "admin", pw)
	}
	for i := 0; i < limiterMax; i++ {
		if err := try("198.51.100.7", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if err := try("198.51.100.7", "correct-password"); !errors.Is(err, ErrTooManyAttempts) {
		t.Errorf("blocked address with correct password: err = %v", err)
	}
	if err := try("198.51.100.8", "correct-password"); err != nil {
		t.Errorf("other address: err = %v", err)
	}

	// A successful login resets the counter.
	for i := 0; i < limiterMax-1; i++ {
		_ = try("203.0.113.1", "wrong")
	}
	if err := try("203.0.113.1", "correct-password"); err != nil {
		t.Fatalf("login: %v", err)
	}
	for i := 0; i < limiterMax-1; i++ {
		if err := try("203.0.113.1", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("after reset, attempt %d: %v", i+1, err)
		}
	}
}

func TestSafeReturnTo(t *testing.T) {
	cases := map[string]string{
		"":                     "/",
		"/":                    "/",
		"/incidents/3?x=1":     "/incidents/3?x=1",
		"https://evil.example": "/",
		"//evil.example":       "/",
		`/\evil.example`:       "/",
		"relative":             "/",
		"/a\r\nSet-Cookie: x":  "/",
	}
	for in, want := range cases {
		if got := SafeReturnTo(in); got != want {
			t.Errorf("SafeReturnTo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("a-long-password")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "a-long-password") || CheckPassword(h, "another-password") || CheckPassword("", "") {
		t.Error("CheckPassword gives wrong results")
	}
}
