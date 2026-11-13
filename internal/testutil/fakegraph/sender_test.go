package fakegraph

import (
	"strings"
	"testing"
)

// The sender mailbox in the path: /v1.0/users/{sender}, or the key syntax
// /v1.0/users('{sender}') (quotes doubled), which Microsoft Graph requires
// for a sender that begins with "$": it answers /users/$… with HTTP 400.
func TestFakeGraphSenderPath(t *testing.T) {
	rec := &recorder{TB: t}
	s := Start(rec)
	s.AddMailbox("$status@contoso.example")
	s.AddMailbox("$o'brien@contoso.example")
	tok := getToken(t, s)
	send := func(segments string) (int, string) {
		t.Helper()
		status, _, body := post(t, s.URL()+"/v1.0/"+segments+"/sendMail", "application/json", tok, validBody)
		return status, body
	}

	accepted := []struct{ segments, sender string }{
		{"users('$status@contoso.example')", "$status@contoso.example"},
		{"users('$o%27%27brien@contoso.example')", "$o'brien@contoso.example"},
		{"users('status@contoso.example')", "status@contoso.example"},
		{"users/status@contoso.example", "status@contoso.example"},
	}
	for _, tc := range accepted {
		if status, body := send(tc.segments); status != 202 {
			t.Errorf("%s: %d %s", tc.segments, status, body)
		}
	}
	msgs := s.Messages()
	for i, tc := range accepted {
		if i >= len(msgs) || msgs[i].Sender != tc.sender {
			t.Errorf("%s: messages %+v, want sender %q", tc.segments, msgs, tc.sender)
		}
	}
	if errs := rec.take(); len(errs) != 0 {
		t.Errorf("valid requests reported: %q", errs)
	}

	// Rejected like Microsoft Graph, and reported: the client must not send
	// these.
	for _, segments := range []string{"users/$status@contoso.example", "users/%24status@contoso.example", "groups('status@contoso.example')", "users('status@contoso.example'"} {
		if status, body := send(segments); status != 400 || !strings.Contains(body, `"code":"BadRequest"`) {
			t.Errorf("%s: %d %s, want 400 BadRequest", segments, status, body)
		}
		if errs := rec.take(); len(errs) != 1 {
			t.Errorf("%s: reported %q, want one error", segments, errs)
		}
	}
	if n := len(s.Messages()); n != len(accepted) {
		t.Errorf("%d messages accepted, want %d", n, len(accepted))
	}
}
