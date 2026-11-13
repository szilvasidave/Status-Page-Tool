package web

// Tests of the e-mail section of the settings page: the choice between
// Microsoft Graph and SMTP, its validation and the test e-mail.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakegraph"
)

const mailEntraIssuer = "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0"

// mailForm returns a valid settings form for the current settings with the
// single sign-on app registration of an Entra ID tenant and the given
// e-mail method.
func mailForm(t *testing.T, e *testEnv, method string) url.Values {
	t.Helper()
	f := adminSettingsForm(adminSettings(t, e))
	f.Set("oidc_enabled", "on")
	f.Set("oidc_issuer", mailEntraIssuer)
	f.Set("oidc_client_id", "client-1")
	f.Set("oidc_client_secret", "secret-1")
	f.Set("mail_method", method)
	return f
}

// mailSave posts the settings form and checks the status code.
func mailSave(t *testing.T, e *testEnv, f url.Values, want int) Response {
	t.Helper()
	r := e.post("/admin/settings", f)
	if r.Status != want {
		t.Fatalf("save settings: got %d, want %d\n%s", r.Status, want, r.Body)
	}
	return r
}

func TestMailSettingsPage(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()

	// A new installation: Microsoft Graph is chosen, the SMTP fields are
	// hidden, and the page says what Microsoft Graph needs.
	r := e.get("/admin/settings")
	mustContain(t, r.Body,
		`<select id="mail_method" name="mail_method" data-show-when="smtp" data-target="#smtp-fields">`,
		`<option value="graph" selected>Microsoft 365 (Microsoft Graph) — recommended</option>`,
		`<option value="smtp">SMTP server</option>`,
		`<div id="smtp-fields" hidden>`, `<div id="graph-fields">`, `name="graph_sender"`,
		"<strong>Mail.Send</strong>", "RBAC for Applications",
		`<p class="flash flash-warning">Single sign-on is not set up yet: enter the issuer, client ID and client secret of your Microsoft Entra ID app registration under Single sign-on.</p>`)

	// Single sign-on with another identity provider.
	st := adminSettings(t, e)
	st.OIDCIssuer, st.OIDCClientID, st.OIDCClientSecret = "https://keycloak.example.com/realms/it", "client", "secret"
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	mustContain(t, e.get("/admin/settings").Body, "The single sign-on issuer is not one of Microsoft Entra ID, so Microsoft Graph cannot be used.")

	// Microsoft Entra ID: nothing is missing.
	st.OIDCIssuer = mailEntraIssuer
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	if r := e.get("/admin/settings"); strings.Contains(r.Body, `<p class="flash flash-warning">`) {
		t.Error("a note about the app registration is shown although single sign-on is set up with Entra ID")
	}

	// SMTP chosen: its fields are shown.
	st.MailMethod, st.SMTPHost, st.SMTPFrom = store.MailSMTP, "smtp.example.com", "status@example.com"
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	r = e.get("/admin/settings")
	mustContain(t, r.Body, `<option value="graph">Microsoft 365`, `<option value="smtp" selected>SMTP server</option>`, `<div id="smtp-fields">`)
}

func TestMailSettingsGraph(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()

	// The sender needs the app registration of an Entra ID tenant.
	f := adminSettingsForm(adminSettings(t, e))
	f.Set("mail_method", "graph")
	f.Set("graph_sender", "status@contoso.example")
	r := mailSave(t, e, f, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, `id="graph_sender" name="graph_sender"`, `value="status@contoso.example"`, `aria-invalid="true"`,
		`<span class="field-error">Single sign-on is not set up yet: enter the issuer, client ID and client secret of your Microsoft Entra ID app registration under Single sign-on.</span>`)
	if strings.Contains(r.Body, `<p class="flash flash-warning">`) {
		t.Error("the note is shown in addition to the error")
	}

	tests := []struct {
		name   string
		change map[string]string
		want   string
	}{
		{"other identity provider", map[string]string{"oidc_issuer": "https://keycloak.example.com/realms/it"},
			"The single sign-on issuer is not one of Microsoft Entra ID, so Microsoft Graph cannot be used. With another identity provider, send e-mail with an SMTP server."},
		{"multi-tenant issuer", map[string]string{"oidc_issuer": "https://login.microsoftonline.com/common/v2.0"},
			"Under Single sign-on, the issuer must name your tenant (https://login.microsoftonline.com/&lt;tenant ID&gt;/v2.0), not &#34;common&#34;."},
		{"no client secret", map[string]string{"oidc_enabled": "", "oidc_client_secret": ""},
			"Enter the client ID and client secret of the app registration under Single sign-on."},
		{"display name", map[string]string{"graph_sender": "IT Status <status@contoso.example>"},
			"Enter the e-mail address of the mailbox, e.g. status@example.com."},
		{"no address", map[string]string{"graph_sender": "status"}, "Enter the e-mail address of the mailbox"},
		{"unknown method", map[string]string{"mail_method": "pigeon"}, "Choose Microsoft 365 (Microsoft Graph) or an SMTP server."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := mailForm(t, e, "graph")
			f.Set("graph_sender", "status@contoso.example")
			for k, v := range tt.change {
				f.Set(k, v)
			}
			r := mailSave(t, e, f, http.StatusUnprocessableEntity)
			mustContain(t, r.Body, "Please correct the highlighted", tt.want)
		})
	}

	// Valid: saved. The incomplete SMTP fields are hidden, so they are not
	// checked (but kept).
	f = mailForm(t, e, "graph")
	f.Set("graph_sender", " status@contoso.example ")
	f.Set("smtp_host", "smtp.example.com")
	f.Set("smtp_port", "not a port")
	f.Set("smtp_security", "ssl")
	f.Set("smtp_from", "")
	mailSave(t, e, f, http.StatusSeeOther)
	st := adminSettings(t, e)
	if st.MailMethod != store.MailGraph || st.GraphSender != "status@contoso.example" || !st.MailConfigured() ||
		st.SMTPHost != "smtp.example.com" || st.SMTPPort != 587 || st.SMTPSecurity != "starttls" {
		t.Fatalf("stored settings: %+v", st)
	}
	r = e.get("/admin/settings")
	mustContain(t, r.Body, `value="status@contoso.example"`, `<option value="graph" selected>`)

	// Single sign-on may be off: the app registration is still used.
	f = mailForm(t, e, "graph")
	f.Del("oidc_enabled")
	f.Set("oidc_client_secret", "") // keep the stored secret
	f.Set("graph_sender", "status@contoso.example")
	mailSave(t, e, f, http.StatusSeeOther)
	if st := adminSettings(t, e); st.OIDCEnabled || !st.MailConfigured() {
		t.Errorf("Microsoft Graph without single sign-on: %+v", st)
	}

	// No sender: e-mail is off, nothing else is needed.
	f = adminSettingsForm(adminSettings(t, e))
	f.Set("oidc_issuer", "")
	f.Set("mail_method", "graph")
	f.Set("graph_sender", "")
	mailSave(t, e, f, http.StatusSeeOther)
	if st := adminSettings(t, e); st.MailConfigured() || st.GraphSender != "" {
		t.Errorf("e-mail without a sender: %+v", st)
	}
}

func TestMailSettingsSMTP(t *testing.T) {
	e := newTestEnv(t)
	e.loginAdmin()

	// SMTP chosen: the hidden Microsoft Graph sender is not checked; the
	// SMTP fields are.
	f := adminSettingsForm(adminSettings(t, e))
	f.Set("mail_method", "smtp")
	f.Set("graph_sender", "not an address")
	f.Set("smtp_host", "smtp.example.com")
	r := mailSave(t, e, f, http.StatusUnprocessableEntity)
	mustContain(t, r.Body, "Enter the sender address for e-mails.", `<option value="smtp" selected>`, `<div id="smtp-fields">`)
	if strings.Contains(r.Body, "Enter the e-mail address of the mailbox") {
		t.Error("the hidden Microsoft Graph sender was checked")
	}
	f.Set("smtp_from", "IT Status <status@example.com>")
	mailSave(t, e, f, http.StatusSeeOther)
	st := adminSettings(t, e)
	if st.MailMethod != store.MailSMTP || !st.MailConfigured() || st.GraphSender != "not an address" {
		t.Fatalf("stored settings: %+v", st)
	}

	// A form without the method (e.g. a script) keeps the stored method;
	// the server is not used to guess it any more.
	f = adminSettingsForm(st)
	f.Del("mail_method")
	mailSave(t, e, f, http.StatusSeeOther)
	if st := adminSettings(t, e); st.MailMethod != store.MailSMTP {
		t.Errorf("method without the field: %q, want smtp", st.MailMethod)
	}
	st.MailMethod = store.MailGraph
	if err := e.Store.SaveSettings(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	f = adminSettingsForm(st) // with the SMTP server
	mailSave(t, e, f, http.StatusSeeOther)
	if st := adminSettings(t, e); st.MailMethod != store.MailGraph {
		t.Errorf("method without the field: %q, want graph", st.MailMethod)
	}
}

func TestMailTestEmailGraph(t *testing.T) {
	fg := fakegraph.Start(t)
	e := newTestEnv(t)
	e.loginAdmin()
	ctx := context.Background()
	send := func() Response {
		t.Helper()
		return adminFollow(t, e, e.post("/admin/settings/test-email", url.Values{"to": {"admin@example.com"}}))
	}

	mustContain(t, send().Body, "flash-error", "E-mail is not configured yet: enter and save the sender mailbox first.")

	st := adminSettings(t, e)
	st.SiteName = "ACME IT"
	st.MailMethod, st.GraphSender = store.MailGraph, fg.Sender
	st.OIDCIssuer, st.OIDCClientID = fg.Issuer(), fg.ClientID
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	mustContain(t, send().Body, "flash-error", "E-mail is not configured yet. Enter the client ID and client secret of the app registration under Single sign-on.")

	st.OIDCClientSecret = fg.ClientSecret
	if err := e.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	mustContain(t, send().Body, "flash-success", "A test e-mail was sent to admin@example.com.")
	got := fg.Messages()
	if len(got) != 1 || got[0].To != "admin@example.com" || got[0].Sender != fg.Sender || got[0].Subject != "[ACME IT] Test e-mail" {
		t.Fatalf("messages = %+v", got)
	}

	// A readable error, e.g. without the Mail.Send permission.
	fg.FailNext(fakegraph.AccessDenied())
	mustContain(t, send().Body, "flash-error", "The test e-mail could not be sent: Microsoft Graph returned HTTP 403 (ErrorAccessDenied)",
		"application permission Mail.Send with admin consent")
}
