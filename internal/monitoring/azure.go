package monitoring

// Azure Monitor: an action group with a webhook action that uses the common
// alert schema:
//
//	{"schemaId": "azureMonitorCommonAlertSchema", "data": {
//	  "essentials": {"alertId": "…", "alertRule": "…", "severity": "Sev1", "monitorCondition": "Fired",
//	                 "alertTargetIDs": ["/subscriptions/…/virtualmachines/vm1"], "configurationItems": ["vm1"],
//	                 "firedDateTime": "…", "description": "…"},
//	  "alertContext": {…}, "customProperties": {"statuspage_service": "VPN"}}}
//
// The alert id is the key; the custom properties statuspage_service and
// service, the rule's name, the configuration items and the names of the
// target resources name the service; the severity (Sev0–Sev4) or a custom
// property statuspage_impact the impact.

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

const azureCommonSchema = "azureMonitorCommonAlertSchema"

type azurePayload struct {
	SchemaID string `json:"schemaId"`
	Data     *struct {
		Essentials struct {
			AlertID            string   `json:"alertId"`
			AlertRule          string   `json:"alertRule"`
			Severity           string   `json:"severity"`
			MonitorCondition   string   `json:"monitorCondition"`
			AlertTargetIDs     []string `json:"alertTargetIDs"`
			ConfigurationItems []string `json:"configurationItems"`
			FiredDateTime      string   `json:"firedDateTime"`
			Description        string   `json:"description"`
		} `json:"essentials"`
		CustomProperties map[string]text `json:"customProperties"`
	} `json:"data"`
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
	e := p.Data.Essentials
	props := lowerKeys(p.Data.CustomProperties)
	a := Alert{
		Key:      e.AlertID,
		Title:    e.AlertRule,
		Message:  e.Description,
		Severity: e.Severity,
		StartsAt: parseTime(e.FiredDateTime),
	}
	b := &Batch{}
	switch strings.ToLower(e.MonitorCondition) {
	case "fired":
		a.Firing = true
	case "resolved":
	default:
		b.Notes = append(b.Notes, fmt.Sprintf("Monitor condition “%s” is neither Fired nor Resolved: ignored.", e.MonitorCondition))
		return b, nil
	}
	if a.Key == "" {
		return nil, parseErrorf("the alert has no alertId")
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
			b.Notes = append(b.Notes, "Unknown custom property statuspage_impact “"+v+"” (use degraded, partial_outage or major_outage): the severity decides.")
		}
	}
	b.Alerts = append(b.Alerts, a)
	return b, nil
}
