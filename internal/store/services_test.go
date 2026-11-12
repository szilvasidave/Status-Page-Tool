package store

import (
	"testing"
	"time"
)

func TestServices(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		services, err := s.ListServices(ctx)
		check(t, err)
		equal(t, "empty", len(services), 0)

		g1 := mustGroup(t, s, "g1", "")
		g2 := mustGroup(t, s, "g2", "")

		mail := &Service{Name: "Mail", Description: "Exchange", Category: "Collaboration", SortOrder: 2, GroupIDs: []int64{g2.ID, g1.ID, g2.ID, 999}}
		check(t, s.CreateService(ctx, mail))
		if mail.ID == 0 || !recent(mail.CreatedAt) {
			t.Fatalf("id/created not set: %+v", mail)
		}
		got, err := s.GetService(ctx, mail.ID)
		check(t, err)
		want := *mail
		want.GroupIDs = []int64{g1.ID, g2.ID}
		equal(t, "service", *got, want)

		wantError(t, "empty name", s.CreateService(ctx, &Service{Name: "  "}))
		_, err = s.GetService(ctx, 999)
		wantNotFound(t, "unknown", err)

		// ordering: category (case-insensitive), sort order, name
		teams := mustService(t, s, "Teams", "collaboration", 1)
		vpn := mustService(t, s, "vpn", "", 5)
		erp := mustService(t, s, "ERP", "", 5)
		wiki := mustService(t, s, "Wiki", "Collaboration", 2)
		services, err = s.ListServices(ctx)
		check(t, err)
		var ids []int64
		for _, svc := range services {
			ids = append(ids, svc.ID)
		}
		equal(t, "order", ids, []int64{erp.ID, vpn.ID, teams.ID, mail.ID, wiki.ID})
		equal(t, "group ids in list", services[3].GroupIDs, []int64{g1.ID, g2.ID})
		equal(t, "no group ids", len(services[0].GroupIDs), 0)

		// update
		upd := *mail
		upd.Name, upd.Description, upd.Category, upd.SortOrder, upd.GroupIDs = "E-Mail", "", "", 9, []int64{g2.ID}
		check(t, s.UpdateService(ctx, &upd))
		got, err = s.GetService(ctx, mail.ID)
		check(t, err)
		equal(t, "updated", *got, upd)
		upd.GroupIDs = nil
		check(t, s.UpdateService(ctx, &upd))
		got, err = s.GetService(ctx, mail.ID)
		check(t, err)
		equal(t, "groups removed", len(got.GroupIDs), 0)
		wantNotFound(t, "update unknown", s.UpdateService(ctx, &Service{ID: 999, Name: "x"}))
		wantError(t, "update empty name", s.UpdateService(ctx, &Service{ID: mail.ID, Name: ""}))

		// delete removes incident and maintenance links
		inc := &Incident{Title: "Down", Status: IncidentInvestigating, Services: []ServiceImpact{
			{ServiceID: vpn.ID, Impact: StatusMajorOutage}, {ServiceID: erp.ID, Impact: StatusDegraded}}}
		check(t, s.CreateIncident(ctx, inc, &IncidentUpdate{Message: "x"}))
		m := &Maintenance{Title: "Patch", StartsAt: time.Now().Add(time.Hour), EndsAt: time.Now().Add(2 * time.Hour),
			Services: []ServiceRef{{ID: vpn.ID}, {ID: erp.ID}}}
		check(t, s.CreateMaintenance(ctx, m))
		check(t, s.UpdateService(ctx, &Service{ID: vpn.ID, Name: "vpn", GroupIDs: []int64{g1.ID}}))

		check(t, s.DeleteService(ctx, vpn.ID))
		_, err = s.GetService(ctx, vpn.ID)
		wantNotFound(t, "deleted", err)
		gi, err := s.GetIncident(ctx, inc.ID)
		check(t, err)
		equal(t, "incident services", gi.Services, []ServiceImpact{{ServiceID: erp.ID, ServiceName: "ERP", Impact: StatusDegraded}})
		gm, err := s.GetMaintenance(ctx, m.ID)
		check(t, err)
		equal(t, "maintenance services", gm.Services, []ServiceRef{{ID: erp.ID, Name: "ERP"}})
		g, err := s.GetGroup(ctx, g1.ID)
		check(t, err)
		equal(t, "group services", len(g.ServiceIDs), 0)
		wantNotFound(t, "delete again", s.DeleteService(ctx, vpn.ID))
	})
}
