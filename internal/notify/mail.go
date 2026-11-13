package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	mail "github.com/wneessen/go-mail"
	"github.com/wneessen/go-mail/smtp"

	"github.com/szilvasidave/status-page-tool/internal/msgraph"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// Message is an e-mail to deliver.
type Message struct {
	To      string // recipient address
	Subject string
	Text    string // plain text body
	// HTML is optional. If set, SMTP sends a multipart/alternative e-mail
	// (Text + HTML) and Microsoft Graph sends the HTML body only.
	HTML string
}

// smtpTimeout limits connecting to the SMTP server and every SMTP command.
const smtpTimeout = 15 * time.Second

// testTLSConfig, if set, is used as the base TLS configuration instead of the
// defaults. Only tests set it (to trust a test certificate authority);
// certificates are always verified.
var testTLSConfig *tls.Config

// errStopped marks the error of sendSMTP and sendGraph when they stopped
// before the last message because the server throttled the delivery. The
// messages after the last reported one were not tried.
var errStopped = errors.New("delivery stopped early")

// errConnClosed marks the error of sendSMTP when the SMTP server closed the
// connection after at least one message was tried (e.g. a relay that takes
// one message per connection). The messages after the last reported one
// were not tried and can be sent over a new connection right away.
var errConnClosed = errors.New("closed the connection")

// Send delivers msgs with the e-mail method of the settings: over one SMTP
// connection, or with Microsoft Graph (one request per message, with a new
// access token). It returns per-message errors (nil entries = delivered)
// and, if not all messages were tried, the reason (see sendSMTP and
// sendGraph); the messages that were not tried have that error too.
func Send(ctx context.Context, st *store.Settings, msgs []Message) ([]error, error) {
	errs := make([]error, len(msgs))
	tried := 0
	err := sendWith(ctx, st, nil, msgs, func(i int, e error) { errs[i], tried = e, i+1 })
	for i := tried; i < len(msgs); i++ {
		errs[i] = err
	}
	return errs, err
}

// sendWith delivers msgs with the e-mail method of the settings: sendSMTP,
// or sendGraph with the Microsoft Graph client c (nil: a new client).
func sendWith(ctx context.Context, st *store.Settings, c *msgraph.Client, msgs []Message, result func(i int, err error)) error {
	if len(msgs) == 0 {
		return nil
	}
	if st != nil && st.MailMethod == store.MailSMTP {
		return sendSMTP(ctx, st, msgs, result)
	}
	if !st.GraphMailConfigured() {
		return errors.New("e-mail is not configured: set the sender mailbox and the single sign-on app registration (issuer, client ID, client secret) in the settings")
	}
	if c == nil {
		var err error
		if c, err = newGraphClient(st); err != nil {
			return err
		}
	}
	return sendGraph(ctx, c, st.GraphSender, msgs, result)
}

// sendSMTP delivers msgs over one SMTP connection and calls result right
// after each message it tried, in order (err nil: the server accepted it),
// so that the caller can record the result at once. It returns a non-nil
// error if it did not try all messages:
//   - connecting or signing in failed, or the connection did not work for
//     the first message: no message was tried;
//   - ctx was cancelled;
//   - the server answered with a temporary error that is not about the
//     recipient, e.g. a rate limit (that message is reported with it): the
//     error wraps errStopped;
//   - the server closed the connection after a message: the error wraps
//     errConnClosed.
func sendSMTP(ctx context.Context, st *store.Settings, msgs []Message, result func(i int, err error)) error {
	if len(msgs) == 0 {
		return nil
	}
	if !st.SMTPConfigured() {
		return errors.New("e-mail is not configured: set the SMTP server and sender address in the settings")
	}
	if err := mail.NewMsg().From(st.SMTPFrom); err != nil {
		return fmt.Errorf("invalid sender address %q: %w", st.SMTPFrom, err)
	}

	client, addr, err := newSMTPClient(st)
	if err != nil {
		return err
	}
	if err := client.DialWithContext(ctx); err != nil {
		return connectError(ctx, addr, st, err)
	}
	defer client.Close()

	for i, m := range msgs {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := buildMessage(st.SMTPFrom, m)
		if err == nil {
			// A message the server accepted counts as delivered even if the
			// reset after it failed: sending it again would duplicate it.
			if err = client.Send(msg); err != nil && msg.IsDelivered() {
				err = nil
			}
		}
		var se *mail.SendError
		switch {
		case errors.As(err, &se) && se.Reason == mail.ErrConnCheck:
			if i == 0 {
				return fmt.Errorf("SMTP server %s: %w", addr, err)
			}
			return fmt.Errorf("SMTP server %s %w: %w", addr, errConnClosed, err)
		case errors.As(err, &se) && se.IsTemp() && se.Reason != mail.ErrSMTPRcptTo:
			result(i, err)
			return fmt.Errorf("%w: %w", errStopped, err)
		}
		result(i, err)
	}
	return nil
}

// newSMTPClient creates a go-mail client for the settings. It returns the
// server address for error messages.
func newSMTPClient(st *store.Settings) (*mail.Client, string, error) {
	security := strings.ToLower(strings.TrimSpace(st.SMTPSecurity))
	if security == "" {
		security = "starttls"
	}
	port := st.SMTPPort
	if port == 0 {
		switch security {
		case "tls":
			port = 465
		case "none":
			port = 25
		default:
			port = 587
		}
	}
	host := strings.TrimSpace(st.SMTPHost)
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if testTLSConfig != nil {
		tlsConfig = testTLSConfig.Clone()
	}
	tlsConfig.ServerName = host

	opts := []mail.Option{
		mail.WithPort(port),
		mail.WithTimeout(smtpTimeout),
		mail.WithTLSConfig(tlsConfig),
	}
	// Prefer the widely supported mechanisms. Over an unencrypted connection
	// try CRAM-MD5 first, as PLAIN and LOGIN would expose the password (go-mail
	// refuses them there unless the server is localhost).
	mechs := []mail.SMTPAuthType{mail.SMTPAuthPlain, mail.SMTPAuthLogin, mail.SMTPAuthCramMD5}
	switch security {
	case "starttls":
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	case "tls":
		opts = append(opts, mail.WithSSL())
	case "none":
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
		mechs = []mail.SMTPAuthType{mail.SMTPAuthCramMD5, mail.SMTPAuthPlain, mail.SMTPAuthLogin}
	default:
		return nil, addr, fmt.Errorf("unknown SMTP security %q (use starttls, tls or none)", st.SMTPSecurity)
	}
	if st.SMTPUsername != "" {
		opts = append(opts,
			mail.WithOpportunisticSMTPAuth(mechs...),
			mail.WithUsername(st.SMTPUsername),
			mail.WithPassword(st.SMTPPassword),
		)
	}

	client, err := mail.NewClient(host, opts...)
	if err != nil {
		return nil, addr, fmt.Errorf("SMTP settings: %w", err)
	}
	return client, addr, nil
}

// connectError turns a dial/handshake/authentication error into a readable
// message.
func connectError(ctx context.Context, addr string, st *store.Settings, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var netErr net.Error
	switch {
	case errors.As(err, &netErr) && netErr.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("SMTP server %s did not respond within %s", addr, smtpTimeout)
	case errors.Is(err, smtp.ErrUnencrypted):
		return fmt.Errorf("SMTP server %s only offers password authentication without encryption; choose STARTTLS or TLS as security", addr)
	case errors.Is(err, mail.ErrNoSupportedAuthDiscovered):
		return fmt.Errorf("SMTP server %s offers no supported authentication method (PLAIN, LOGIN or CRAM-MD5)", addr)
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "SMTP AUTH failed"):
		return fmt.Errorf("SMTP server %s rejected the sign-in of %q: %w", addr, st.SMTPUsername, err)
	case strings.Contains(msg, "server does not support SMTP AUTH"):
		return fmt.Errorf("SMTP server %s does not support authentication; clear the username or check the security setting", addr)
	case strings.Contains(msg, "does not support STARTTLS"):
		return fmt.Errorf("SMTP server %s does not support STARTTLS; check the port and the security setting", addr)
	}
	return fmt.Errorf("could not connect to SMTP server %s: %w", addr, err)
}

// buildMessage creates the go-mail message for m.
func buildMessage(from string, m Message) (*mail.Msg, error) {
	msg := mail.NewMsg(mail.WithNoDefaultUserAgent())
	if err := msg.From(from); err != nil {
		return nil, fmt.Errorf("invalid sender address %q: %w", from, err)
	}
	if err := msg.To(strings.TrimSpace(m.To)); err != nil {
		return nil, fmt.Errorf("invalid recipient address %q: %w", m.To, err)
	}
	msg.Subject(oneLine(m.Subject))
	msg.SetUserAgent("Status Page")
	// Ask auto-responders (out of office) not to reply.
	msg.SetGenHeader("Auto-Submitted", "auto-generated")
	msg.SetBodyString(mail.TypeTextPlain, m.Text)
	if m.HTML != "" {
		msg.AddAlternativeString(mail.TypeTextHTML, m.HTML)
	}
	return msg, nil
}

// oneLine collapses all whitespace (including line breaks) into single spaces.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
