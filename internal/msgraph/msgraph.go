// Package msgraph is a small Microsoft Graph client that uses the client
// credentials of an app registration in Microsoft Entra ID (the one of the
// single sign-on settings). The status page uses it to send e-mail
// (SendMail) and to look up the names of groups (GroupNames).
package msgraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// Endpoints are the base URLs (without trailing slash) of the Microsoft
// Entra ID token service and of Microsoft Graph.
type Endpoints struct {
	Login string // e.g. https://login.microsoftonline.com
	Graph string // e.g. https://graph.microsoft.com
}

// DefaultEndpoints are the endpoints New uses: those of Microsoft's global
// cloud. Tests point them to a fake server (see internal/testutil/fakegraph).
var DefaultEndpoints = Endpoints{Login: "https://login.microsoftonline.com", Graph: "https://graph.microsoft.com"}

const (
	scope   = "https://graph.microsoft.com/.default"
	timeout = 30 * time.Second // per request (access token or Microsoft Graph)
	// maxErrorBody limits how much of an error response is read.
	maxErrorBody = 64 << 10
	// maxResponseBody limits how much of a successful response is read.
	maxResponseBody = 4 << 20
)

// Client calls Microsoft Graph with an access token from the client
// credentials flow. It is safe for concurrent use; the access token is
// cached and renewed shortly before it expires.
type Client struct {
	graphURL string
	creds    *clientcredentials.Config
	http     *http.Client // for the access token requests and Microsoft Graph

	mu    sync.Mutex
	token *oauth2.Token // the cached access token
}

// New creates a client for the Microsoft Entra ID tenant of the OIDC issuer
// (see Tenant), with the client ID and client secret of the app
// registration.
func New(issuer, clientID, clientSecret string) (*Client, error) {
	tenant, err := Tenant(issuer)
	if err != nil {
		return nil, err
	}
	clientID = strings.TrimSpace(clientID)
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("the client ID and client secret of the app registration are required")
	}
	ep := DefaultEndpoints
	tokenURL := strings.TrimRight(ep.Login, "/") + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
	return newClient(tokenURL, ep.Graph, clientID, clientSecret), nil
}

func newClient(tokenURL, graphURL, clientID, clientSecret string) *Client {
	return &Client{
		graphURL: strings.TrimRight(graphURL, "/"),
		creds: &clientcredentials.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			TokenURL:     tokenURL,
			Scopes:       []string{scope},
			AuthStyle:    oauth2.AuthStyleInParams,
		},
		// Redirects are not followed: the client secret and the access
		// token must not be sent anywhere else.
		http: &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// accessToken returns the cached access token, or gets a new one from
// Microsoft Entra ID if it expires within 10 seconds. The token request uses
// ctx, so that it ends when the caller gives up (e.g. at shutdown); the
// client of oauth2.Transport would request it without ctx.
func (c *Client) accessToken(ctx context.Context) (*oauth2.Token, error) {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok.Valid() {
		return tok, nil
	}
	tok, err := c.creds.Token(context.WithValue(ctx, oauth2.HTTPClient, c.http))
	if err != nil {
		return nil, tokenError(ctx, err)
	}
	c.mu.Lock()
	c.token = tok
	c.mu.Unlock()
	return tok, nil
}

// ErrNotEntra is returned by Tenant and New for an issuer that is not one of
// Microsoft Entra ID.
var ErrNotEntra = errors.New("the issuer is not a Microsoft Entra ID issuer (https://login.microsoftonline.com/<tenant ID>/v2.0)")

// Tenant returns the tenant (ID or domain name) of a Microsoft Entra ID
// issuer URL: https://login.microsoftonline.com/{tenant}/v2.0 or
// https://sts.windows.net/{tenant}/. Other issuers return ErrNotEntra; the
// Entra ID issuers that name no tenant ("common", "organizations",
// "consumers") return another error.
func Tenant(issuer string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(issuer))
	if err != nil || u.Scheme != "https" || u.RawQuery != "" || u.Fragment != "" {
		return "", ErrNotEntra
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	var tenant string
	switch strings.ToLower(u.Host) {
	case "login.microsoftonline.com":
		if len(parts) == 2 && parts[1] == "v2.0" {
			tenant = parts[0]
		}
	case "sts.windows.net":
		if len(parts) == 1 {
			tenant = parts[0]
		}
	}
	if tenant == "" || strings.ContainsAny(tenant, "?#% ") {
		return "", ErrNotEntra
	}
	switch strings.ToLower(tenant) {
	case "common", "organizations", "consumers":
		return "", fmt.Errorf("the issuer must name your tenant (https://login.microsoftonline.com/<tenant ID>/v2.0), not %q", tenant)
	}
	return tenant, nil
}

// Mail is an e-mail for SendMail.
type Mail struct {
	To      string // recipient address
	Subject string
	HTML    string // HTML body
	Text    string // plain text body, only used if HTML is empty
}

// The JSON body of sendMail.
type (
	sendMailRequest struct {
		Message         message `json:"message"`
		SaveToSentItems bool    `json:"saveToSentItems"`
	}
	message struct {
		Subject      string      `json:"subject"`
		Body         itemBody    `json:"body"`
		ToRecipients []recipient `json:"toRecipients"`
	}
	itemBody struct {
		ContentType string `json:"contentType"` // "HTML" or "Text"
		Content     string `json:"content"`
	}
	recipient struct {
		EmailAddress emailAddress `json:"emailAddress"`
	}
	emailAddress struct {
		Address string `json:"address"`
	}
)

// SendMail sends m from the mailbox sender (its e-mail address, user
// principal name or object ID) with POST /users/{sender}/sendMail (see
// userPath), without keeping a copy in its Sent Items. The app registration
// needs the application permission Mail.Send. nil means that Microsoft Graph
// accepted the e-mail for delivery (HTTP 202); an error response of
// Microsoft Graph is returned as *Error.
func (c *Client) SendMail(ctx context.Context, sender string, m Mail) error {
	body := itemBody{ContentType: "HTML", Content: m.HTML}
	if m.HTML == "" {
		body = itemBody{ContentType: "Text", Content: m.Text}
	}
	req := sendMailRequest{Message: message{
		Subject:      m.Subject,
		Body:         body,
		ToRecipients: []recipient{{EmailAddress: emailAddress{Address: m.To}}},
	}}
	return c.post(ctx, "/v1.0"+userPath(sender)+"/sendMail", req)
}

// userPath returns the path of a user or mailbox: /users/{id}, path-escaped.
// An id that begins with "$" is written /users('{id}') (a quote in it
// doubled): Microsoft Graph answers /users/$… with HTTP 400 because OData
// reserves the "$" prefix (see "Get a user" on Microsoft Learn).
func userPath(id string) string {
	if strings.HasPrefix(id, "$") {
		return "/users('" + url.PathEscape(strings.ReplaceAll(id, "'", "''")) + "')"
	}
	return "/users/" + url.PathEscape(id)
}

// MaxGroupIDs is the number of group IDs GroupNames looks up with one
// request: Microsoft Graph allows at most 15 values for the "in" operator of
// a filter.
const MaxGroupIDs = 15

// maxGroupPages limits the pages of one GroupNames request (there is only
// one: a page holds up to 100 groups).
const maxGroupPages = 5

// groupList is the response of GET /groups.
type groupList struct {
	Value []struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	} `json:"value"`
	NextLink string `json:"@odata.nextLink"`
}

// GroupNames returns the display names of the groups with the given object
// IDs, by ID in lower case, with GET
// /groups?$filter=id in (…)&$select=id,displayName (MaxGroupIDs per
// request). IDs that are not GUIDs are skipped; IDs that Microsoft Entra ID
// does not know as groups (e.g. deleted groups) are missing from the
// result. The app registration needs the application permission
// GroupMember.Read.All. An error response of Microsoft Graph is returned as
// *Error, and no names are returned with an error.
func (c *Client) GroupNames(ctx context.Context, ids []string) (map[string]string, error) {
	var valid []string
	for _, id := range ids {
		// Only GUIDs: the IDs are put into the filter expression.
		if id = strings.ToLower(strings.TrimSpace(id)); isGUID(id) && !slices.Contains(valid, id) {
			valid = append(valid, id)
		}
	}
	names := make(map[string]string, len(valid))
	for chunk := range slices.Chunk(valid, MaxGroupIDs) {
		q := url.Values{}
		q.Set("$filter", "id in ('"+strings.Join(chunk, "', '")+"')")
		q.Set("$select", "id,displayName")
		// Spaces as %20: OData does not read "+" as a space everywhere ("+"
		// itself is encoded as %2B).
		next := c.graphURL + "/v1.0/groups?" + strings.ReplaceAll(q.Encode(), "+", "%20")
		for page := 0; next != ""; page++ {
			if page == maxGroupPages {
				return nil, errors.New("Microsoft Graph returned more pages of groups than expected")
			}
			// A next page link must not take the access token elsewhere.
			if !strings.HasPrefix(next, c.graphURL+"/") {
				return nil, errors.New("Microsoft Graph returned a next page link to another server")
			}
			var list groupList
			if err := c.get(ctx, next, &list); err != nil {
				return nil, err
			}
			for _, g := range list.Value {
				names[strings.ToLower(g.ID)] = strings.TrimSpace(g.DisplayName)
			}
			next = list.NextLink
		}
	}
	return names, nil
}

// isGUID reports whether s is a GUID in lower case
// (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx), the form of object IDs.
func isGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// get requests the absolute URL u and decodes the JSON response into v. An
// error response is returned as *Error.
func (c *Client) get(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	tok.SetAuthHeader(req)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return requestError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp, time.Now())
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(v); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return requestError(ctx, ctxErr)
		}
		return fmt.Errorf("could not read the response of Microsoft Graph: %w", err)
	}
	return nil
}

// post sends v as JSON. A 2xx response is success; its body is ignored.
func (c *Client) post(ctx context.Context, path string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphURL+path, &buf)
	if err != nil {
		return err
	}
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	tok.SetAuthHeader(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return requestError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	return readError(resp, time.Now())
}

// Error is an error response of Microsoft Graph.
type Error struct {
	StatusCode int
	Code       string        // e.g. "ErrorAccessDenied"
	Message    string        // e.g. "Access is denied. Check credentials and try again."
	RetryAfter time.Duration // from the Retry-After header; 0 if there is none
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("Microsoft Graph returned HTTP %d", e.StatusCode)
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func readError(resp *http.Response, now time.Time) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	e := &Error{StatusCode: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"), now)}
	if json.Unmarshal(body, &parsed) == nil && (parsed.Error.Code != "" || parsed.Error.Message != "") {
		e.Code = parsed.Error.Code
		e.Message = strings.Join(strings.Fields(parsed.Error.Message), " ")
	} else {
		e.Message = http.StatusText(resp.StatusCode)
	}
	return e
}

// retryAfter parses a Retry-After header (seconds or an HTTP date). It
// returns 0 if the header is missing or invalid.
func retryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(header); err == nil {
		return time.Duration(max(secs, 0)) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}

// requestError makes a transport error of a Microsoft Graph request
// readable. A cancelled ctx is returned as is.
func requestError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return fmt.Errorf("Microsoft Graph did not respond in time: %w", ctxErr)
		}
		return ctxErr
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return fmt.Errorf("Microsoft Graph did not respond within %s", timeout)
		}
		err = ue.Err
	}
	return fmt.Errorf("could not reach Microsoft Graph: %w", err)
}

// tokenError makes an error of the access token request to Microsoft Entra
// ID readable, so that it is not mistaken for a problem with Microsoft
// Graph. A cancelled ctx is returned as is.
func tokenError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return fmt.Errorf("Microsoft Entra ID did not respond in time to the access token request: %w", ctxErr)
		}
		return ctxErr
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		reason := re.ErrorCode
		if desc := entraDescription(re.ErrorDescription); desc != "" {
			if reason != "" {
				reason += ": "
			}
			reason += desc
		}
		if reason == "" && re.Response != nil {
			reason = re.Response.Status
		}
		return fmt.Errorf("could not get an access token from Microsoft Entra ID (check the issuer, client ID and client secret): %s", reason)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return fmt.Errorf("Microsoft Entra ID did not respond within %s to the access token request", timeout)
		}
		return fmt.Errorf("could not reach Microsoft Entra ID to get an access token: %w", ue.Err)
	}
	return fmt.Errorf("could not get an access token from Microsoft Entra ID: %w", err)
}

// entraDescription shortens the error description of Entra ID, e.g.
// "AADSTS7000215: Invalid client secret provided. … Trace ID: … Correlation
// ID: … Timestamp: …", to the message: the IDs are of no use in the user
// interface.
func entraDescription(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, " Trace ID:"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
