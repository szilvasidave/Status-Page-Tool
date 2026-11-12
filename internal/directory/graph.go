package directory

import (
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
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// ErrGroupNotFound is returned when a group does not exist in the directory.
var ErrGroupNotFound = errors.New("group not found in Microsoft Entra ID")

const (
	entraLoginURL = "https://login.microsoftonline.com"
	graphURL      = "https://graph.microsoft.com"
	graphScope    = "https://graph.microsoft.com/.default"

	graphTimeout     = 30 * time.Second
	graphAttempts    = 3                // per request, for HTTP 429 and 503
	graphMaxWait     = 30 * time.Second // longest wait between attempts
	graphMaxPages    = 1000             // safety limit when following @odata.nextLink
	graphMaxBodySize = 32 << 20
)

// GraphClient reads groups and their members from Microsoft Graph using the
// client credentials flow. Safe for concurrent use; the access token is cached
// and renewed automatically.
type GraphClient struct {
	tokenURL string
	graphURL string // base URL without trailing slash
	http     *http.Client
	// sleep waits between retries (replaced in tests).
	sleep func(ctx context.Context, d time.Duration) error
}

// NewGraphClient creates a client for the Microsoft Entra ID tenant of the
// OIDC issuer (https://login.microsoftonline.com/{tenant}/v2.0 or
// https://sts.windows.net/{tenant}/). Other issuers return an error wrapping
// ErrNotEnabled.
func NewGraphClient(issuer, clientID, clientSecret string) (*GraphClient, error) {
	tenant, err := entraTenant(issuer)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(clientID) == "" || clientSecret == "" {
		return nil, fmt.Errorf("%w: the OIDC client ID and client secret are required for directory sync", ErrNotEnabled)
	}
	tokenURL := entraLoginURL + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
	return newGraphClient(tokenURL, graphURL, strings.TrimSpace(clientID), clientSecret), nil
}

func newGraphClient(tokenURL, graphBaseURL, clientID, clientSecret string) *GraphClient {
	cc := &clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     tokenURL,
		Scopes:       []string{graphScope},
		AuthStyle:    oauth2.AuthStyleInParams,
	}
	// The token requests use their own client with a timeout; the context is
	// only used to pass that client to the token source.
	tokenCtx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: graphTimeout})
	httpClient := cc.Client(tokenCtx)
	httpClient.Timeout = graphTimeout
	return &GraphClient{
		tokenURL: tokenURL,
		graphURL: strings.TrimRight(graphBaseURL, "/"),
		http:     httpClient,
		sleep:    sleepContext,
	}
}

// entraTenant extracts the tenant (id or domain) from an Entra ID issuer URL.
func entraTenant(issuer string) (string, error) {
	notEntra := fmt.Errorf("%w: issuer is not a Microsoft Entra ID issuer", ErrNotEnabled)
	u, err := url.Parse(strings.TrimSpace(issuer))
	if err != nil || u.Scheme != "https" {
		return "", notEntra
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
		return "", notEntra
	}
	switch strings.ToLower(tenant) {
	case "common", "organizations", "consumers":
		return "", fmt.Errorf("%w: the issuer must name a specific tenant, not %q", ErrNotEnabled, tenant)
	}
	return tenant, nil
}

// graphGroup is a group as returned by Microsoft Graph.
type graphGroup struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

func (g graphGroup) group() Group {
	return Group{ID: g.ID, Name: g.DisplayName, Description: g.Description}
}

// Group returns the group with object id id. A missing group returns an
// error wrapping ErrGroupNotFound.
func (c *GraphClient) Group(ctx context.Context, id string) (Group, error) {
	if !IsGUID(id) {
		return Group{}, fmt.Errorf("%q is not a Microsoft Entra ID group object id", id)
	}
	var g graphGroup
	u := c.graphURL + "/v1.0/groups/" + url.PathEscape(id) + "?$select=id,displayName,description"
	if err := c.get(ctx, u, nil, &g); err != nil {
		return Group{}, groupError(id, "reading group", err)
	}
	return g.group(), nil
}

// GroupMembers returns all enabled users that are direct or nested members of
// the group with object id id.
func (c *GraphClient) GroupMembers(ctx context.Context, id string) ([]store.DirectoryUser, error) {
	if !IsGUID(id) {
		return nil, fmt.Errorf("%q is not a Microsoft Entra ID group object id", id)
	}
	// OData casts on transitiveMembers are "advanced queries" which require
	// $count=true and the ConsistencyLevel header.
	header := http.Header{"ConsistencyLevel": {"eventual"}}
	next := c.graphURL + "/v1.0/groups/" + url.PathEscape(id) +
		"/transitiveMembers/microsoft.graph.user?$select=id,displayName,mail,userPrincipalName,accountEnabled&$top=999&$count=true"

	var users []store.DirectoryUser
	for page := 0; next != ""; page++ {
		if page == graphMaxPages {
			return nil, fmt.Errorf("reading members of group %s: too many pages", id)
		}
		var resp struct {
			Value []struct {
				ID                string `json:"id"`
				DisplayName       string `json:"displayName"`
				Mail              string `json:"mail"`
				UserPrincipalName string `json:"userPrincipalName"`
				AccountEnabled    *bool  `json:"accountEnabled"`
			} `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := c.get(ctx, next, header, &resp); err != nil {
			return nil, groupError(id, "reading members of group", err)
		}
		for _, u := range resp.Value {
			if u.AccountEnabled != nil && !*u.AccountEnabled {
				continue
			}
			users = append(users, store.DirectoryUser{
				DirectoryID: u.ID,
				Email:       userEmail(u.Mail, u.UserPrincipalName),
				Name:        strings.TrimSpace(u.DisplayName),
			})
		}
		next = resp.NextLink
		// Never send the access token anywhere but to Microsoft Graph.
		if next != "" && !strings.HasPrefix(next, c.graphURL+"/") {
			return nil, fmt.Errorf("reading members of group %s: unexpected next page link %q", id, next)
		}
	}
	return users, nil
}

// SearchGroups returns up to limit groups whose display name starts with
// query (case-insensitive). If query is a group object id, that group is
// returned.
func (c *GraphClient) SearchGroups(ctx context.Context, query string, limit int) ([]Group, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if IsGUID(query) {
		g, err := c.Group(ctx, query)
		if errors.Is(err, ErrGroupNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []Group{g}, nil
	}
	if limit <= 0 {
		limit = 25
	}
	limit = min(limit, 999)
	filter := "startswith(displayName,'" + strings.ReplaceAll(query, "'", "''") + "')"
	u := c.graphURL + "/v1.0/groups?$filter=" + queryEscape(filter) +
		"&$select=id,displayName,description&$top=" + strconv.Itoa(limit)

	var resp struct {
		Value []graphGroup `json:"value"`
	}
	if err := c.get(ctx, u, nil, &resp); err != nil {
		return nil, fmt.Errorf("searching groups: %w", err)
	}
	groups := make([]Group, 0, len(resp.Value))
	for _, g := range resp.Value {
		groups = append(groups, g.group())
	}
	slices.SortFunc(groups, func(a, b Group) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	if len(groups) > limit {
		groups = groups[:limit]
	}
	return groups, nil
}

// IsGUID reports whether s looks like a GUID such as an Entra ID object id
// (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx).
func IsGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// userEmail returns mail, or else the user principal name if it is a usable
// e-mail address (guest UPNs like "jane_example.com#EXT#@contoso.onmicrosoft.com"
// are not).
func userEmail(mail, upn string) string {
	if m := strings.TrimSpace(mail); m != "" {
		return strings.ToLower(m)
	}
	upn = strings.TrimSpace(upn)
	if strings.Contains(strings.ToUpper(upn), "#EXT#") || !looksLikeEmail(upn) {
		return ""
	}
	return strings.ToLower(upn)
}

func looksLikeEmail(s string) bool {
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 || strings.Count(s, "@") != 1 || strings.ContainsAny(s, " \t<>\"") {
		return false
	}
	domain := s[at+1:]
	return strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}

// queryEscape escapes a query parameter value; spaces become %20 (not "+").
func queryEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func groupError(id, action string, err error) error {
	if errors.Is(err, ErrGroupNotFound) {
		return fmt.Errorf("%w (id %s)", ErrGroupNotFound, id)
	}
	return fmt.Errorf("%s %s: %w", action, id, err)
}

// get performs a GET request and decodes the JSON response into out. It
// retries throttled (429) and unavailable (503) responses.
func (c *GraphClient) get(ctx context.Context, rawURL string, header http.Header, out any) error {
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			return requestError(ctx, err)
		}
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable
		if retry && attempt < graphAttempts {
			wait := retryAfter(resp.Header.Get("Retry-After"), attempt)
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		err = decodeResponse(resp, out)
		resp.Body.Close()
		return err
	}
}

func decodeResponse(resp *http.Response, out any) error {
	if resp.StatusCode == http.StatusNotFound {
		return ErrGroupNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return readGraphError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, graphMaxBodySize)).Decode(out); err != nil {
		return fmt.Errorf("reading the Microsoft Graph response: %w", err)
	}
	return nil
}

// graphError is an error response of Microsoft Graph.
type graphError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *graphError) Error() string {
	msg := fmt.Sprintf("Microsoft Graph returned HTTP %d", e.StatusCode)
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.StatusCode == http.StatusForbidden {
		msg += " – the app registration needs the Microsoft Graph application permissions GroupMember.Read.All and User.ReadBasic.All with admin consent"
	}
	return msg
}

func readGraphError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	ge := &graphError{StatusCode: resp.StatusCode}
	if json.Unmarshal(body, &parsed) == nil && (parsed.Error.Code != "" || parsed.Error.Message != "") {
		ge.Code = parsed.Error.Code
		ge.Message = strings.TrimSpace(parsed.Error.Message)
	} else {
		ge.Message = http.StatusText(resp.StatusCode)
	}
	return ge
}

// connectionError marks an error that affects every request, not only the
// requested group: no access token, or Microsoft Graph cannot be reached.
type connectionError struct{ err error }

func (e *connectionError) Error() string { return e.err.Error() }
func (e *connectionError) Unwrap() error { return e.err }

// requestError makes transport and token errors readable. They are returned
// as *connectionError (except for a cancelled ctx).
func requestError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		reason := re.ErrorCode
		if desc := firstLine(re.ErrorDescription); desc != "" {
			reason += ": " + desc
		}
		if reason == "" && re.Response != nil {
			reason = re.Response.Status
		}
		return &connectionError{fmt.Errorf("could not get an access token from Microsoft Entra ID (check the client ID and client secret): %s", reason)}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return &connectionError{fmt.Errorf("Microsoft Graph did not respond within %s", graphTimeout)}
		}
		err = ue.Err
	}
	return &connectionError{fmt.Errorf("could not reach Microsoft Graph: %w", err)}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// retryAfter returns how long to wait before the next attempt: the
// Retry-After header (seconds or HTTP date) if present, else a short backoff;
// at most graphMaxWait.
func retryAfter(header string, attempt int) time.Duration {
	wait := time.Duration(attempt) * 2 * time.Second
	if header = strings.TrimSpace(header); header != "" {
		if secs, err := strconv.Atoi(header); err == nil {
			wait = time.Duration(max(secs, 0)) * time.Second
		} else if t, err := http.ParseTime(header); err == nil {
			wait = max(time.Until(t), 0)
		}
	}
	return min(wait, graphMaxWait)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
