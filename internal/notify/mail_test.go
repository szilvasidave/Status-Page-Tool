package notify

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// ---------------------------------------------------------------- fake SMTP server

type fakeMail struct {
	From string
	To   []string
	Data []byte
	TLS  bool   // delivered over an encrypted connection
	Auth string // SASL mechanism used, "" if not authenticated
}

// fakeSMTP is a minimal SMTP server for tests.
type fakeSMTP struct {
	serverTLS   *tls.Config
	implicitTLS bool   // TLS from the first byte (SMTPS)
	startTLS    bool   // offer STARTTLS
	authMechs   string // advertised AUTH mechanisms, e.g. "PLAIN LOGIN"
	username    string // if set, MAIL FROM requires authentication
	password    string
	rejectRcpt  string // recipient rejected with 550

	ln    net.Listener
	mu    sync.Mutex
	mails []fakeMail
}

func (s *fakeSMTP) start(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func (s *fakeSMTP) received() []fakeMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeMail(nil), s.mails...)
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer func() { conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	isTLS := false
	if s.implicitTLS {
		conn = tls.Server(conn, s.serverTLS)
		isTLS = true
	}
	tp := textproto.NewConn(conn)
	reply := func(format string, args ...any) { _ = tp.PrintfLine(format, args...) }

	reply("220 fake.test ESMTP ready")
	var auth, from string
	var rcpts []string
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			ext := []string{"fake.test", "8BITMIME"}
			if s.startTLS && !isTLS {
				ext = append(ext, "STARTTLS")
			}
			if s.authMechs != "" {
				ext = append(ext, "AUTH "+s.authMechs)
			}
			for i, e := range ext {
				sep := "-"
				if i == len(ext)-1 {
					sep = " "
				}
				reply("250%s%s", sep, e)
			}
		case "STARTTLS":
			reply("220 2.0.0 Ready to start TLS")
			tc := tls.Server(conn, s.serverTLS)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, tp, isTLS = tc, textproto.NewConn(tc), true
			auth, from, rcpts = "", "", nil
		case "AUTH":
			mech, initial, _ := strings.Cut(arg, " ")
			mech = strings.ToUpper(mech)
			if s.checkAuth(tp, mech, initial) {
				auth = mech
				reply("235 2.7.0 Authentication successful")
			} else {
				reply("535 5.7.8 Authentication credentials invalid")
			}
		case "MAIL":
			if s.username != "" && auth == "" {
				reply("530 5.7.0 Authentication required")
				continue
			}
			from, rcpts = angleAddr(arg), nil
			reply("250 2.1.0 OK")
		case "RCPT":
			to := angleAddr(arg)
			if to == s.rejectRcpt {
				reply("550 5.1.1 <%s>: Recipient address rejected: User unknown", to)
				continue
			}
			rcpts = append(rcpts, to)
			reply("250 2.1.5 OK")
		case "DATA":
			reply("354 End data with <CR><LF>.<CR><LF>")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.mails = append(s.mails, fakeMail{From: from, To: rcpts, Data: data, TLS: isTLS, Auth: auth})
			s.mu.Unlock()
			reply("250 2.0.0 OK queued")
		case "RSET":
			from, rcpts = "", nil
			reply("250 2.0.0 OK")
		case "NOOP":
			reply("250 2.0.0 OK")
		case "QUIT":
			reply("221 2.0.0 Bye")
			return
		default:
			reply("502 5.5.2 Command not recognized")
		}
	}
}

func (s *fakeSMTP) checkAuth(tp *textproto.Conn, mech, initial string) bool {
	readLine := func(challenge string) string {
		_ = tp.PrintfLine("334 %s", challenge)
		l, _ := tp.ReadLine()
		return l
	}
	decode := func(v string) string {
		b, _ := base64.StdEncoding.DecodeString(v)
		return string(b)
	}
	switch mech {
	case "PLAIN":
		if initial == "" {
			initial = readLine("")
		}
		parts := strings.Split(decode(initial), "\x00")
		return len(parts) == 3 && parts[1] == s.username && parts[2] == s.password
	case "LOGIN":
		if initial == "" {
			initial = readLine(base64.StdEncoding.EncodeToString([]byte("Username:")))
		}
		user := decode(initial)
		pass := decode(readLine(base64.StdEncoding.EncodeToString([]byte("Password:"))))
		return user == s.username && pass == s.password
	case "CRAM-MD5":
		challenge := "<1896.697170952@fake.test>"
		user, digest, _ := strings.Cut(decode(readLine(base64.StdEncoding.EncodeToString([]byte(challenge)))), " ")
		mac := hmac.New(md5.New, []byte(s.password))
		mac.Write([]byte(challenge))
		return user == s.username && digest == hex.EncodeToString(mac.Sum(nil))
	}
	return false
}

func angleAddr(arg string) string {
	i, j := strings.IndexByte(arg, '<'), strings.IndexByte(arg, '>')
	if i < 0 || j < i {
		return ""
	}
	return arg[i+1 : j]
}

// testCert creates a self-signed certificate for 127.0.0.1 and returns the
// server configuration and a client configuration trusting it.
func testCert(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	server = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	return server, &tls.Config{RootCAs: pool}
}

// trustTestCert makes Send trust the test certificate for the duration of the test.
func trustTestCert(t *testing.T, client *tls.Config) {
	old := testTLSConfig
	testTLSConfig = client
	t.Cleanup(func() { testTLSConfig = old })
}

func smtpSettings(host string, port int, security string) *store.Settings {
	st := store.DefaultSettings()
	st.SMTPHost = host
	st.SMTPPort = port
	st.SMTPSecurity = security
	st.SMTPFrom = "IT Status <status@example.com>"
	return &st
}

// ---------------------------------------------------------------- parsing helpers

type parsedMail struct {
	Header  mail.Header
	Subject string
	Text    string
	HTML    string
}

func parseMail(t *testing.T, data []byte) parsedMail {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse message: %v\n%s", err, data)
	}
	subj, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	p := parsedMail{Header: msg.Header, Subject: subj}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("content type: %v", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		// The SMTP DATA framing ends the body with a line break.
		p.Text = strings.TrimSuffix(string(decodeBody(t, msg.Header.Get("Content-Transfer-Encoding"), msg.Body)), "\n")
		return p
	}
	if mediaType != "multipart/alternative" {
		t.Fatalf("content type = %s, want multipart/alternative", mediaType)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body := decodeBody(t, part.Header.Get("Content-Transfer-Encoding"), part)
		ct, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		switch ct {
		case "text/plain":
			p.Text = string(body)
		case "text/html":
			p.HTML = string(body)
		default:
			t.Fatalf("unexpected part %s", ct)
		}
	}
	return p
}

func decodeBody(t *testing.T, encoding string, r io.Reader) []byte {
	t.Helper()
	switch strings.ToLower(encoding) {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, r)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// ---------------------------------------------------------------- tests

func TestSendPlainMultipart(t *testing.T) {
	srv := &fakeSMTP{}
	host, port := srv.start(t)
	st := smtpSettings(host, port, "none")

	msgs := []Message{
		{To: "alice@example.com", Subject: "[Status] Update – Monitoring: Mail\nis slow", Text: "Hello Alice\nLine 2 – ünïcode", HTML: "<p>Hello <b>Alice</b></p>"},
		{To: "bob@example.com", Subject: "Text only", Text: "Hello Bob"},
	}
	errs, err := Send(context.Background(), st, msgs)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	for i, e := range errs {
		if e != nil {
			t.Errorf("message %d: %v", i, e)
		}
	}
	got := srv.received()
	if len(got) != 2 {
		t.Fatalf("received %d mails, want 2", len(got))
	}
	if got[0].TLS || got[0].Auth != "" {
		t.Errorf("plain delivery used TLS=%v auth=%q", got[0].TLS, got[0].Auth)
	}
	if got[0].From != "status@example.com" || len(got[0].To) != 1 || got[0].To[0] != "alice@example.com" {
		t.Errorf("envelope = %q -> %q", got[0].From, got[0].To)
	}

	m := parseMail(t, got[0].Data)
	if from := m.Header.Get("From"); from != `"IT Status" <status@example.com>` {
		t.Errorf("From = %q", from)
	}
	if to := m.Header.Get("To"); to != "<alice@example.com>" {
		t.Errorf("To = %q", to)
	}
	if m.Subject != "[Status] Update – Monitoring: Mail is slow" {
		t.Errorf("Subject = %q", m.Subject)
	}
	if m.Header.Get("Date") == "" || m.Header.Get("Message-ID") == "" {
		t.Error("Date or Message-ID header missing")
	}
	if got := m.Header.Get("Auto-Submitted"); got != "auto-generated" {
		t.Errorf("Auto-Submitted = %q", got)
	}
	if m.Text != "Hello Alice\nLine 2 – ünïcode" {
		t.Errorf("text part = %q", m.Text)
	}
	if m.HTML != "<p>Hello <b>Alice</b></p>" {
		t.Errorf("html part = %q", m.HTML)
	}

	m2 := parseMail(t, got[1].Data)
	if ct := m2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("text-only Content-Type = %q", ct)
	}
	if m2.Text != "Hello Bob" || m2.Subject != "Text only" {
		t.Errorf("text-only mail = %+v", m2)
	}
}

func TestSendStartTLSWithLogin(t *testing.T) {
	serverTLS, clientTLS := testCert(t)
	trustTestCert(t, clientTLS)
	// Office 365 style: STARTTLS and AUTH LOGIN.
	srv := &fakeSMTP{serverTLS: serverTLS, startTLS: true, authMechs: "LOGIN XOAUTH2", username: "status@example.com", password: "s3cret"}
	host, port := srv.start(t)
	st := smtpSettings(host, port, "starttls")
	st.SMTPUsername, st.SMTPPassword = "status@example.com", "s3cret"

	errs, err := Send(context.Background(), st, []Message{{To: "carol@example.com", Subject: "Hi", Text: "Body"}})
	if err != nil || errs[0] != nil {
		t.Fatalf("Send: %v %v", err, errs)
	}
	got := srv.received()
	if len(got) != 1 || !got[0].TLS || got[0].Auth != "LOGIN" {
		t.Fatalf("received %+v, want one mail over TLS with AUTH LOGIN", got)
	}
}

func TestSendImplicitTLSWithPlain(t *testing.T) {
	serverTLS, clientTLS := testCert(t)
	trustTestCert(t, clientTLS)
	srv := &fakeSMTP{serverTLS: serverTLS, implicitTLS: true, authMechs: "PLAIN LOGIN", username: "user", password: "pw"}
	host, port := srv.start(t)
	st := smtpSettings(host, port, "tls")
	st.SMTPUsername, st.SMTPPassword = "user", "pw"

	errs, err := Send(context.Background(), st, []Message{{To: "dave@example.com", Subject: "Hi", Text: "Body", HTML: "<p>Body</p>"}})
	if err != nil || errs[0] != nil {
		t.Fatalf("Send: %v %v", err, errs)
	}
	got := srv.received()
	if len(got) != 1 || !got[0].TLS || got[0].Auth != "PLAIN" {
		t.Fatalf("received %+v, want one mail over TLS with AUTH PLAIN", got)
	}
}

func TestSendPlainWithCramMD5(t *testing.T) {
	srv := &fakeSMTP{authMechs: "PLAIN LOGIN CRAM-MD5", username: "user", password: "pw"}
	host, port := srv.start(t)
	st := smtpSettings(host, port, "none")
	st.SMTPUsername, st.SMTPPassword = "user", "pw"

	errs, err := Send(context.Background(), st, []Message{{To: "erin@example.com", Subject: "Hi", Text: "Body"}})
	if err != nil || errs[0] != nil {
		t.Fatalf("Send: %v %v", err, errs)
	}
	if got := srv.received(); len(got) != 1 || got[0].Auth != "CRAM-MD5" {
		t.Fatalf("received %+v, want CRAM-MD5 on an unencrypted connection", got)
	}
}

func TestSendAuthFailure(t *testing.T) {
	serverTLS, clientTLS := testCert(t)
	trustTestCert(t, clientTLS)
	srv := &fakeSMTP{serverTLS: serverTLS, startTLS: true, authMechs: "PLAIN LOGIN", username: "user", password: "right"}
	host, port := srv.start(t)
	st := smtpSettings(host, port, "starttls")
	st.SMTPUsername, st.SMTPPassword = "user", "wrong"

	msgs := []Message{{To: "a@example.com", Text: "x"}, {To: "b@example.com", Text: "y"}}
	errs, err := Send(context.Background(), st, msgs)
	if err == nil {
		t.Fatal("expected an error for wrong credentials")
	}
	if !strings.Contains(err.Error(), "rejected the sign-in") || !strings.Contains(err.Error(), "535") {
		t.Errorf("error = %q", err)
	}
	for i, e := range errs {
		if e == nil {
			t.Errorf("message %d: no error although authentication failed", i)
		}
	}
	if n := len(srv.received()); n != 0 {
		t.Errorf("%d mails delivered", n)
	}
}

func TestSendVerifiesCertificate(t *testing.T) {
	serverTLS, _ := testCert(t)
	trustTestCert(t, nil) // production defaults: system roots only
	srv := &fakeSMTP{serverTLS: serverTLS, startTLS: true}
	host, port := srv.start(t)

	_, err := Send(context.Background(), smtpSettings(host, port, "starttls"), []Message{{To: "a@example.com", Text: "x"}})
	if err == nil {
		t.Fatal("expected a certificate error with an untrusted certificate")
	}
	var unknownAuthority x509.UnknownAuthorityError
	if !errors.As(err, &unknownAuthority) && !strings.Contains(err.Error(), "certificate") {
		t.Errorf("error = %v, want a certificate verification error", err)
	}
}

func TestSendStartTLSRequired(t *testing.T) {
	srv := &fakeSMTP{} // no STARTTLS offered
	host, port := srv.start(t)
	_, err := Send(context.Background(), smtpSettings(host, port, "starttls"), []Message{{To: "a@example.com", Text: "x"}})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("error = %v, want STARTTLS not supported", err)
	}
	if len(srv.received()) != 0 {
		t.Error("mail delivered without TLS")
	}
}

func TestSendPerMessageErrors(t *testing.T) {
	srv := &fakeSMTP{rejectRcpt: "unknown@example.com"}
	host, port := srv.start(t)
	msgs := []Message{
		{To: "ok1@example.com", Subject: "1", Text: "1"},
		{To: "unknown@example.com", Subject: "2", Text: "2"},
		{To: "not an address", Subject: "3", Text: "3"},
		{To: "ok2@example.com", Subject: "4", Text: "4"},
	}
	errs, err := Send(context.Background(), smtpSettings(host, port, "none"), msgs)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if errs[0] != nil || errs[3] != nil {
		t.Errorf("valid messages failed: %v, %v", errs[0], errs[3])
	}
	if errs[1] == nil || !strings.Contains(errs[1].Error(), "550") {
		t.Errorf("rejected recipient error = %v", errs[1])
	}
	if errs[2] == nil || !strings.Contains(errs[2].Error(), "invalid recipient address") {
		t.Errorf("invalid address error = %v", errs[2])
	}
	got := srv.received()
	if len(got) != 2 || got[0].To[0] != "ok1@example.com" || got[1].To[0] != "ok2@example.com" {
		t.Errorf("received %+v", got)
	}
}

func TestSendConnectErrors(t *testing.T) {
	// A port nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	errs, err := Send(context.Background(), smtpSettings("127.0.0.1", port, "none"), []Message{{To: "a@example.com", Text: "x"}})
	if err == nil || !strings.Contains(err.Error(), "could not connect to SMTP server 127.0.0.1:"+strconv.Itoa(port)) {
		t.Errorf("error = %v", err)
	}
	if errs[0] == nil {
		t.Error("per-message error missing")
	}

	// Cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv := &fakeSMTP{}
	host, p := srv.start(t)
	if _, err := Send(ctx, smtpSettings(host, p, "none"), []Message{{To: "a@example.com", Text: "x"}}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: error = %v", err)
	}
}

func TestSendSettingsErrors(t *testing.T) {
	msgs := []Message{{To: "a@example.com", Text: "x"}}
	if _, err := Send(context.Background(), nil, msgs); err == nil {
		t.Error("nil settings: expected error")
	}
	st := smtpSettings("127.0.0.1", 25, "none")
	st.SMTPFrom = "not an address"
	if _, err := Send(context.Background(), st, msgs); err == nil || !strings.Contains(err.Error(), "invalid sender address") {
		t.Errorf("invalid sender: error = %v", err)
	}
	st = smtpSettings("127.0.0.1", 25, "ssl-maybe")
	if _, err := Send(context.Background(), st, msgs); err == nil || !strings.Contains(err.Error(), "unknown SMTP security") {
		t.Errorf("invalid security: error = %v", err)
	}
	errs, err := Send(context.Background(), nil, nil)
	if err != nil || len(errs) != 0 {
		t.Errorf("no messages: %v %v", errs, err)
	}
}
