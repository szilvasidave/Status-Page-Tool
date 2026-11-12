// Package fakesmtp is a minimal SMTP server for tests. It listens on
// 127.0.0.1 (no TLS, no authentication) and keeps every message it
// receives.
//
//	srv := fakesmtp.Start(t)
//	// configure the application with host 127.0.0.1, port srv.Port(), security "none"
//	msgs := srv.WaitFor(2, 10*time.Second)
//
// It is only meant to be used by tests.
package fakesmtp

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// Message is a received e-mail.
type Message struct {
	From    string   // envelope sender (MAIL FROM)
	To      []string // envelope recipients (RCPT TO)
	Data    []byte   // raw message (headers and body) as sent after DATA
	Subject string   // decoded Subject header
}

// Header returns the parsed message header (nil if it cannot be parsed).
func (m Message) Header() mail.Header {
	msg, err := mail.ReadMessage(bytes.NewReader(m.Data))
	if err != nil {
		return nil
	}
	return msg.Header
}

// Text returns the decoded text/plain body: the first text/plain part of a
// multipart message, or the whole body of a single-part message.
func (m Message) Text() string {
	msg, err := mail.ReadMessage(bytes.NewReader(m.Data))
	if err != nil {
		return ""
	}
	return findText(textproto.MIMEHeader(msg.Header), msg.Body)
}

func findText(h textproto.MIMEHeader, body io.Reader) string {
	mediaType, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart() // decodes quoted-printable itself
			if err != nil {
				return ""
			}
			if s := findText(part.Header, part); s != "" {
				return s
			}
		}
	}
	if mediaType != "text/plain" {
		return ""
	}
	switch strings.ToLower(h.Get("Content-Transfer-Encoding")) {
	case "base64":
		body = base64.NewDecoder(base64.StdEncoding, &newlineStripper{r: body})
	case "quoted-printable":
		body = quotedprintable.NewReader(body)
	}
	b, _ := io.ReadAll(body)
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// newlineStripper removes CR and LF (line breaks of base64 bodies).
type newlineStripper struct{ r io.Reader }

func (s *newlineStripper) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	j := 0
	for _, c := range p[:n] {
		if c != '\r' && c != '\n' {
			p[j] = c
			j++
		}
	}
	return j, err
}

// Server is a running fake SMTP server.
type Server struct {
	t  testing.TB
	ln net.Listener

	mu       sync.Mutex
	msgs     []Message
	conns    map[net.Conn]bool // open connections, closed when the test ends
	received chan struct{}     // signalled after every message
	wg       sync.WaitGroup
}

// Start starts a server on 127.0.0.1 and a random port; it is stopped when
// the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{t: t, ln: ln, conns: map[net.Conn]bool{}, received: make(chan struct{}, 1)}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		ln.Close()
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}

// Addr returns the address (127.0.0.1:port).
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Host returns the host name to connect to (127.0.0.1).
func (s *Server) Host() string { return "127.0.0.1" }

// Port returns the port.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Messages returns the messages received so far.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

// Reset forgets the received messages.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = nil
}

// WaitFor waits until at least n messages have been received and returns
// all received messages. It fails the test after timeout.
func (s *Server) WaitFor(n int, timeout time.Duration) []Message {
	s.t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if msgs := s.Messages(); len(msgs) >= n {
			return msgs
		}
		select {
		case <-s.received:
		case <-deadline.C:
			msgs := s.Messages()
			var got []string
			for _, m := range msgs {
				got = append(got, fmt.Sprintf("%v %q", m.To, m.Subject))
			}
			s.t.Fatalf("fakesmtp: got %d message(s) within %s, want %d: %v", len(msgs), timeout, n, got)
			return msgs
		}
	}
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = true
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
		}()
	}
}

// handle speaks just enough SMTP for a client without TLS and AUTH.
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	tp := textproto.NewConn(conn)
	reply := func(format string, args ...any) { _ = tp.PrintfLine(format, args...) }
	reply("220 fakesmtp ESMTP ready")
	var from string
	var to []string
	for {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			reply("250-fakesmtp")
			reply("250-8BITMIME")
			reply("250 PIPELINING")
		case "HELO":
			reply("250 fakesmtp")
		case "MAIL":
			from, to = address(arg), nil
			reply("250 2.1.0 OK")
		case "RCPT":
			if from == "" && !strings.Contains(arg, "<>") {
				reply("503 5.5.1 MAIL first")
				continue
			}
			to = append(to, address(arg))
			reply("250 2.1.5 OK")
		case "DATA":
			if len(to) == 0 {
				reply("503 5.5.1 RCPT first")
				continue
			}
			reply("354 End data with <CR><LF>.<CR><LF>")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			s.store(Message{From: from, To: to, Data: data})
			from, to = "", nil
			reply("250 2.0.0 OK queued")
		case "RSET":
			from, to = "", nil
			reply("250 2.0.0 OK")
		case "NOOP":
			reply("250 2.0.0 OK")
		case "QUIT":
			reply("221 2.0.0 Bye")
			return
		default:
			reply("502 5.5.2 command not implemented")
		}
	}
}

func (s *Server) store(m Message) {
	if h := m.Header(); h != nil {
		subject, err := new(mime.WordDecoder).DecodeHeader(h.Get("Subject"))
		if err != nil {
			subject = h.Get("Subject")
		}
		m.Subject = subject
	}
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.mu.Unlock()
	select {
	case s.received <- struct{}{}:
	default:
	}
}

// address returns the address in angle brackets of a MAIL FROM / RCPT TO
// argument ("FROM:<a@b> BODY=8BITMIME" → "a@b").
func address(arg string) string {
	start := strings.IndexByte(arg, '<')
	end := strings.IndexByte(arg, '>')
	if start < 0 || end < start {
		_, v, _ := strings.Cut(arg, ":")
		return strings.TrimSpace(v)
	}
	return arg[start+1 : end]
}
