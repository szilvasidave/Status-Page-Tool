package e2e

// Test harness: runs the application and drives it like a browser.

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/app"
)

// ------------------------------------------------------------------ application

// logBuffer collects the application log; it is printed if the test fails.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// instance is the application running in a goroutine via app.Run.
type instance struct {
	t      *testing.T
	URL    string // http://127.0.0.1:<port>
	cancel context.CancelFunc
	done   chan error
}

// freeAddr returns a free 127.0.0.1 address.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// startApp runs the application with dataDir on addr and waits until it
// answers. It is stopped at the end of the test unless stopped before.
func startApp(t *testing.T, dataDir, addr string, logs *logBuffer) *instance {
	t.Helper()
	return startAppURL(t, dataDir, addr, "http://"+addr, http.DefaultClient, logs)
}

// startAppURL is startApp for an application reachable at baseURL with client
// (e.g. https).
func startAppURL(t *testing.T, dataDir, addr, baseURL string, client *http.Client, logs *logBuffer) *instance {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a := &instance{t: t, URL: baseURL, cancel: cancel, done: make(chan error, 1)}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	go func() {
		a.done <- app.Run(ctx, app.Options{DataDir: dataDir, Listen: addr, Version: "e2e", Log: log})
	}()
	t.Cleanup(func() { a.stop() })

	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := client.Get(a.URL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return a
			}
		}
		select {
		case err := <-a.done:
			a.done <- err
			t.Fatalf("the application stopped during start: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the application did not start: %v", err)
		}
	}
}

// stop cancels the application's context and waits for app.Run to return.
func (a *instance) stop() {
	a.t.Helper()
	if a.cancel == nil {
		return
	}
	a.cancel()
	a.cancel = nil
	select {
	case err := <-a.done:
		if err != nil {
			a.t.Errorf("app.Run returned an error on shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		a.t.Fatal("app.Run did not return within 30s after the context was cancelled")
	}
}

// ------------------------------------------------------------------ browser

// browser is an HTTP client with its own cookie jar that behaves like a
// browser: it follows redirects when asked to, submits forms with their
// current field values (including the hidden CSRF token) and sends the
// Origin header with every POST.
type browser struct {
	t      *testing.T
	name   string
	client *http.Client
	header http.Header // added to every request (e.g. X-Forwarded-For)
}

func newBrowser(t *testing.T, name string) *browser {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &browser{t: t, name: name, client: &http.Client{
		Jar:     jar,
		Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// page is a response.
type page struct {
	b      *browser
	URL    *url.URL
	Status int
	Header http.Header
	Body   string
}

func (b *browser) do(req *http.Request) *page {
	b.t.Helper()
	for k, v := range b.header {
		req.Header[k] = v
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("%s: %s %s: %v", b.name, req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatalf("%s: %s %s: read body: %v", b.name, req.Method, req.URL, err)
	}
	return &page{b: b, URL: req.URL, Status: resp.StatusCode, Header: resp.Header, Body: string(body)}
}

// get requests rawURL without following redirects.
func (b *browser) get(rawURL string) *page {
	b.t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	return b.do(req)
}

// open requests rawURL and follows redirects like a browser.
func (b *browser) open(rawURL string) *page {
	b.t.Helper()
	return b.get(rawURL).follow()
}

// redirect returns the absolute target of a redirect response ("" if the
// response is not a redirect).
func (p *page) redirect() string {
	if p.Status < 300 || p.Status > 399 {
		return ""
	}
	loc, err := p.URL.Parse(p.Header.Get("Location"))
	if err != nil {
		p.b.t.Fatalf("%s: bad Location %q", p.b.name, p.Header.Get("Location"))
	}
	return loc.String()
}

// follow follows redirects (at most 10).
func (p *page) follow() *page {
	p.b.t.Helper()
	for i := 0; i < 10; i++ {
		target := p.redirect()
		if target == "" {
			return p
		}
		p = p.b.get(target)
	}
	p.b.t.Fatalf("%s: too many redirects, last %s", p.b.name, p.URL)
	return nil
}

// expectRedirect checks that p redirects to path (and query) on the same
// host and returns the target.
func (p *page) expectRedirect(want string) string {
	p.b.t.Helper()
	target := p.redirect()
	if target == "" {
		p.b.t.Fatalf("%s: %s: status %d, want a redirect to %s\n%s", p.b.name, p.URL, p.Status, want, truncate(p.text(), 2000))
	}
	u, _ := url.Parse(target)
	if got := u.RequestURI(); got != want {
		p.b.t.Fatalf("%s: %s redirects to %s, want %s", p.b.name, p.URL, got, want)
	}
	return target
}

// expectStatus checks the status code.
func (p *page) expectStatus(want int) *page {
	p.b.t.Helper()
	if p.Status != want {
		p.b.t.Fatalf("%s: %s: status %d, want %d (Location %q)\n%s", p.b.name, p.URL, p.Status, want, p.Header.Get("Location"), truncate(p.text(), 2000))
	}
	return p
}

var (
	tagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
	spacesRe = regexp.MustCompile(`\s+`)
)

// text returns the page as text: tags removed, entities decoded, white space
// collapsed.
func (p *page) text() string {
	s := tagRe.ReplaceAllString(p.Body, " ")
	return strings.TrimSpace(spacesRe.ReplaceAllString(html.UnescapeString(s), " "))
}

// expectText checks that the page text contains every string of want.
func (p *page) expectText(want ...string) *page {
	p.b.t.Helper()
	text := p.text()
	for _, w := range want {
		if !strings.Contains(text, w) {
			p.b.t.Fatalf("%s: %s: page does not contain %q:\n%s", p.b.name, p.URL, w, truncate(text, 3000))
		}
	}
	return p
}

// expectNoText checks that the page text contains none of the strings.
func (p *page) expectNoText(unwanted ...string) *page {
	p.b.t.Helper()
	text := p.text()
	for _, w := range unwanted {
		if strings.Contains(text, w) {
			p.b.t.Fatalf("%s: %s: page unexpectedly contains %q:\n%s", p.b.name, p.URL, w, truncate(text, 3000))
		}
	}
	return p
}

var flashRe = regexp.MustCompile(`(?s)<div class="flash flash-(\w+)" role="(?:status|alert)">(.*?)</div>`)

// flash returns the kind and text of the flash message of the page.
func (p *page) flash() (kind, msg string) {
	m := flashRe.FindStringSubmatch(p.Body)
	if m == nil {
		return "", ""
	}
	return m[1], strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(m[2], "")))
}

// expectFlash checks the flash message of the page.
func (p *page) expectFlash(kind, contains string) *page {
	p.b.t.Helper()
	k, msg := p.flash()
	if k != kind || !strings.Contains(msg, contains) {
		p.b.t.Fatalf("%s: %s: flash %s %q, want %s containing %q\n%s", p.b.name, p.URL, k, msg, kind, contains, truncate(p.text(), 2000))
	}
	return p
}

var hrefRe = regexp.MustCompile(`href="([^"#]*)`)

// crawl visits every page of the site reachable from start by links (GET
// only, at most limit pages) and fails the test for every server error. It
// returns the number of pages visited.
func (b *browser) crawl(start string, limit int) int {
	b.t.Helper()
	base, err := url.Parse(start)
	if err != nil {
		b.t.Fatal(err)
	}
	seen := map[string]bool{base.RequestURI(): true}
	queue := []string{start}
	for len(queue) > 0 && len(seen) <= limit {
		p := b.get(queue[0])
		queue = queue[1:]
		if p.Status >= 500 || strings.Contains(p.Body, "Something went wrong") {
			b.t.Errorf("%s: %s: status %d\n%s", b.name, p.URL, p.Status, truncate(p.text(), 500))
			continue
		}
		links := []string{p.Header.Get("Location")}
		for _, m := range hrefRe.FindAllStringSubmatch(p.Body, -1) {
			links = append(links, html.UnescapeString(m[1]))
		}
		for _, l := range links {
			u, err := p.URL.Parse(l)
			if l == "" || err != nil || u.Host != base.Host || strings.HasPrefix(u.Path, "/auth/oidc/") || strings.HasPrefix(u.Path, "/static/") {
				continue
			}
			if !seen[u.RequestURI()] {
				seen[u.RequestURI()] = true
				queue = append(queue, u.String())
			}
		}
	}
	return len(seen)
}

// json decodes the body into v.
func (p *page) json(v any) {
	p.b.t.Helper()
	if err := json.Unmarshal([]byte(p.Body), v); err != nil {
		p.b.t.Fatalf("%s: %s: invalid JSON: %v\n%s", p.b.name, p.URL, err, truncate(p.Body, 1000))
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ------------------------------------------------------------------ forms

// form is an HTML form with the current values of its controls.
type form struct {
	t      *testing.T
	page   *page
	action string
	method string
	fields []*field
	submit *field // the pressed button, nil = first submit button / none
}

type field struct {
	tag     string // input, select, textarea, button
	typ     string // input/button type
	name    string
	value   string
	checked bool
	options []string // select: option values
}

var (
	formTagRe = regexp.MustCompile(`(?is)<(/?)(form|input|select|option|textarea|button)\b((?:[^>"']|"[^"]*"|'[^']*')*)>`)
	attrRe    = regexp.MustCompile(`([^\s=/>"']+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>"']+)))?`)
)

func parseAttrs(s string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(s, -1) {
		name := strings.ToLower(m[1])
		if _, dup := out[name]; !dup {
			out[name] = html.UnescapeString(m[2] + m[3] + m[4])
		}
	}
	return out
}

// forms parses the forms of the page.
func (p *page) forms() []*form {
	var (
		out    []*form
		cur    *form
		sel    *field
		selVal string // value of the selected option of sel
	)
	matches := formTagRe.FindAllStringSubmatchIndex(p.Body, -1)
	for _, m := range matches {
		closing := p.Body[m[2]:m[3]] == "/"
		tag := strings.ToLower(p.Body[m[4]:m[5]])
		attrs := parseAttrs(p.Body[m[6]:m[7]])
		switch {
		case tag == "form" && !closing:
			method := strings.ToUpper(attrs["method"])
			if method == "" {
				method = http.MethodGet
			}
			cur = &form{t: p.b.t, page: p, action: attrs["action"], method: method}
			out = append(out, cur)
		case tag == "form" && closing:
			cur = nil
		case cur == nil:
			continue
		case tag == "input" && !closing:
			typ := strings.ToLower(attrs["type"])
			if typ == "" {
				typ = "text"
			}
			_, checked := attrs["checked"]
			value, hasValue := attrs["value"]
			if !hasValue && (typ == "checkbox" || typ == "radio") {
				value = "on"
			}
			if typ == "submit" {
				cur.fields = append(cur.fields, &field{tag: "button", typ: "submit", name: attrs["name"], value: value})
				continue
			}
			cur.fields = append(cur.fields, &field{tag: "input", typ: typ, name: attrs["name"], value: value, checked: checked})
		case tag == "button" && !closing:
			typ := strings.ToLower(attrs["type"])
			if typ == "" {
				typ = "submit"
			}
			if _, other := attrs["form"]; other {
				continue // belongs to another form
			}
			cur.fields = append(cur.fields, &field{tag: "button", typ: typ, name: attrs["name"], value: attrs["value"]})
		case tag == "select" && !closing:
			sel = &field{tag: "select", name: attrs["name"]}
			selVal = ""
			cur.fields = append(cur.fields, sel)
		case tag == "select" && closing:
			if sel != nil {
				sel.value = selVal
				if selVal == "" && len(sel.options) > 0 {
					sel.value = sel.options[0]
				}
			}
			sel = nil
		case tag == "option" && !closing && sel != nil:
			value, ok := attrs["value"]
			if !ok { // the option's text
				rest := p.Body[m[1]:]
				if i := strings.IndexByte(rest, '<'); i >= 0 {
					rest = rest[:i]
				}
				value = strings.TrimSpace(html.UnescapeString(rest))
			}
			sel.options = append(sel.options, value)
			if _, selected := attrs["selected"]; selected {
				selVal = value
			}
		case tag == "textarea" && !closing:
			rest := p.Body[m[1]:]
			end := strings.Index(strings.ToLower(rest), "</textarea>")
			if end < 0 {
				end = len(rest)
			}
			value := html.UnescapeString(rest[:end])
			value = strings.TrimPrefix(value, "\n") // the HTML parser drops a leading newline
			cur.fields = append(cur.fields, &field{tag: "textarea", name: attrs["name"], value: value})
		}
	}
	return out
}

// form returns the form whose action is action (method POST unless the
// action is a GET form). It fails the test if there is none.
func (p *page) form(action string) *form {
	p.b.t.Helper()
	var actions []string
	for _, f := range p.forms() {
		if f.action == action {
			return f
		}
		actions = append(actions, f.action)
	}
	p.b.t.Fatalf("%s: %s: no form with action %q (forms: %q)\n%s", p.b.name, p.URL, action, actions, truncate(p.text(), 2000))
	return nil
}

func (f *form) find(name string, match func(*field) bool) []*field {
	var out []*field
	for _, fl := range f.fields {
		if fl.name == name && match(fl) {
			out = append(out, fl)
		}
	}
	return out
}

func (f *form) names() []string {
	var out []string
	for _, fl := range f.fields {
		out = append(out, fl.tag+":"+fl.name)
	}
	return out
}

// set sets the value of a text-like input or a textarea.
func (f *form) set(name, value string) *form {
	f.t.Helper()
	fields := f.find(name, func(fl *field) bool {
		return fl.tag == "textarea" || (fl.tag == "input" && fl.typ != "checkbox" && fl.typ != "radio")
	})
	if len(fields) == 0 {
		f.t.Fatalf("form %s has no text field %q (fields: %v)", f.action, name, f.names())
	}
	fields[0].value = value
	return f
}

// check ticks the checkbox or radio button name with the given value (any
// value if empty); for radio buttons the others of the group are unticked.
func (f *form) check(name, value string) *form {
	f.t.Helper()
	return f.tick(name, value, true)
}

// uncheck unticks a checkbox.
func (f *form) uncheck(name, value string) *form {
	f.t.Helper()
	return f.tick(name, value, false)
}

func (f *form) tick(name, value string, on bool) *form {
	f.t.Helper()
	fields := f.find(name, func(fl *field) bool { return fl.typ == "checkbox" || fl.typ == "radio" })
	found := false
	for _, fl := range fields {
		if value == "" || fl.value == value {
			fl.checked = on
			found = true
		} else if on && fl.typ == "radio" {
			fl.checked = false
		}
	}
	if !found {
		f.t.Fatalf("form %s has no checkbox/radio %q with value %q (fields: %v)", f.action, name, value, f.names())
	}
	return f
}

// checked reports whether the checkbox name/value is ticked.
func (f *form) checked(name, value string) bool {
	for _, fl := range f.find(name, func(fl *field) bool { return fl.typ == "checkbox" || fl.typ == "radio" }) {
		if fl.value == value {
			return fl.checked
		}
	}
	f.t.Fatalf("form %s has no checkbox/radio %q with value %q", f.action, name, value)
	return false
}

// choose selects an option of a select.
func (f *form) choose(name, value string) *form {
	f.t.Helper()
	fields := f.find(name, func(fl *field) bool { return fl.tag == "select" })
	if len(fields) == 0 {
		f.t.Fatalf("form %s has no select %q (fields: %v)", f.action, name, f.names())
	}
	for _, o := range fields[0].options {
		if o == value {
			fields[0].value = value
			return f
		}
	}
	f.t.Fatalf("select %q of form %s has no option %q (options: %q)", name, f.action, value, fields[0].options)
	return f
}

// value returns the current value of a text field or select.
func (f *form) value(name string) string {
	f.t.Helper()
	fields := f.find(name, func(fl *field) bool { return fl.typ != "checkbox" && fl.typ != "radio" && fl.tag != "button" })
	if len(fields) == 0 {
		f.t.Fatalf("form %s has no field %q (fields: %v)", f.action, name, f.names())
	}
	return fields[0].value
}

// press chooses the submit button with the given name and value.
func (f *form) press(name, value string) *form {
	f.t.Helper()
	fields := f.find(name, func(fl *field) bool { return fl.tag == "button" && fl.typ == "submit" && fl.value == value })
	if len(fields) == 0 {
		f.t.Fatalf("form %s has no submit button %s=%s (fields: %v)", f.action, name, value, f.names())
	}
	f.submit = fields[0]
	return f
}

// values returns the form data set as a browser would submit it.
func (f *form) values() url.Values {
	v := url.Values{}
	submitter := f.submit
	if submitter == nil {
		for _, fl := range f.fields {
			if fl.tag == "button" && fl.typ == "submit" {
				submitter = fl
				break
			}
		}
	}
	for _, fl := range f.fields {
		if fl.name == "" {
			continue
		}
		switch {
		case fl.tag == "button":
			if fl == submitter {
				v.Add(fl.name, fl.value)
			}
		case fl.typ == "checkbox" || fl.typ == "radio":
			if fl.checked {
				v.Add(fl.name, fl.value)
			}
		case fl.typ == "file" || fl.typ == "reset" || fl.typ == "image":
		default:
			v.Add(fl.name, fl.value)
		}
	}
	return v
}

// post submits the form (without following the redirect).
func (f *form) post() *page {
	f.t.Helper()
	b := f.page.b
	target, err := f.page.URL.Parse(f.action)
	if err != nil {
		f.t.Fatal(err)
	}
	if f.method != http.MethodPost {
		target.RawQuery = f.values().Encode()
		return b.get(target.String())
	}
	req, err := http.NewRequest(http.MethodPost, target.String(), strings.NewReader(f.values().Encode()))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", f.page.URL.Scheme+"://"+f.page.URL.Host)
	return b.do(req)
}
