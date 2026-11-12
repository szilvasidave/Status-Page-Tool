package e2e

import (
	"context"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakesmtp"
	"github.com/szilvasidave/status-page-tool/internal/testutil/mockidp"
)

const (
	siteName  = "E2E Störungen & Status"
	adminPW   = "correct-horse-battery"
	mailFrom  = "status@e2e.example"
	mailWait  = 15 * time.Second
	berlinTZ  = "Europe/Berlin"
	inputTime = "2006-01-02T15:04" // <input type="datetime-local">
)

func TestJourneySQLite(t *testing.T) {
	runJourney(t, func(f *form) { f.choose("driver", "sqlite") })
}

// TestJourneyPostgres runs the journey against STATUSPAGE_TEST_PG_DSN; the
// public schema of that database is dropped and recreated first.
func TestJourneyPostgres(t *testing.T) {
	dsn := os.Getenv("STATUSPAGE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("STATUSPAGE_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	d, err := db.Open(ctx, config.DriverPostgres, dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.ExecContext(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public")
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	sslmode := u.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = "prefer"
	}
	runJourney(t, func(f *form) {
		f.choose("driver", "postgres").
			set("pg_host", u.Hostname()).set("pg_port", port).
			set("pg_database", strings.TrimPrefix(u.Path, "/")).
			set("pg_user", u.User.Username()).set("pg_password", pw).
			choose("pg_sslmode", sslmode)
	})
}

// journey holds the state shared by the steps.
type journey struct {
	t       *testing.T
	base    string // application URL, also the configured base URL
	idp     *mockidp.Provider
	smtp    *fakesmtp.Server
	admin   *browser // local administrator
	anon    *browser // not signed in
	erp     string   // service ids
	vpn     string
	ops     string // group ids
	staff   string
	inc     string // incident id
	berlin  *time.Location
	dataDir string
}

func runJourney(t *testing.T, fillDatabase func(*form)) {
	logs := &logBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("application log:\n%s", logs)
		}
	})
	berlin, err := time.LoadLocation(berlinTZ)
	if err != nil {
		t.Fatal(err)
	}
	j := &journey{
		t: t, idp: mockidp.New(t), smtp: fakesmtp.Start(t), berlin: berlin, dataDir: t.TempDir(),
		admin: newBrowser(t, "admin"), anon: newBrowser(t, "anonymous"),
	}
	addr := freeAddr(t)
	app := startApp(t, j.dataDir, addr, logs)
	j.base = app.URL

	t.Log("a: setup wizard")
	j.setup(fillDatabase)
	t.Log("b: settings, test e-mail")
	j.settings()
	t.Log("c: services and groups")
	j.servicesAndGroups()
	t.Log("d: single sign-on")
	alice := j.singleSignOn()
	t.Log("e: incident")
	j.incident(alice)
	t.Log("f: resolve the incident")
	j.resolve(alice)
	t.Log("g: maintenance")
	j.maintenance(alice)
	t.Log("h: restart")
	app.stop()
	app = startApp(t, j.dataDir, addr, logs)
	j.afterRestart(alice)
	t.Log("i: break-glass login")
	j.breakGlass()
}

// ------------------------------------------------------------------ a

func (j *journey) setup(fillDatabase func(*form)) {
	t, b := j.t, j.admin
	b.get(j.base + "/").expectRedirect("/setup")
	b.get(j.base + "/admin/settings").expectRedirect("/setup")
	p := b.get(j.base + "/setup").expectStatus(200).expectText("Welcome to Status Page")

	// Test the connection first, then save from the re-rendered form, as a
	// user would.
	f := p.form("/setup")
	fillDatabase(f)
	p = f.press("action", "test").post().expectStatus(200)
	if kind, _ := p.flash(); kind != "success" {
		t.Fatalf("test connection failed:\n%s", truncate(p.text(), 2000))
	}
	p = p.form("/setup").press("action", "save").post()
	p.expectRedirect("/setup")
	if _, err := os.Stat(filepath.Join(j.dataDir, config.FileName)); err != nil {
		t.Fatalf("config.json was not written: %v", err)
	}

	p = p.follow().expectStatus(200).
		expectFlash("success", "The database is ready").
		expectText("Create the administrator")
	b.get(j.base + "/").expectRedirect("/setup") // still pending: no administrator yet
	f = p.form("/setup")
	if got := f.value("base_url"); got != j.base {
		t.Errorf("base URL suggested by the setup = %q, want %q", got, j.base)
	}
	f.set("username", "admin").set("name", "Ada Admin").
		set("password", adminPW).set("password_confirm", adminPW).
		set("site_name", siteName).set("timezone", berlinTZ)
	p = f.post()
	p.expectRedirect("/admin/settings")
	p.follow().expectStatus(200).expectFlash("success", "Setup complete").expectText("Ada Admin", "Settings")

	if _, err := os.Stat(filepath.Join(j.dataDir, config.SetupTokenFile)); !os.IsNotExist(err) {
		t.Errorf("the setup token file still exists after the setup: %v", err)
	}
	b.get(j.base + "/setup").expectRedirect("/login")
	j.anon.get(j.base + "/").expectStatus(200).expectText(siteName)
}

// ------------------------------------------------------------------ b

func (j *journey) settings() {
	t, b := j.t, j.admin
	p := b.get(j.base + "/admin/settings").expectStatus(200)
	f := p.form("/admin/settings")
	f.set("site_name", siteName).set("base_url", j.base).set("timezone", berlinTZ).check("public_access", "")
	f.check("oidc_enabled", "").set("oidc_issuer", j.idp.Issuer()).
		set("oidc_client_id", j.idp.ClientID).set("oidc_client_secret", j.idp.ClientSecret)
	f.set("smtp_host", j.smtp.Host()).set("smtp_port", strconv.Itoa(j.smtp.Port())).choose("smtp_security", "none").
		set("smtp_username", "").set("smtp_password", "").set("smtp_from", "IT Status <"+mailFrom+">")
	p = f.post()
	p.expectRedirect("/admin/settings")
	callback := j.base + "/auth/oidc/callback"
	p = p.follow().expectFlash("success", "Settings saved.").expectText(callback, "A secret is stored")
	f = p.form("/admin/settings")
	if f.value("oidc_client_secret") != "" {
		t.Error("the stored client secret is sent to the browser")
	}
	if f.value("timezone") != berlinTZ || f.value("smtp_security") != "none" || !f.checked("oidc_enabled", "on") {
		t.Errorf("saved settings not shown: timezone %q, security %q", f.value("timezone"), f.value("smtp_security"))
	}
	// From now on the provider only accepts the redirect URI shown above.
	j.idp.RequireRedirectURI(callback)

	p = p.form("/admin/settings/test-email").set("to", "ada@e2e.example").post()
	p.expectRedirect("/admin/settings")
	p.follow().expectFlash("success", "A test e-mail was sent to ada@e2e.example")
	msgs := j.expectMails("["+siteName+"] Test e-mail", "ada@e2e.example")
	if msgs[0].From != mailFrom {
		t.Errorf("envelope sender %q, want %q", msgs[0].From, mailFrom)
	}
	if !strings.Contains(msgs[0].Text(), j.base+"/") {
		t.Errorf("test e-mail does not link to the status page:\n%s", msgs[0].Text())
	}
}

// expectMails waits for one e-mail per recipient with the given subject and
// returns them (in recipient order). The fake server is reset afterwards.
func (j *journey) expectMails(subject string, recipients ...string) []fakesmtp.Message {
	j.t.Helper()
	j.smtp.WaitFor(len(recipients), mailWait)
	time.Sleep(100 * time.Millisecond) // catch unexpected extra e-mails
	msgs := j.smtp.Messages()
	j.smtp.Reset()
	var got []string
	for _, m := range msgs {
		if len(m.To) != 1 {
			j.t.Errorf("e-mail %q has %d envelope recipients, want 1 per e-mail", m.Subject, len(m.To))
			continue
		}
		got = append(got, m.To[0])
		if m.Subject != subject {
			j.t.Errorf("e-mail to %s has subject %q, want %q", m.To[0], m.Subject, subject)
		}
	}
	slices.Sort(got)
	want := slices.Sorted(slices.Values(recipients))
	if !slices.Equal(got, want) {
		j.t.Fatalf("e-mails %q went to %q, want exactly one to each of %q", subject, got, want)
	}
	slices.SortFunc(msgs, func(a, b fakesmtp.Message) int { return strings.Compare(a.To[0], b.To[0]) })
	return msgs
}

// ------------------------------------------------------------------ c

var (
	groupURLRe = regexp.MustCompile(`^/admin/groups/(\d+)$`)
)

// apiStatus is the part of /api/status the test looks at.
type apiStatus struct {
	Status   string `json:"status"`
	Services []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"services"`
	Incidents []struct {
		ID        int64  `json:"id"`
		Title     string `json:"title"`
		Status    string `json:"status"`
		Impact    string `json:"impact"`
		StartedAt string `json:"started_at"`
		URL       string `json:"url"`
	} `json:"incidents"`
	Maintenances []struct {
		ID       int64  `json:"id"`
		Title    string `json:"title"`
		State    string `json:"state"`
		StartsAt string `json:"starts_at"`
		EndsAt   string `json:"ends_at"`
		URL      string `json:"url"`
	} `json:"maintenances"`
}

func (j *journey) api(b *browser) apiStatus {
	j.t.Helper()
	var s apiStatus
	b.get(j.base + "/api/status").expectStatus(200).json(&s)
	return s
}

// serviceStatus returns the status of the named service in s.
func (j *journey) serviceStatus(s apiStatus, name string) string {
	for _, svc := range s.Services {
		if svc.Name == name {
			return svc.Status
		}
	}
	j.t.Fatalf("service %q not in /api/status: %+v", name, s.Services)
	return ""
}

func (j *journey) servicesAndGroups() {
	t, b := j.t, j.admin
	for _, name := range []string{"ERP", "VPN"} {
		p := b.get(j.base + "/admin/services/new").expectStatus(200)
		p = p.form("/admin/services").set("name", name).set("description", name+" system").post()
		p.expectRedirect("/admin/services")
		p.follow().expectFlash("success", "Service “"+name+"” created.")
	}
	for _, svc := range j.api(b).Services {
		switch svc.Name {
		case "ERP":
			j.erp = strconv.FormatInt(svc.ID, 10)
		case "VPN":
			j.vpn = strconv.FormatInt(svc.ID, 10)
		}
	}
	if j.erp == "" || j.vpn == "" {
		t.Fatalf("services not found in /api/status")
	}

	// ops: editors, notified about ERP and VPN (assigned on the group page).
	var p *page
	j.ops, p = j.addGroup("ops")
	p = p.form("/admin/groups/"+j.ops).check("role", "editor").check("services", j.erp).check("services", j.vpn).post()
	p.expectRedirect("/admin/groups")
	p.follow().expectFlash("success", "Group “ops” saved.")

	// staff: no role, notified about ERP (assigned on the service form).
	j.staff, _ = j.addGroup("staff")
	p = b.get(j.base + "/admin/services/" + j.erp).expectStatus(200)
	f := p.form("/admin/services/" + j.erp)
	if !f.checked("groups", j.ops) || f.checked("groups", j.staff) {
		t.Fatalf("ERP form: ops assigned %v, staff assigned %v; want true, false", f.checked("groups", j.ops), f.checked("groups", j.staff))
	}
	p = f.check("groups", j.staff).post()
	p.expectRedirect("/admin/services")
	p.follow().expectFlash("success", "Service “ERP” saved.")

	// Check the assignments from both sides.
	f = b.get(j.base + "/admin/groups/" + j.ops).expectStatus(200).form("/admin/groups/" + j.ops)
	if !f.checked("role", "editor") || !f.checked("services", j.erp) || !f.checked("services", j.vpn) {
		t.Errorf("ops: role/services not saved")
	}
	f = b.get(j.base + "/admin/groups/" + j.staff).expectStatus(200).form("/admin/groups/" + j.staff)
	if !f.checked("role", "viewer") || !f.checked("services", j.erp) || f.checked("services", j.vpn) {
		t.Errorf("staff: role/services not saved")
	}
	f = b.get(j.base + "/admin/services/" + j.vpn).expectStatus(200).form("/admin/services/" + j.vpn)
	if !f.checked("groups", j.ops) || f.checked("groups", j.staff) {
		t.Errorf("VPN: wrong groups")
	}
}

// addGroup adds a group on the Groups page and returns its id and the group
// page.
func (j *journey) addGroup(ext string) (string, *page) {
	j.t.Helper()
	p := j.admin.get(j.base + "/admin/groups").expectStatus(200)
	p = p.form("/admin/groups").set("external_id", ext).post()
	u, _ := url.Parse(p.redirect())
	m := groupURLRe.FindStringSubmatch(u.Path)
	if m == nil {
		j.t.Fatalf("adding group %s redirects to %q", ext, p.redirect())
	}
	return m[1], p.follow().expectFlash("success", "Group “"+ext+"” added.")
}

// setGroupRole changes the role of a group on its page.
func (j *journey) setGroupRole(id, role string) {
	j.t.Helper()
	p := j.admin.get(j.base+"/admin/groups/"+id).expectStatus(200).form("/admin/groups/"+id).check("role", role).post()
	p.expectRedirect("/admin/groups")
	p.follow().expectFlash("success", "saved.")
}

// ------------------------------------------------------------------ d

var ssoLinkRe = regexp.MustCompile(`href="(/auth/oidc/login[^"]*)"`)

// ssoLogin signs b in as u through the login page: it opens path (which
// redirects to /login), clicks the single sign-on button and follows the
// redirects through the identity provider. It returns the final page.
func (j *journey) ssoLogin(b *browser, u mockidp.User, path string) *page {
	j.t.Helper()
	j.idp.SetUser(u)
	p := b.get(j.base + path)
	p.expectRedirect("/login?next=" + url.QueryEscape(path))
	p = p.follow().expectStatus(200)
	m := ssoLinkRe.FindStringSubmatch(p.Body)
	if m == nil {
		j.t.Fatalf("%s: no single sign-on button on the login page:\n%s", b.name, truncate(p.text(), 2000))
	}
	logins := j.idp.Logins()
	p = b.open(j.base + html.UnescapeString(m[1]))
	if j.idp.Logins() != logins+1 {
		j.t.Fatalf("%s: the sign-in did not reach the identity provider's token endpoint (final page %s %d):\n%s",
			b.name, p.URL, p.Status, truncate(p.text(), 2000))
	}
	if p.URL.Path != path {
		j.t.Fatalf("%s: after the sign-in the browser is at %s, want %s", b.name, p.URL, path)
	}
	return p
}

func (j *journey) singleSignOn() *browser {
	t := j.t
	alice := newBrowser(t, "alice")
	aliceUser := mockidp.User{Subject: "sub-alice", Email: "alice@e2e.example", Name: "Alice Editor", OID: "oid-alice", Groups: []string{"ops", "staff"}}
	j.ssoLogin(alice, aliceUser, "/manage").expectStatus(200).expectText("Alice Editor", "Report incident")
	alice.get(j.base + "/admin/settings").expectStatus(403)
	alice.get(j.base+"/account").expectStatus(200).expectText("Editor", "ops", "staff", "ERP", "VPN", "alice@e2e.example")

	bob := newBrowser(t, "bob")
	j.ssoLogin(bob, mockidp.User{Subject: "sub-bob", Email: "bob@e2e.example", Name: "Bob Viewer", Groups: []string{"staff"}}, "/manage").
		expectStatus(403)
	p := bob.get(j.base+"/account").expectStatus(200).expectText("Bob Viewer", "Viewer", "staff", "ERP", "bob@e2e.example")
	p.expectNoText("VPN", "ops")

	carol := newBrowser(t, "carol")
	carolUser := mockidp.User{Subject: "sub-carol", Email: "carol@e2e.example", Name: "Carol Leaver", Groups: []string{"staff"}}
	j.ssoLogin(carol, carolUser, "/account").expectStatus(200).expectText("staff", "ERP")
	j.admin.get(j.base+"/admin/groups/"+j.staff).expectStatus(200).expectText("Carol Leaver", "Bob Viewer", "Alice Editor")

	// Carol left the group at the identity provider: her next sign-in
	// removes the membership.
	carolUser.Groups = []string{}
	carol.get(j.base + "/account").expectStatus(200) // still signed in
	j.idp.SetUser(carolUser)
	p = carol.open(j.base + "/auth/oidc/login?next=%2Faccount")
	if p.URL.Path != "/account" {
		t.Fatalf("carol: second sign-in ends at %s", p.URL)
	}
	p.expectStatus(200).expectText("You are not a member of any group")
	j.admin.get(j.base + "/admin/groups/" + j.staff).expectStatus(200).expectText("Bob Viewer").expectNoText("Carol Leaver")
	j.admin.get(j.base+"/admin/users").expectStatus(200).expectText("Carol Leaver", "Alice Editor", "Bob Viewer")

	// Administrators can sign in with single sign-on, too: a group added in
	// advance gets the Admin role.
	admins, _ := j.addGroup("it-admins")
	j.setGroupRole(admins, "admin")
	erin := newBrowser(t, "erin")
	j.ssoLogin(erin, mockidp.User{Subject: "sub-erin", Email: "erin@e2e.example", Name: "Erin Admin", Groups: []string{"it-admins"}}, "/admin/settings").
		expectStatus(200).expectText("Erin Admin", "Settings")
	erin.get(j.base + "/manage").expectStatus(200)
	p = erin.get(j.base+"/admin/services/"+j.vpn).expectStatus(200).form("/admin/services/"+j.vpn).set("description", "Remote access").post()
	p.expectRedirect("/admin/services")
	p.follow().expectFlash("success", "Service “VPN” saved.").expectText("Remote access")

	// Roles are evaluated on every request.
	j.setGroupRole(j.ops, "viewer")
	alice.get(j.base + "/manage").expectStatus(403)
	j.setGroupRole(j.ops, "editor")
	alice.get(j.base + "/manage").expectStatus(200)
	return alice
}

// ------------------------------------------------------------------ e

var incidentURLRe = regexp.MustCompile(`^/manage/incidents/(\d+)$`)

func (j *journey) incident(alice *browser) {
	t := j.t
	p := alice.get(j.base + "/manage/incidents/new").expectStatus(200)
	f := p.form("/manage/incidents")
	if !f.checked("notify", "on") {
		t.Error(`"Send e-mail notifications" is not ticked by default`)
	}
	// The start time is entered in the configured time zone (Europe/Berlin).
	started := time.Now().In(j.berlin).Truncate(time.Minute).Add(-30 * time.Minute)
	f.set("title", "ERP is down").choose("impact_"+j.erp, "major_outage").set("message", "We are investigating.").
		set("started_at", started.Format(inputTime))
	p = f.post()
	u, _ := url.Parse(p.redirect())
	m := incidentURLRe.FindStringSubmatch(u.Path)
	if m == nil {
		t.Fatalf("reporting the incident: status %d, redirect %q\n%s", p.Status, p.redirect(), truncate(p.text(), 2000))
	}
	j.inc = m[1]
	p.follow().expectStatus(200).expectFlash("success", "The incident has been reported. 2 notification e-mails queued.")

	link := j.base + "/incidents/" + j.inc
	for _, msg := range j.expectMails("["+siteName+"] Investigating: ERP is down", "alice@e2e.example", "bob@e2e.example") {
		text := msg.Text()
		if !strings.Contains(text, link) || !strings.Contains(text, "We are investigating.") || !strings.Contains(text, "ERP: Major outage") {
			t.Errorf("e-mail to %s lacks the link %s, the message or the impact:\n%s", msg.To[0], link, text)
		}
	}

	// Status page, JSON API and anonymous viewers.
	p = j.anon.get(j.base+"/").expectStatus(200).expectText("Major system outage", "ERP is down", "We are investigating.")
	if !regexp.MustCompile(`>ERP</strong>\s*<span class="pill status-major_outage">`).MatchString(p.Body) {
		t.Errorf("the status page does not show ERP as major outage")
	}
	if !regexp.MustCompile(`>VPN</strong>\s*<span class="pill status-operational">`).MatchString(p.Body) {
		t.Errorf("the status page does not show VPN as operational")
	}
	s := j.api(j.anon)
	if s.Status != "major_outage" || j.serviceStatus(s, "ERP") != "major_outage" || j.serviceStatus(s, "VPN") != "operational" {
		t.Errorf("/api/status: %+v", s)
	}
	if len(s.Incidents) != 1 || strconv.FormatInt(s.Incidents[0].ID, 10) != j.inc || s.Incidents[0].URL != link ||
		s.Incidents[0].Status != "investigating" || s.Incidents[0].Impact != "major_outage" ||
		s.Incidents[0].StartedAt != started.UTC().Format(time.RFC3339) {
		t.Errorf("/api/status incidents: %+v, want started_at %s", s.Incidents, started.UTC().Format(time.RFC3339))
	}
	j.anon.get(link).expectStatus(200).expectText("ERP is down", "Investigating")

	// Without public access anonymous viewers have to sign in.
	j.setPublicAccess(false)
	j.anon.get(j.base + "/").expectRedirect("/login?next=%2F")
	j.anon.get(link).expectRedirect("/login?next=" + url.QueryEscape("/incidents/"+j.inc))
	j.anon.get(j.base + "/api/status").expectRedirect("/login?next=%2Fapi%2Fstatus")
	alice.get(j.base + "/").expectStatus(200).expectText("Major system outage")
	j.setPublicAccess(true)
	j.anon.get(j.base + "/").expectStatus(200).expectText("Major system outage")
}

func (j *journey) setPublicAccess(on bool) {
	j.t.Helper()
	f := j.admin.get(j.base + "/admin/settings").expectStatus(200).form("/admin/settings")
	if on {
		f.check("public_access", "")
	} else {
		f.uncheck("public_access", "")
	}
	p := f.post()
	p.expectRedirect("/admin/settings")
	p.follow().expectFlash("success", "Settings saved.")
}

// ------------------------------------------------------------------ f

func (j *journey) resolve(alice *browser) {
	t := j.t
	manage := "/manage/incidents/" + j.inc
	p := alice.get(j.base + manage).expectStatus(200)
	f := p.form(manage + "/updates")
	if f.value("impact_"+j.erp) != "major_outage" || f.value("impact_"+j.vpn) != "" {
		t.Errorf("update form: impacts not prefilled (ERP %q, VPN %q)", f.value("impact_"+j.erp), f.value("impact_"+j.vpn))
	}

	// An update without notifications that also affects VPN: no e-mails.
	p = f.choose("status", "identified").set("message", "A disk is full; VPN is slow, too.").
		choose("impact_"+j.vpn, "degraded").uncheck("notify", "on").post()
	p.expectRedirect(manage)
	p.follow().expectStatus(200).expectFlash("success", "The update has been posted.")
	if k, msg := p.follow().flash(); k != "" {
		t.Errorf("flash shown twice: %s", msg)
	}
	s := j.api(j.anon)
	if s.Status != "major_outage" || j.serviceStatus(s, "VPN") != "degraded" || len(s.Incidents) != 1 || s.Incidents[0].Status != "identified" {
		t.Errorf("/api/status after the update: %+v", s)
	}
	time.Sleep(200 * time.Millisecond)
	if msgs := j.smtp.Messages(); len(msgs) != 0 {
		t.Errorf("e-mails were sent for an update without notifications: %d", len(msgs))
	}

	// Resolve with notifications.
	f = alice.get(j.base + manage).expectStatus(200).expectText("A disk is full").form(manage + "/updates")
	if f.value("status") != "identified" || f.value("impact_"+j.vpn) != "degraded" || !f.checked("notify", "on") {
		t.Errorf("update form not prefilled: status %q, VPN %q", f.value("status"), f.value("impact_"+j.vpn))
	}
	p = f.choose("status", "resolved").set("message", "ERP is back.").post()
	p.expectRedirect(manage)
	p.follow().expectStatus(200).expectFlash("success", "The incident has been resolved. 2 notification e-mails queued.")
	for _, msg := range j.expectMails("["+siteName+"] Resolved: ERP is down", "alice@e2e.example", "bob@e2e.example") {
		if text := msg.Text(); !strings.Contains(text, "ERP is back.") || !strings.Contains(text, j.base+"/incidents/"+j.inc) {
			t.Errorf("resolved e-mail to %s:\n%s", msg.To[0], text)
		}
	}

	j.anon.get(j.base + "/").expectStatus(200).expectText("All systems operational").expectNoText("Active incidents")
	s = j.api(j.anon)
	if s.Status != "operational" || len(s.Incidents) != 0 || j.serviceStatus(s, "ERP") != "operational" || j.serviceStatus(s, "VPN") != "operational" {
		t.Errorf("/api/status after resolving: %+v", s)
	}
	j.anon.get(j.base+"/history").expectStatus(200).expectText("ERP is down", "Resolved")
	j.anon.get(j.base+"/incidents/"+j.inc).expectStatus(200).expectText("ERP is back.", "We are investigating.", "Resolved")
}

// ------------------------------------------------------------------ g

func (j *journey) maintenance(alice *browser) {
	t := j.t
	now := time.Now().In(j.berlin)
	start := time.Date(now.Year(), now.Month(), now.Day()+2, 22, 0, 0, 0, j.berlin)
	end := start.Add(90 * time.Minute)

	p := alice.get(j.base + "/manage/maintenance/new").expectStatus(200)
	f := p.form("/manage/maintenance")
	f.set("title", "VPN upgrade").set("description", "New VPN gateway.").
		set("starts_at", start.Format(inputTime)).set("ends_at", end.Format(inputTime)).check("services", j.vpn)
	p = f.post()
	p.expectRedirect("/manage")
	p.follow().expectStatus(200).expectFlash("success", "The maintenance has been scheduled. 1 notification e-mail queued.").
		expectText("VPN upgrade")

	window := start.Format("Mon 2 Jan 2006 15:04") + " – " + end.Format("15:04 MST")
	msg := j.expectMails("["+siteName+"] Scheduled maintenance: VPN upgrade", "alice@e2e.example")[0]
	if text := msg.Text(); !strings.Contains(text, window) || !strings.Contains(text, "- VPN") {
		t.Errorf("maintenance e-mail does not contain the window %q (Europe/Berlin) and the service:\n%s", window, text)
	}

	s := j.api(j.anon)
	if len(s.Maintenances) != 1 {
		t.Fatalf("/api/status maintenances: %+v", s.Maintenances)
	}
	mt := s.Maintenances[0]
	if mt.Title != "VPN upgrade" || mt.State != "scheduled" ||
		mt.StartsAt != start.UTC().Format(time.RFC3339) || mt.EndsAt != end.UTC().Format(time.RFC3339) {
		t.Errorf("/api/status maintenance %+v, want scheduled %s – %s", mt, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	}
	if j.serviceStatus(s, "VPN") != "operational" {
		t.Errorf("VPN is %s before the maintenance started", j.serviceStatus(s, "VPN"))
	}
	j.anon.get(j.base+"/").expectStatus(200).expectText("All systems operational", "Maintenance", "VPN upgrade", "Scheduled")

	id := strconv.FormatInt(mt.ID, 10)
	edit := "/manage/maintenance/" + id
	p = alice.get(j.base + edit).expectStatus(200)
	f = p.form(edit)
	if got := f.value("starts_at"); got != start.Format(inputTime) {
		t.Errorf("edit form start %q, want %q (Europe/Berlin)", got, start.Format(inputTime))
	}
	// Saving without a change sends nothing; moving the window notifies.
	p = f.post()
	p.expectRedirect("/manage")
	p.follow().expectFlash("success", "The maintenance has been updated.")
	end = end.Add(20 * time.Minute) // 23:50, same day
	p = alice.get(j.base+edit).expectStatus(200).form(edit).set("ends_at", end.Format(inputTime)).post()
	p.expectRedirect("/manage")
	p.follow().expectFlash("success", "The maintenance has been updated. 1 notification e-mail queued.")
	window = start.Format("Mon 2 Jan 2006 15:04") + " – " + end.Format("15:04 MST")
	msg = j.expectMails("["+siteName+"] Maintenance updated: VPN upgrade", "alice@e2e.example")[0]
	if !strings.Contains(msg.Text(), window) {
		t.Errorf("update e-mail does not contain the new window %q:\n%s", window, msg.Text())
	}

	p = alice.get(j.base + edit).expectStatus(200)
	p = p.form(edit + "/cancel").post()
	p.expectRedirect("/manage")
	p.follow().expectFlash("success", "The maintenance has been cancelled. 1 notification e-mail queued.")
	j.expectMails("["+siteName+"] Maintenance cancelled: VPN upgrade", "alice@e2e.example")

	j.anon.get(j.base + "/").expectStatus(200).expectNoText("VPN upgrade")
	if s := j.api(j.anon); len(s.Maintenances) != 0 {
		t.Errorf("cancelled maintenance still listed: %+v", s.Maintenances)
	}
	j.anon.get(j.base+"/maintenance/"+id).expectStatus(200).expectText("VPN upgrade", "Cancelled")

	// A maintenance that is already in progress, ended early with "End now".
	start = time.Now().In(j.berlin).Truncate(time.Minute).Add(-10 * time.Minute)
	end = start.Add(2 * time.Hour)
	f = alice.get(j.base + "/manage/maintenance/new").expectStatus(200).form("/manage/maintenance")
	p = f.set("title", "VPN reboot").set("starts_at", start.Format(inputTime)).set("ends_at", end.Format(inputTime)).
		check("services", j.vpn).post()
	p.expectRedirect("/manage")
	p.follow().expectFlash("success", "The maintenance has been scheduled. 1 notification e-mail queued.")
	j.expectMails("["+siteName+"] Scheduled maintenance: VPN reboot", "alice@e2e.example")
	p = j.anon.get(j.base+"/").expectStatus(200).expectText("Maintenance in progress", "VPN reboot", "In progress")
	if !regexp.MustCompile(`>VPN</strong>\s*<span class="pill status-maintenance">`).MatchString(p.Body) {
		t.Errorf("the status page does not show VPN under maintenance")
	}
	s = j.api(j.anon)
	if s.Status != "maintenance" || j.serviceStatus(s, "VPN") != "maintenance" || len(s.Maintenances) != 1 || s.Maintenances[0].State != "in_progress" {
		t.Errorf("/api/status during the maintenance: %+v", s)
	}
	id = strconv.FormatInt(s.Maintenances[0].ID, 10)
	p = alice.get(j.base + "/manage/maintenance/" + id).expectStatus(200).form("/manage/maintenance/" + id + "/end").post()
	p.expectRedirect("/manage")
	p.follow().expectFlash("success", "The maintenance has been ended.")
	s = j.api(j.anon)
	if s.Status != "operational" || j.serviceStatus(s, "VPN") != "operational" || len(s.Maintenances) != 0 {
		t.Errorf("/api/status after ending the maintenance: %+v", s)
	}
	// The "completed" e-mail is sent by the background worker (checked
	// after the restart, which runs the check at start).
}

// ------------------------------------------------------------------ h

func (j *journey) afterRestart(alice *browser) {
	t := j.t
	j.expectMails("["+siteName+"] Maintenance completed: VPN reboot", "alice@e2e.example")
	j.admin.get(j.base + "/admin/settings").expectStatus(200).expectText("A secret is stored")
	p := j.admin.get(j.base+"/admin/notifications").expectStatus(200).expectText("alice@e2e.example", "bob@e2e.example", "Sent")
	if strings.Contains(p.Body, "notif-pending") || strings.Contains(p.Body, "notif-failed") {
		t.Errorf("the outbox has pending or failed e-mails:\n%s", truncate(p.text(), 3000))
	}
	alice.get(j.base + "/manage").expectStatus(200).expectText("ERP is down")
	j.anon.get(j.base+"/").expectStatus(200).expectText(siteName, "All systems operational", "ERP", "VPN")
	j.anon.get(j.base + "/history").expectStatus(200).expectText("ERP is down")
	if s := j.api(j.anon); len(s.Services) != 2 {
		t.Errorf("services after restart: %+v", s.Services)
	}

	// Single sign-on still works: the client secret is decrypted with the
	// key from config.json.
	dave := newBrowser(t, "dave")
	j.ssoLogin(dave, mockidp.User{Subject: "sub-dave", Email: "dave@e2e.example", Name: "Dave Ops", Groups: []string{"ops"}}, "/manage").
		expectStatus(200)

	// No page reachable by links fails, for every role.
	for _, c := range []struct {
		b     *browser
		start string
	}{{j.admin, "/admin/settings"}, {alice, "/manage"}, {dave, "/account"}, {j.anon, "/"}} {
		if n := c.b.crawl(j.base+c.start, 120); n < 5 {
			t.Errorf("%s: only %d pages crawled", c.b.name, n)
		}
	}

	// E-mail still works.
	p = j.admin.get(j.base+"/admin/settings").form("/admin/settings/test-email").set("to", "ada@e2e.example").post()
	p.expectRedirect("/admin/settings")
	p.follow().expectFlash("success", "A test e-mail was sent")
	j.expectMails("["+siteName+"] Test e-mail", "ada@e2e.example")
}

// ------------------------------------------------------------------ i

func (j *journey) breakGlass() {
	b := newBrowser(j.t, "break-glass")
	p := b.get(j.base + "/login").expectStatus(200)
	if !ssoLinkRe.MatchString(p.Body) {
		j.t.Error("the login page has no single sign-on button")
	}
	p = p.form("/login").set("username", "admin").set("password", "wrong-password").post()
	p.expectStatus(401).expectText("Invalid user name or password.")

	p = b.get(j.base+"/login?local=1&next=%2Fadmin%2Fsettings").expectStatus(200).
		form("/login").set("username", "admin").set("password", adminPW).post()
	p.expectRedirect("/admin/settings")
	p = p.follow().expectStatus(200).expectText("Ada Admin", "Settings")

	// Sign out again.
	p = p.form("/logout").post()
	p.expectRedirect("/")
	b.get(j.base + "/admin/settings").expectRedirect("/login?next=%2Fadmin%2Fsettings")
}
