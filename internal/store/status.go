package store

import "time"

// ServiceStatuses computes the current status of every service from the open
// (unresolved) incidents and the maintenances that are in progress at now.
// Services that are not affected are StatusOperational.
func ServiceStatuses(services []Service, openIncidents []Incident, maintenances []Maintenance, now time.Time) map[int64]Status {
	out := make(map[int64]Status, len(services))
	for _, s := range services {
		out[s.ID] = StatusOperational
	}
	for _, inc := range openIncidents {
		if inc.Resolved() {
			continue
		}
		for _, si := range inc.Services {
			if cur, ok := out[si.ServiceID]; ok {
				out[si.ServiceID] = Worst(cur, si.Impact)
			}
		}
	}
	for i := range maintenances {
		m := &maintenances[i]
		if m.State(now) != MaintenanceInProgress {
			continue
		}
		for _, s := range m.Services {
			if cur, ok := out[s.ID]; ok {
				out[s.ID] = Worst(cur, StatusMaintenance)
			}
		}
	}
	return out
}

// OverallStatus returns the worst status of all services.
func OverallStatus(statuses map[int64]Status) Status {
	worst := StatusOperational
	for _, s := range statuses {
		worst = Worst(worst, s)
	}
	return worst
}

// OverallLabel is the headline shown in the status banner.
func OverallLabel(s Status) string {
	switch s {
	case StatusMaintenance:
		return "Maintenance in progress"
	case StatusDegraded:
		return "Some systems are degraded"
	case StatusPartialOutage:
		return "Partial system outage"
	case StatusMajorOutage:
		return "Major system outage"
	}
	return "All systems operational"
}

// DayStatus is one bar of the per-service history.
type DayStatus struct {
	Date      time.Time // start of the day in the display time zone
	Status    Status    // worst status during the day
	NoData    bool      // the service did not exist yet
	Incidents int       // number of incidents affecting the service that day
}

// DailyHistory builds, for every service, one DayStatus per day for the last
// `days` days (oldest first, the last entry is today) in time zone loc.
// incidents and maintenances should cover at least that period (services must
// be filled in). Cancelled maintenances and maintenances that have not started
// yet are ignored.
func DailyHistory(services []Service, incidents []Incident, maintenances []Maintenance, days int, now time.Time, loc *time.Location) map[int64][]DayStatus {
	if loc == nil {
		loc = time.UTC
	}
	if days < 0 {
		days = 0
	}
	// Day i lasts from bounds[i] until bounds[i+1].
	today := StartOfDay(now.In(loc))
	bounds := make([]time.Time, days+1)
	for i := range bounds {
		bounds[i] = AddDays(today, i-days+1)
	}
	starts := bounds[:days]
	out := make(map[int64][]DayStatus, len(services))
	for _, s := range services {
		row := make([]DayStatus, days)
		for i, st := range starts {
			row[i] = DayStatus{Date: st, Status: StatusOperational, NoData: !bounds[i+1].After(s.CreatedAt)}
		}
		out[s.ID] = row
	}
	for _, inc := range incidents {
		for i, st := range starts {
			if !incidentOnDay(&inc, st, bounds[i+1]) {
				continue
			}
			for _, si := range inc.Services {
				row, ok := out[si.ServiceID]
				if !ok {
					continue
				}
				row[i].Status = Worst(row[i].Status, si.Impact)
				row[i].Incidents++
				row[i].NoData = false
			}
		}
	}
	for mi := range maintenances {
		m := &maintenances[mi]
		if m.Cancelled || m.StartsAt.After(now) {
			continue
		}
		mEnd := m.EndsAt
		if mEnd.After(now) {
			mEnd = now
		}
		for i, st := range starts {
			if !m.StartsAt.Before(bounds[i+1]) || !mEnd.After(st) {
				continue
			}
			for _, s := range m.Services {
				row, ok := out[s.ID]
				if !ok {
					continue
				}
				row[i].Status = Worst(row[i].Status, StatusMaintenance)
				row[i].NoData = false
			}
		}
	}
	return out
}

// StartOfDay returns the first instant of t's day in t's location: midnight,
// or where a daylight saving time change skips midnight (e.g. in
// America/Santiago) the moment the new offset begins.
func StartOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	if start.Day() != d {
		// time.Date put the skipped midnight before the change, on the
		// previous day; the day starts where that zone period ends.
		if _, end := start.ZoneBounds(); !end.IsZero() {
			start = end
		}
	}
	return start
}

// AddDays returns the start (see StartOfDay) of the day n days after t's
// day. Unlike t.AddDate it does not depend on the time of day of t, which
// is not midnight for a day that starts after a skipped midnight.
func AddDays(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	return StartOfDay(time.Date(y, m, d+n, 12, 0, 0, 0, t.Location()))
}

// incidentOnDay reports whether an incident overlaps the day [dayStart,
// dayEnd). The incident lasts from StartedAt until ResolvedAt (exclusive) or
// is still open; an incident resolved the moment it started counts for the
// day it happened on.
func incidentOnDay(inc *Incident, dayStart, dayEnd time.Time) bool {
	if !inc.StartedAt.Before(dayEnd) {
		return false // starts after this day
	}
	r := inc.ResolvedAt
	if r == nil || r.After(dayStart) {
		return true
	}
	// resolved at or before the start of the day: only a zero-length
	// incident that happened exactly at dayStart counts
	return !r.After(inc.StartedAt) && !inc.StartedAt.Before(dayStart)
}
