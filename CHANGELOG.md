# Changelog

All notable changes are listed here. Versions follow
[semantic versioning](https://semver.org/); the release notes on GitHub start
with the section of the released version.

## Unreleased

- **Group names from Microsoft Entra ID.** The groups claim of Entra ID holds
  only the groups' object IDs, so groups were shown by their IDs. With the
  new setting *Look up group names in Microsoft Entra ID* the status page
  reads their names with Microsoft Graph (application permission
  GroupMember.Read.All on the single sign-on app registration) and shows them
  with the IDs in Admin → Groups, the service form, Admin → Users and the
  account page. A new group gets its name a few seconds after the sign-in
  that recorded it; all names are looked up again daily, or at once with
  *Look up names now*; a group added by its object ID is checked in Entra ID
  right away. A display name given in the status page takes precedence.
- **Roles from app roles.** With *Assign roles with app roles*, users get the
  Administrator or Editor role from the app roles assigned to them, or to
  their groups, in Microsoft Entra ID (the `roles` claim; values `Admin` and
  `Editor` by default), in addition to the roles of their groups. The local
  administrator sign-in can also be turned off when somebody has signed in
  with the app role for administrators.
- Groups recorded from sign-ins no longer get their ID as their name: a
  group without a name of its own shows its name from Entra ID, else its ID.
  The upgrade clears the names that only repeat the ID.

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
