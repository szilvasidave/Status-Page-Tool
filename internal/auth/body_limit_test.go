package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// bodyTestToken is the CSRF token (cookie and form field) of the body limit
// tests; the middleware accepts any cookie value of at least 20 characters.
const bodyTestToken = "body-limit-test-csrf-token"

// bodyLimitServer serves a handler behind the middleware that reports what
// it gets: the size of the uploaded file "logo" if the middleware parsed a
// multipart form, else the result of reading the body itself.
func bodyLimitServer(t *testing.T, m *Manager) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.MultipartForm != nil {
			if fhs := r.MultipartForm.File["logo"]; len(fhs) == 1 {
				fmt.Fprintf(w, "ok file=%d", fhs[0].Size)
				return
			}
			_, _ = io.WriteString(w, "ok no file")
			return
		}
		b, err := io.ReadAll(r.Body)
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			_, _ = io.WriteString(w, "ok read=too-large")
		case err != nil:
			fmt.Fprintf(w, "ok read=%v", err)
		default:
			fmt.Fprintf(w, "ok read=%d note=%d", len(b), len(r.PostForm.Get("note")))
		}
	})))
	t.Cleanup(srv.Close)
	return srv
}

// bodyMultipart returns a multipart form with the CSRF token and a file
// "logo" of size bytes, and its content type.
func bodyMultipart(t *testing.T, token string, size int) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField(CSRFField, token); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("logo", "logo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte{'x'}, size)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// bodyURLEncoded returns an urlencoded form with the CSRF token and a field
// "note" of size bytes, and its content type.
func bodyURLEncoded(token string, size int) ([]byte, string) {
	form := url.Values{CSRFField: {token}, "note": {strings.Repeat("x", size)}}
	return []byte(form.Encode()), "application/x-www-form-urlencoded"
}

// bodyRequest is one request of the body limit tests.
type bodyRequest struct {
	method, path string
	body         []byte
	contentType  string
	chunked      bool              // no Content-Length (Transfer-Encoding: chunked)
	header       map[string]string // extra headers
}

// bodySend sends req with the CSRF cookie and returns the status and body.
func bodySend(t *testing.T, srv *httptest.Server, br bodyRequest) (int, string) {
	t.Helper()
	method := br.method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequest(method, srv.URL+br.path, bytes.NewReader(br.body))
	if err != nil {
		t.Fatal(err)
	}
	if br.chunked {
		req.Body = io.NopCloser(bytes.NewReader(br.body))
		req.ContentLength = -1
	}
	req.Header.Set("Content-Type", br.contentType)
	req.Header.Set("Origin", srv.URL)
	for k, v := range br.header {
		req.Header.Set(k, v)
	}
	req.AddCookie(&http.Cookie{Name: CSRFCookie, Value: bodyTestToken})
	status, body, _ := authDo(t, req)
	return status, strings.TrimSpace(body)
}

// TestMiddlewareBodyLimit: POST /admin/branding (the logo upload) may send
// up to MaxUploadBytes, everything else MaxFormBytes; a form over the limit
// gets 413 instead of a misleading CSRF error, with and without a
// Content-Length.
func TestMiddlewareBodyLimit(t *testing.T) {
	m, _ := authTestManager(t)
	srv := bodyLimitServer(t, m)

	const mib = 1 << 20
	overUpload, overUploadType := bodyMultipart(t, bodyTestToken, MaxUploadBytes+1024)
	middle, middleType := bodyMultipart(t, bodyTestToken, 2*mib) // over MaxFormBytes, under MaxUploadBytes
	middleWrongToken, middleWrongTokenType := bodyMultipart(t, "wrong-token-wrong-token", 2*mib)
	small, smallType := bodyMultipart(t, bodyTestToken, 1024)
	overForm, formType := bodyURLEncoded(bodyTestToken, MaxFormBytes+1024)
	middleForm, _ := bodyURLEncoded(bodyTestToken, 2*mib)
	smallForm, _ := bodyURLEncoded(bodyTestToken, 1024)

	const (
		tooLarge1 = "Request too large: at most 1 MB can be sent at once."
		tooLarge4 = "Request too large: at most 4 MB can be sent at once."
		csrfError = "Forbidden: invalid or missing CSRF token."
	)
	cases := []struct {
		name   string
		req    bodyRequest
		status int
		want   string // prefix of the response body
	}{
		// Over the limit: 413.
		{"multipart over 4 MB to the upload", bodyRequest{path: "/admin/branding", body: overUpload, contentType: overUploadType}, http.StatusRequestEntityTooLarge, tooLarge4},
		{"multipart over 4 MB to the upload, chunked", bodyRequest{path: "/admin/branding", body: overUpload, contentType: overUploadType, chunked: true}, http.StatusRequestEntityTooLarge, tooLarge4},
		{"urlencoded over 1 MB", bodyRequest{path: "/x", body: overForm, contentType: formType}, http.StatusRequestEntityTooLarge, tooLarge1},
		{"urlencoded over 1 MB, chunked", bodyRequest{path: "/x", body: overForm, contentType: formType, chunked: true}, http.StatusRequestEntityTooLarge, tooLarge1},
		{"multipart of 2 MB elsewhere", bodyRequest{path: "/admin/settings", body: middle, contentType: middleType}, http.StatusRequestEntityTooLarge, tooLarge1},
		{"multipart of 2 MB elsewhere, chunked", bodyRequest{path: "/admin/settings", body: middle, contentType: middleType, chunked: true}, http.StatusRequestEntityTooLarge, tooLarge1},
		// The larger limit is for POST to that exact path only.
		{"multipart of 2 MB to the reset", bodyRequest{path: "/admin/branding/reset", body: middle, contentType: middleType}, http.StatusRequestEntityTooLarge, tooLarge1},
		{"multipart of 2 MB with a trailing slash", bodyRequest{path: "/admin/branding/", body: middle, contentType: middleType}, http.StatusRequestEntityTooLarge, tooLarge1},
		{"multipart of 2 MB with PUT", bodyRequest{method: http.MethodPut, path: "/admin/branding", body: middle, contentType: middleType}, http.StatusRequestEntityTooLarge, tooLarge1},

		// Within the limit: the handler gets the parsed form.
		{"multipart of 2 MB to the upload", bodyRequest{path: "/admin/branding", body: middle, contentType: middleType}, http.StatusOK, fmt.Sprintf("ok file=%d", 2*mib)},
		{"multipart of 2 MB to the upload, chunked", bodyRequest{path: "/admin/branding", body: middle, contentType: middleType, chunked: true}, http.StatusOK, fmt.Sprintf("ok file=%d", 2*mib)},
		{"urlencoded of 2 MB to the upload", bodyRequest{path: "/admin/branding", body: middleForm, contentType: formType}, http.StatusOK, fmt.Sprintf("ok read=0 note=%d", 2*mib)},
		{"small multipart elsewhere", bodyRequest{path: "/x", body: small, contentType: smallType}, http.StatusOK, "ok file=1024"},
		{"small urlencoded elsewhere", bodyRequest{path: "/x", body: smallForm, contentType: formType}, http.StatusOK, "ok read=0 note=1024"},

		// The other checks still apply, and the Origin check comes first.
		{"multipart of 2 MB to the upload, wrong token", bodyRequest{path: "/admin/branding", body: middleWrongToken, contentType: middleWrongTokenType}, http.StatusForbidden, csrfError},
		{"over the limit, foreign origin", bodyRequest{path: "/admin/branding", body: overUpload, contentType: overUploadType, header: map[string]string{"Origin": "https://evil.example.com"}}, http.StatusForbidden, "Forbidden: cross-origin request"},
		// With the CSRF header the middleware does not read the body; the
		// handler meets the limit itself.
		{"over the limit, CSRF header", bodyRequest{path: "/x", body: overForm, contentType: formType, header: map[string]string{CSRFHeader: bodyTestToken}}, http.StatusOK, "ok read=too-large"},
		{"small, CSRF header", bodyRequest{path: "/x", body: []byte(`{"a":1}`), contentType: "application/json", header: map[string]string{CSRFHeader: bodyTestToken}}, http.StatusOK, "ok read=7"},
		// A body that is no form carries no token, whatever its size.
		{"JSON over the limit without CSRF header", bodyRequest{path: "/x", body: overForm, contentType: "application/json"}, http.StatusForbidden, csrfError},
	}
	for _, tc := range cases {
		status, body := bodySend(t, srv, tc.req)
		if status != tc.status || !strings.HasPrefix(body, tc.want) {
			t.Errorf("%s: status %d %q, want %d %q", tc.name, status, body, tc.status, tc.want)
		}
	}
}

// TestParseBodyTooLarge: the standard library passes the error of
// http.MaxBytesReader on through ParseForm and ParseMultipartForm, so a body
// over the limit is recognised without a Content-Length.
func TestParseBodyTooLarge(t *testing.T) {
	multipartBody, multipartType := bodyMultipart(t, bodyTestToken, MaxFormBytes+1024)
	formBody, formType := bodyURLEncoded(bodyTestToken, MaxFormBytes+1024)
	for _, tc := range []struct {
		name, contentType string
		body              []byte
	}{
		{"multipart", multipartType, multipartBody},
		{"urlencoded", formType, formBody},
	} {
		req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(tc.body))
		req.ContentLength = -1 // unknown, as with chunked transfer encoding
		req.Header.Set("Content-Type", tc.contentType)
		req.Body = http.MaxBytesReader(httptest.NewRecorder(), req.Body, MaxFormBytes)
		err := parseBody(req, MaxFormBytes)
		var tooLarge *http.MaxBytesError
		if !errors.As(err, &tooLarge) || !bodyTooLarge(req, err, MaxFormBytes) {
			t.Errorf("%s: err = %v (%T), want an *http.MaxBytesError", tc.name, err, err)
		}
	}
}

// TestMiddlewareBrandSkipsSession: like static files, the public brand images
// (/brand/) are served without looking up the session.
func TestMiddlewareBrandSkipsSession(t *testing.T) {
	m, admin := authTestManager(t)
	srv := csrfServer(t, m)
	rec := httptest.NewRecorder()
	if err := m.StartSession(context.Background(), rec, httptest.NewRequest(http.MethodGet, "/", nil), admin.ID); err != nil {
		t.Fatal(err)
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie")
	}
	for path, want := range map[string]string{
		"/":                 "ok Administrator/admin",
		"/admin/branding":   "ok Administrator/admin",
		"/brandx":           "ok Administrator/admin",
		"/brand/logo-light": "ok anonymous token=",
		"/brand/favicon":    "ok anonymous token=",
		"/static/style.css": "ok anonymous token=",
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.AddCookie(session)
		if status, body, _ := authDo(t, req); status != http.StatusOK || !strings.HasPrefix(body, want) {
			t.Errorf("GET %s: status %d %q, want %q", path, status, body, want)
		}
	}
}

// TestMiddlewarePublicAssetsNoCookie: static files and brand images may be
// kept in shared caches, so a GET or HEAD of them sets no CSRF cookie (pages
// still do); a POST to such a path is still checked.
func TestMiddlewarePublicAssetsNoCookie(t *testing.T) {
	m, _ := authTestManager(t)
	srv := csrfServer(t, m)
	for _, c := range []struct {
		method, path string
		cookie       bool
	}{
		{http.MethodGet, "/", true},
		{http.MethodGet, "/login", true},
		{http.MethodGet, "/brandx", true},
		{http.MethodGet, "/brand/logo-light", false},
		{http.MethodHead, "/brand/favicon", false},
		{http.MethodGet, "/static/style.css", false},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, nil)
		status, _, resp := authDo(t, req)
		if status != http.StatusOK {
			t.Errorf("%s %s: status %d", c.method, c.path, status)
		}
		got := false
		for _, ck := range resp.Cookies() {
			got = got || ck.Name == CSRFCookie
		}
		if got != c.cookie {
			t.Errorf("%s %s: CSRF cookie set = %v, want %v", c.method, c.path, got, c.cookie)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/brand/favicon", strings.NewReader("a=b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if status, _, _ := authDo(t, req); status != http.StatusForbidden {
		t.Errorf("POST /brand/favicon without token: status %d, want 403", status)
	}
}
