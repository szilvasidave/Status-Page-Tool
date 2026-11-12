package store

import (
	"testing"
	"time"
)

func TestMaintenanceCRUD(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		user := mustUser(t, s, "sub", "u@example.com", "U")
		a := mustService(t, s, "Alpha", "", 0)
		b := mustService(t, s, "Beta", "", 0)
		now := time.Now()

		m := &Maintenance{Title: "Patch day", Description: "Monthly patches", Notify: true, CreatedBy: user.ID,
			StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour),
			Services: []ServiceRef{{ID: b.ID}, {ID: a.ID}, {ID: b.ID}, {ID: 999}}}
		check(t, s.CreateMaintenance(ctx, m))
		if m.ID == 0 || !recent(m.CreatedAt) || !m.UpdatedAt.Equal(m.CreatedAt) || m.NotifiedStart || m.NotifiedEnd {
			t.Fatalf("fields not set: %+v", m)
		}
		got, err := s.GetMaintenance(ctx, m.ID)
		check(t, err)
		want := *m
		want.Services = []ServiceRef{{ID: a.ID, Name: "Alpha"}, {ID: b.ID, Name: "Beta"}}
		equal(t, "maintenance", *got, want)
		equal(t, "state", got.State(now), MaintenanceScheduled)

		_, err = s.GetMaintenance(ctx, 999)
		wantNotFound(t, "unknown", err)
		wantError(t, "empty title", s.CreateMaintenance(ctx, &Maintenance{StartsAt: now, EndsAt: now.Add(time.Hour)}))
		wantError(t, "ends before start", s.CreateMaintenance(ctx, &Maintenance{Title: "x", StartsAt: now, EndsAt: now.Add(-time.Hour)}))
		wantError(t, "zero length", s.CreateMaintenance(ctx, &Maintenance{Title: "x", StartsAt: now, EndsAt: now}))
		wantError(t, "no times", s.CreateMaintenance(ctx, &Maintenance{Title: "x"}))

		// windows in the past / in progress at creation time
		past := &Maintenance{Title: "Past", Notify: true, StartsAt: now.Add(-3 * time.Hour), EndsAt: now.Add(-2 * time.Hour)}
		check(t, s.CreateMaintenance(ctx, past))
		equal(t, "past flags", []bool{past.NotifiedStart, past.NotifiedEnd}, []bool{true, true})
		running := &Maintenance{Title: "Running", Notify: true, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)}
		check(t, s.CreateMaintenance(ctx, running))
		equal(t, "running flags", []bool{running.NotifiedStart, running.NotifiedEnd}, []bool{true, false})
		got, err = s.GetMaintenance(ctx, running.ID)
		check(t, err)
		equal(t, "stored flags", []bool{got.NotifiedStart, got.NotifiedEnd}, []bool{true, false})
		equal(t, "no services", len(got.Services), 0)

		// update: moving the past window into the future resets both flags
		past.StartsAt, past.EndsAt = now.Add(time.Hour), now.Add(3*time.Hour)
		past.Title, past.Description, past.Notify = "Moved", "new text", false
		past.Services = []ServiceRef{{ID: a.ID}}
		check(t, s.UpdateMaintenance(ctx, past))
		got, err = s.GetMaintenance(ctx, past.ID)
		check(t, err)
		equal(t, "title", got.Title, "Moved")
		equal(t, "description", got.Description, "new text")
		equal(t, "notify", got.Notify, false)
		equal(t, "flags reset", []bool{got.NotifiedStart, got.NotifiedEnd}, []bool{false, false})
		equal(t, "services", got.Services, []ServiceRef{{ID: a.ID, Name: "Alpha"}})
		equal(t, "struct flags", []bool{past.NotifiedStart, past.NotifiedEnd}, []bool{false, false})

		// in progress window extended: only notified_end is reset
		mustExec(t, s, `UPDATE maintenances SET notified_end = ? WHERE id = ?`, true, running.ID)
		running.EndsAt = now.Add(5 * time.Hour)
		check(t, s.UpdateMaintenance(ctx, running))
		got, err = s.GetMaintenance(ctx, running.ID)
		check(t, err)
		equal(t, "end reset", []bool{got.NotifiedStart, got.NotifiedEnd}, []bool{true, false})

		// window moved entirely into the past: flags unchanged
		mustExec(t, s, `UPDATE maintenances SET notified_start = ?, notified_end = ? WHERE id = ?`, false, true, running.ID)
		running.StartsAt, running.EndsAt = now.Add(-5*time.Hour), now.Add(-4*time.Hour)
		check(t, s.UpdateMaintenance(ctx, running))
		got, err = s.GetMaintenance(ctx, running.ID)
		check(t, err)
		equal(t, "unchanged", []bool{got.NotifiedStart, got.NotifiedEnd}, []bool{false, true})

		wantNotFound(t, "update unknown", s.UpdateMaintenance(ctx, &Maintenance{ID: 999, Title: "x", StartsAt: now, EndsAt: now.Add(time.Hour)}))
		wantError(t, "update invalid", s.UpdateMaintenance(ctx, &Maintenance{ID: m.ID, Title: "x", StartsAt: now, EndsAt: now}))

		// cancel and delete
		check(t, s.CancelMaintenance(ctx, m.ID))
		got, err = s.GetMaintenance(ctx, m.ID)
		check(t, err)
		equal(t, "cancelled", got.Cancelled, true)
		equal(t, "state", got.State(now), MaintenanceCancelled)
		wantNotFound(t, "cancel unknown", s.CancelMaintenance(ctx, 999))
		check(t, s.DeleteMaintenance(ctx, m.ID))
		_, err = s.GetMaintenance(ctx, m.ID)
		wantNotFound(t, "deleted", err)
		wantNotFound(t, "delete again", s.DeleteMaintenance(ctx, m.ID))
	})
}

func TestListMaintenances(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		svc := mustService(t, s, "Svc", "", 0)
		mk := func(title string, start, end time.Time) *Maintenance {
			t.Helper()
			m := &Maintenance{Title: title, Notify: true, StartsAt: start, EndsAt: end, Services: []ServiceRef{{ID: svc.ID}}}
			check(t, s.CreateMaintenance(ctx, m))
			return m
		}
		titles := func(ms []Maintenance) []string {
			var out []string
			for _, m := range ms {
				out = append(out, m.Title)
				if len(m.Services) != 1 || m.Services[0].Name != "Svc" {
					t.Fatalf("%s: services not filled: %+v", m.Title, m.Services)
				}
			}
			return out
		}

		now := time.Now().Truncate(time.Second)
		mk("ended", now.Add(-3*time.Hour), now.Add(-2*time.Hour))
		mk("ends now", now.Add(-time.Hour), now)
		mk("later", now.Add(5*time.Hour), now.Add(6*time.Hour))
		mk("running", now.Add(-time.Hour), now.Add(time.Hour))
		mk("soon", now.Add(time.Hour), now.Add(2*time.Hour))
		cancelled := mk("cancelled", now.Add(time.Hour), now.Add(2*time.Hour))
		check(t, s.CancelMaintenance(ctx, cancelled.ID))

		current, err := s.ListCurrentMaintenances(ctx, now)
		check(t, err)
		equal(t, "current", titles(current), []string{"running", "soon", "later"})

		from, to := now.Add(-2*time.Hour), now.Add(time.Hour)
		between, err := s.ListMaintenancesBetween(ctx, from, to)
		check(t, err)
		// "ended" ends exactly at from, "soon"/"cancelled" start exactly at to
		equal(t, "between", titles(between), []string{"running", "ends now"})

		between, err = s.ListMaintenancesBetween(ctx, now.Add(-24*time.Hour), now.Add(24*time.Hour))
		check(t, err)
		equal(t, "between incl. cancelled", len(between), 6)
		equal(t, "newest first", between[0].Title, "later")

		none, err := s.ListMaintenancesBetween(ctx, now.Add(100*time.Hour), now.Add(200*time.Hour))
		check(t, err)
		equal(t, "none", len(none), 0)
	})
}

func TestMaintenancesToNotify(t *testing.T) {
	forEachDB(t, func(t *testing.T, s *Store) {
		svc := mustService(t, s, "Svc", "", 0)
		now := time.Now().Truncate(time.Second)
		m := &Maintenance{Title: "Window", Notify: true, StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour),
			Services: []ServiceRef{{ID: svc.ID}}}
		check(t, s.CreateMaintenance(ctx, m))
		silent := &Maintenance{Title: "Silent", Notify: false, StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour)}
		check(t, s.CreateMaintenance(ctx, silent))
		cancelled := &Maintenance{Title: "Cancelled", Notify: true, StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour)}
		check(t, s.CreateMaintenance(ctx, cancelled))
		check(t, s.CancelMaintenance(ctx, cancelled.ID))

		due := func(at time.Time) []int64 {
			t.Helper()
			ms, err := s.MaintenancesToNotify(ctx, at)
			check(t, err)
			var ids []int64
			for _, x := range ms {
				ids = append(ids, x.ID)
				equal(t, "services", len(x.Services), len(m.Services))
			}
			return ids
		}

		equal(t, "nothing yet", len(due(now)), 0)
		equal(t, "started (at start time)", due(m.StartsAt), []int64{m.ID})
		check(t, s.MarkMaintenanceNotified(ctx, m.ID, true))
		equal(t, "start notified", len(due(now.Add(90*time.Minute))), 0)
		equal(t, "ended", due(m.EndsAt), []int64{m.ID})
		check(t, s.MarkMaintenanceNotified(ctx, m.ID, false))
		equal(t, "all notified", len(due(now.Add(10*time.Hour))), 0)
		got, err := s.GetMaintenance(ctx, m.ID)
		check(t, err)
		equal(t, "flags", []bool{got.NotifiedStart, got.NotifiedEnd}, []bool{true, true})

		// end notification pending without start notification (e.g. server was down)
		mustExec(t, s, `UPDATE maintenances SET notified_start = ?, notified_end = ? WHERE id = ?`, false, false, m.ID)
		equal(t, "both pending", due(now.Add(10*time.Hour)), []int64{m.ID})

		wantNotFound(t, "mark unknown", s.MarkMaintenanceNotified(ctx, 999, true))
	})
}
