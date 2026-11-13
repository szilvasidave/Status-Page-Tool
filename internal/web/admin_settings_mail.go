package web

// Settings page section "E-mail" and the test e-mail. Templates:
// templates/partials/settings_mail.html.
//
// E-mails are sent with Microsoft Graph (Microsoft 365, the default) or an
// SMTP server. The fields of both methods are kept, so that switching back
// restores them, but only those of the chosen method are checked, as the
// others are hidden.

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/msgraph"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminMailData is the view data of the e-mail section.
type adminMailData struct {
	SMTPPort        string // as entered
	HasSMTPPassword bool   // an SMTP password is stored
	GraphNote       string // why Microsoft Graph cannot be used with the single sign-on settings ("" if it can)
	TestEmailTo     string
	newSMTPPassword string // password entered in a form that failed validation
	smtpPortEntered *string
}

func (s *Server) adminMailView(r *http.Request, cur *store.Settings, d *adminSettingsData) {
	m := &d.Mail
	switch {
	case m.smtpPortEntered != nil:
		m.SMTPPort = *m.smtpPortEntered
	case d.St.SMTPPort == 0:
		m.SMTPPort = ""
	default:
		m.SMTPPort = strconv.Itoa(d.St.SMTPPort)
	}
	m.HasSMTPPassword = cur.SMTPPassword != "" && d.St.SMTPUsername != ""
	// Show a newly entered password again so it need not be retyped; the
	// stored password is never sent to the browser.
	d.St.SMTPPassword = m.newSMTPPassword

	// The single sign-on section has replaced a stored client secret in
	// d.St with the newly entered one ("" if none).
	app := d.St
	if app.OIDCClientSecret == "" {
		app.OIDCClientSecret = cur.OIDCClientSecret
	}
	if d.Errors["graph_sender"] == "" {
		m.GraphNote = adminGraphAppProblem(&app)
	}
	if id := auth.FromContext(r.Context()); id != nil && id.User != nil {
		m.TestEmailTo = id.User.Email
	}
}

func (s *Server) adminMailSave(r *http.Request, cur, st *store.Settings, d *adminSettingsData) {
	errs := d.Errors
	switch method := r.FormValue("mail_method"); method {
	case store.MailGraph, store.MailSMTP:
		st.MailMethod = method
	case "":
		// Not in the form (e.g. a script that posts some fields): keep it.
	default:
		errs.add("mail_method", "Choose Microsoft 365 (Microsoft Graph) or an SMTP server.")
	}
	useSMTP := st.MailMethod == store.MailSMTP

	// Microsoft Graph: the sender mailbox. The app registration is the one
	// of the single sign-on section, which is saved before this one.
	st.GraphSender = formTrim(r, "graph_sender")
	if !useSMTP && st.GraphSender != "" {
		if a, err := mail.ParseAddress(st.GraphSender); err != nil || a.Address != st.GraphSender {
			errs.add("graph_sender", "Enter the e-mail address of the mailbox, e.g. status@example.com.")
		} else if msg := adminGraphAppProblem(st); msg != "" {
			errs.add("graph_sender", msg)
		}
	}

	// SMTP.
	d.Mail.newSMTPPassword = r.FormValue("smtp_password")
	st.SMTPHost = formTrim(r, "smtp_host")
	switch security := r.FormValue("smtp_security"); security {
	case "starttls", "tls", "none":
		st.SMTPSecurity = security
	default:
		if useSMTP {
			errs.add("smtp_security", "Choose STARTTLS, TLS or none.")
			st.SMTPSecurity = "starttls"
		}
	}
	smtpPort := formTrim(r, "smtp_port")
	d.Mail.smtpPortEntered = &smtpPort
	switch {
	case smtpPort == "":
		st.SMTPPort = map[string]int{"starttls": 587, "tls": 465, "none": 25}[st.SMTPSecurity]
	default:
		p, err := strconv.Atoi(smtpPort)
		switch {
		case err == nil && p >= 1 && p <= 65535:
			st.SMTPPort = p
		case useSMTP:
			errs.add("smtp_port", "Enter a port number between 1 and 65535 (usually 587 for STARTTLS, 465 for TLS).")
		}
	}
	st.SMTPUsername = formTrim(r, "smtp_username")
	if d.Mail.newSMTPPassword != "" {
		st.SMTPPassword = d.Mail.newSMTPPassword
	}
	if st.SMTPUsername == "" {
		st.SMTPPassword = "" // no authentication: do not keep an unused password
	}
	st.SMTPFrom = formTrim(r, "smtp_from")
	if !useSMTP {
		return
	}
	if st.SMTPFrom != "" {
		if _, err := mail.ParseAddress(st.SMTPFrom); err != nil {
			errs.add("smtp_from", "Enter a valid sender address, e.g. status@example.com or IT Status <status@example.com>.")
		}
	} else if st.SMTPHost != "" {
		errs.add("smtp_from", "Enter the sender address for e-mails.")
	}
	if st.SMTPHost == "" && st.SMTPFrom != "" {
		errs.add("smtp_host", "Enter the SMTP server, or clear the sender address to turn e-mail off.")
	}
}

// adminGraphAppProblem tells what Microsoft Graph is missing in the single
// sign-on settings of st, whose app registration it uses ("" if nothing).
func adminGraphAppProblem(st *store.Settings) string {
	if strings.TrimSpace(st.OIDCIssuer) == "" {
		return "Single sign-on is not set up yet: enter the issuer, client ID and client secret of your Microsoft Entra ID " +
			"app registration under Single sign-on."
	}
	if _, err := msgraph.Tenant(st.OIDCIssuer); errors.Is(err, msgraph.ErrNotEntra) {
		return "The single sign-on issuer is not one of Microsoft Entra ID, so Microsoft Graph cannot be used. " +
			"With another identity provider, send e-mail with an SMTP server."
	} else if err != nil {
		return "Under Single sign-on, " + err.Error() + "."
	}
	if strings.TrimSpace(st.OIDCClientID) == "" || st.OIDCClientSecret == "" {
		return "Enter the client ID and client secret of the app registration under Single sign-on."
	}
	return ""
}

func (s *Server) handleTestEmail(w http.ResponseWriter, r *http.Request) {
	const back = "/admin/settings"
	to := formTrim(r, "to")
	if _, err := mail.ParseAddress(to); err != nil || to == "" {
		s.redirectWithFlash(w, r, back, "error", "Enter a valid e-mail address to send the test e-mail to.")
		return
	}
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !st.MailConfigured() {
		msg := "E-mail is not configured yet: enter and save the sender mailbox first."
		switch {
		case st.MailMethod == store.MailSMTP:
			msg = "E-mail is not configured yet: enter and save the SMTP server and sender address first."
		case st.GraphSender != "":
			msg = "E-mail is not configured yet. " + adminGraphAppProblem(st)
		}
		s.redirectWithFlash(w, r, back, "error", msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.Notifier.SendTestEmail(ctx, to); err != nil {
		s.Log.Warn("test e-mail failed", "to", to, "err", err)
		s.redirectWithFlash(w, r, back, "error", "The test e-mail could not be sent: "+err.Error())
		return
	}
	s.redirectWithFlash(w, r, back, "success", "A test e-mail was sent to "+to+". Check the inbox (and the spam folder).")
}
