package store

import (
	"testing"
	"time"
)

func TestCreateAndGetIncident(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		author := mustUser(t, s, "sub", "jane@example.com", "Jane")
		a := mustService(t, s, "Alpha", "B", 0)
		b := mustService(t, s, "Beta", "A", 0)

		inc := &Incident{Title: "Mail down", Status: IncidentInvestigating, CreatedBy: author.ID, Services: []ServiceImpact{
			{ServiceID: a.ID, Impact: StatusMajorOutage},
			{ServiceID: b.ID, Impact: StatusDegraded},
			{ServiceID: a.ID, Impact: StatusDegraded}, // duplicate: first wins
			{ServiceID: 999, Impact: StatusDegraded},  // unknown: ignored
		}}
		first := &IncidentUpdate{Message: "We are looking into it", CreatedBy: author.ID}
		check(t, s.CreateIncident(ctx, inc, first))
		if inc.ID == 0 || !recent(inc.StartedAt) || !recent(inc.CreatedAt) || !inc.UpdatedAt.Equal(inc.CreatedAt) || inc.ResolvedAt != nil {
			t.Fatalf("incident fields not set: %+v", inc)
		}
		if first.ID == 0 || first.IncidentID != inc.ID || !first.CreatedAt.Equal(inc.CreatedAt) || first.Status != IncidentInvestigating {
			t.Fatalf("update fields not set: %+v", first)
		}

		got, err := s.GetIncident(ctx, inc.ID)
		check(t, err)
		equal(t, "title", got.Title, "Mail down")
		equal(t, "status", got.Status, IncidentInvestigating)
		equal(t, "started", got.StartedAt, inc.StartedAt)
		equal(t, "created by", got.CreatedBy, author.ID)
		equal(t, "services (status page order)", got.Services, []ServiceImpact{
			{ServiceID: b.ID, ServiceName: "Beta", Impact: StatusDegraded},
			{ServiceID: a.ID, ServiceName: "Alpha", Impact: StatusMajorOutage},
		})
		wantFirst := *first
		wantFirst.AuthorName = "Jane"
		equal(t, "updates", got.Updates, []IncidentUpdate{wantFirst})
		equal(t, "impact", got.Impact(), StatusMajorOutage)

		_, err = s.GetIncident(ctx, 999)
		wantNotFound(t, "unknown", err)

		// explicit start time, created resolved with and without resolved time
		start := date(2026, 3, 1, 8, 30).Add(500 * time.Millisecond)
		res := &Incident{Title: "Past", Status: IncidentResolved, StartedAt: start}
		check(t, s.CreateIncident(ctx, res, &IncidentUpdate{Message: "Was broken"}))
		equal(t, "started truncated", res.StartedAt, date(2026, 3, 1, 8, 30))
		if res.ResolvedAt == nil || !recent(*res.ResolvedAt) {
			t.Fatalf("resolved_at not set: %+v", res)
		}
		resolvedAt := date(2026, 3, 1, 9, 0)
		res2 := &Incident{Title: "Past 2", Status: IncidentResolved, StartedAt: start, ResolvedAt: &resolvedAt}
		check(t, s.CreateIncident(ctx, res2, nil))
		got, err = s.GetIncident(ctx, res2.ID)
		check(t, err)
		equal(t, "resolved at kept", *got.ResolvedAt, resolvedAt)
		equal(t, "no updates", len(got.Updates), 0)
		equal(t, "no author", got.CreatedBy, int64(0))

		// a resolved time is dropped for unresolved incidents
		open := &Incident{Title: "Open", Status: IncidentMonitoring, ResolvedAt: &resolvedAt}
		check(t, s.CreateIncident(ctx, open, nil))
		got, err = s.GetIncident(ctx, open.ID)
		check(t, err)
		if got.ResolvedAt != nil || open.ResolvedAt != nil {
			t.Fatal("resolved_at set for an open incident")
		}

		wantError(t, "empty title", s.CreateIncident(ctx, &Incident{Title: " ", Status: IncidentInvestigating}, nil))
		wantError(t, "bad status", s.CreateIncident(ctx, &Incident{Title: "x", Status: "broken"}, nil))
		wantError(t, "bad update status", s.CreateIncident(ctx, &Incident{Title: "x", Status: IncidentInvestigating}, &IncidentUpdate{Status: "nope"}))
		wantError(t, "bad impact", s.CreateIncident(ctx, &Incident{Title: "x", Status: IncidentInvestigating,
			Services: []ServiceImpact{{ServiceID: a.ID, Impact: StatusMaintenance}}}, nil))
	})
}

func TestIncidentUpdates(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		named := mustUser(t, s, "1", "named@example.com", "Named")
		mailOnly := mustUser(t, s, "2", "mail@example.com", "")
		admin, err := s.CreateLocalAdmin(ctx, "root", "", "h")
		check(t, err)
		gone := mustUser(t, s, "3", "gone@example.com", "Gone")
		a := mustService(t, s, "A", "", 0)
		b := mustService(t, s, "B", "", 0)

		inc := &Incident{Title: "Slow", Status: IncidentInvestigating, CreatedBy: gone.ID,
			Services: []ServiceImpact{{ServiceID: a.ID, Impact: StatusDegraded}}}
		check(t, s.CreateIncident(ctx, inc, &IncidentUpdate{Message: "first", CreatedBy: named.ID}))

		// impacts nil: services unchanged
		u2 := &IncidentUpdate{Status: IncidentIdentified, Message: "second", CreatedBy: mailOnly.ID}
		check(t, s.AddIncidentUpdate(ctx, inc.ID, u2, nil))
		if u2.ID == 0 || u2.IncidentID != inc.ID || !recent(u2.CreatedAt) {
			t.Fatalf("update fields not set: %+v", u2)
		}
		got, err := s.GetIncident(ctx, inc.ID)
		check(t, err)
		equal(t, "status", got.Status, IncidentIdentified)
		equal(t, "services unchanged", got.ServiceIDs(), []int64{a.ID})

		// impacts given: replaced
		check(t, s.AddIncidentUpdate(ctx, inc.ID, &IncidentUpdate{Status: IncidentMonitoring, Message: "third", CreatedBy: admin.ID},
			[]ServiceImpact{{ServiceID: b.ID, Impact: StatusPartialOutage}}))
		got, err = s.GetIncident(ctx, inc.ID)
		check(t, err)
		equal(t, "services replaced", got.Services, []ServiceImpact{{ServiceID: b.ID, ServiceName: "B", Impact: StatusPartialOutage}})

		// resolve, resolve again (time kept), reopen
		check(t, s.AddIncidentUpdate(ctx, inc.ID, &IncidentUpdate{Status: IncidentResolved, Message: "fixed", CreatedBy: gone.ID}, nil))
		got, err = s.GetIncident(ctx, inc.ID)
		check(t, err)
		if got.ResolvedAt == nil || !recent(*got.ResolvedAt) || !got.Resolved() {
			t.Fatalf("not resolved: %+v", got)
		}
		old := date(2020, 1, 1, 0, 0)
		mustExec(t, s, `UPDATE incidents SET resolved_at = ? WHERE id = ?`, old.Unix(), inc.ID)
		check(t, s.AddIncidentUpdate(ctx, inc.ID, &IncidentUpdate{Status: IncidentResolved, Message: "still fixed"}, nil))
		got, err = s.GetIncident(ctx, inc.ID)
		check(t, err)
		equal(t, "resolved time kept", *got.ResolvedAt, old)
		check(t, s.AddIncidentUpdate(ctx, inc.ID, &IncidentUpdate{Status: IncidentInvestigating, Message: "back again"}, []ServiceImpact{}))
		got, err = s.GetIncident(ctx, inc.ID)
		check(t, err)
		if got.ResolvedAt != nil || got.Resolved() {
			t.Fatalf("not reopened: %+v", got)
		}
		equal(t, "empty impacts remove all services", len(got.Services), 0)
		if !recent(got.UpdatedAt) {
			t.Fatalf("updated_at not set: %v", got.UpdatedAt)
		}

		// timeline newest first with author names; deleted author -> ""
		mustExec(t, s, `DELETE FROM users WHERE id = ?`, gone.ID)
		got, err = s.GetIncident(ctx, inc.ID)
		check(t, err)
		var msgs, authors []string
		for _, u := range got.Updates {
			msgs = append(msgs, u.Message)
			authors = append(authors, u.AuthorName)
		}
		equal(t, "messages", msgs, []string{"back again", "still fixed", "fixed", "third", "second", "first"})
		equal(t, "authors", authors, []string{"", "", "", "root", "mail@example.com", "Named"})
		equal(t, "deleted author id", got.Updates[2].CreatedBy, int64(0))
		equal(t, "deleted creator id", got.CreatedBy, int64(0))

		wantNotFound(t, "unknown incident", s.AddIncidentUpdate(ctx, 999, &IncidentUpdate{Status: IncidentResolved}, nil))
		wantError(t, "bad status", s.AddIncidentUpdate(ctx, inc.ID, &IncidentUpdate{Status: "x"}, nil))
		wantError(t, "bad impact", s.AddIncidentUpdate(ctx, inc.ID, &IncidentUpdate{Status: IncidentResolved},
			[]ServiceImpact{{ServiceID: a.ID, Impact: "bad"}}))

		// title
		check(t, s.UpdateIncidentTitle(ctx, inc.ID, "Very slow"))
		got, err = s.GetIncident(ctx, inc.ID)
		check(t, err)
		equal(t, "title", got.Title, "Very slow")
		wantNotFound(t, "title of unknown", s.UpdateIncidentTitle(ctx, 999, "x"))
		wantError(t, "empty title", s.UpdateIncidentTitle(ctx, inc.ID, " "))

		// delete
		check(t, s.DeleteIncident(ctx, inc.ID))
		_, err = s.GetIncident(ctx, inc.ID)
		wantNotFound(t, "deleted", err)
		var n int
		check(t, s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM incident_updates`).Scan(&n))
		equal(t, "updates deleted", n, 0)
		wantNotFound(t, "delete again", s.DeleteIncident(ctx, inc.ID))
	})
}

func TestListIncidents(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		svc := mustService(t, s, "Svc", "", 0)
		other := mustService(t, s, "Other", "", 0)
		mk := func(title string, status IncidentStatus, started time.Time, resolved *time.Time) *Incident {
			t.Helper()
			inc := &Incident{Title: title, Status: status, StartedAt: started, ResolvedAt: resolved,
				Services: []ServiceImpact{{ServiceID: svc.ID, Impact: StatusDegraded}, {ServiceID: other.ID, Impact: StatusMajorOutage}}}
			check(t, s.CreateIncident(ctx, inc, &IncidentUpdate{Message: title + " 1"}))
			return inc
		}
		ptr := func(t time.Time) *time.Time { return &t }
		from, to := date(2026, 5, 10, 0, 0), date(2026, 5, 20, 0, 0)

		before := mk("resolved before", IncidentResolved, date(2026, 5, 1, 0, 0), ptr(date(2026, 5, 9, 23, 59)))
		atFrom := mk("resolved at from", IncidentResolved, date(2026, 5, 1, 0, 0), ptr(from))
		inside := mk("inside", IncidentResolved, date(2026, 5, 12, 0, 0), ptr(date(2026, 5, 12, 1, 0)))
		atTo := mk("starts at to", IncidentInvestigating, to, nil)
		openOld := mk("open since long", IncidentMonitoring, date(2026, 1, 1, 0, 0), nil)
		_ = before

		// more updates for "inside": the list shows only the newest
		check(t, s.AddIncidentUpdate(ctx, inside.ID, &IncidentUpdate{Status: IncidentResolved, Message: "inside 2"}, nil))
		check(t, s.AddIncidentUpdate(ctx, inside.ID, &IncidentUpdate{Status: IncidentResolved, Message: "inside 3"}, nil))

		list, err := s.ListIncidentsBetween(ctx, from, to)
		check(t, err)
		var titles []string
		for _, inc := range list {
			titles = append(titles, inc.Title)
			if len(inc.Updates) != 1 {
				t.Fatalf("%s: want exactly the latest update, got %d", inc.Title, len(inc.Updates))
			}
			if len(inc.Services) != 2 || inc.Services[0].ServiceName != "Other" {
				t.Fatalf("%s: services not filled: %+v", inc.Title, inc.Services)
			}
		}
		equal(t, "between (newest first)", titles, []string{"inside", "resolved at from", "open since long"})
		equal(t, "latest update", list[0].Updates[0].Message, "inside 3")
		equal(t, "ids", []int64{list[1].ID, list[2].ID}, []int64{atFrom.ID, openOld.ID})

		open, err := s.ListOpenIncidents(ctx)
		check(t, err)
		titles = nil
		for _, inc := range open {
			titles = append(titles, inc.Title)
			equal(t, "one update", len(inc.Updates), 1)
		}
		equal(t, "open (newest first)", titles, []string{"starts at to", "open since long"})
		equal(t, "open ids", open[0].ID, atTo.ID)
		equal(t, "latest update of open", open[1].Updates[0].Message, "open since long 1")

		// no matches: empty, no error
		list, err = s.ListIncidentsBetween(ctx, date(2000, 1, 1, 0, 0), date(2000, 1, 2, 0, 0))
		check(t, err)
		equal(t, "none", len(list), 0)
	})
}
