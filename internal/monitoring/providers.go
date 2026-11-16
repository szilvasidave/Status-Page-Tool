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
			"In alertmanager.yml add a webhook_configs entry with the webhook URL to the receivers whose alerts the status page should show – e.g. the receiver of your critical alerts, or the default receiver of the route – as in the example, and reload Alertmanager. The routes stay as they are, so the alerts still reach everybody they reached before.",
			"Alerts that name no service are only listed below (No service found), so the receiver can get other alerts too. Do not send alerts that always fire, such as the Watchdog alert of kube-prometheus: they never resolve. A route of its own for the status page works only as the first of the routes and with continue: true – and an alert that matches it no longer reaches the receiver of the parent route, unless a later route matches it too.",
			"An alert affects the service named by its label statuspage_service, else by its label service, alertname, job or instance: add a label statuspage_service to the alert rules, or add the values to the services' names in monitoring tools.",
			"The label severity sets the impact (critical: major outage, error: partial outage, warning or info: degraded performance; without a severity: major outage), a label statuspage_impact (degraded, partial_outage, major_outage) sets it directly. The annotation summary (else the alert's name) is the title of the incident, the annotation description its text.",
			"When the alert is resolved (send_resolved, on by default), the incident is resolved.",
		},
		ExampleLabel: "alertmanager.yml",
		Example: `receivers:
  # A receiver that your routes already use, e.g. that of the critical
  # alerts: keep what it has and add the webhook.
  - name: on-call
    email_configs:
      - to: 'on-call@example.com'
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
			"In Grafana open the contact point that the alert rules or notification policies of the alerts the status page should show already use (Alerting → Contact points; in newer versions Alerts & IRM → Alerting → Notification configuration → Contact points), click Add contact point integration, choose Webhook and enter the webhook URL. A contact point can hold several integrations: its others still get the alerts. Leave “Disable resolved message” off, so that resolved alerts resolve the incident.",
			"A contact point of its own works too, chosen in the alert rules; in the notification policies the same holds as for Alertmanager: an alert that matches a nested policy no longer reaches the default contact point. Alerts that name no service are only listed below (No service found). Use Test next to the Webhook integration to check the connection: the test is listed below and changes nothing.",
			"When Grafana cannot evaluate a rule (its data source fails or sends no data), it sends alerts named DatasourceError or DatasourceNoData with the rule's labels: they are listed below and ignored, as only the monitoring is broken. Set the rule's no data and error handling to “Keep last state”, so that an incident stays open meanwhile.",
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
		Service: "the monitor's tag statuspage_service, else its tag service, else its name, host name or URL",
		Steps: []string{
			"In Uptime Kuma open Settings → Notifications → Setup Notification, choose the notification type Webhook, enter the webhook URL as Post URL and keep the request body “Preset – application/json”. Use Test to check the connection: the test is listed below and changes nothing.",
			"Turn the notification on for the monitors the status page should show (or tick “Default enabled” and “Apply on all existing monitors”).",
			"A monitor that goes down opens an incident (major outage) of the service named by the monitor's tag statuspage_service, else by its tag service, else by its name, host name or URL; when it is up again, the incident is resolved. Name the monitors like the services, add the monitors' names to the services' names in monitoring tools, or give the monitors a tag statuspage_service with the service as value. A tag statuspage_impact (degraded, partial_outage, major_outage) sets the impact.",
			"An outage that follows a lost up notification opens a new incident, even if an editor resolved the old one: Uptime Kuma says when a monitor went down. With Uptime Kuma 1.23 this holds only for monitors without retries (Retries 0), because 1.23 starts the retries again when it restarts or a monitor is saved or resumed: a monitor with retries that is still down is then reported down again, and if it comes up during these retries, no up notification is sent – then clear the alert under Firing alerts on this page. Uptime Kuma 2 keeps the retries.",
			"Uptime Kuma sends nothing when a monitor enters one of its maintenances, and nothing when the monitor is up at its end or comes up during its retries after it: if a monitor that was down recovers like this, clear the alert under Firing alerts on this page. A monitor that is still down after the maintenance is reported down again, as a new outage – a new incident if an editor resolved the old one (with Uptime Kuma 1.23 only for monitors without retries). Better use the status page's maintenance for the service (Manage → Schedule maintenance).",
		},
	},
	{
		ID:      "azuremonitor",
		Name:    "Azure Monitor",
		Parse:   ParseAzureMonitor,
		Service: "the custom property statuspage_service of the alert rule, else its custom property service, else the rule's name, else the names of the affected resources",
		Steps: []string{
			"In the Azure portal open Monitor → Alerts → Action groups, create an action group (or open one) and add an action of the type Webhook with the webhook URL. Select Yes for “Enable the common alert schema”.",
			"Add the action group to the alert rules whose alerts the status page should show. Metric alerts are resolved automatically; for log search alerts turn on “Automatically resolve alerts” in the rule.",
			"Azure never resolves activity log alerts, but two kinds say when a problem ends. Service Health alerts: an incident of Azure (event type Service issue) opens an incident while it is active and resolves it when Azure reports it resolved; planned maintenance, health and security advisories are ignored.",
			"Resource Health alerts: a resource that is Unavailable is a major outage, Degraded degraded performance, and Available again resolves the incident – in the rule's condition keep all event statuses selected (at least Active and Resolved: the recovery is a Resolved event) and Available among the current resource statuses; Unknown is ignored. Other activity log alerts (e.g. administrative operations) are ignored.",
			"An alert affects the service named by the rule's custom property statuspage_service (Details → Advanced options → Custom properties), else by its custom property service, else by the rule's name, else by the names of the affected resources. The severity sets the impact (Sev0: major outage, Sev1: partial outage, Sev2–Sev4: degraded performance; Azure sends Service Health alerts as Sev4), a custom property statuspage_impact (degraded, partial_outage, major_outage) sets it directly.",
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
			"In Zabbix create a media type (Alerts → Media types; before Zabbix 6.4: Administration → Media types) of the type Webhook, with the parameters and the script below. Before Zabbix 5.4 the object is CurlHttpRequest and its methods are AddHeader, Post and Status (instead of HttpRequest, addHeader, post and getStatus).",
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
		Parse:   ParsePRTG,
		Service: "the device's name, else its group",
		Steps: []string{
			"In PRTG open Setup → Account Settings → Notification Templates → Add Notification Template, turn on “Execute HTTP Action”, enter the webhook URL, choose the HTTP method POST and enter the payload below (before PRTG 20.1.57 use %status instead of %laststatus). The template's test sends the placeholders unfilled: it is listed below as a test and changes nothing.",
			"Add a state trigger with this template to the sensors the status page should show (or to their devices or groups): “When sensor status is Down for at least … seconds, perform” the template, and “When sensor status is no longer Down, perform” the same template. Add a second state trigger, “When sensor status is Up for at least … seconds, perform” the same template (its “no longer Up” without a notification): the first one sends nothing when a sensor whose alarm was acknowledged comes back up. The second one also reports sensors that come up from Warning or Unusual: these are only listed below (No change). In a failover cluster add a trigger like the first one for Down (Partial) too.",
			"The status is read as the sensor's state: Down is a major outage, Down (Partial) a partial outage, and a sensor that leaves Down – for Up, Warning, Unusual, Paused or Unknown – resolves the incident; Warning and Unusual are not shown. An acknowledged alarm (Down (Acknowledged)) stays an outage until the sensor is Up again; if it does not come up again (e.g. it is deleted), clear the alert under Firing alerts on this page.",
			"A sensor affects the service named by its device, else by the device's group: name the devices like the services, or add the device names to the services' names in monitoring tools.",
		},
		ExampleLabel: "Payload",
		Example:      `key=%sensorid&status=%laststatus&service=%device&service=%group&title=%device: %name&message=%message`,
	},
	{
		ID:      "datadog",
		Name:    "Datadog",
		Parse:   ParseDatadog,
		Service: "the tag statuspage_service, else the tag service, else the host name",
		Steps: []string{
			"In Datadog open Integrations → Webhooks → New, enter a name (e.g. statuspage), the webhook URL and the payload below.",
			"Mention the webhook (e.g. @webhook-statuspage) in the message of the monitors whose alerts the status page should show. Test Notifications (Run Test) send a monitor's alerts with [TEST] in the title, also to its other recipients: they are listed below as tests and change nothing.",
			"An alert affects the service named by the tag statuspage_service, else by the tag service, else by the host name. A triggered alert (alert type error) is a major outage, a warning degraded performance; the recovery resolves the incident. No Data changes nothing, and a renotification only repeats the state it renotifies.",
		},
		ExampleLabel: "Payload",
		Example: `{
  "key": "$AGGREG_KEY",
  "status": "$ALERT_TRANSITION",
  "severity": "$ALERT_TYPE",
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
			"UptimeRobot sends each notification once and does not retry it: if the status page could not be reached when a monitor came up again, clear the alert under Firing alerts on this page.",
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
			"started is the last time the service was OK (the host was UP): the same in every notification of a problem, also when it changes between WARNING and CRITICAL, so that a problem that starts again after its recovery got lost is a new one. curl tries again for about 15 seconds (--retry), within Nagios' notification_timeout (30 seconds in the sample nagios.cfg), so that a restart of the status page does not lose a recovery. --retry-connrefused (also retry a refused connection, e.g. while the status page restarts) needs curl 7.52 or later: with an older curl, e.g. on RHEL or CentOS 7, leave it out – else curl stops at the unknown option and sends nothing.",
		},
		ExampleLabel: "Notification commands (Nagios, Naemon)",
		Example: `define command {
  command_name  notify-service-by-statuspage
  command_line  /usr/bin/curl -fsS -m 10 --retry 4 --retry-connrefused --retry-max-time 15 '{url}' --data-urlencode 'key=$HOSTNAME$/$SERVICEDESC$' --data-urlencode 'status=$SERVICESTATE$' --data-urlencode 'started=$LASTSERVICEOK$' --data-urlencode 'service=$SERVICEDESC$' --data-urlencode 'service=$HOSTNAME$' --data-urlencode 'title=$SERVICEDESC$ on $HOSTNAME$' --data-urlencode 'message=$SERVICEOUTPUT$'
}

define command {
  command_name  notify-host-by-statuspage
  command_line  /usr/bin/curl -fsS -m 10 --retry 4 --retry-connrefused --retry-max-time 15 '{url}' --data-urlencode 'key=$HOSTNAME$' --data-urlencode 'status=$HOSTSTATE$' --data-urlencode 'started=$LASTHOSTUP$' --data-urlencode 'service=$HOSTNAME$' --data-urlencode 'title=$HOSTNAME$' --data-urlencode 'message=$HOSTOUTPUT$'
}`,
	},
	{
		ID:      Generic,
		Name:    "Other tool or script",
		Parse:   ParseGeneric,
		Service: "the field service (a name, or a list of names tried in order)",
		Steps: []string{
			"Send an HTTP POST to the webhook URL when a problem starts and when it ends, with a JSON body like in the example (or the same fields as form fields or query parameters). Most monitoring tools can send a webhook with a body of your choice (e.g. SolarWinds, New Relic, Dynatrace, Elastic, Healthchecks.io); scripts can use curl or PowerShell.",
			"Fields: service – the name of the service, or one of its names in monitoring tools (a list is tried in order); status – firing, problem, down, critical, error or warning for a problem, resolved, ok, up or recovered for its end; key – identifies the alert, so that its end clears it (default: the service); title and message – the text of the incident; severity – e.g. critical, error, warning; impact – degraded, partial_outage or major_outage; started – when the problem began (RFC 3339, “YYYY-MM-DD HH:MM:SS” in UTC, or Unix seconds or milliseconds), optional. Several alerts can be sent as a JSON list or as {\"alerts\": [ … ]}.",
			"Send started only if it stays the same in every repeat of a problem (e.g. the time of the first failed check, not the time of the request). If an editor resolves the incident of a problem that still fires, its repeats change nothing; with started, the problem opens a new incident when it starts again later, without it only after its end has been sent.",
			"A request with the status test is only listed below, to test the connection.",
		},
		ExampleLabel: "curl and PowerShell",
		Example: `curl -X POST '{url}' -H 'Content-Type: application/json' \
  -d '{"service": "VPN", "status": "down", "title": "VPN gateway unreachable"}'

Invoke-RestMethod -Method Post -Uri '{url}' -ContentType 'application/json; charset=utf-8' ` + "`" + `
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
