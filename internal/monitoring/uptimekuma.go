package monitoring

// Uptime Kuma: notification type "Webhook" with the request body "Preset –
// application/json":
//
//	{"heartbeat": {"monitorID": 1, "status": 0, "msg": "…", "time": "…", …},
//	 "monitor": {"id": 1, "name": "VPN gateway", "url": "…", "hostname": "…", "tags": [{"name": "…", "value": "…"}], …},
//	 "msg": "[VPN gateway] [🔴 Down] …"}
//
// It notifies when a monitor goes down (status 0) and comes up again
// (status 1). The monitor is the key; its tags statuspage_service and
// service, its name, host name and URL name the service; a tag
// statuspage_impact sets the impact (else a major outage). Without a
// heartbeat (the "Test" button, certificate expiry reminders) nothing
// changes.
//
// The heartbeat of a change of state is "important"; the resends of a
// monitor that stays down ("Resend Notification if Down X times
// consecutively") are not (server/model/monitor.js, 1.23 to 2.1). So the
// time of an important down heartbeat (UTC, "YYYY-MM-DD HH:mm:ss.SSS") is
// when the problem began, and a later outage is a new occurrence. But
// Uptime Kuma 1.23 counts the retries of a monitor from 0 again whenever it
// starts the monitor (when Uptime Kuma starts, and when the monitor is
// saved or resumed): a monitor with retries that is still down goes through
// them again – DOWN → PENDING, not sent – and is then reported down as
// important (PENDING → DOWN), although the problem never ended; if it comes
// up during them (PENDING → UP), nothing is sent at all. Uptime Kuma 2
// keeps the retries, and its heartbeats carry them ("retries"). So the
// start is taken from the heartbeats of Uptime Kuma 2, and from those of
// 1.23 only for monitors without retries (the monitor's "maxretries" 0).
//
// Uptime Kuma sends nothing when a monitor enters one of its maintenances,
// and nothing when the monitor is up when it ends (MAINTENANCE → UP) or
// comes up during its retries after it (PENDING → UP; the maintenance
// resets the retries). A monitor that is still down after it is reported
// down again, as important (MAINTENANCE → DOWN, or PENDING → DOWN after its
// retries): a new start, as for Uptime Kuma itself.

import (
	"encoding/json"
	"strings"
	"time"
)

type kumaPayload struct {
	Heartbeat *struct {
		MonitorID text   `json:"monitorID"`
		Status    *int   `json:"status"`
		Msg       string `json:"msg"`
		Time      text   `json:"time"`
		Important text   `json:"important"` // true or 1
		Retries   text   `json:"retries"`   // Uptime Kuma 2 only
	} `json:"heartbeat"`
	Monitor *struct {
		ID         text   `json:"id"`
		Name       string `json:"name"`
		URL        string `json:"url"`
		Hostname   string `json:"hostname"`
		MaxRetries text   `json:"maxretries"`
		Tags       []struct {
			Name  string `json:"name"`
			Value text   `json:"value"`
		} `json:"tags"`
	} `json:"monitor"`
	Msg string `json:"msg"`
}

// Uptime Kuma heartbeat states.
const (
	kumaDown        = 0
	kumaUp          = 1
	kumaPending     = 2
	kumaMaintenance = 3
)

// ParseUptimeKuma reads the webhook of Uptime Kuma.
func ParseUptimeKuma(r *Request) (*Batch, error) {
	var p kumaPayload
	if err := json.Unmarshal(r.Body, &p); err != nil {
		return nil, parseErrorf("%s – in Uptime Kuma choose the request body “Preset – application/json”", jsonError(err))
	}
	b := &Batch{}
	if p.Heartbeat == nil || p.Heartbeat.Status == nil {
		if p.Monitor == nil && p.Msg == "" {
			return nil, parseErrorf("this is not a notification of Uptime Kuma: it has neither “heartbeat” nor “msg” – in Uptime Kuma choose the request body “Preset – application/json”")
		}
		// The test button, or a message without a state (e.g. a
		// certificate expiry reminder).
		b.Test = p.Monitor == nil && strings.HasSuffix(strings.TrimSpace(p.Msg), "Testing")
		if !b.Test {
			b.Notes = append(b.Notes, "A message without a monitor state (“"+echo(p.Msg)+"”): nothing to change.")
		}
		return b, nil
	}
	if p.Monitor == nil {
		return nil, parseErrorf("the notification has no “monitor”")
	}
	m := p.Monitor
	a := Alert{Message: p.Heartbeat.Msg}
	switch *p.Heartbeat.Status {
	case kumaDown:
		a.Firing = true
		a.Title = m.Name + " is down"
		a.StartsAt = kumaStart(&p)
	case kumaUp:
		a.Title = m.Name + " is up"
	default:
		state := "pending"
		if *p.Heartbeat.Status == kumaMaintenance {
			state = "in maintenance"
		}
		b.Notes = append(b.Notes, "“"+echo(m.Name)+"” is "+state+": nothing to change.")
		return b, nil
	}
	id := string(m.ID)
	if id == "" {
		id = string(p.Heartbeat.MonitorID)
	}
	if id == "" {
		id = strings.ToLower(m.Name)
	}
	a.Key = "monitor:" + id
	tags := map[string]string{}
	for _, t := range m.Tags {
		if k := strings.ToLower(strings.TrimSpace(t.Name)); tags[k] == "" {
			tags[k] = strings.TrimSpace(string(t.Value))
		}
	}
	a.Targets = []string{tags["statuspage_service"], tags["service"], m.Name, m.Hostname, m.URL}
	if v := tags["statuspage_impact"]; v != "" {
		if impact, ok := ParseImpact(v); ok {
			a.Impact = impact
		} else {
			b.Notes = append(b.Notes, "Unknown tag value statuspage_impact “"+echo(v)+"” (use degraded, partial_outage or major_outage): ignored.")
		}
	}
	b.Alerts = append(b.Alerts, a)
	return b, nil
}

// kumaStart returns when the problem of a down heartbeat began: the time of
// an important heartbeat (the monitor went down; not a resend) of Uptime
// Kuma 2, which carries the retries, or of 1.23 for a monitor without
// retries. Otherwise it returns the zero time (see above).
func kumaStart(p *kumaPayload) time.Time {
	hb := p.Heartbeat
	if important := strings.ToLower(string(hb.Important)); important != "true" && important != "1" {
		return time.Time{} // a resend
	}
	if hb.Retries == "" && strings.TrimSpace(string(p.Monitor.MaxRetries)) != "0" {
		return time.Time{} // Uptime Kuma 1.23, a monitor with retries
	}
	return parseTime(string(hb.Time))
}
