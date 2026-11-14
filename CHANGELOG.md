# Changelog

All notable changes are listed here. Versions follow
[semantic versioning](https://semver.org/); the release notes on GitHub start
with the section of the released version.

## 1.1.0 – 2026-11-14

Group names and roles from Microsoft Entra ID. See the
[README](https://github.com/szilvasidave/Status-Page-Tool#group-names-from-microsoft-entra-id)
for the setup.

- **Group names from Microsoft Entra ID.** The groups claim of Entra ID holds
  only the groups' object IDs, so groups were shown by their IDs. With the
  new setting *Look up group names in Microsoft Entra ID* the status page
  reads the names of the groups it uses with Microsoft Graph (application
  permission GroupMember.Read.All on the single sign-on app registration).
  Admin → Groups and the service form show them with the IDs; Admin → Users,
  the services list and the account page show the names. A new group gets
  its name a few seconds after the sign-in that recorded it (a group created
  in Entra ID a moment ago is looked up again for a quarter of an hour); all
  names are looked up again daily, or at once with *Look up names now*; a
  group added by its object ID is checked in Entra ID right away. A display
  name given in the status page takes precedence and stays when the group is
  renamed in Entra ID. Groups with the same name, and groups Entra ID no
  longer knows, are marked.
- **Roles from app roles.** With *Assign roles with app roles*, users get the
  Administrator or Editor role from the app roles assigned to them, or to
  their groups, in Microsoft Entra ID (the `roles` claim; values `Admin` and
  `Editor` by default, compared exactly), in addition to the roles of their
  groups; with Keycloak, client roles work the same way. The local
  administrator sign-in can also be turned off when somebody has signed in
  with the app role for administrators in the last 30 days.
- Groups recorded from sign-ins no longer get their ID as their name: a
  group without a name of its own shows its name from Entra ID, else its ID.
  The upgrade clears the names that only repeat the ID. The names of
  provisioned groups come only from provisioning and can no longer be edited
  in the status page; a group that stops being provisioned shows its last
  provisioned name until the lookup finds its current one.
- When provisioning takes over a user who signed in before, or activates a
  user again, the user is signed out once and loses the app roles and the
  group memberships of the previous sign-in (provisioned groups excepted):
  the next sign-in links them again (a user name can be reused for somebody
  else).
- Databases that ran a development build of this version are repaired at
  the start (a missing column of the group names).
- Removing a user on Admin → Users now says that anybody who can still sign
  in comes back, with the groups and app roles the identity provider sends.

## 1.0.0 – 2026-11-13

First stable release: a small, self-hosted status page for internal IT
services, as one executable with a Windows installer. See the
[README](https://github.com/szilvasidave/Status-Page-Tool#readme) for installation and configuration.

- **Status page** with every service as *operational*, *maintenance*,
  *degraded*, *partial outage* or *major outage*, open incidents, upcoming
  maintenance, a 90-day history per service, a history page and JSON at
  `/api/status`.
- **Incidents** with a timeline of updates and an impact per affected
  service; **maintenance windows** with start and end time. A service's
  status is always derived from them.
- **Single sign-on with OpenID Connect** (Microsoft Entra ID, Keycloak or any
  standard provider) and one local administrator account for the setup and
  emergencies, which can be turned off once single sign-on works and turned
  on again with `statuspage local-login enable`.
- **Users and groups from the identity provider:** SCIM provisioning from
  Microsoft Entra ID, or the groups claim at sign-in. Roles (*Viewer*,
  *Editor*, *Administrator*) and services are assigned to groups.
- **E-mail notifications** to the members of the groups of affected services,
  through Microsoft Graph (Microsoft 365) or any SMTP server.
- **Branding:** logos for light and dark mode, favicon, accent and header
  colours for both modes and a fixed or automatic appearance; text and focus
  colours are kept readable automatically.
- **Storage** in built-in SQLite or PostgreSQL; the tables are created and
  upgraded automatically.
- **Windows installer** that sets up the service, the firewall rule and the
  database; portable executables for Windows, Linux (amd64, arm64) and macOS
  (arm64), with a service mode for Windows, systemd and launchd.
