package web

// Settings page section "General": site name, base URL, time zone, public
// access. Template: templates/partials/settings_general.html. The time zone
// select is also used by the setup wizard (template "timezone-field").

import (
	"cmp"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/store"
)

// adminGeneralData is the view data of the "General" section.
type adminGeneralData struct {
	LocalBaseURL bool // the base URL points to localhost: other computers cannot use it
	Timezones    adminTZChoices
}

func (s *Server) adminGeneralView(r *http.Request, cur *store.Settings, d *adminSettingsData) {
	d.General.Timezones = adminTimezoneChoices(d.St.Timezone, time.Now())
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

// ------------------------------------------------------------------ time zone select

// adminTZOption is an option of the time zone select.
type adminTZOption struct {
	Value    string // IANA name
	Label    string // e.g. "Berlin (UTC+01:00)"
	Selected bool
}

// adminTZGroup is an option group (a region) of the time zone select.
type adminTZGroup struct {
	Label   string // e.g. "Europe"
	Options []adminTZOption
}

// adminTZChoices are the options of the time zone select (template
// "timezone-field"): UTC (and a current value that is not offered) first,
// then the offered time zones grouped by region.
type adminTZChoices struct {
	Top    []adminTZOption
	Groups []adminTZGroup
}

// adminTimezoneChoices returns the options of the time zone select with
// current ("" = UTC) selected; the labels show the UTC offsets at now. A
// current value that is not offered (e.g. stored before the select existed,
// or an invalid value shown with its error message) is added after UTC, so
// that saving the form again keeps it.
func adminTimezoneChoices(current string, now time.Time) adminTZChoices {
	if current == "" {
		current = "UTC"
	}
	c := adminTZChoices{Top: []adminTZOption{{Value: "UTC", Label: "UTC", Selected: current == "UTC"}}}
	found := current == "UTC"
	var region string
	for _, z := range adminZones() {
		if len(c.Groups) == 0 || z.region != region {
			region = z.region
			c.Groups = append(c.Groups, adminTZGroup{Label: cmp.Or(adminRegionLabels[region], region)})
		}
		g := &c.Groups[len(c.Groups)-1]
		selected := z.name == current
		found = found || selected
		g.Options = append(g.Options, adminTZOption{Value: z.name, Label: z.city + " (" + adminUTCOffset(now.In(z.loc)) + ")", Selected: selected})
	}
	if !found {
		label := current
		if loc, err := time.LoadLocation(current); err == nil && current != "Local" {
			label += " (" + adminUTCOffset(now.In(loc)) + ")"
		}
		c.Top = append(c.Top, adminTZOption{Value: current, Label: label, Selected: true})
	}
	return c
}

// adminUTCOffset formats the UTC offset of t, e.g. "UTC+01:00" or "UTC-03:30".
func adminUTCOffset(t time.Time) string {
	_, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, off/3600, off%3600/60)
}

// adminZone is an offered time zone.
type adminZone struct {
	name   string // IANA name, e.g. "America/Argentina/Buenos_Aires"
	region string // e.g. "America"
	city   string // e.g. "Buenos Aires, Argentina"
	loc    *time.Location
}

// adminZones returns the offered time zones that can be loaded, sorted by
// region and city. They are loaded once.
var adminZones = sync.OnceValue(func() []adminZone {
	zones := make([]adminZone, 0, len(adminTimezones))
	for _, name := range adminTimezones {
		loc, err := time.LoadLocation(name)
		if err != nil {
			continue
		}
		region, rest, _ := strings.Cut(name, "/")
		parts := strings.Split(rest, "/")
		slices.Reverse(parts) // "Argentina/Buenos_Aires" -> "Buenos Aires, Argentina"
		city := strings.ReplaceAll(strings.Join(parts, ", "), "_", " ")
		zones = append(zones, adminZone{name: name, region: region, city: city, loc: loc})
	}
	slices.SortStableFunc(zones, func(a, b adminZone) int {
		return cmp.Or(strings.Compare(a.region, b.region), strings.Compare(strings.ToLower(a.city), strings.ToLower(b.city)))
	})
	return zones
})

// adminRegionLabels are the labels of the regions (option groups) whose IANA
// name is not plain English.
var adminRegionLabels = map[string]string{
	"America": "Americas", "Atlantic": "Atlantic Ocean", "Indian": "Indian Ocean", "Pacific": "Pacific Ocean",
}

// adminTimezones are the time zones offered in the time zone select: the
// zones of zone.tab of the IANA time zone database (version 2025c, as in Go's
// time/tzdata), which has at least one zone per country. Any other IANA name
// is accepted as well (see adminTimezoneChoices).
var adminTimezones = []string{
	"Africa/Abidjan", "Africa/Accra", "Africa/Addis_Ababa", "Africa/Algiers", "Africa/Asmara", "Africa/Bamako",
	"Africa/Bangui", "Africa/Banjul", "Africa/Bissau", "Africa/Blantyre", "Africa/Brazzaville",
	"Africa/Bujumbura", "Africa/Cairo", "Africa/Casablanca", "Africa/Ceuta", "Africa/Conakry", "Africa/Dakar",
	"Africa/Dar_es_Salaam", "Africa/Djibouti", "Africa/Douala", "Africa/El_Aaiun", "Africa/Freetown",
	"Africa/Gaborone", "Africa/Harare", "Africa/Johannesburg", "Africa/Juba", "Africa/Kampala",
	"Africa/Khartoum", "Africa/Kigali", "Africa/Kinshasa", "Africa/Lagos", "Africa/Libreville", "Africa/Lome",
	"Africa/Luanda", "Africa/Lubumbashi", "Africa/Lusaka", "Africa/Malabo", "Africa/Maputo", "Africa/Maseru",
	"Africa/Mbabane", "Africa/Mogadishu", "Africa/Monrovia", "Africa/Nairobi", "Africa/Ndjamena",
	"Africa/Niamey", "Africa/Nouakchott", "Africa/Ouagadougou", "Africa/Porto-Novo", "Africa/Sao_Tome",
	"Africa/Tripoli", "Africa/Tunis", "Africa/Windhoek",
	"America/Adak", "America/Anchorage", "America/Anguilla", "America/Antigua", "America/Araguaina",
	"America/Argentina/Buenos_Aires", "America/Argentina/Catamarca", "America/Argentina/Cordoba",
	"America/Argentina/Jujuy", "America/Argentina/La_Rioja", "America/Argentina/Mendoza",
	"America/Argentina/Rio_Gallegos", "America/Argentina/Salta", "America/Argentina/San_Juan",
	"America/Argentina/San_Luis", "America/Argentina/Tucuman", "America/Argentina/Ushuaia", "America/Aruba",
	"America/Asuncion", "America/Atikokan", "America/Bahia", "America/Bahia_Banderas", "America/Barbados",
	"America/Belem", "America/Belize", "America/Blanc-Sablon", "America/Boa_Vista", "America/Bogota",
	"America/Boise", "America/Cambridge_Bay", "America/Campo_Grande", "America/Cancun", "America/Caracas",
	"America/Cayenne", "America/Cayman", "America/Chicago", "America/Chihuahua", "America/Ciudad_Juarez",
	"America/Costa_Rica", "America/Coyhaique", "America/Creston", "America/Cuiaba", "America/Curacao",
	"America/Danmarkshavn", "America/Dawson", "America/Dawson_Creek", "America/Denver", "America/Detroit",
	"America/Dominica", "America/Edmonton", "America/Eirunepe", "America/El_Salvador", "America/Fort_Nelson",
	"America/Fortaleza", "America/Glace_Bay", "America/Goose_Bay", "America/Grand_Turk", "America/Grenada",
	"America/Guadeloupe", "America/Guatemala", "America/Guayaquil", "America/Guyana", "America/Halifax",
	"America/Havana", "America/Hermosillo", "America/Indiana/Indianapolis", "America/Indiana/Knox",
	"America/Indiana/Marengo", "America/Indiana/Petersburg", "America/Indiana/Tell_City",
	"America/Indiana/Vevay", "America/Indiana/Vincennes", "America/Indiana/Winamac", "America/Inuvik",
	"America/Iqaluit", "America/Jamaica", "America/Juneau", "America/Kentucky/Louisville",
	"America/Kentucky/Monticello", "America/Kralendijk", "America/La_Paz", "America/Lima", "America/Los_Angeles",
	"America/Lower_Princes", "America/Maceio", "America/Managua", "America/Manaus", "America/Marigot",
	"America/Martinique", "America/Matamoros", "America/Mazatlan", "America/Menominee", "America/Merida",
	"America/Metlakatla", "America/Mexico_City", "America/Miquelon", "America/Moncton", "America/Monterrey",
	"America/Montevideo", "America/Montserrat", "America/Nassau", "America/New_York", "America/Nome",
	"America/Noronha", "America/North_Dakota/Beulah", "America/North_Dakota/Center",
	"America/North_Dakota/New_Salem", "America/Nuuk", "America/Ojinaga", "America/Panama", "America/Paramaribo",
	"America/Phoenix", "America/Port-au-Prince", "America/Port_of_Spain", "America/Porto_Velho",
	"America/Puerto_Rico", "America/Punta_Arenas", "America/Rankin_Inlet", "America/Recife", "America/Regina",
	"America/Resolute", "America/Rio_Branco", "America/Santarem", "America/Santiago", "America/Santo_Domingo",
	"America/Sao_Paulo", "America/Scoresbysund", "America/Sitka", "America/St_Barthelemy", "America/St_Johns",
	"America/St_Kitts", "America/St_Lucia", "America/St_Thomas", "America/St_Vincent", "America/Swift_Current",
	"America/Tegucigalpa", "America/Thule", "America/Tijuana", "America/Toronto", "America/Tortola",
	"America/Vancouver", "America/Whitehorse", "America/Winnipeg", "America/Yakutat",
	"Antarctica/Casey", "Antarctica/Davis", "Antarctica/DumontDUrville", "Antarctica/Macquarie",
	"Antarctica/Mawson", "Antarctica/McMurdo", "Antarctica/Palmer", "Antarctica/Rothera", "Antarctica/Syowa",
	"Antarctica/Troll", "Antarctica/Vostok",
	"Arctic/Longyearbyen",
	"Asia/Aden", "Asia/Almaty", "Asia/Amman", "Asia/Anadyr", "Asia/Aqtau", "Asia/Aqtobe", "Asia/Ashgabat",
	"Asia/Atyrau", "Asia/Baghdad", "Asia/Bahrain", "Asia/Baku", "Asia/Bangkok", "Asia/Barnaul", "Asia/Beirut",
	"Asia/Bishkek", "Asia/Brunei", "Asia/Chita", "Asia/Colombo", "Asia/Damascus", "Asia/Dhaka", "Asia/Dili",
	"Asia/Dubai", "Asia/Dushanbe", "Asia/Famagusta", "Asia/Gaza", "Asia/Hebron", "Asia/Ho_Chi_Minh",
	"Asia/Hong_Kong", "Asia/Hovd", "Asia/Irkutsk", "Asia/Jakarta", "Asia/Jayapura", "Asia/Jerusalem",
	"Asia/Kabul", "Asia/Kamchatka", "Asia/Karachi", "Asia/Kathmandu", "Asia/Khandyga", "Asia/Kolkata",
	"Asia/Krasnoyarsk", "Asia/Kuala_Lumpur", "Asia/Kuching", "Asia/Kuwait", "Asia/Macau", "Asia/Magadan",
	"Asia/Makassar", "Asia/Manila", "Asia/Muscat", "Asia/Nicosia", "Asia/Novokuznetsk", "Asia/Novosibirsk",
	"Asia/Omsk", "Asia/Oral", "Asia/Phnom_Penh", "Asia/Pontianak", "Asia/Pyongyang", "Asia/Qatar",
	"Asia/Qostanay", "Asia/Qyzylorda", "Asia/Riyadh", "Asia/Sakhalin", "Asia/Samarkand", "Asia/Seoul",
	"Asia/Shanghai", "Asia/Singapore", "Asia/Srednekolymsk", "Asia/Taipei", "Asia/Tashkent", "Asia/Tbilisi",
	"Asia/Tehran", "Asia/Thimphu", "Asia/Tokyo", "Asia/Tomsk", "Asia/Ulaanbaatar", "Asia/Urumqi",
	"Asia/Ust-Nera", "Asia/Vientiane", "Asia/Vladivostok", "Asia/Yakutsk", "Asia/Yangon", "Asia/Yekaterinburg",
	"Asia/Yerevan",
	"Atlantic/Azores", "Atlantic/Bermuda", "Atlantic/Canary", "Atlantic/Cape_Verde", "Atlantic/Faroe",
	"Atlantic/Madeira", "Atlantic/Reykjavik", "Atlantic/South_Georgia", "Atlantic/St_Helena", "Atlantic/Stanley",
	"Australia/Adelaide", "Australia/Brisbane", "Australia/Broken_Hill", "Australia/Darwin", "Australia/Eucla",
	"Australia/Hobart", "Australia/Lindeman", "Australia/Lord_Howe", "Australia/Melbourne", "Australia/Perth",
	"Australia/Sydney",
	"Europe/Amsterdam", "Europe/Andorra", "Europe/Astrakhan", "Europe/Athens", "Europe/Belgrade",
	"Europe/Berlin", "Europe/Bratislava", "Europe/Brussels", "Europe/Bucharest", "Europe/Budapest",
	"Europe/Busingen", "Europe/Chisinau", "Europe/Copenhagen", "Europe/Dublin", "Europe/Gibraltar",
	"Europe/Guernsey", "Europe/Helsinki", "Europe/Isle_of_Man", "Europe/Istanbul", "Europe/Jersey",
	"Europe/Kaliningrad", "Europe/Kirov", "Europe/Kyiv", "Europe/Lisbon", "Europe/Ljubljana", "Europe/London",
	"Europe/Luxembourg", "Europe/Madrid", "Europe/Malta", "Europe/Mariehamn", "Europe/Minsk", "Europe/Monaco",
	"Europe/Moscow", "Europe/Oslo", "Europe/Paris", "Europe/Podgorica", "Europe/Prague", "Europe/Riga",
	"Europe/Rome", "Europe/Samara", "Europe/San_Marino", "Europe/Sarajevo", "Europe/Saratov",
	"Europe/Simferopol", "Europe/Skopje", "Europe/Sofia", "Europe/Stockholm", "Europe/Tallinn", "Europe/Tirane",
	"Europe/Ulyanovsk", "Europe/Vaduz", "Europe/Vatican", "Europe/Vienna", "Europe/Vilnius", "Europe/Volgograd",
	"Europe/Warsaw", "Europe/Zagreb", "Europe/Zurich",
	"Indian/Antananarivo", "Indian/Chagos", "Indian/Christmas", "Indian/Cocos", "Indian/Comoro",
	"Indian/Kerguelen", "Indian/Mahe", "Indian/Maldives", "Indian/Mauritius", "Indian/Mayotte", "Indian/Reunion",
	"Pacific/Apia", "Pacific/Auckland", "Pacific/Bougainville", "Pacific/Chatham", "Pacific/Chuuk",
	"Pacific/Easter", "Pacific/Efate", "Pacific/Fakaofo", "Pacific/Fiji", "Pacific/Funafuti",
	"Pacific/Galapagos", "Pacific/Gambier", "Pacific/Guadalcanal", "Pacific/Guam", "Pacific/Honolulu",
	"Pacific/Kanton", "Pacific/Kiritimati", "Pacific/Kosrae", "Pacific/Kwajalein", "Pacific/Majuro",
	"Pacific/Marquesas", "Pacific/Midway", "Pacific/Nauru", "Pacific/Niue", "Pacific/Norfolk", "Pacific/Noumea",
	"Pacific/Pago_Pago", "Pacific/Palau", "Pacific/Pitcairn", "Pacific/Pohnpei", "Pacific/Port_Moresby",
	"Pacific/Rarotonga", "Pacific/Saipan", "Pacific/Tahiti", "Pacific/Tarawa", "Pacific/Tongatapu",
	"Pacific/Wake", "Pacific/Wallis",
}
