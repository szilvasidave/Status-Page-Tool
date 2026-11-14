package e2e

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/store"
	"github.com/szilvasidave/status-page-tool/internal/testutil/fakegraph"
)

const graphSite = "Graph Mail E2E"

// TestGraphMailJourney: e-mail with Microsoft Graph, the default method.
// The administrator sets it up (the settings point to single sign-on, whose
// app registration it uses), sends a test e-mail, sees a readable error,
// and incident notifications reach the members of the service's group
// through the fake Microsoft Graph server; an e-mail Microsoft Graph rejects
// shows its error on the notifications page.
func TestGraphMailJourney(t *testing.T) {
	fg := fakegraph.Start(t) // before the application: stopped after it
	logs := &logBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("application log:\n%s", logs)
		}
	})
	dataDir, addr := t.TempDir(), freeAddr(t)
	erp := graphMailPrepare(t, dataDir, "http://"+addr)
	base := startApp(t, dataDir, addr, logs).URL

	admin := newBrowser(t, "admin")
	p := admin.get(base+"/login?local=1&next=%2Fadmin%2Fsettings").expectStatus(200).
		form("/login").set("username", "admin").set("password", adminPW).post()
	p.expectRedirect("/admin/settings")

	// Microsoft Graph is chosen; a sender without single sign-on is refused
	// with a hint.
	f := p.follow().expectStatus(200).form("/admin/settings")
	if got := f.value("mail_method"); got != "graph" {
		t.Errorf("e-mail method of a new installation = %q, want graph", got)
	}
	p = f.set("graph_sender", fg.Sender).post().expectStatus(422).
		expectText("Single sign-on is not set up yet: enter the issuer, client ID and client secret of your Microsoft Entra ID app registration under Single sign-on.")

	// With the app registration of the Microsoft Entra ID tenant: saved.
	f = p.form("/admin/settings")
	if f.value("graph_sender") != fg.Sender {
		t.Errorf("the entered sender is not kept: %q", f.value("graph_sender"))
	}
	p = f.check("oidc_enabled", "").set("oidc_issuer", fg.Issuer()).
		set("oidc_client_id", fg.ClientID).set("oidc_client_secret", fg.ClientSecret).post()
	p.expectRedirect("/admin/settings")
	p = p.follow().expectFlash("success", "Settings saved.")

	// Test e-mail.
	p = p.form("/admin/settings/test-email").set("to", "ada@contoso.example").post()
	p.expectRedirect("/admin/settings")
	p = p.follow().expectFlash("success", "A test e-mail was sent to ada@contoso.example")
	msgs := fg.WaitFor(1, mailWait)
	if m := msgs[0]; m.Sender != fg.Sender || m.To != "ada@contoso.example" || m.Subject != "["+graphSite+"] Test e-mail" ||
		m.ContentType != "HTML" || !strings.Contains(m.Content, "Microsoft 365 (Microsoft Graph)") || !strings.Contains(m.Content, base+"/") {
		t.Errorf("test e-mail = %+v", m)
	}
	fg.Reset()

	// Without the Mail.Send permission the error says what to do.
	fg.FailNext(fakegraph.AccessDenied())
	p = p.form("/admin/settings/test-email").set("to", "ada@contoso.example").post()
	p.expectRedirect("/admin/settings")
	p = p.follow().expectFlash("error", "The test e-mail could not be sent: Microsoft Graph returned HTTP 403 (ErrorAccessDenied)")
	if _, msg := p.flash(); !strings.Contains(msg, "application permission Mail.Send with admin consent") {
		t.Errorf("the error does not name the permission: %s", msg)
	}

	// An incident: the members of the group of the service are notified.
	f = admin.get(base + "/manage/incidents/new").expectStatus(200).form("/manage/incidents")
	p = f.set("title", "ERP is down").choose("impact_"+erp, "major_outage").set("message", "We are investigating.").post()
	u, _ := url.Parse(p.redirect())
	m := regexp.MustCompile(`^/manage/incidents/(\d+)$`).FindStringSubmatch(u.Path)
	if m == nil {
		t.Fatalf("reporting the incident: status %d, redirect %q\n%s", p.Status, p.redirect(), truncate(p.text(), 2000))
	}
	manage := u.Path
	p.follow().expectFlash("success", "The incident has been reported. 2 notification e-mails queued.")
	msgs = fg.WaitFor(2, mailWait)
	link := base + "/incidents/" + m[1]
	var rcpts []string
	for _, msg := range msgs {
		rcpts = append(rcpts, msg.To)
		if msg.Sender != fg.Sender || msg.Subject != "["+graphSite+"] Investigating: ERP is down" ||
			!strings.Contains(msg.Content, link) || !strings.Contains(msg.Content, "We are investigating.") {
			t.Errorf("notification to %s: %+v", msg.To, msg)
		}
	}
	if slices.Sort(rcpts); !slices.Equal(rcpts, []string{"alice@contoso.example", "bob@contoso.example"}) {
		t.Errorf("notified %v, want alice and bob", rcpts)
	}

	// Microsoft Graph rejects bob's address: alice still gets the update,
	// and the notifications page shows bob's error.
	fg.FailRecipient("bob@contoso.example", fakegraph.InvalidRecipient("bob@contoso.example"))
	f = admin.get(base + manage).expectStatus(200).form(manage + "/updates")
	p = f.choose("status", "resolved").set("message", "ERP is back.").post()
	p.expectRedirect(manage)
	p.follow().expectFlash("success", "The incident has been resolved. 2 notification e-mails queued.")
	if msg := fg.WaitFor(3, mailWait)[2]; msg.To != "alice@contoso.example" || msg.Subject != "["+graphSite+"] Resolved: ERP is down" {
		t.Errorf("update = %+v, want the resolved e-mail to alice", msg)
	}
	deadline := time.Now().Add(mailWait)
	for {
		p = admin.get(base + "/admin/notifications").expectStatus(200)
		if strings.Contains(p.text(), "ErrorInvalidRecipients") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bob's failed e-mail is not shown:\n%s", truncate(p.text(), 3000))
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.expectText("Microsoft Graph returned HTTP 400 (ErrorInvalidRecipients): At least one recipient is not valid., Recipient 'bob@contoso.example' is not resolved.")
	if n := strings.Count(p.Body, `class="badge notif-sent"`); n != 3 {
		t.Errorf("%d e-mails shown as sent, want 3", n)
	}
	if n := strings.Count(p.Body, `class="badge notif-pending"`); n != 1 {
		t.Errorf("%d e-mails shown as pending, want 1 (bob's, to be retried)", n)
	}
	// The worker reused its access token; each test e-mail got a new one.
	if n := fg.TokenRequests(); n != 3 {
		t.Errorf("%d token requests, want 3 (2 test e-mails, 1 for the notifications)", n)
	}
}

// graphMailPrepare creates a configured data directory: the local
// administrator "admin" (password adminPW), two users who signed in with
// single sign-on before and are members of the group "ops", and the service
// ERP, whose notifications go to "ops"; then it calls more with the store.
// It returns the id of ERP.
func graphMailPrepare(t *testing.T, dataDir, baseURL string, more ...func(*store.Store)) string {
	t.Helper()
	ctx := context.Background()
	cfg := &config.Config{Listen: strings.TrimPrefix(baseURL, "http://"), DBDriver: config.DriverSQLite, DBDSN: "statuspage.db", SecretKey: config.NewSecretKey()}
	if err := cfg.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	d, err := db.OpenConfig(ctx, cfg, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, err := cfg.Key()
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(d, key)
	hash, err := auth.HashPassword(adminPW)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateLocalAdmin(ctx, "admin", "Ada Admin", hash); err != nil {
		t.Fatal(err)
	}
	settings := store.DefaultSettings()
	settings.SiteName, settings.BaseURL = graphSite, baseURL
	if err := st.SaveSettings(ctx, &settings); err != nil {
		t.Fatal(err)
	}
	ops, err := st.CreateGroup(ctx, "ops", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob"} {
		u, err := st.UpsertOIDCUser(ctx, store.OIDCUser{Subject: "sub-" + name, Email: name + "@contoso.example", Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetUserGroups(ctx, u.ID, []store.GroupRef{{ExternalID: "ops"}}); err != nil {
			t.Fatal(err)
		}
	}
	svc := &store.Service{Name: "ERP", GroupIDs: []int64{ops.ID}}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	for _, f := range more {
		f(st)
	}
	return strconv.FormatInt(svc.ID, 10)
}

// TestGroupNamesJourney: group names from Microsoft Entra ID in the running
// application. Turning the lookup on wakes the worker, which names the group
// that a sign-in recorded by its object ID; a group added by its object ID
// shows its name at once; "Look up names now" follows a rename.
func TestGroupNamesJourney(t *testing.T) {
	const opsID, staffID = "3f2b8c1e-5d4a-4e6b-9c7d-1a2b3c4d5e6f", "a1b2c3d4-0f9e-4d8c-b7a6-958473625140"
	fg := fakegraph.Start(t) // before the application: stopped after it
	logs := &logBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("application log:\n%s", logs)
		}
	})
	dataDir, addr := t.TempDir(), freeAddr(t)
	graphMailPrepare(t, dataDir, "http://"+addr, func(st *store.Store) {
		// alice's last sign-in with Microsoft Entra ID: the groups claim
		// holds object IDs.
		u, err := st.UpsertOIDCUser(context.Background(), store.OIDCUser{Subject: "sub-alice"})
		if err == nil {
			err = st.SetUserGroups(context.Background(), u.ID, []store.GroupRef{{ExternalID: "ops"}, {ExternalID: opsID}})
		}
		if err != nil {
			t.Fatal(err)
		}
	})
	fg.SetGroup(opsID, "IT-Operations")
	base := startApp(t, dataDir, addr, logs).URL

	admin := newBrowser(t, "admin")
	p := admin.get(base+"/login?local=1&next=%2Fadmin%2Fgroups").expectStatus(200).
		form("/login").set("username", "admin").set("password", adminPW).post()
	p.expectRedirect("/admin/groups")
	p.follow().expectStatus(200).expectText(opsID).expectNoText("IT-Operations")

	// Turned on with the app registration of single sign-on: the worker is
	// woken and looks up the name within seconds.
	f := admin.get(base + "/admin/settings").expectStatus(200).form("/admin/settings")
	p = f.check("oidc_enabled", "").set("oidc_issuer", fg.Issuer()).set("oidc_client_id", fg.ClientID).
		set("oidc_client_secret", fg.ClientSecret).check("entra_group_names", "").post()
	p.expectRedirect("/admin/settings")
	p.follow().expectFlash("success", "Settings saved.")
	deadline := time.Now().Add(10 * time.Second)
	for {
		p = admin.get(base + "/admin/groups").expectStatus(200)
		if strings.Contains(p.text(), "IT-Operations") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the name of the group was not looked up:\n%s", truncate(p.text(), 3000))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A group added by its object ID gets its name at once.
	fg.SetGroup(staffID, "All Staff")
	p = p.form("/admin/groups").set("external_id", staffID).post()
	p.follow().expectFlash("success", "Group “All Staff” added.")

	// "Look up names now" follows a rename in Microsoft Entra ID.
	fg.SetGroup(opsID, "IT Operations")
	p = admin.get(base + "/admin/groups").expectStatus(200).form("/admin/groups/names").post()
	p.expectRedirect("/admin/groups")
	p.follow().expectFlash("success", "The names of 2 groups were looked up in Microsoft Entra ID.").expectText("IT Operations")
	admin.get(base + "/admin/users").expectStatus(200).expectText("IT Operations")
}
