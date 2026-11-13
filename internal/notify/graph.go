package notify

// E-mail delivery with Microsoft Graph (Microsoft 365), the default method.
// It uses the app registration of the single sign-on settings (Microsoft
// Entra ID issuer, client ID and client secret), which needs the Microsoft
// Graph application permission Mail.Send.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/msgraph"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

const (
	// graphDefaultPause is the pause after Microsoft Graph throttled the
	// delivery without a Retry-After header; graphMaxPause limits a longer
	// Retry-After.
	graphDefaultPause = time.Minute
	graphMaxPause     = 10 * time.Minute
)

// pauseError is returned by sendGraph when Microsoft Graph throttled the
// delivery: the delivery pauses for pause. It matches errStopped.
type pauseError struct {
	pause time.Duration
	err   error
}

func (e *pauseError) Error() string   { return e.err.Error() }
func (e *pauseError) Unwrap() []error { return []error{errStopped, e.err} }

// newGraphClient creates a Microsoft Graph client for the single sign-on app
// registration of the settings.
func newGraphClient(st *store.Settings) (*msgraph.Client, error) {
	c, err := msgraph.New(st.OIDCIssuer, st.OIDCClientID, st.OIDCClientSecret)
	if err != nil {
		return nil, fmt.Errorf("Microsoft Graph uses the app registration of the single sign-on settings: %w", err)
	}
	return c, nil
}

// sendGraph delivers msgs from the mailbox sender with Microsoft Graph, one
// request per message, and calls result right after each message it tried
// (err nil: Microsoft Graph accepted it), so that the caller can record the
// result at once. It returns a non-nil error if it did not try all
// messages:
//   - Microsoft Graph throttled the delivery or is temporarily unavailable
//     (HTTP 429, 503 or 504): a *pauseError, which matches errStopped; the
//     throttled message counts as not tried, so it keeps its attempts;
//   - the request failed for a reason that concerns every message: no access
//     token, Microsoft Graph cannot be reached, HTTP 401, 403, 404 (the
//     sender mailbox) or another server error; the failing message counts
//     as not tried as well;
//   - ctx was cancelled.
//
// A message Microsoft Graph rejected with another client error, such as an
// invalid recipient (HTTP 400), is reported with its error and the delivery
// goes on.
func sendGraph(ctx context.Context, c *msgraph.Client, sender string, msgs []Message, result func(i int, err error)) error {
	for i, m := range msgs {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := c.SendMail(ctx, sender, msgraph.Mail{To: strings.TrimSpace(m.To), Subject: oneLine(m.Subject), HTML: m.HTML, Text: m.Text})
		if err == nil {
			result(i, nil)
			continue
		}
		var ge *msgraph.Error
		if !errors.As(err, &ge) {
			return err // no access token, Microsoft Graph unreachable, or ctx cancelled
		}
		err = graphError(sender, ge)
		switch status := ge.StatusCode; {
		case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
			pause := ge.RetryAfter
			if pause <= 0 {
				pause = graphDefaultPause
			}
			return &pauseError{pause: min(pause, graphMaxPause), err: err}
		case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound || status >= 500:
			return err
		}
		result(i, err) // only this message is affected
	}
	return nil
}

// graphError adds a hint for administrators to an error response of
// Microsoft Graph.
func graphError(sender string, e *msgraph.Error) error {
	switch e.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w – Microsoft Graph did not accept the access token of the app registration", e)
	case http.StatusForbidden:
		return fmt.Errorf("%w – the app registration needs the Microsoft Graph application permission Mail.Send with admin consent, "+
			"or an Exchange Online RBAC for Applications assignment that includes the mailbox %s", e, sender)
	case http.StatusNotFound:
		return fmt.Errorf("%w – check the sender mailbox %s: it must exist and have an Exchange Online mailbox", e, sender)
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if e.RetryAfter > 0 {
			return fmt.Errorf("%w (retry after %s)", e, e.RetryAfter)
		}
	}
	return e
}
