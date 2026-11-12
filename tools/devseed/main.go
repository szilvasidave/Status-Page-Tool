// Command devseed prepares a data directory with a local administrator and
// optional sample data. Development and demo use only; not part of releases.
//
//	go run ./tools/devseed -data /tmp/sp -sample
//	go run ./cmd/statuspage -data /tmp/sp -listen :8081
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/config"
	"github.com/szilvasidave/status-page-tool/internal/db"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

func main() {
	dataDir := flag.String("data", "", "data directory (required)")
	driver := flag.String("db", "sqlite", "sqlite or postgres")
	pgDSN := flag.String("pg-dsn", "", "PostgreSQL URL when -db postgres")
	listen := flag.String("listen", ":8081", "listen address written to config.json")
	baseURL := flag.String("base-url", "", "base URL setting (default http://localhost<listen>)")
	user := flag.String("admin-user", "admin", "local admin username")
	pass := flag.String("admin-pass", "admin-password", "local admin password")
	sample := flag.Bool("sample", false, "insert sample services, groups, users, incidents and maintenance")
	flag.Parse()
	if *dataDir == "" {
		log.Fatal("-data is required")
	}
	ctx := context.Background()

	cfg, err := config.Load(*dataDir)
	if err == config.ErrNotConfigured {
		cfg = &config.Config{Listen: *listen, DBDriver: *driver, SecretKey: config.NewSecretKey()}
		if *driver == config.DriverPostgres {
			cfg.DBDSN = *pgDSN
		}
		if err := cfg.Save(*dataDir); err != nil {
			log.Fatal(err)
		}
	} else if err != nil {
		log.Fatal(err)
	}
	d, err := db.OpenConfig(ctx, cfg, *dataDir)
	must(err)
	defer d.Close()
	must(d.Migrate(ctx))
	key, err := cfg.Key()
	must(err)
	st := store.New(d, key)

	if _, err := st.GetUserByUsername(ctx, *user); err == store.ErrNotFound {
		h, err := auth.HashPassword(*pass)
		must(err)
		_, err = st.CreateLocalAdmin(ctx, *user, "Administrator", h)
		must(err)
		fmt.Printf("created local admin %q with password %q\n", *user, *pass)
	} else {
		must(err)
	}
	os.Remove(fmt.Sprintf("%s/%s", *dataDir, config.SetupTokenFile))

	if !*sample {
		return
	}
	set, err := st.GetSettings(ctx)
	must(err)
	set.SiteName = "ACME IT Status"
	set.Timezone = "Europe/Berlin"
	set.BaseURL = *baseURL
	if set.BaseURL == "" {
		set.BaseURL = "http://localhost" + cfg.Listen
	}
	must(st.SaveSettings(ctx, set))

	existing, err := st.ListServices(ctx)
	must(err)
	if len(existing) > 0 {
		fmt.Println("sample data already present")
		return
	}
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	day := 24 * time.Hour

	gOps, err := st.CreateGroup(ctx, "it-operations", "IT Operations")
	must(err)
	must(st.UpdateGroup(ctx, gOps.ID, "IT Operations", store.RoleEditor))
	gERP, err := st.CreateGroup(ctx, "erp-users", "ERP Users")
	must(err)
	gAll, err := st.CreateGroup(ctx, "all-staff", "All Staff")
	must(err)

	svc := func(name, desc, cat string, order int, groups ...int64) int64 {
		s := &store.Service{Name: name, Description: desc, Category: cat, SortOrder: order, GroupIDs: groups}
		must(st.CreateService(ctx, s))
		// pretend services exist for a while so history bars show data
		_, err := d.ExecContext(ctx, `UPDATE services SET created_at = ? WHERE id = ?`, ago(120*day).Unix(), s.ID)
		must(err)
		return s.ID
	}
	erp := svc("ERP (SAP)", "Order processing, finance and logistics", "Business applications", 1, gERP.ID, gOps.ID)
	crm := svc("CRM", "Customer relationship management", "Business applications", 2, gOps.ID)
	mail := svc("E-Mail & Calendar", "Exchange Online / Outlook", "Collaboration", 1, gAll.ID, gOps.ID)
	teams := svc("Teams & Telephony", "Chat, meetings and phone system", "Collaboration", 2, gAll.ID)
	vpn := svc("VPN", "Remote access for mobile workers", "Infrastructure", 1, gAll.ID, gOps.ID)
	files := svc("File shares", "Department and project drives", "Infrastructure", 2, gOps.ID)
	_ = crm

	users := []struct {
		sub, email, name string
		groups           []store.GroupRef
	}{
		{"sub-alice", "alice@example.com", "Alice Admin-Ops", []store.GroupRef{{ExternalID: "it-operations"}, {ExternalID: "all-staff"}}},
		{"sub-bob", "bob@example.com", "Bob Buyer", []store.GroupRef{{ExternalID: "erp-users"}, {ExternalID: "all-staff"}}},
		{"sub-carol", "carol@example.com", "Carol Clerk", []store.GroupRef{{ExternalID: "all-staff"}}},
	}
	var aliceID int64
	for _, u := range users {
		usr, err := st.UpsertOIDCUser(ctx, store.OIDCUser{Subject: u.sub, Email: u.email, Name: u.name})
		must(err)
		must(st.SetUserGroups(ctx, usr.ID, u.groups))
		if aliceID == 0 {
			aliceID = usr.ID
		}
	}

	incident := func(title string, started time.Time, resolvedAfter time.Duration, services []store.ServiceImpact, msgs ...string) {
		status := store.IncidentInvestigating
		inc := &store.Incident{Title: title, Status: status, StartedAt: started, Services: services, CreatedBy: aliceID}
		first := &store.IncidentUpdate{Status: status, Message: msgs[0], CreatedBy: aliceID}
		must(st.CreateIncident(ctx, inc, first))
		_, err := d.ExecContext(ctx, `UPDATE incident_updates SET created_at = ? WHERE id = ?`, started.Unix(), first.ID)
		must(err)
		steps := []store.IncidentStatus{store.IncidentIdentified, store.IncidentMonitoring}
		for i, m := range msgs[1:] {
			st2 := steps[min(i, len(steps)-1)]
			last := i == len(msgs)-2 && resolvedAfter > 0
			if last {
				st2 = store.IncidentResolved
			}
			u := &store.IncidentUpdate{Status: st2, Message: m, CreatedBy: aliceID}
			must(st.AddIncidentUpdate(ctx, inc.ID, u, nil))
			at := started.Add(time.Duration(i+1) * 20 * time.Minute)
			if last {
				at = started.Add(resolvedAfter)
				_, err := d.ExecContext(ctx, `UPDATE incidents SET resolved_at = ?, updated_at = ? WHERE id = ?`, at.Unix(), at.Unix(), inc.ID)
				must(err)
			}
			_, err := d.ExecContext(ctx, `UPDATE incident_updates SET created_at = ? WHERE id = ?`, at.Unix(), u.ID)
			must(err)
		}
		_, err = d.ExecContext(ctx, `UPDATE incidents SET created_at = ? WHERE id = ?`, started.Unix(), inc.ID)
		must(err)
	}

	incident("VPN connections dropping", ago(50*time.Minute), 0,
		[]store.ServiceImpact{{ServiceID: vpn, Impact: store.StatusMajorOutage}, {ServiceID: files, Impact: store.StatusDegraded}},
		"Remote users report that VPN connections drop every few minutes. We are investigating.",
		"The cause has been identified as a failing firewall node. A failover is in progress.")
	incident("ERP slow response times", ago(3*day+5*time.Hour), 3*time.Hour,
		[]store.ServiceImpact{{ServiceID: erp, Impact: store.StatusDegraded}},
		"Users experience slow response times in the ERP system.",
		"A long-running batch job was identified and stopped.",
		"Response times are back to normal. This incident has been resolved.")
	incident("E-mail delivery delayed", ago(12*day+2*time.Hour), 90*time.Minute,
		[]store.ServiceImpact{{ServiceID: mail, Impact: store.StatusPartialOutage}},
		"Outgoing e-mails are delayed by up to 30 minutes.",
		"Our provider fixed a queue issue; all queued mails have been delivered.")
	incident("Telephony outage", ago(40*day), 2*time.Hour,
		[]store.ServiceImpact{{ServiceID: teams, Impact: store.StatusMajorOutage}},
		"Incoming and outgoing calls are not possible.",
		"The SIP trunk is working again.")

	maint := func(title, desc string, start, end time.Time, services ...int64) {
		refs := make([]store.ServiceRef, len(services))
		for i, id := range services {
			refs[i] = store.ServiceRef{ID: id}
		}
		must(st.CreateMaintenance(ctx, &store.Maintenance{Title: title, Description: desc, StartsAt: start, EndsAt: end, Notify: true, Services: refs, CreatedBy: aliceID}))
	}
	maint("ERP kernel upgrade", "The ERP system will be unavailable while the kernel is upgraded.", now.Add(2*day).Truncate(time.Hour), now.Add(2*day+3*time.Hour).Truncate(time.Hour), erp)
	maint("File server patching", "Monthly security updates for the file servers.", ago(6*day), ago(6*day-2*time.Hour), files)
	fmt.Println("sample data created")
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
