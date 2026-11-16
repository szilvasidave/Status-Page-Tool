package monitoring

// Azure Monitor: an action group with a webhook action that uses the common
// alert schema:
//
//	{"schemaId": "azureMonitorCommonAlertSchema", "data": {
//	  "essentials": {"alertId": "…", "alertRule": "…", "severity": "Sev1", "signalType": "Metric",
//	                 "monitorCondition": "Fired", "monitoringService": "Platform",
//	                 "alertTargetIDs": ["/subscriptions/…/virtualmachines/vm1"], "configurationItems": ["vm1"],
//	                 "firedDateTime": "…", "description": "…"},
//	  "alertContext": {…}, "customProperties": {"statuspage_service": "VPN"}}}
//
// Metric and log search alerts are stateful: Azure sends the alert when it
// fires (monitor condition Fired) and when it is resolved, with the same
// alert id, the key.
//
// Activity log alerts are stateless: every event is an alert of its own,
// with an id of its own, and its monitor condition stays Fired. Two kinds
// say in the alert context whether a problem began or ended:
//
//   - Service Health (monitoring service ServiceHealth): an incident of
//     Azure (properties.incidentType Incident) is a problem while its stage
//     is Active, and over once it is Resolved or its root cause analysis
//     (RCA) is published. The tracking id is the key, the impact start time
//     the start. Maintenance, information, action required and security
//     notices are ignored.
//   - Resource Health (monitoring service Resource Health): a resource that
//     is Unavailable is a major outage, Degraded degraded performance, and
//     Available the end of it; the resource is the key, the time of the
//     event the start when the resource was Available before. Unknown says
//     nothing about the resource and is ignored.
//
// Other activity log alerts (administrative operations, policy, autoscale,
// security, …) report single events that never resolve: they are ignored.
//
// The custom properties statuspage_service and service, the rule's name,
// the configuration items and the names of the target resources name the
// service; the severity (Sev0–Sev4; Azure sends activity log alerts as
// Sev4) or a custom property statuspage_impact the impact. The rule's name
// is the title, its description (for Service Health and Resource Health:
// the title of the event) the text.

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

const azureCommonSchema = "azureMonitorCommonAlertSchema"

type azureEssentials struct {
	AlertID            string   `json:"alertId"`
	AlertRule          string   `json:"alertRule"`
	Severity           string   `json:"severity"`
	SignalType         string   `json:"signalType"`
	MonitorCondition   string   `json:"monitorCondition"`
	MonitoringService  string   `json:"monitoringService"`
	AlertTargetIDs     []string `json:"alertTargetIDs"`
	ConfigurationItems []string `json:"configurationItems"`
	FiredDateTime      string   `json:"firedDateTime"`
	Description        string   `json:"description"`
}

type azurePayload struct {
	SchemaID string `json:"schemaId"`
	Data     *struct {
		Essentials       azureEssentials `json:"essentials"`
		AlertContext     json.RawMessage `json:"alertContext"`
		CustomProperties map[string]text `json:"customProperties"`
	} `json:"data"`
}

// Kinds of Azure Monitor alerts (azureKind).
const (
	azureStateful       = iota // metric, log search and other alerts that are resolved
	azureServiceHealth         // activity log: Service Health
	azureResourceHealth        // activity log: Resource Health
	azureActivityLog           // other activity log alerts
)

// azureKind returns the kind of an alert, by its signal type and its
// monitoring service (written in several ways: "Activity Log" and
// "ActivityLog", "Resource Health" and "ResourceHealth"), else by the
// operation of its event.
func azureKind(e *azureEssentials, operation string) int {
	service, op := severityKey(e.MonitoringService), strings.ToLower(operation)
	switch {
	case service == "servicehealth" || strings.HasPrefix(op, "microsoft.servicehealth/"):
		return azureServiceHealth
	case service == "resourcehealth" || strings.HasPrefix(op, "microsoft.resourcehealth/"):
		return azureResourceHealth
	case severityKey(e.SignalType) == "activitylog" || strings.HasPrefix(service, "activitylog"):
		return azureActivityLog
	}
	return azureStateful
}

// azureFields are the fields of a JSON object by their names in lower case:
// the alert context of an activity log alert and its properties, whose
// values are not always of the same type (e.g. level is a number or a
// text), so only the fields that are needed are read, as text.
type azureFields map[string]json.RawMessage

func azureObject(raw json.RawMessage) azureFields {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return lowerKeys(m)
}

// text returns a field as text: "" if it is missing, null, an object or a
// list.
func (f azureFields) text(name string) string {
	var t text
	if json.Unmarshal(f[name], &t) != nil {
		return ""
	}
	return strings.TrimSpace(string(t))
}

// ParseAzureMonitor reads the webhook of an Azure Monitor action group.
func ParseAzureMonitor(r *Request) (*Batch, error) {
	var p azurePayload
	if err := json.Unmarshal(r.Body, &p); err != nil {
		return nil, parseErrorf("%s", jsonError(err))
	}
	if !strings.EqualFold(p.SchemaID, azureCommonSchema) || p.Data == nil {
		return nil, parseErrorf("the alert does not use the common alert schema: select Yes for “Enable the common alert schema” in the webhook action of the action group")
	}
	e := &p.Data.Essentials
	props := lowerKeys(p.Data.CustomProperties)
	a := Alert{
		Key:      e.AlertID,
		Title:    e.AlertRule,
		Message:  e.Description,
		Severity: e.Severity,
		StartsAt: parseTime(e.FiredDateTime),
	}
	b := &Batch{}
	ac := azureObject(p.Data.AlertContext)
	var note string
	switch azureKind(e, ac.text("operationname")) {
	case azureServiceHealth:
		note = azureServiceHealthAlert(&a, ac)
	case azureResourceHealth:
		note = azureResourceHealthAlert(&a, e, ac)
	case azureActivityLog:
		note = fmt.Sprintf("“%s” is an activity log alert (%s), which never resolves: ignored (of the activity log alerts, only Service Health and Resource Health alerts are read).",
			echo(e.AlertRule), echo(e.MonitoringService))
	default:
		switch strings.ToLower(e.MonitorCondition) {
		case "fired":
			a.Firing = true
		case "resolved":
		default:
			note = fmt.Sprintf("Monitor condition “%s” is neither Fired nor Resolved: ignored.", echo(e.MonitorCondition))
		}
		if note == "" && a.Key == "" {
			return nil, parseErrorf("the alert has no alertId")
		}
	}
	if note != "" {
		b.Notes = append(b.Notes, note)
		return b, nil
	}
	a.Targets = []string{string(props["statuspage_service"]), string(props["service"]), e.AlertRule}
	a.Targets = append(a.Targets, e.ConfigurationItems...)
	for _, id := range e.AlertTargetIDs {
		a.Targets = append(a.Targets, path.Base(strings.TrimRight(id, "/")))
	}
	if v := string(props["statuspage_impact"]); v != "" {
		if impact, ok := ParseImpact(v); ok {
			a.Impact = impact
		} else {
			b.Notes = append(b.Notes, "Unknown custom property statuspage_impact “"+echo(v)+"” (use degraded, partial_outage or major_outage): the severity decides.")
		}
	}
	b.Alerts = append(b.Alerts, a)
	return b, nil
}

// azureServiceHealthAlert reads a Service Health alert into a; it returns
// the note why the alert is ignored, or "".
func azureServiceHealthAlert(a *Alert, ac azureFields) string {
	props := azureObject(ac["properties"])
	if kind := props.text("incidenttype"); !strings.EqualFold(kind, "Incident") {
		return fmt.Sprintf("“%s”: a Service Health notice of the type “%s”, not an incident: ignored.", echo(a.Title), echo(kind))
	}
	id := props.text("trackingid")
	if id == "" {
		return fmt.Sprintf("“%s”: a Service Health incident without a tracking id: ignored.", echo(a.Title))
	}
	stage := props.text("stage")
	if stage == "" {
		stage = ac.text("status")
	}
	switch strings.ToLower(stage) {
	case "active":
		a.Firing = true
	case "resolved", "rca": // the root cause analysis follows the resolution
	default:
		return fmt.Sprintf("“%s”: stage “%s” of the Service Health incident %s is neither Active nor Resolved: ignored.", echo(a.Title), echo(stage), echo(id))
	}
	a.Key = "servicehealth:" + id
	a.StartsAt = parseTime(props.text("impactstarttime")) // the same in every notice of the incident
	if t := firstOf(props.text("title"), props.text("defaultlanguagetitle")); t != "" {
		a.Message = t
	}
	return ""
}

// azureResourceHealthAlert reads a Resource Health alert into a; it
// returns the note why the alert is ignored, or "".
func azureResourceHealthAlert(a *Alert, e *azureEssentials, ac azureFields) string {
	props := azureObject(ac["properties"])
	health := props.text("currenthealthstatus")
	switch strings.ToLower(health) {
	case "unavailable":
		a.Firing, a.Severity = true, "critical" // the health implies the severity (Azure sends Sev4)
	case "degraded":
		a.Firing, a.Severity = true, "warning"
	case "available":
	case "":
		// E.g. an Updated event: Azure leaves the health empty when it has
		// not changed. Only the end of the event says something.
		if !strings.EqualFold(ac.text("status"), "Resolved") {
			return fmt.Sprintf("“%s”: a Resource Health event without a health status: nothing to change.", echo(a.Title))
		}
	default:
		return fmt.Sprintf("“%s”: the health of the resource is “%s”: ignored.", echo(a.Title), echo(health))
	}
	resource := firstOf(append(append([]string{}, e.AlertTargetIDs...), e.ConfigurationItems...)...)
	if resource == "" {
		return fmt.Sprintf("“%s”: a Resource Health event without a resource: ignored.", echo(a.Title))
	}
	a.Key = "resourcehealth:" + strings.ToLower(resource)
	a.StartsAt = time.Time{}
	if a.Firing && strings.EqualFold(props.text("previoushealthstatus"), "Available") {
		a.StartsAt = parseTime(ac.text("eventtimestamp")) // the resource became unhealthy
	}
	if t := firstOf(props.text("title"), props.text("details")); t != "" {
		a.Message = t
	}
	return ""
}
