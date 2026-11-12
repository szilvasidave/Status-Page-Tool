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

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// Message is an e-mail to deliver.
type Message struct {
	To      string // recipient address
	Subject string
	Text    string // plain text body
	HTML    string // optional; if set the e-mail is multipart/alternative (Text + HTML)
}

// smtpTimeout limits connecting to the SMTP server and every SMTP command.
const smtpTimeout = 15 * time.Second

// testTLSConfig, if set, is used as the base TLS configuration instead of the
// defaults. Only tests set it (to trust a test certificate authority);
// certificates are always verified.
var testTLSConfig *tls.Config

// Send delivers msgs over one SMTP connection using the SMTP settings.
// Returns per-message errors (nil entries = delivered) and a non-nil error if
// connecting/authenticating failed (then all messages failed).
func Send(ctx context.Context, st *store.Settings, msgs []Message) ([]error, error) {
	errs := make([]error, len(msgs))
	failAll := func(err error) ([]error, error) {
		for i := range errs {
			errs[i] = err
		}
		return errs, err
	}
	if len(msgs) == 0 {
		return errs, nil
	}
	if !st.SMTPConfigured() {
		return failAll(errors.New("e-mail is not configured: set the SMTP server and sender address in the settings"))
	}
	if err := mail.NewMsg().From(st.SMTPFrom); err != nil {
		return failAll(fmt.Errorf("invalid sender address %q: %w", st.SMTPFrom, err))
	}

	client, addr, err := newSMTPClient(st)
	if err != nil {
		return failAll(err)
	}
	if err := client.DialWithContext(ctx); err != nil {
		return failAll(connectError(ctx, addr, st, err))
	}
	defer client.Close()

	for i, m := range msgs {
		if err := ctx.Err(); err != nil {
			errs[i] = err
			continue
		}
		msg, err := buildMessage(st.SMTPFrom, m)
		if err != nil {
			errs[i] = err
			continue
		}
		errs[i] = client.Send(msg)
	}
	return errs, nil
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
