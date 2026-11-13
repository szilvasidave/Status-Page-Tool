package fakesmtp

import (
	"context"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/notify"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

func TestNetSMTP(t *testing.T) {
	srv := Start(t)
	body := "Subject: =?utf-8?q?Gr=C3=BC=C3=9Fe?=\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Hello =C3=BC, see https://example.com/a?b=3Dc and a long line that is wrapped =\r\nhere.\r\n" +
		".leading dot\r\n"
	if err := smtp.SendMail(srv.Addr(), nil, "from@example.com", []string{"a@example.com", "b@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	msgs := srv.WaitFor(1, 5*time.Second)
	m := msgs[0]
	if m.From != "from@example.com" || strings.Join(m.To, ",") != "a@example.com,b@example.com" {
		t.Fatalf("envelope: %q %q", m.From, m.To)
	}
	if m.Subject != "Grüße" {
		t.Fatalf("subject %q", m.Subject)
	}
	want := "Hello ü, see https://example.com/a?b=c and a long line that is wrapped here.\n.leading dot\n"
	if got := m.Text(); got != want {
		t.Fatalf("text:\n%q\nwant\n%q", got, want)
	}
	srv.Reset()
	if len(srv.Messages()) != 0 {
		t.Fatal("Reset did not forget the messages")
	}
}

// TestNotifySend checks that the application's mailer (go-mail) can deliver
// to the fake server and that multipart bodies are decoded.
func TestNotifySend(t *testing.T) {
	srv := Start(t)
	st := &store.Settings{MailMethod: store.MailSMTP, SMTPHost: srv.Host(), SMTPPort: srv.Port(), SMTPSecurity: "none", SMTPFrom: "Status <status@example.com>"}
	long := "https://status.example.com/incidents/12345?with=a&very=long&query=string&that=needs&soft=line&breaks=yes"
	errs, err := notify.Send(context.Background(), st, []notify.Message{
		{To: "a@example.com", Subject: "[Status] Update – Identified: Grüße", Text: "Line 1\nLink: " + long + "\n", HTML: "<p>html</p>"},
		{To: "b@example.com", Subject: "Second", Text: "Two"},
	})
	if err != nil || errs[0] != nil || errs[1] != nil {
		t.Fatalf("send: %v %v", err, errs)
	}
	msgs := srv.WaitFor(2, 5*time.Second)
	if msgs[0].Subject != "[Status] Update – Identified: Grüße" || msgs[0].To[0] != "a@example.com" || msgs[0].From != "status@example.com" {
		t.Fatalf("first message: %q %q %q", msgs[0].Subject, msgs[0].To, msgs[0].From)
	}
	if text := msgs[0].Text(); !strings.Contains(text, "Link: "+long+"\n") {
		t.Fatalf("text body: %q", text)
	}
	if msgs[1].Subject != "Second" || strings.TrimSpace(msgs[1].Text()) != "Two" {
		t.Fatalf("second message: %q %q", msgs[1].Subject, msgs[1].Text())
	}
}

func TestWaitForFails(t *testing.T) {
	ft := &fakeT{TB: t}
	srv := Start(t)
	srv.t = ft
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		srv.WaitFor(1, 50*time.Millisecond)
	}()
	<-done
	if !ft.failed {
		t.Fatal("WaitFor did not fail")
	}
}

// fakeT records Fatalf instead of stopping the test.
type fakeT struct {
	testing.TB
	failed bool
}

func (f *fakeT) Helper() {}
func (f *fakeT) Fatalf(string, ...any) {
	f.failed = true
	panic("fatal")
}
