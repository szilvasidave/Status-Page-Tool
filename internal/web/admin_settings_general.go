package web

// Settings page section "General": site name, base URL, time zone, public
// access. Template: templates/partials/settings_general.html.

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminGeneralData is the view data of the "General" section.
type adminGeneralData struct {
	LocalBaseURL bool // the base URL points to localhost: other computers cannot use it
	Timezones    []string
}

// adminTimezones are offered in the time zone <datalist>; any IANA name is
// accepted.
var adminTimezones = []string{
	"UTC",
	"Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Paris", "Europe/Brussels",
	"Europe/Amsterdam", "Europe/Berlin", "Europe/Zurich", "Europe/Vienna", "Europe/Rome",
	"Europe/Madrid", "Europe/Prague", "Europe/Budapest", "Europe/Warsaw", "Europe/Stockholm",
	"Europe/Oslo", "Europe/Copenhagen", "Europe/Helsinki", "Europe/Athens", "Europe/Bucharest",
	"Europe/Istanbul", "Europe/Kyiv", "Europe/Moscow",
	"America/New_York", "America/Chicago", "America/Denver", "America/Phoenix",
	"America/Los_Angeles", "America/Anchorage", "America/Toronto", "America/Vancouver",
	"America/Mexico_City", "America/Bogota", "America/Sao_Paulo", "America/Argentina/Buenos_Aires",
	"Africa/Johannesburg", "Africa/Lagos", "Africa/Cairo", "Africa/Nairobi",
	"Asia/Dubai", "Asia/Kolkata", "Asia/Bangkok", "Asia/Singapore", "Asia/Hong_Kong",
	"Asia/Shanghai", "Asia/Seoul", "Asia/Tokyo",
	"Australia/Perth", "Australia/Brisbane", "Australia/Sydney", "Pacific/Auckland",
}

func (s *Server) adminGeneralView(r *http.Request, cur *store.Settings, d *adminSettingsData) {
	d.General.Timezones = adminTimezones
	if u, err := url.Parse(d.St.BaseURL); err == nil && d.St.BaseURL != "" {
		d.General.LocalBaseURL = adminIsLocalhost(u.Hostname())
	}
}

func (s *Server) adminGeneralSave(r *http.Request, cur, st *store.Settings, d *adminSettingsData) {
	errs := d.Errors
	st.SiteName = formTrim(r, "site_name")
	switch {
	case st.SiteName == "":
		errs.add("site_name", "Enter a name for the status page.")
	case tooLong(st.SiteName, 100):
		errs.add("site_name", "The name is too long (at most 100 characters).")
	}
	var msg string
	if st.BaseURL, msg = adminValidateBaseURL(r.FormValue("base_url")); msg != "" {
		errs.add("base_url", msg)
	}
	if st.Timezone, msg = adminValidateTimezone(r.FormValue("timezone")); msg != "" {
		errs.add("timezone", msg)
	}
	st.PublicAccess = formBool(r, "public_access")
}

// adminValidateBaseURL checks the base URL setting: empty, or an absolute
// http(s) URL without path, query or fragment. It returns the value without
// a trailing slash and an error message for the user ("" if valid).
func adminValidateBaseURL(v string) (string, string) {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v == "" {
		return "", ""
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" {
		return v, "Enter the full address starting with http:// or https://, e.g. https://status.example.com."
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(v, "?#") {
		return v, "Enter only the scheme and host name (and port), e.g. https://status.example.com; the status page cannot be served from a sub-path."
	}
	return v, ""
}

// adminValidateTimezone checks an IANA time zone name ("" means UTC). It
// returns the name and an error message for the user ("" if valid).
func adminValidateTimezone(v string) (string, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "UTC", ""
	}
	if _, err := time.LoadLocation(v); err != nil || v == "Local" {
		return v, fmt.Sprintf("Unknown time zone %q. Use a name such as Europe/Berlin, America/New_York or UTC.", v)
	}
	return v, ""
}

func adminIsLocalhost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
