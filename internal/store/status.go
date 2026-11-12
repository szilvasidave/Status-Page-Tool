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
	nowL := now.In(loc)
	today := time.Date(nowL.Year(), nowL.Month(), nowL.Day(), 0, 0, 0, 0, loc)
	starts := make([]time.Time, days)
	for i := 0; i < days; i++ {
		starts[i] = today.AddDate(0, 0, i-days+1)
	}
	out := make(map[int64][]DayStatus, len(services))
	index := make(map[int64]int, len(services))
	for _, s := range services {
		row := make([]DayStatus, days)
		for i, st := range starts {
			end := st.AddDate(0, 0, 1)
			row[i] = DayStatus{Date: st, Status: StatusOperational, NoData: !end.After(s.CreatedAt)}
		}
		out[s.ID] = row
		index[s.ID] = 0
	}
	for _, inc := range incidents {
		for i, st := range starts {
			end := st.AddDate(0, 0, 1)
			if !inc.StartedAt.Before(end) {
				continue
			}
			if inc.ResolvedAt != nil && inc.ResolvedAt.Before(st) {
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
			end := st.AddDate(0, 0, 1)
			if !m.StartsAt.Before(end) || !mEnd.After(st) {
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
