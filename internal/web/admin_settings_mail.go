package web

// Settings page section "E-mail" and the test e-mail. Templates:
// templates/partials/settings_mail.html.

import (
	"context"
	"net/http"
	"net/mail"
	"strconv"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminMailData is the view data of the e-mail section.
type adminMailData struct {
	SMTPPort        string // as entered
	HasSMTPPassword bool   // an SMTP password is stored
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
	if id := auth.FromContext(r.Context()); id != nil && id.User != nil {
		m.TestEmailTo = id.User.Email
	}
}

func (s *Server) adminMailSave(r *http.Request, cur, st *store.Settings, d *adminSettingsData) {
	errs := d.Errors
	switch m := r.FormValue("mail_method"); {
	case m == store.MailGraph || m == store.MailSMTP:
		st.MailMethod = m
	case formTrim(r, "smtp_host") != "":
		st.MailMethod = store.MailSMTP // form without a method selector
	}
	d.Mail.newSMTPPassword = r.FormValue("smtp_password")
	st.SMTPHost = formTrim(r, "smtp_host")
	st.SMTPSecurity = r.FormValue("smtp_security")
	if st.SMTPSecurity != "starttls" && st.SMTPSecurity != "tls" && st.SMTPSecurity != "none" {
		errs.add("smtp_security", "Choose STARTTLS, TLS or none.")
		st.SMTPSecurity = "starttls"
	}
	smtpPort := formTrim(r, "smtp_port")
	d.Mail.smtpPortEntered = &smtpPort
	switch {
	case smtpPort == "":
		st.SMTPPort = map[string]int{"starttls": 587, "tls": 465, "none": 25}[st.SMTPSecurity]
	default:
		p, err := strconv.Atoi(smtpPort)
		if err != nil || p < 1 || p > 65535 {
			errs.add("smtp_port", "Enter a port number between 1 and 65535 (usually 587 for STARTTLS, 465 for TLS).")
		} else {
			st.SMTPPort = p
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
		s.redirectWithFlash(w, r, back, "error", "E-mail is not configured yet: enter and save the SMTP server and sender address first.")
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
