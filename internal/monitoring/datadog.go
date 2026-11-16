package monitoring

// Datadog: a webhook (Integrations → Webhooks) that POSTs the generic
// format as JSON (the payload in providers.go):
//
//	{"key": "$AGGREG_KEY", "status": "$ALERT_TRANSITION", "severity": "$ALERT_TYPE",
//	 "service": ["$TAGS[statuspage_service]", "$TAGS[service]", "$HOSTNAME"],
//	 "title": "$ALERT_TITLE", "message": "$TEXT_ONLY_MSG"}
//
// The transition (Triggered, Re-Triggered, Warn, Re-Warn, Recovered, No
// Data, Re-No Data or Renotify) says whether the monitor alerts, the alert
// type (error, warning, success or info) how badly: error is a major
// outage, warning degraded performance. A Renotify does not say which state
// it renotifies (alert, warn or no data), so only its alert type tells:
// one that is neither error nor warning is ignored. The aggregation key is
// the key: one per monitor and group, so that the recovery of one group of
// a multi alert clears that group. Datadog's Test Notifications send the
// monitor's alerts with [TEST] in the title: a test.

import "strings"

// ParseDatadog reads the webhook of Datadog.
func ParseDatadog(r *Request) (*Batch, error) {
	return parseGeneric(r, dialect{read: datadogRead, test: datadogTest})
}

// datadogRead reads the state of an alert from its transition and its
// severity from the alert type (the field severity; without one, as the
// transition implies: Triggered major, Warn degraded).
func datadogRead(g genericAlert) (state, string, string) {
	transition, alertType := g.get("status"), g.get("severity")
	st, severity := parseStatus(transition)
	switch strings.ToLower(alertType) {
	case "error":
		severity = "critical"
	case "warning":
		severity = "warning"
	default:
		if severityKey(transition) != "renotify" {
			break
		}
		if alertType == "" {
			return stateUnknown, "", "status “Renotify” without the alert type: ignored (send \"severity\": \"$ALERT_TYPE\" as in the payload of the instructions)."
		}
		return stateUnknown, "", "status “Renotify” of the alert type “" + echo(alertType) + "”, neither error nor warning (e.g. No Data): ignored."
	}
	return st, severity, ""
}

// datadogTest reports Datadog's test notifications: [TEST] among the tags
// in square brackets that the title starts with, e.g. "[TEST] [Triggered on
// {host:vpn-gw01}] Host is down" or "[P2] [Triggered] [TEST] Test Monitor".
func datadogTest(g genericAlert) bool {
	title := strings.TrimSpace(g.get("title"))
	for strings.HasPrefix(title, "[") {
		end := strings.Index(title, "]")
		if end < 0 {
			return false
		}
		if strings.EqualFold(strings.TrimSpace(title[1:end]), "test") {
			return true
		}
		title = strings.TrimSpace(title[end+1:])
	}
	return false
}
