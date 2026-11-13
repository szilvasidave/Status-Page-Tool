// Package msgraph is a small Microsoft Graph client that uses the client
// credentials of an app registration in Microsoft Entra ID (the one of the
// single sign-on settings). The status page uses it to send e-mail
// (SendMail).
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
	"strconv"
	"strings"
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
)

// Client calls Microsoft Graph with an access token from the client
// credentials flow. It is safe for concurrent use; the access token is
// cached and renewed shortly before it expires.
type Client struct {
	graphURL string
	http     *http.Client
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
	cc := &clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     tokenURL,
		Scopes:       []string{scope},
		AuthStyle:    oauth2.AuthStyleInParams,
	}
	// Redirects are not followed: the client secret and the access token
	// must not be sent anywhere else.
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// The token requests use their own client with a timeout; the context
	// only passes that client to the token source.
	tokenClient := &http.Client{Timeout: timeout, CheckRedirect: noRedirect}
	httpClient := cc.Client(context.WithValue(context.Background(), oauth2.HTTPClient, tokenClient))
	httpClient.Timeout = timeout
	httpClient.CheckRedirect = noRedirect
	return &Client{graphURL: strings.TrimRight(graphURL, "/"), http: httpClient}
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
// principal name or object ID) with POST /users/{sender}/sendMail, without
// keeping a copy in its Sent Items. The app registration needs the
// application permission Mail.Send. nil means that Microsoft Graph accepted
// the e-mail for delivery (HTTP 202); an error response of Microsoft Graph
// is returned as *Error.
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
	return c.post(ctx, "/v1.0/users/"+url.PathEscape(sender)+"/sendMail", req)
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

// requestError makes transport and token errors readable. A cancelled ctx
// is returned as is.
func requestError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return fmt.Errorf("Microsoft Graph did not respond in time: %w", ctxErr)
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
			return fmt.Errorf("Microsoft Graph did not respond within %s", timeout)
		}
		err = ue.Err
	}
	return fmt.Errorf("could not reach Microsoft Graph: %w", err)
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
