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
// consecutively") are not (server/model/monitor.js, 1.23 and 2.0). So the
// time of an important down heartbeat (UTC, "YYYY-MM-DD HH:mm:ss.SSS") is
// when the problem began, and a later outage is a new occurrence. Uptime
// Kuma sends nothing when a monitor enters or leaves one of its own
// maintenances.

import (
	"encoding/json"
	"strings"
)

type kumaPayload struct {
	Heartbeat *struct {
		MonitorID text   `json:"monitorID"`
		Status    *int   `json:"status"`
		Msg       string `json:"msg"`
		Time      text   `json:"time"`
		Important text   `json:"important"` // true or 1
	} `json:"heartbeat"`
	Monitor *struct {
		ID       text   `json:"id"`
		Name     string `json:"name"`
		URL      string `json:"url"`
		Hostname string `json:"hostname"`
		Tags     []struct {
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
		if important := strings.ToLower(string(p.Heartbeat.Important)); important == "true" || important == "1" {
			a.StartsAt = parseTime(string(p.Heartbeat.Time)) // it went down
		}
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
