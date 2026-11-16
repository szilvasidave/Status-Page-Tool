package monitoring

// Prometheus Alertmanager (webhook_config, payload version 4) and Grafana
// Alerting (contact point "Webhook"), which sends the same alerts with a
// few more fields:
//
//	{"version": "4", "status": "firing", "alerts": [
//	  {"status": "firing", "labels": {"alertname": "…", "severity": "critical", …},
//	   "annotations": {"summary": "…", "description": "…"},
//	   "startsAt": "2026-11-16T10:00:00Z", "endsAt": "0001-01-01T00:00:00Z",
//	   "fingerprint": "c6e9b6e2a5b4d1f0"}, …]}
//
// Each alert of the group is read (with its own status): the fingerprint
// is the key, the labels statuspage_service, service, alertname, job and
// instance name the service (in this order), the label severity (else
// priority) the impact, unless a label statuspage_impact sets it, and the
// annotations summary (else the alert name) and description the text.
//
// When Grafana cannot evaluate a rule (its data source fails, or sends no
// data) and the rule's no data or error handling is the default, it sends
// an alert DatasourceError or DatasourceNoData instead, with the rule's
// labels and annotations and the rule's name as label rulename (Grafana's
// pkg/services/ngalert/state/compat.go): only the monitoring is broken, not
// necessarily the service, so these alerts are ignored.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// alertmanagerTargets are the labels that name the service, in order.
var alertmanagerTargets = []string{"statuspage_service", "service", "alertname", "job", "instance"}

// grafanaNotEvaluated are the notes about Grafana's alerts for a rule it
// could not evaluate, by alert name: resolved, firing.
var grafanaNotEvaluated = map[string][2]string{
	"DatasourceError": {
		"Grafana can evaluate the rule “%s” again (DatasourceError resolved): ignored.",
		"Grafana could not evaluate the rule “%s” (DatasourceError): ignored.",
	},
	"DatasourceNoData": {
		"Grafana receives data for the rule “%s” again (DatasourceNoData resolved): ignored.",
		"Grafana received no data for the rule “%s” (DatasourceNoData): ignored.",
	},
}

type alertmanagerAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
	Fingerprint string            `json:"fingerprint"`
}

// ParseAlertmanager reads the webhook of Prometheus Alertmanager or of
// Grafana Alerting. Grafana's test notification (an alert TestAlert of the
// instance Grafana) is a test; its alerts DatasourceError and
// DatasourceNoData are ignored.
func ParseAlertmanager(r *Request) (*Batch, error) {
	var p struct {
		Alerts *[]alertmanagerAlert `json:"alerts"`
	}
	if err := json.Unmarshal(r.Body, &p); err != nil {
		return nil, parseErrorf("%s", jsonError(err))
	}
	if p.Alerts == nil {
		return nil, parseErrorf("the request has no “alerts”: Alertmanager and Grafana send a JSON object with a list of alerts – is the integration set to the right monitoring tool?")
	}
	alerts := *p.Alerts
	b := &Batch{}
	if len(alerts) == 1 && alerts[0].Labels["alertname"] == "TestAlert" && alerts[0].Labels["instance"] == "Grafana" {
		b.Test = true
		return b, nil
	}
	if len(alerts) > MaxAlerts {
		b.Notes = append(b.Notes, fmt.Sprintf("Only the first %d of %d alerts were read.", MaxAlerts, len(alerts)))
		alerts = alerts[:MaxAlerts]
	}
	for i, am := range alerts {
		labels, annotations := lowerKeys(am.Labels), lowerKeys(am.Annotations)
		a := Alert{
			Key:      am.Fingerprint,
			Title:    firstOf(annotations["summary"], annotations["title"], labels["alertname"]),
			Message:  firstOf(annotations["description"], annotations["message"]),
			Severity: firstOf(labels["severity"], labels["priority"]),
			StartsAt: parseTime(am.StartsAt),
		}
		switch strings.ToLower(am.Status) {
		case "firing":
			a.Firing = true
		case "resolved":
		default:
			b.Notes = append(b.Notes, fmt.Sprintf("Alert %d: status “%s” is neither firing nor resolved: ignored.", i+1, echo(am.Status)))
			continue
		}
		if notes, ok := grafanaNotEvaluated[labels["alertname"]]; ok && labels["rulename"] != "" {
			note := notes[0]
			if a.Firing {
				note = notes[1]
			}
			b.Notes = append(b.Notes, fmt.Sprintf("Alert %d: "+note, i+1, echo(labels["rulename"])))
			continue
		}
		if a.Key == "" {
			a.Key = labelsKey(labels)
		}
		for _, name := range alertmanagerTargets {
			if v := strings.TrimSpace(labels[name]); v != "" {
				a.Targets = append(a.Targets, v)
			}
		}
		if v := labels["statuspage_impact"]; v != "" {
			if impact, ok := ParseImpact(v); ok {
				a.Impact = impact
			} else {
				b.Notes = append(b.Notes, fmt.Sprintf("Alert %d: unknown statuspage_impact “%s” (use degraded, partial_outage or major_outage): the severity decides.", i+1, echo(v)))
			}
		}
		b.Alerts = append(b.Alerts, a)
	}
	return b, nil
}

// labelsKey identifies an alert without a fingerprint by its labels.
func labelsKey(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, k := range names {
		fmt.Fprintf(&b, "%s=%q,", k, labels[k])
	}
	return "labels:" + HashToken(b.String())[:32]
}

// firstOf returns the first value that is not empty after trimming.
func firstOf(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
