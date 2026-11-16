package monitoring

// The monitoring tools an integration can be set up for. To support another
// tool, add an entry here:
//
//   - if the tool can send a request body of your choice (most can: a
//     "custom payload", "POST value" or a script), use ParseGeneric and
//     give the body in Example – no new code is needed;
//   - otherwise write a ParseXxx function for the request the tool sends
//     (see alertmanager.go, uptimekuma.go, azure.go) with a test that uses
//     a request the tool really sends.
//
// The ID is stored with each integration: never change or remove one.

import (
	"slices"
	"strings"
)

// Provider is a monitoring tool: how its requests are read, and how it is
// set up to send them. In Steps and Example "{url}" stands for the webhook
// URL.
type Provider struct {
	ID    string
	Name  string
	Parse func(*Request) (*Batch, error)
	// Service says which values of an alert name the service, in the order
	// they are tried (shown on the integration page).
	Service string
	// Steps are the setup instructions (plain text, one step each).
	Steps []string
	// Example is configuration or a request body to copy, and ExampleLabel
	// its heading; empty if there is none.
	Example      string
	ExampleLabel string
}

// Generic is the ID of the generic format ("Other tool or script").
const Generic = "generic"

// providers are all monitoring tools; Providers returns them in the order of
// the select on the integration form.
var providers = []*Provider{
	{
		ID:      "alertmanager",
		Name:    "Prometheus Alertmanager",
		Parse:   ParseAlertmanager,
		Service: "the alert's label statuspage_service, else service, alertname, job or instance",
		Steps: []string{
			"In alertmanager.yml add a receiver with a webhook_config that sends to the webhook URL, and a route that sends it the alerts the status page should show (e.g. those with severity critical or warning), as in the example. Then reload Alertmanager.",
			"An alert affects the service named by its label statuspage_service, else by its label service, alertname, job or instance: add a label statuspage_service to the alert rules, or add the values to the services' names in monitoring tools.",
			"The label severity sets the impact (critical: major outage, error: partial outage, warning or info: degraded performance; without a severity: major outage), a label statuspage_impact (degraded, partial_outage, major_outage) sets it directly. The annotation summary (else the alert's name) is the title of the incident, the annotation description its text.",
			"When the alert is resolved (send_resolved, on by default), the incident is resolved.",
		},
		ExampleLabel: "alertmanager.yml",
		Example: `route:
  routes:
    - receiver: status-page
      matchers:
        - severity =~ "critical|warning"
      continue: true    # the alerts also reach your other receivers

receivers:
  - name: status-page
    webhook_configs:
      - url: '{url}'
        send_resolved: true`,
	},
	{
		ID:      "grafana",
		Name:    "Grafana Alerting",
		Parse:   ParseAlertmanager,
		Service: "the alert's label statuspage_service, else service, the alert rule's name (alertname), job or instance",
		Steps: []string{
			"In Grafana open Alerting → Contact points → Add contact point, choose the integration Webhook and enter the webhook URL. Leave “Disable resolved message” off, so that resolved alerts resolve the incident.",
			"Send the alerts the status page should show to the contact point: choose it in the alert rules, or in a notification policy (e.g. for alerts with a label statuspage_service). Use Test to check the connection: the test is listed below and changes nothing.",
			"An alert affects the service named by its label statuspage_service, else by its label service, the rule's name, job or instance. The label severity sets the impact (critical: major outage, error: partial outage, warning or info: degraded performance; without a severity: major outage), a label statuspage_impact (degraded, partial_outage, major_outage) sets it directly. The summary annotation (else the rule's name) is the title of the incident, the description its text.",
		},
		ExampleLabel: "Labels of an alert rule",
		Example: `statuspage_service = VPN
severity = critical`,
	},
	{
		ID:      "uptimekuma",
		Name:    "Uptime Kuma",
		Parse:   ParseUptimeKuma,
		Service: "the monitor's tag statuspage_service (or service), else its name, host name or URL",
		Steps: []string{
			"In Uptime Kuma open Settings → Notifications → Setup Notification, choose the notification type Webhook, enter the webhook URL as Post URL and keep the request body “Preset – application/json”. Use Test to check the connection: the test is listed below and changes nothing.",
			"Turn the notification on for the monitors the status page should show (or tick “Default enabled” and “Apply on all existing monitors”).",
			"A monitor that goes down opens an incident (major outage) of the service with the monitor's name; when it is up again, the incident is resolved. Name the monitors like the services, add the monitors' names to the services' names in monitoring tools, or give the monitors a tag statuspage_service with the service as value. A tag statuspage_impact (degraded, partial_outage, major_outage) sets the impact.",
		},
	},
	{
		ID:      "azuremonitor",
		Name:    "Azure Monitor",
		Parse:   ParseAzureMonitor,
		Service: "the custom property statuspage_service of the alert rule, else the rule's name, else the names of the affected resources",
		Steps: []string{
			"In the Azure portal open Monitor → Alerts → Action groups, create an action group (or open one) and add an action of the type Webhook with the webhook URL. Select Yes for “Enable the common alert schema”.",
			"Add the action group to the alert rules whose alerts the status page should show. Metric alerts are resolved automatically; for log search alerts turn on “Automatically resolve alerts” in the rule.",
			"An alert affects the service named by the rule's custom property statuspage_service (Details → Advanced options → Custom properties), else by the rule's name, else by the names of the affected resources. The severity sets the impact (Sev0: major outage, Sev1: partial outage, Sev2–Sev4: degraded performance), a custom property statuspage_impact (degraded, partial_outage, major_outage) sets it directly.",
		},
		ExampleLabel: "Custom properties of an alert rule",
		Example:      `statuspage_service = VPN`,
	},
	{
		ID:      "zabbix",
		Name:    "Zabbix",
		Parse:   ParseGeneric,
		Service: "the event's tag statuspage_service, else the host's visible name",
		Steps: []string{
			"In Zabbix create a media type (Alerts → Media types; before Zabbix 6.4: Administration → Media types) of the type Webhook, with the parameters and the script below. In older Zabbix versions the HttpRequest object is called CurlHttpRequest.",
			"Give a user (e.g. a service user) this media with the webhook URL as “Send to”, and create a trigger action (Alerts → Actions → Trigger actions) that sends to this user with the media type, with operations for problems and recovery operations.",
			"A problem affects the service named by the event's tag statuspage_service, else by the host's visible name: add the tag to the triggers or hosts, or add the host names to the services' names in monitoring tools. The severity sets the impact (Disaster and High: major outage, Average: partial outage, Warning and lower: degraded performance). The recovery resolves the incident.",
		},
		ExampleLabel: "Media type: parameters and script",
		Example: `Parameters:
  URL       {ALERT.SENDTO}
  EventID   {EVENT.ID}
  Value     {EVENT.VALUE}
  Name      {EVENT.NAME}
  Severity  {EVENT.SEVERITY}
  Host      {HOST.NAME}
  Service   {EVENT.TAGS.statuspage_service}

Script:
var p = JSON.parse(value);
var req = new HttpRequest();
req.addHeader('Content-Type: application/json');
var resp = req.post(p.URL, JSON.stringify({
  key: p.EventID,
  status: p.Value === '0' ? 'resolved' : 'firing',
  service: [p.Service, p.Host],
  title: p.Name,
  severity: p.Severity
}));
if (req.getStatus() !== 200) {
  throw 'Status page: HTTP ' + req.getStatus() + ' ' + resp;
}
return 'OK';`,
	},
	{
		ID:      "prtg",
		Name:    "PRTG Network Monitor",
		Parse:   ParseGeneric,
		Service: "the device's name, else its group",
		Steps: []string{
			"In PRTG open Setup → Account Settings → Notification Templates → Add Notification Template, turn on “Execute HTTP Action”, enter the webhook URL, choose the HTTP method POST and enter the payload below (before PRTG 20.1.57 use %status instead of %laststatus).",
			"Use the template in notification triggers of the devices or sensors the status page should show: a state trigger for Down (and Warning, if it should show too) that also notifies when the condition clears, so that the incident is resolved.",
			"A sensor affects the service named by its device, else by the device's group: name the devices like the services, or add the device names to the services' names in monitoring tools. Down is a major outage, a partial down a partial outage, Warning and Unusual are degraded performance; Up resolves the incident.",
		},
		ExampleLabel: "Payload",
		Example:      `key=%sensorid&status=%laststatus&service=%device&service=%group&title=%device: %name&message=%message`,
	},
	{
		ID:      "datadog",
		Name:    "Datadog",
		Parse:   ParseGeneric,
		Service: "the tag statuspage_service, else the tag service, else the host name",
		Steps: []string{
			"In Datadog open Integrations → Webhooks → New, enter a name (e.g. statuspage), the webhook URL and the payload below.",
			"Mention the webhook (e.g. @webhook-statuspage) in the message of the monitors whose alerts the status page should show.",
			"An alert affects the service named by the tag statuspage_service, else by the tag service, else by the host name. A triggered alert is a major outage, a warning degraded performance; the recovery resolves the incident.",
		},
		ExampleLabel: "Payload",
		Example: `{
  "key": "$AGGREG_KEY",
  "status": "$ALERT_TRANSITION",
  "service": ["$TAGS[statuspage_service]", "$TAGS[service]", "$HOSTNAME"],
  "title": "$ALERT_TITLE",
  "message": "$TEXT_ONLY_MSG"
}`,
	},
	{
		ID:      "uptimerobot",
		Name:    "UptimeRobot",
		Parse:   ParseGeneric,
		Service: "the monitor's friendly name",
		Steps: []string{
			"In UptimeRobot add an alert contact (Integrations) of the type Webhook: enter the webhook URL as “URL to notify”, tick “Send as JSON (application/json)” and enter the POST value below.",
			"Turn the alert contact on for the monitors the status page should show.",
			"A monitor that goes down opens an incident (major outage) of the service with the monitor's friendly name; when it is up again, the incident is resolved. Name the monitors like the services, or add their names to the services' names in monitoring tools.",
		},
		ExampleLabel: "POST value (JSON)",
		Example: `{
  "key": "*monitorID*",
  "status": "*alertTypeFriendlyName*",
  "service": "*monitorFriendlyName*",
  "title": "*monitorFriendlyName*",
  "message": "*alertDetails*"
}`,
	},
	{
		ID:      "nagios",
		Name:    "Nagios, Icinga or Checkmk",
		Parse:   ParseGeneric,
		Service: "the service description, else the host name",
		Steps: []string{
			"Add notification commands that send the state with curl, as in the example for Nagios and Naemon; with Icinga 2 or Checkmk send the same fields with their runtime macros or environment variables (e.g. a Checkmk notification script with $NOTIFY_HOSTNAME, $NOTIFY_SERVICEDESC and $NOTIFY_SERVICESTATE).",
			"Add a contact that uses these commands for host and service notifications of problems and recoveries, and assign it to the hosts and services the status page should show.",
			"An alert affects the service named by the service description, else by the host name: add the names to the services' names in monitoring tools. CRITICAL, DOWN and UNREACHABLE are major outages, WARNING is degraded performance, UNKNOWN changes nothing; OK and UP resolve the incident.",
		},
		ExampleLabel: "Notification commands (Nagios, Naemon)",
		Example: `define command {
  command_name  notify-service-by-statuspage
  command_line  /usr/bin/curl -fsS -m 10 '{url}' --data-urlencode 'key=$HOSTNAME$/$SERVICEDESC$' --data-urlencode 'status=$SERVICESTATE$' --data-urlencode 'service=$SERVICEDESC$' --data-urlencode 'service=$HOSTNAME$' --data-urlencode 'title=$SERVICEDESC$ on $HOSTNAME$' --data-urlencode 'message=$SERVICEOUTPUT$'
}

define command {
  command_name  notify-host-by-statuspage
  command_line  /usr/bin/curl -fsS -m 10 '{url}' --data-urlencode 'key=$HOSTNAME$' --data-urlencode 'status=$HOSTSTATE$' --data-urlencode 'service=$HOSTNAME$' --data-urlencode 'title=$HOSTNAME$' --data-urlencode 'message=$HOSTOUTPUT$'
}`,
	},
	{
		ID:      Generic,
		Name:    "Other tool or script",
		Parse:   ParseGeneric,
		Service: "the field service (a name, or a list of names tried in order)",
		Steps: []string{
			"Send an HTTP POST to the webhook URL when a problem starts and when it ends, with a JSON body like in the example (or the same fields as form fields or query parameters). Most monitoring tools can send a webhook with a body of your choice (e.g. SolarWinds, New Relic, Dynatrace, Elastic, Healthchecks.io); scripts can use curl or PowerShell.",
			"Fields: service – the name of the service, or one of its names in monitoring tools (a list is tried in order); status – firing, problem, down, critical, error or warning for a problem, resolved, ok, up or recovered for its end; key – identifies the alert, so that its end clears it (default: the service); title and message – the text of the incident; severity – e.g. critical, error, warning; impact – degraded, partial_outage or major_outage. Several alerts can be sent as a JSON list or as {\"alerts\": [ … ]}.",
			"A request with the status test is only listed below, to test the connection.",
		},
		ExampleLabel: "curl and PowerShell",
		Example: `curl -X POST '{url}' -H 'Content-Type: application/json' \
  -d '{"service": "VPN", "status": "down", "title": "VPN gateway unreachable"}'

Invoke-RestMethod -Method Post -Uri '{url}' -ContentType 'application/json' ` + "`" + `
  -Body (@{ service = 'VPN'; status = 'down'; title = 'VPN gateway unreachable' } | ConvertTo-Json)`,
	},
}

// Providers returns all monitoring tools by name, "Other tool or script"
// last.
func Providers() []*Provider {
	out := slices.Clone(providers)
	slices.SortStableFunc(out, func(a, b *Provider) int {
		switch {
		case a.ID == Generic:
			return 1
		case b.ID == Generic:
			return -1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
}

// Lookup returns the monitoring tool with this ID, or nil.
func Lookup(id string) *Provider {
	for _, p := range providers {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// Expand returns s with "{url}" replaced by url.
func Expand(s, url string) string { return strings.ReplaceAll(s, "{url}", url) }
