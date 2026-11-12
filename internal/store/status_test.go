package store

import (
	"testing"
	"time"
	_ "time/tzdata" // time zones independent of the host
)

func TestServiceStatuses(t *testing.T) {
	now := date(2026, 6, 1, 12, 0)
	services := []Service{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}}
	incidents := []Incident{
		{Status: IncidentInvestigating, Services: []ServiceImpact{{ServiceID: 1, Impact: StatusDegraded}, {ServiceID: 99, Impact: StatusMajorOutage}}},
		{Status: IncidentMonitoring, Services: []ServiceImpact{{ServiceID: 1, Impact: StatusPartialOutage}, {ServiceID: 2, Impact: StatusDegraded}}},
		{Status: IncidentResolved, Services: []ServiceImpact{{ServiceID: 3, Impact: StatusMajorOutage}}}, // ignored
	}
	maint := []Maintenance{
		{StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Services: []ServiceRef{{ID: 2}, {ID: 3}}},         // in progress
		{StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour), Services: []ServiceRef{{ID: 4}}},               // scheduled
		{StartsAt: now.Add(-2 * time.Hour), EndsAt: now, Services: []ServiceRef{{ID: 4}}},                             // completed (ends now)
		{StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Cancelled: true, Services: []ServiceRef{{ID: 4}}}, // cancelled
	}
	got := ServiceStatuses(services, incidents, maint, now)
	equal(t, "statuses", got, map[int64]Status{
		1: StatusPartialOutage, // worst of two incidents
		2: StatusDegraded,      // incident beats maintenance
		3: StatusMaintenance,
		4: StatusOperational,
	})
	equal(t, "overall", OverallStatus(got), StatusPartialOutage)

	empty := ServiceStatuses(services, nil, nil, now)
	equal(t, "all operational", OverallStatus(empty), StatusOperational)
	equal(t, "no services", len(ServiceStatuses(nil, incidents, maint, now)), 0)
	equal(t, "overall empty", OverallStatus(nil), StatusOperational)
	equal(t, "overall maintenance", OverallStatus(map[int64]Status{1: StatusOperational, 2: StatusMaintenance}), StatusMaintenance)
	equal(t, "label", OverallLabel(StatusMajorOutage), "Major system outage")
	equal(t, "label", OverallLabel(StatusOperational), "All systems operational")
}

// statuses returns the Status of every day of a history row.
func statuses(row []DayStatus) []Status {
	out := make([]Status, len(row))
	for i, d := range row {
		out[i] = d.Status
	}
	return out
}

func TestDailyHistoryBasics(t *testing.T) {
	now := date(2026, 6, 10, 12, 0)
	created := date(2026, 1, 1, 0, 0)
	services := []Service{{ID: 1, CreatedAt: created}, {ID: 2, CreatedAt: created}}
	ptr := func(t time.Time) *time.Time { return &t }
	incidents := []Incident{
		// yesterday 10:00-12:00
		{StartedAt: date(2026, 6, 9, 10, 0), ResolvedAt: ptr(date(2026, 6, 9, 12, 0)),
			Services: []ServiceImpact{{ServiceID: 1, Impact: StatusPartialOutage}}},
		// two days ago 23:00 until yesterday 00:00 exactly: not yesterday
		{StartedAt: date(2026, 6, 8, 23, 0), ResolvedAt: ptr(date(2026, 6, 9, 0, 0)),
			Services: []ServiceImpact{{ServiceID: 2, Impact: StatusDegraded}}},
		// still open since 3 days ago
		{StartedAt: date(2026, 6, 7, 18, 0), Services: []ServiceImpact{{ServiceID: 1, Impact: StatusDegraded}, {ServiceID: 42, Impact: StatusMajorOutage}}},
	}
	maint := []Maintenance{
		// yesterday evening
		{StartsAt: date(2026, 6, 9, 20, 0), EndsAt: date(2026, 6, 9, 22, 0), Services: []ServiceRef{{ID: 2}}},
		// cancelled: ignored
		{StartsAt: date(2026, 6, 10, 1, 0), EndsAt: date(2026, 6, 10, 2, 0), Cancelled: true, Services: []ServiceRef{{ID: 2}}},
		// starts later today: ignored
		{StartsAt: date(2026, 6, 10, 18, 0), EndsAt: date(2026, 6, 10, 19, 0), Services: []ServiceRef{{ID: 2}}},
		// ends exactly at midnight: not on the next day
		{StartsAt: date(2026, 6, 6, 22, 0), EndsAt: date(2026, 6, 7, 0, 0), Services: []ServiceRef{{ID: 2}, {ID: 77}}},
	}
	h := DailyHistory(services, incidents, maint, 5, now, time.UTC)
	equal(t, "rows", len(h), 2)
	row1, row2 := h[1], h[2]
	equal(t, "days", len(row1), 5)
	for i, d := range row1 {
		want := date(2026, 6, 6+i, 0, 0)
		if !d.Date.Equal(want) || d.NoData {
			t.Fatalf("day %d: %+v, want date %v with data", i, d, want)
		}
	}
	//                    6/6           6/7              6/8              6/9                  6/10 (today)
	equal(t, "service 1", statuses(row1), []Status{StatusOperational, StatusDegraded, StatusDegraded, StatusPartialOutage, StatusDegraded})
	equal(t, "service 2", statuses(row2), []Status{StatusMaintenance, StatusOperational, StatusDegraded, StatusMaintenance, StatusOperational})
	equal(t, "incident count 6/9", row1[3].Incidents, 2)
	equal(t, "incident count 6/6", row1[0].Incidents, 0)
	equal(t, "incident count 6/8 svc 2", row2[2].Incidents, 1)
	equal(t, "incident count 6/9 svc 2", row2[3].Incidents, 0)
}

func TestDailyHistoryNoData(t *testing.T) {
	now := date(2026, 6, 10, 12, 0)
	services := []Service{
		{ID: 1, CreatedAt: date(2026, 6, 9, 15, 0)}, // created yesterday afternoon
		{ID: 2, CreatedAt: date(2026, 6, 10, 0, 0)}, // created at midnight today
	}
	ptr := func(t time.Time) *time.Time { return &t }
	incidents := []Incident{
		// back-dated incident before service 2 existed: that day has data
		{StartedAt: date(2026, 6, 7, 10, 0), ResolvedAt: ptr(date(2026, 6, 7, 11, 0)),
			Services: []ServiceImpact{{ServiceID: 2, Impact: StatusMajorOutage}}},
	}
	h := DailyHistory(services, incidents, nil, 4, now, time.UTC)
	var nd1, nd2 []bool
	for i := range h[1] {
		nd1 = append(nd1, h[1][i].NoData)
		nd2 = append(nd2, h[2][i].NoData)
	}
	equal(t, "service 1 no data", nd1, []bool{true, true, false, false})
	equal(t, "service 2 no data", nd2, []bool{false, true, true, false})
	equal(t, "status on back-dated day", h[2][0].Status, StatusMajorOutage)
}

func TestDailyHistoryTimeZone(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	check(t, err)
	created := date(2026, 1, 1, 0, 0)
	services := []Service{{ID: 1, CreatedAt: created}}
	ptr := func(t time.Time) *time.Time { return &t }
	// 2026-06-09 22:30-23:30 UTC = 2026-06-10 00:30-01:30 in Berlin (CEST, UTC+2)
	incidents := []Incident{{StartedAt: date(2026, 6, 9, 22, 30), ResolvedAt: ptr(date(2026, 6, 9, 23, 30)),
		Services: []ServiceImpact{{ServiceID: 1, Impact: StatusMajorOutage}}}}
	now := date(2026, 6, 10, 12, 0)

	utc := DailyHistory(services, incidents, nil, 2, now, time.UTC)[1]
	equal(t, "utc", statuses(utc), []Status{StatusMajorOutage, StatusOperational})

	local := DailyHistory(services, incidents, nil, 2, now, berlin)[1]
	equal(t, "berlin", statuses(local), []Status{StatusOperational, StatusMajorOutage})
	equal(t, "berlin day start", local[1].Date, time.Date(2026, 6, 10, 0, 0, 0, 0, berlin))
	equal(t, "berlin day start in UTC", local[1].Date.UTC(), date(2026, 6, 9, 22, 0))

	// "today" depends on the time zone: 23:30 UTC is already the next day in Berlin
	late := date(2026, 6, 10, 23, 30)
	row := DailyHistory(services, nil, nil, 1, late, berlin)[1]
	equal(t, "today in berlin", row[0].Date, time.Date(2026, 6, 11, 0, 0, 0, 0, berlin))

	// nil location means UTC
	row = DailyHistory(services, nil, nil, 1, late, nil)[1]
	equal(t, "nil location", row[0].Date, date(2026, 6, 10, 0, 0))
}

func TestDailyHistoryDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	check(t, err)
	services := []Service{{ID: 1, CreatedAt: date(2026, 1, 1, 0, 0)}}
	// DST ends on 2026-11-01 at 02:00 local: that day has 25 hours.
	// 2026-11-01 23:30 EST = 2026-11-02 04:30 UTC
	resolved := time.Date(2026, 11, 1, 23, 45, 0, 0, ny)
	incidents := []Incident{{StartedAt: time.Date(2026, 11, 1, 23, 30, 0, 0, ny), ResolvedAt: &resolved,
		Services: []ServiceImpact{{ServiceID: 1, Impact: StatusDegraded}}}}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, ny)
	row := DailyHistory(services, incidents, nil, 3, now, ny)[1]
	var dates []time.Time
	for _, d := range row {
		dates = append(dates, d.Date)
	}
	equal(t, "local midnights", dates, []time.Time{
		time.Date(2026, 10, 31, 0, 0, 0, 0, ny),
		time.Date(2026, 11, 1, 0, 0, 0, 0, ny),
		time.Date(2026, 11, 2, 0, 0, 0, 0, ny),
	})
	equal(t, "25 hour day", row[2].Date.Sub(row[1].Date), 25*time.Hour)
	equal(t, "incident on the long day", statuses(row), []Status{StatusOperational, StatusDegraded, StatusOperational})
}

func TestDailyHistoryEdgeCases(t *testing.T) {
	now := date(2026, 6, 10, 12, 0)
	services := []Service{{ID: 1, CreatedAt: date(2026, 1, 1, 0, 0)}}
	midnight := date(2026, 6, 9, 0, 0)

	// zero-length incident exactly at midnight counts for that day only
	zero := []Incident{{StartedAt: midnight, ResolvedAt: &midnight, Services: []ServiceImpact{{ServiceID: 1, Impact: StatusDegraded}}}}
	row := DailyHistory(services, zero, nil, 3, now, time.UTC)[1]
	equal(t, "zero length", statuses(row), []Status{StatusOperational, StatusDegraded, StatusOperational})
	equal(t, "zero length count", row[1].Incidents, 1)

	// maintenance in progress: counts up to now
	running := []Maintenance{{StartsAt: date(2026, 6, 9, 23, 0), EndsAt: date(2026, 6, 11, 0, 0), Services: []ServiceRef{{ID: 1}}}}
	row = DailyHistory(services, nil, running, 3, now, time.UTC)[1]
	equal(t, "running maintenance", statuses(row), []Status{StatusOperational, StatusMaintenance, StatusMaintenance})

	// maintenance that starts exactly now counts for today
	startsNow := []Maintenance{{StartsAt: now, EndsAt: now.Add(time.Hour), Services: []ServiceRef{{ID: 1}}}}
	row = DailyHistory(services, nil, startsNow, 1, now, time.UTC)[1]
	equal(t, "starts now", statuses(row), []Status{StatusMaintenance})

	// no days: empty rows, never a panic
	for _, days := range []int{0, -3} {
		h := DailyHistory(services, nil, nil, days, now, time.UTC)
		equal(t, "rows", len(h), 1)
		equal(t, "days", len(h[1]), 0)
	}
	equal(t, "no services", len(DailyHistory(nil, zero, running, 3, now, time.UTC)), 0)
}
