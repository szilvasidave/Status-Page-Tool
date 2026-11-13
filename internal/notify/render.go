package notify

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/theme"
)

// Email is a rendered notification e-mail.
type Email struct {
	Subject string
	Text    string // plain text body
	HTML    string // HTML body
}

// MaintenanceEvent says what happened to a maintenance window.
type MaintenanceEvent string

const (
	MaintenanceEventScheduled MaintenanceEvent = "scheduled"
	MaintenanceEventUpdated   MaintenanceEvent = "updated"
	MaintenanceEventCancelled MaintenanceEvent = "cancelled"
	MaintenanceEventStarted   MaintenanceEvent = "started"
	MaintenanceEventCompleted MaintenanceEvent = "completed"
)

// emailFooter explains why the recipient gets the e-mail.
const emailFooter = "You receive this e-mail because you are a member of a group that is subscribed to one of the affected services."

// timeLayout is used for all times in e-mails, e.g. "Thu 12 Nov 2026 14:05 CET".
const timeLayout = "Mon 2 Jan 2006 15:04 MST"

// Colours as used by the web UI.
const (
	colorOperational   = "#1f9d55"
	colorMaintenance   = "#2f6fdb"
	colorDegraded      = "#d4a106"
	colorPartialOutage = "#e5731a"
	colorMajorOutage   = "#d32f2f"
	colorMuted         = "#5f6b7a"
)

func statusColor(s store.Status) string {
	switch s {
	case store.StatusMaintenance:
		return colorMaintenance
	case store.StatusDegraded:
		return colorDegraded
	case store.StatusPartialOutage:
		return colorPartialOutage
	case store.StatusMajorOutage:
		return colorMajorOutage
	}
	return colorOperational
}

//go:embed templates/email.html
var templateFS embed.FS

var emailTemplate = template.Must(template.New("email.html").Funcs(template.FuncMap{
	// nl2br escapes text and turns line breaks into <br>.
	"nl2br": func(text string) template.HTML {
		esc := template.HTMLEscapeString(strings.ReplaceAll(text, "\r\n", "\n"))
		return template.HTML(strings.ReplaceAll(esc, "\n", "<br>\n"))
	},
}).ParseFS(templateFS, "templates/email.html"))

// emailView is the data for both the text and the HTML version of an e-mail.
type emailView struct {
	Subject    string
	SiteName   string
	Badge      string // short state, e.g. "Investigating"
	BadgeColor string
	Title      string
	Intro      string
	Facts      []emailFact
	Message    string // multi-line text
	Services   []emailService
	LinkURL    string
	LinkText   string
	Footer     string
	// ButtonColor and ButtonText are the colours of the link button: the
	// light-mode accent colour of the branding (theme.EmailButton).
	ButtonColor string
	ButtonText  string
}

type emailFact struct{ Label, Value string }

type emailService struct {
	Name  string
	Label string // impact, may be empty
	Color string
}

// RenderIncident renders the e-mail for a new incident (isNew) or for the
// update upd of an incident. If upd is nil the latest update of inc is used.
func RenderIncident(st *store.Settings, inc *store.Incident, upd *store.IncidentUpdate, isNew bool) Email {
	st = settingsOrDefault(st)
	loc := st.Location()
	if upd == nil && len(inc.Updates) > 0 {
		upd = &inc.Updates[0]
	}
	status := inc.Status
	message := ""
	if upd != nil {
		if upd.Status != "" {
			status = upd.Status
		}
		message = upd.Message
	}

	v := emailView{
		SiteName: st.SiteName,
		Badge:    status.Label(),
		Title:    oneLine(inc.Title),
		Message:  strings.TrimSpace(message),
		Footer:   emailFooter,
	}
	v.ButtonColor, v.ButtonText = theme.EmailButton(st.ThemeAccentLight)
	switch {
	case status == store.IncidentResolved:
		v.Subject = subject(st, "Resolved: "+v.Title)
		v.Intro = "This incident has been resolved."
	case isNew:
		v.Subject = subject(st, status.Label()+": "+v.Title)
		v.Intro = "A new incident has been reported."
	default:
		v.Subject = subject(st, "Update – "+status.Label()+": "+v.Title)
		v.Intro = "There is an update on this incident."
	}

	impact := inc.Impact()
	switch {
	case status == store.IncidentResolved:
		v.BadgeColor = colorOperational
	case impact == store.StatusOperational:
		v.BadgeColor = colorDegraded
	default:
		v.BadgeColor = statusColor(impact)
	}

	v.Facts = append(v.Facts, emailFact{"Status", status.Label()})
	if !inc.StartedAt.IsZero() {
		v.Facts = append(v.Facts, emailFact{"Started", inc.StartedAt.In(loc).Format(timeLayout)})
	}
	if status == store.IncidentResolved && inc.ResolvedAt != nil && !inc.ResolvedAt.IsZero() {
		v.Facts = append(v.Facts, emailFact{"Resolved", inc.ResolvedAt.In(loc).Format(timeLayout)})
	}

	for _, s := range inc.Services {
		v.Services = append(v.Services, emailService{
			Name:  serviceName(s.ServiceName, s.ServiceID),
			Label: s.Impact.Label(),
			Color: statusColor(s.Impact),
		})
	}
	if base := baseURL(st); base != "" {
		v.LinkURL = base + "/incidents/" + strconv.FormatInt(inc.ID, 10)
		v.LinkText = "View incident"
	}
	return render(v)
}

// RenderMaintenance renders the e-mail for event ev of maintenance m.
func RenderMaintenance(st *store.Settings, m *store.Maintenance, ev MaintenanceEvent) Email {
	st = settingsOrDefault(st)
	title := oneLine(m.Title)
	v := emailView{
		SiteName:   st.SiteName,
		Title:      title,
		Message:    strings.TrimSpace(m.Description),
		Footer:     emailFooter,
		BadgeColor: colorMaintenance,
		Facts:      []emailFact{{"When", formatWindow(m.StartsAt, m.EndsAt, st.Location())}},
	}
	v.ButtonColor, v.ButtonText = theme.EmailButton(st.ThemeAccentLight)
	switch ev {
	case MaintenanceEventScheduled:
		v.Subject = subject(st, "Scheduled maintenance: "+title)
		v.Badge = "Scheduled maintenance"
		v.Intro = "Maintenance has been scheduled for the services below."
	case MaintenanceEventCancelled:
		v.Subject = subject(st, "Maintenance cancelled: "+title)
		v.Badge = "Cancelled"
		v.BadgeColor = colorMuted
		v.Intro = "This maintenance has been cancelled. The services below will not be affected by it."
	case MaintenanceEventStarted:
		v.Subject = subject(st, "Maintenance started: "+title)
		v.Badge = "In progress"
		v.Intro = "This maintenance has started. The services below may be unavailable or slow until it ends."
	case MaintenanceEventCompleted:
		v.Subject = subject(st, "Maintenance completed: "+title)
		v.Badge = "Completed"
		v.BadgeColor = colorOperational
		v.Intro = "This maintenance has been completed."
	default: // MaintenanceEventUpdated
		v.Subject = subject(st, "Maintenance updated: "+title)
		v.Badge = "Maintenance updated"
		v.Intro = "This maintenance has been changed. Please check the time window and the affected services."
	}

	for _, s := range m.Services {
		v.Services = append(v.Services, emailService{Name: serviceName(s.Name, s.ID)})
	}
	if base := baseURL(st); base != "" {
		v.LinkURL = base + "/maintenance/" + strconv.FormatInt(m.ID, 10)
		v.LinkText = "View maintenance"
	}
	return render(v)
}

// formatWindow formats a time window, e.g. "Mon 16 Nov 2026 22:00 – 23:30 CET"
// or "Mon 16 Nov 2026 22:00 CET – Tue 17 Nov 2026 02:00 CET".
func formatWindow(start, end time.Time, loc *time.Location) string {
	s, e := start.In(loc), end.In(loc)
	if s.Format("2006-01-02 MST") == e.Format("2006-01-02 MST") {
		return s.Format("Mon 2 Jan 2006 15:04") + " – " + e.Format("15:04 MST")
	}
	return s.Format(timeLayout) + " – " + e.Format(timeLayout)
}

func render(v emailView) Email {
	return Email{Subject: v.Subject, Text: renderText(v), HTML: renderHTML(v)}
}

// renderHTML renders the HTML version of v. A view without ButtonColor
// (a renderer that did not set it) gets the default button colours, so that
// the link button is never drawn without a background.
func renderHTML(v emailView) string {
	if v.ButtonColor == "" {
		v.ButtonColor, v.ButtonText = theme.EmailButton("")
	}
	var buf bytes.Buffer
	if err := emailTemplate.Execute(&buf, v); err != nil {
		// Only possible with a broken template; the text version is enough.
		return ""
	}
	return buf.String()
}

func renderText(v emailView) string {
	var b strings.Builder
	b.WriteString(v.Title + "\n")
	if v.Intro != "" {
		b.WriteString(v.Intro + "\n")
	}
	if len(v.Facts) > 0 {
		width := 0
		for _, f := range v.Facts {
			width = max(width, len(f.Label)+1)
		}
		b.WriteString("\n")
		for _, f := range v.Facts {
			fmt.Fprintf(&b, "%-*s %s\n", width, f.Label+":", f.Value)
		}
	}
	if v.Message != "" {
		b.WriteString("\n" + strings.ReplaceAll(v.Message, "\r\n", "\n") + "\n")
	}
	if len(v.Services) > 0 {
		b.WriteString("\nAffected services:\n")
		for _, s := range v.Services {
			if s.Label != "" {
				fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Label)
			} else {
				fmt.Fprintf(&b, "- %s\n", s.Name)
			}
		}
	}
	if v.LinkURL != "" {
		fmt.Fprintf(&b, "\n%s: %s\n", v.LinkText, v.LinkURL)
	}
	b.WriteString("\n-- \n" + v.Footer + "\n")
	return b.String()
}

func settingsOrDefault(st *store.Settings) *store.Settings {
	if st == nil {
		d := store.DefaultSettings()
		return &d
	}
	return st
}

// subject prefixes s with the site name in brackets.
func subject(st *store.Settings, s string) string {
	if name := oneLine(st.SiteName); name != "" {
		return "[" + name + "] " + s
	}
	return s
}

func baseURL(st *store.Settings) string {
	return strings.TrimRight(strings.TrimSpace(st.BaseURL), "/")
}

func serviceName(name string, id int64) string {
	if name != "" {
		return name
	}
	return "Service #" + strconv.FormatInt(id, 10)
}
