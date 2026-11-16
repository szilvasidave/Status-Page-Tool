package monitoring

// Payloads of Azure Monitor alerts as Microsoft documents them, verbatim:
// https://learn.microsoft.com/en-us/azure/azure-monitor/alerts/alerts-payload-samples

// Sample test action “Activity log alert” (Activity Log - Administrative).
const azureActivityLogTest = `{
  "schemaId": "azureMonitorCommonAlertSchema",
  "data": {
    "essentials": {
      "alertId": "/subscriptions/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e/resourceGroups/test-RG/providers/microsoft.insights/activityLogAlerts/Test_Alert",
      "alertRule": "test-activityLogAlertRule",
      "severity": "Sev4",
      "signalType": "Activity Log",
      "monitorCondition": "Fired",
      "monitoringService": "Activity Log - Administrative",
      "alertTargetIDs": [
        "/subscriptions/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e/resourcegroups/test-RG/providers/microsoft.compute/virtualmachines/test-VM"
      ],
      "configurationItems": [
        "test-VM"
      ],
      "originAlertId": "dddd3333-ee44-5555-66ff-777777aaaaaa_123456789012345678901234567890ab",
      "firedDateTime": "2025-04-15T11:26:21.386Z",
      "description": "Alert rule description",
      "essentialsVersion": "1.0",
      "alertContextVersion": "1.0"
    },
    "alertContext": {
      "authorization": {
        "action": "Microsoft.Compute/virtualMachines/restart/action",
        "scope": "/subscriptions/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e/resourceGroups/test-RG/providers/Microsoft.Compute/virtualMachines/test-VM"
      },
      "channels": "Operation",
      "claims": "{}",
      "caller": "user@contoso.com",
      "correlationId": "cccc2222-dd33-4444-55ee-666666ffffff",
      "eventSource": "Administrative",
      "eventTimestamp": "2025-04-15T11:26:21.386Z",
      "eventDataId": "dddd3333-ee44-5555-66ff-777777aaaaaa",
      "level": "Informational",
      "operationName": "Microsoft.Compute/virtualMachines/restart/action",
      "operationId": "bbbbbbbb-1111-2222-3333-cccccccccccc",
      "properties": {
        "eventCategory": "Administrative",
        "entity": "/subscriptions/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e/resourceGroups/test-RG/providers/Microsoft.Compute/virtualMachines/test-VM",
        "message": "Microsoft.Compute/virtualMachines/restart/action",
        "hierarchy": "22222222-2222-2222-2222-222222222222/CnAIOrchestrationServicePublicCorpprod/33333333-3333-3333-3333-3333333333333/cccccccc-2222-3333-4444-dddddddddddd/dddddddd-3333-4444-5555-eeeeeeeeeeee/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e"
      },
      "status": "Succeeded",
      "subStatus": "",
      "submissionTimestamp": "2025-04-15T11:26:21.386Z",
      "Activity Log Event Description": ""
    }
  }
}`

// Sample test action “Resource health alert”: Unavailable, previously Available.
const azureResourceHealthTest = `{
  "schemaId": "azureMonitorCommonAlertSchema",
  "data": {
    "essentials": {
      "alertId": "/subscriptions/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e/providers/Microsoft.AlertsManagement/alerts/eeeeeeee-4444-5555-6666-ffffffffffff",
      "alertRule": "test-ResourceHealthAlertRule",
      "severity": "Sev4",
      "signalType": "Activity Log",
      "monitorCondition": "Fired",
      "monitoringService": "Resource Health",
      "alertTargetIDs": [
        "/subscriptions/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e/resourcegroups/test-RG/providers/microsoft.compute/virtualmachines/test-VM"
      ],
      "configurationItems": [
        "test-VM"
      ],
      "originAlertId": "dddd3333-ee44-5555-66ff-777777aaaaaa_123456789012345678901234567890ab",
      "firedDateTime": "2025-04-15T18:19:45.304Z",
      "description": "Alert rule description",
      "essentialsVersion": "1.0",
      "alertContextVersion": "1.0"
    },
    "alertContext": {
      "channels": "Admin, Operation",
      "correlationId": "cccc2222-dd33-4444-55ee-666666ffffff",
      "eventSource": "ResourceHealth",
      "eventTimestamp": "2025-04-15T18:19:45.304Z",
      "eventDataId": "dddd3333-ee44-5555-66ff-777777aaaaaa",
      "level": "Informational",
      "operationName": "Microsoft.Resourcehealth/healthevent/Activated/action",
      "operationId": "dddd3333-ee44-5555-66ff-777777aaaaaa",
      "properties": {
        "title": "Rebooted by user",
        "details": null,
        "currentHealthStatus": "Unavailable",
        "previousHealthStatus": "Available",
        "type": "Downtime",
        "cause": "UserInitiated"
      },
      "status": "Active",
      "submissionTimestamp": "2025-04-15T18:19:45.304Z",
      "Activity Log Event Description": null
    }
  }
}`

// Sample test action “Service health alert”: an incident, stage Resolved.
const azureServiceHealthTest = `{
  "schemaId": "azureMonitorCommonAlertSchema",
  "data": {
    "essentials": {
      "alertId": "/subscriptions/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e/providers/Microsoft.AlertsManagement/alerts/1234abcd5678efgh1234abcd5678efgh1234abcd5678efgh1234abcd5678efgh",
      "alertRule": "test-ServiceHealthAlertRule",
      "severity": "Sev4",
      "signalType": "ActivityLog",
      "monitorCondition": "Fired",
      "monitoringService": "ServiceHealth",
      "alertTargetIDs": [
        "/subscriptions/aaaa0a0a-bb1b-cc2c-dd3d-eeeeee4e4e4e"
      ],
      "originAlertId": "eeeeeeee-4444-5555-6666-ffffffffffff",
      "firedDateTime": "2025-04-15T18:20:36.256Z",
      "description": "Alertruledescription",
      "essentialsVersion": "1.0",
      "alertContextVersion": "1.0"
    },
    "alertContext": {
      "authorization": null,
      "channels": 1,
      "claims": null,
      "caller": null,
      "correlationId": "aaaa0000-bb11-2222-33cc-444444dddddd",
      "eventSource": 2,
      "eventTimestamp": "2025-04-15T18:20:36.256Z",
      "httpRequest": null,
      "eventDataId": "eeeeeeee-4444-5555-6666-ffffffffffff",
      "level": 3,
      "operationName": "Microsoft.ServiceHealth/incident/action",
      "operationId": "aaaa0000-bb11-2222-33cc-444444dddddd",
      "properties": {
        "title": "TestActionGroup-TestServiceHealthAlert",
        "service": "AzureServiceName",
        "region": "Global",
        "communication": "<p>ThisisatestfromServiceHealthAlert</p>",
        "incidentType": "Incident",
        "trackingId": "TEST-TTT",
        "impactStartTime": "2025-04-15T18:20:36.256Z",
        "impactMitigationTime": "2025-04-15T18:20:36.256Z",
        "impactedServices": [
          {
            "ImpactedRegions": [
              {
                "RegionName": "Global"
              }
            ],
            "ServiceName": "AzureServiceName"
          }
        ],
        "impactedServicesTableRows": "<tr><td>Thisisatestfromservicehealthalert</<td>/<tr>",
        "defaultLanguageTitle": "TestActionGroup-TestServiceHealthAlert",
        "defaultLanguageContent": "<p>ThisisatestfromServiceHealthAlert</p>",
        "stage": "Resolved",
        "communicationId": "11223344556677",
        "isHIR": "false",
        "IsSynthetic": "True",
        "impactType": "SubscriptionList",
        "version": "0.1.1"
      },
      "status": "Resolved",
      "subStatus": null,
      "submissionTimestamp": "2025-04-15T18:20:36.256Z",
      "ResourceType": null
    }
  }
}`

// Alert context of an activity log alert with monitoringService = ServiceHealth: a
// maintenance (the samples give only the alert context).
const azureServiceHealthContext = `{
  "alertContext": {
    "authorization": null,
    "channels": 1,
    "claims": null,
    "caller": null,
    "correlationId": "bbbb1111-cc22-3333-44dd-555555eeeeee",
    "eventSource": 2,
    "eventTimestamp": "2019-06-24T11:31:19.0312699+00:00",
    "httpRequest": null,
    "eventDataId": "<GUID>",
    "level": 3,
    "operationName": "Microsoft.ServiceHealth/maintenance/action",
    "operationId": "<GUID>",
    "properties": {
      "title": "Azure Synapse Analytics Scheduled Maintenance Pending",
      "service": "Azure Synapse Analytics",
      "region": "East US",
      "communication": "<MESSAGE>",
      "incidentType": "Maintenance",
      "trackingId": "<GUID>",
      "impactStartTime": "2019-06-26T04:00:00Z",
      "impactMitigationTime": "2019-06-26T12:00:00Z",
      "impactedServices": "[{\"ImpactedRegions\":[{\"RegionName\":\"East US\"}],\"ServiceName\":\"Azure Synapse Analytics\"}]",
      "impactedServicesTableRows": "<tr>\r\n<td align='center' style='padding: 5px 10px; border-right:1px solid black; border-bottom:1px solid black'>Azure Synapse Analytics</td>\r\n<td align='center' style='padding: 5px 10px; border-bottom:1px solid black'>East US<br></td>\r\n</tr>\r\n",
      "defaultLanguageTitle": "Azure Synapse Analytics Scheduled Maintenance Pending",
      "defaultLanguageContent": "<MESSAGE>",
      "stage": "Planned",
      "communicationId": "<GUID>",
      "maintenanceId": "<GUID>",
      "isHIR": "false",
      "version": "0.1.1"
    },
    "status": "Active",
    "subStatus": null,
    "submissionTimestamp": "2019-06-24T11:31:31.7147357+00:00",
    "ResourceType": null
  }
}`

// Alert context of an activity log alert with monitoringService = ResourceHealth
// (the samples give only the alert context).
const azureResourceHealthContext = `{
  "alertContext": {
    "channels": "Admin, Operation",
    "correlationId": "<GUID>",
    "eventSource": "ResourceHealth",
    "eventTimestamp": "2019-06-24T15:42:54.074+00:00",
    "eventDataId": "<GUID>",
    "level": "Informational",
    "operationName": "Microsoft.Resourcehealth/healthevent/Activated/action",
    "operationId": "<GUID>",
    "properties": {
      "title": "This virtual machine is stopping and deallocating as requested by an authorized user or process",
      "details": null,
      "currentHealthStatus": "Unavailable",
      "previousHealthStatus": "Available",
      "type": "Downtime",
      "cause": "UserInitiated"
    },
    "status": "Active",
    "submissionTimestamp": "2019-06-24T15:45:20.4488186+00:00"
  }
}`
