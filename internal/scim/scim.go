// Package scim is the SCIM 2.0 provisioning endpoint (RFC 7643, RFC 7644)
// for Microsoft Entra ID at /scim/v2. Entra ID pushes the users and groups
// assigned to an enterprise application, including people who never signed
// in, so that they are known and notified.
//
// Only what Entra ID uses is implemented: Users and Groups with GET (by id,
// or a list with filter and paging), POST, PUT, PATCH and DELETE, and the
// discovery endpoints ServiceProviderConfig, ResourceTypes and Schemas.
// Filters support "eq" and "and" (filter.go); PATCH accepts both styles Entra
// ID sends (with or without the aadOptscim062020 flag, see users.go and
// groups.go). Attributes the status page does not store are ignored.
//
// Requests are authenticated with a bearer token that an administrator
// generates in Admin → Settings; only its SHA-256 is stored. No token means
// provisioning is off. What provisioning changes in the database is
// described in internal/store/scim.go.
package scim

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/secret"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// Path is the base path of the endpoint; the Tenant URL to enter in
// Microsoft Entra ID is <Base URL>/scim/v2.
const Path = "/scim/v2"

const (
	schemaUser          = "urn:ietf:params:scim:schemas:core:2.0:User"
	schemaGroup         = "urn:ietf:params:scim:schemas:core:2.0:Group"
	schemaListResponse  = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	schemaError         = "urn:ietf:params:scim:api:messages:2.0:Error"
	schemaPatchOp       = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	schemaSPConfig      = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	schemaResourceType  = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	schemaSchema        = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	contentType         = "application/scim+json"
	defaultCount        = 100     // list page size without "count"
	maxCount            = 500     // largest page size
	maxBodyBytes        = 2 << 20 // request body limit
	maxValueLength      = 256     // characters of a user name, display name, external id or e-mail
	authenticationRealm = "Status Page SCIM"
)

// Handler serves /scim/v2. Create it with New.
type Handler struct {
	store *store.Store
	log   *slog.Logger
}

// New returns the SCIM endpoint for the routes under Path.
func New(st *store.Store, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: st, log: log}
}

// NewToken returns a new random bearer token (32 bytes, base64url) and the
// hash to store as Settings.SCIMTokenHash.
func NewToken() (token, hash string) {
	token = secret.RandomToken(32)
	return token, HashToken(token)
}

// HashToken returns the stored form of a token: SHA-256, hex.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// EndpointURL returns the address of the endpoint (the Tenant URL for
// Microsoft Entra ID): the base URL setting, or the address of the request
// if it is not set, followed by Path.
func EndpointURL(r *http.Request, baseURL string) string {
	if baseURL = strings.TrimRight(baseURL, "/"); baseURL == "" {
		scheme := "http"
		if auth.IsHTTPS(r) {
			scheme = "https"
		}
		baseURL = scheme + "://" + r.Host
	}
	return baseURL + Path
}

// ------------------------------------------------------------------ errors

// apiError is an error response (RFC 7644 section 3.12).
type apiError struct {
	status   int
	scimType string // e.g. invalidFilter, invalidValue, uniqueness; "" if none applies
	detail   string // shown to administrators in the provisioning logs
}

func (e *apiError) Error() string { return e.detail }

func errorf(status int, scimType, format string, args ...any) *apiError {
	return &apiError{status: status, scimType: scimType, detail: fmt.Sprintf(format, args...)}
}

// invalidValue reports a missing or unusable attribute value (400).
func invalidValue(format string, args ...any) *apiError {
	return errorf(http.StatusBadRequest, "invalidValue", format, args...)
}

func notFoundError(resource string, id string) *apiError {
	return errorf(http.StatusNotFound, "", "%s %s not found.", resource, id)
}

type errorJSON struct {
	Schemas  []string `json:"schemas"`
	Status   string   `json:"status"`
	ScimType string   `json:"scimType,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// writeError writes err as a SCIM error response. Errors of the store are
// mapped to 404 (not found), 409 (uniqueness) and 500 (logged).
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		e        *apiError
		conflict *store.SCIMConflictError
	)
	switch {
	case errors.As(err, &e):
	case errors.Is(err, store.ErrNotFound):
		e = errorf(http.StatusNotFound, "", "The resource does not exist.")
	case errors.As(err, &conflict):
		e = errorf(http.StatusConflict, "uniqueness", "%s", conflict.Detail)
	default:
		h.log.Error("SCIM request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		e = errorf(http.StatusInternalServerError, "", "Internal error of the status page; details are in its log file.")
	}
	if e.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+authenticationRealm+`"`)
	}
	writeJSON(w, e.status, errorJSON{Schemas: []string{schemaError}, Status: strconv.Itoa(e.status), ScimType: e.scimType, Detail: e.detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// ------------------------------------------------------------------ requests

// request is one API request after authentication.
type request struct {
	w    http.ResponseWriter
	r    *http.Request
	base string // endpoint URL, e.g. https://status.example.com/scim/v2
}

// ServeHTTP authenticates the request and dispatches it by its path.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st, err := h.store.GetSettings(r.Context())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if e := authenticate(r, st); e != nil {
		h.log.Warn("SCIM request rejected", "reason", e.detail, "method", r.Method, "path", r.URL.Path, "ip", auth.ClientIP(r))
		h.writeError(w, r, e)
		return
	}
	req := &request{w: w, r: r, base: EndpointURL(r, st.BaseURL)}
	if err := h.route(req); err != nil {
		h.writeError(w, r, err)
	}
}

// authenticate checks the bearer token against the stored hash.
func authenticate(r *http.Request, st *store.Settings) *apiError {
	if !st.SCIMEnabled() {
		return errorf(http.StatusUnauthorized, "",
			"Provisioning is turned off. An administrator turns it on by generating a token in Admin → Settings of the status page.")
	}
	scheme, token, _ := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	token = strings.TrimSpace(token)
	if !strings.EqualFold(scheme, "Bearer") || token == "" {
		return errorf(http.StatusUnauthorized, "",
			"The request has no bearer token. Enter the token from Admin → Settings of the status page as Secret Token in Microsoft Entra ID.")
	}
	if subtle.ConstantTimeCompare([]byte(HashToken(token)), []byte(st.SCIMTokenHash)) != 1 {
		return errorf(http.StatusUnauthorized, "",
			"The bearer token is not valid. Enter the current token from Admin → Settings of the status page as Secret Token in Microsoft Entra ID (generating a new token replaces the previous one).")
	}
	return nil
}

// route dispatches an authenticated request.
func (h *Handler) route(req *request) error {
	rest, ok := strings.CutPrefix(req.r.URL.Path, Path)
	if !ok || (rest != "" && rest[0] != '/') {
		return errorf(http.StatusNotFound, "", "Not found. The SCIM endpoint (Tenant URL) is %s.", req.base)
	}
	parts := strings.FieldsFunc(rest, func(c rune) bool { return c == '/' })
	if len(parts) == 0 {
		return errorf(http.StatusNotFound, "", "Not found. Use the resources Users and Groups below %s.", req.base)
	}
	resource, args := parts[0], parts[1:]
	switch {
	case strings.EqualFold(resource, "Users"):
		return h.users(req, args)
	case strings.EqualFold(resource, "Groups"):
		return h.groups(req, args)
	case strings.EqualFold(resource, "ServiceProviderConfig") && len(args) == 0:
		return onlyGet(req, func() error { return serviceProviderConfig(req) })
	case strings.EqualFold(resource, "ResourceTypes") && len(args) <= 1:
		return onlyGet(req, func() error { return resourceTypes(req, args) })
	case strings.EqualFold(resource, "Schemas") && len(args) <= 1:
		return onlyGet(req, func() error { return schemas(req, args) })
	}
	return errorf(http.StatusNotFound, "", "Not found: %s.", req.r.URL.Path)
}

// methodNotAllowed is the response for an unsupported method.
func methodNotAllowed(req *request, allowed ...string) error {
	req.w.Header().Set("Allow", strings.Join(allowed, ", "))
	return errorf(http.StatusMethodNotAllowed, "", "The method %s is not supported here.", req.r.Method)
}

// resourceMethod accepts the methods of /Users/{id} and /Groups/{id}.
func resourceMethod(req *request) error {
	switch req.r.Method {
	case http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return nil
	}
	return methodNotAllowed(req, http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete)
}

func onlyGet(req *request, fn func() error) error {
	if req.r.Method != http.MethodGet {
		return methodNotAllowed(req, http.MethodGet)
	}
	return fn()
}

// parseID parses the id of a user or group; ok is false if it cannot be one
// of ours.
func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

// readBody decodes the JSON object in the request body. Numbers are kept as
// json.Number.
func readBody(req *request) (map[string]any, error) {
	if ct := req.r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || (mt != contentType && mt != "application/json") {
			return nil, errorf(http.StatusUnsupportedMediaType, "", "Send the request body as JSON (Content-Type: application/scim+json), not %q.", ct)
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(req.w, req.r.Body, maxBodyBytes))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, errorf(http.StatusRequestEntityTooLarge, "", "The request body is larger than %d MB.", maxBodyBytes>>20)
		}
		return nil, errorf(http.StatusBadRequest, "invalidSyntax", "The request body is not valid JSON: %v.", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errorf(http.StatusBadRequest, "invalidSyntax", "The request body must be a JSON object.")
	}
	return m, nil
}

// listResponse is the response to a query (RFC 7644 section 3.4.2).
type listResponse struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    []any    `json:"Resources"`
}

func newListResponse(total, startIndex int, resources []any) listResponse {
	if resources == nil {
		resources = []any{}
	}
	return listResponse{Schemas: []string{schemaListResponse}, TotalResults: total, StartIndex: startIndex,
		ItemsPerPage: len(resources), Resources: resources}
}

// paging returns the 1-based startIndex and the count of a list request
// (RFC 7644 section 3.4.2.4).
func paging(req *request) (startIndex, count int, err error) {
	q := req.r.URL.Query()
	startIndex, count = 1, defaultCount
	if v := q.Get("startIndex"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, 0, invalidValue("startIndex must be a number, not %q.", v)
		}
		startIndex = max(n, 1)
	}
	if v := q.Get("count"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, 0, invalidValue("count must be a number, not %q.", v)
		}
		count = min(max(n, 0), maxCount)
	}
	return startIndex, count, nil
}

// excluded reports whether the request excludes the attribute name with
// excludedAttributes (e.g. excludedAttributes=members).
func excluded(req *request, name string) bool {
	for _, a := range strings.Split(req.r.URL.Query().Get("excludedAttributes"), ",") {
		a = strings.TrimSpace(a)
		if i := strings.LastIndexByte(a, ':'); i >= 0 {
			a = a[i+1:] // urn:…:Group:members
		}
		if strings.EqualFold(a, name) {
			return true
		}
	}
	return false
}

// meta is the meta attribute of a resource.
type meta struct {
	ResourceType string `json:"resourceType"`
	Created      string `json:"created,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	Location     string `json:"location,omitempty"`
}
