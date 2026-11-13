# Status Page

A small, self-hosted status page for internal IT services. One executable,
no runtime dependencies, installed on Windows with a normal setup wizard.

![Status page](docs/screenshots/status.png)

- **Status page** showing every service as *operational*, *maintenance*,
  *degraded*, *partial outage* or *major outage*, plus open incidents,
  upcoming maintenance and a history page. JSON at `/api/status`.
- **Incidents** (investigating → identified → monitoring → resolved) with a
  timeline of updates and an impact per affected service. **Maintenance
  windows** with start/end time and affected services. A service's status is
  always derived from these, never set by hand.
- **Single sign-on with OpenID Connect** (Microsoft Entra ID, Keycloak, any
  standard OIDC provider) plus one local administrator account for the first
  setup and emergencies. Once single sign-on works, the local sign-in can be
  turned off.
- **Users and groups come from the identity provider.** Assign groups to
  services and their members are e-mailed when an incident or maintenance
  affects the service. With Microsoft Entra ID, **SCIM provisioning** sends
  the members of the chosen groups to the status page, so people are notified
  even if they never signed in; with any provider, the groups claim updates
  the groups of everybody who signs in.
- **Roles per group:** *Viewer* (every signed-in user), *Editor* (posts
  incidents and maintenance windows) and *Administrator* (configuration).
- **E-mail** with Microsoft 365 through Microsoft Graph (default; uses the
  single sign-on app registration) or through any SMTP server.
- **Branding:** your logo, favicon and colours for light and dark mode, set
  by administrators in the web interface ([Branding](#branding)).
- **Storage:** built-in SQLite or PostgreSQL.
- Administrators configure everything in the web interface, except the
  listen address, the TLS certificate and the database connection, which are
  in [`config.json`](#configjson).

It is deliberately simple: no approval workflows, no plugins, e-mail is the
only notification channel.

## Contents

- [Quick start](#quick-start)
- [First-run setup](#first-run-setup)
- [Using the status page](#using-the-status-page)
- [Configuration in the web interface](#configuration-in-the-web-interface)
  - [Branding](#branding)
- [Single sign-on](#single-sign-on)
- [How users and groups get into the status page](#how-users-and-groups-get-into-the-status-page)
- [Roles and groups](#roles-and-groups)
- [Services and notifications](#services-and-notifications)
- [E-mail](#e-mail)
- [config.json](#configjson)
- [PostgreSQL](#postgresql)
- [Data directory, backup and upgrade](#data-directory-backup-and-upgrade)
- [Command line reference](#command-line-reference)
- [Security notes](#security-notes)
- [Building from source](#building-from-source)

## Quick start

### Windows (installer)

1. Download `StatusPageSetup-<version>.exe` from the
   [releases page](https://github.com/szilvasidave/Status-Page-Tool/releases/latest) and run it
   (administrator rights are required; Windows 10 / Server 2016 or newer, x64).
2. Choose the **HTTP port** (default 8080) and the **database**:
   - *SQLite* – built in, nothing else to install; recommended for small
     installations.
   - *PostgreSQL* – enter server, port, database, user, password and SSL mode.
     The database must already exist (see [PostgreSQL](#postgresql)); the
     installer tests the connection before it continues and creates the
     tables.
3. Leave *Allow incoming connections … in the Windows Firewall* ticked if
   other computers should reach the page.
4. On the last page keep *Open the status page setup in the browser* ticked
   and continue with the [first-run setup](#first-run-setup).

The installer puts `statuspage.exe` into `C:\Program Files\Status Page`, keeps
all data in `C:\ProgramData\StatusPage` (accessible only to SYSTEM and
Administrators), registers the Windows service **Status Page** (`StatusPage`,
automatic delayed start, restarted automatically if it fails), starts it and
checks that the page answers. If it does not (for example because another
program uses the port), the installer says so; the reason is in
`C:\ProgramData\StatusPage\statuspage.log`.

If the database setup fails during the installation, the service starts in
setup mode on the chosen port and you can finish the database setup in the
browser (or with `statuspage setup`, which keeps that port).

Unattended installation: `StatusPageSetup-<version>.exe /VERYSILENT` installs
with SQLite on port 8080.

Running the installer again on a machine that already has a configuration
performs an **upgrade**: it stops the service, replaces the program and starts
the service again. A `StatusPage` service registered earlier by a portable
`statuspage.exe` (see below) is switched to the installed program.

Uninstalling removes the service and the firewall rule but **keeps the data
directory**; delete `C:\ProgramData\StatusPage` yourself if you no longer
need it.

### Windows (portable exe)

Double-click `statuspage.exe` (or run it in a command prompt). It starts in
setup mode, opens `http://localhost:8080/setup` in your browser and stores its
data in `C:\ProgramData\StatusPage`. Use `statuspage.exe -data .\data` to keep
the data next to the exe and `-listen :9090` for another port. Press Ctrl+C to
stop it. Install it as a service later with
`statuspage.exe service install` and `statuspage.exe service start` (in an
administrator command prompt; add the same `-data` if you used one).

### Linux (systemd)

```sh
sudo install -m 755 statuspage-linux-amd64 /usr/local/bin/statuspage
sudo statuspage service install -data /var/lib/statuspage
sudo statuspage service start -data /var/lib/statuspage
```

This creates and starts the systemd unit `StatusPage.service`, which runs
`statuspage run -data /var/lib/statuspage` as root and restarts it if it
stops. Logs go to `/var/lib/statuspage/statuspage.log` and to the journal
(`journalctl -u StatusPage`). Pass the same `-data` to all `service` and
`setup` commands. Without `-data` the data directory is `data` next to the
binary. The same commands work on macOS (launchd).

Then open `http://<server>:8080/setup`. When the setup page is opened from
another computer it asks for the **setup token** from
`/var/lib/statuspage/setup-token.txt` (also shown in the log when it is
created). You can also configure the database on the command line first,
e.g. `sudo statuspage setup -data /var/lib/statuspage -db sqlite`.

## First-run setup

`/setup` is a short wizard:

1. **Database** – only if it was not configured by the installer or
   `statuspage setup`: choose SQLite or PostgreSQL. The tables are created
   immediately.
2. **Administrator** – the local administrator account and the basic
   settings:
   - **User name** – at most 64 letters, digits and the characters `. _ - @`,
     no spaces; upper and lower case do not matter. No name is suggested:
     choose one that is not easy to guess, not `admin` (failed sign-ins lock a
     name out for a while, see [Security notes](#security-notes)). The
     display name is optional.
   - **Password** – at least 10 characters.
   - **Site name**, **base URL**, **time zone** and **public access** (see
     [General settings](#general-settings)). The base URL is suggested from
     the address you opened, with `localhost` replaced by the name of the
     computer. The time zone is chosen from a list grouped by region, each
     entry with its current UTC offset (e.g. *Berlin (UTC+01:00)*); the wizard
     preselects the time zone of your browser if it is in the list.

   This account always has the Administrator role. It is meant for the
   initial configuration and as a break-glass login when single sign-on is
   unavailable; once single sign-on works, its sign-in can be
   [turned off](#local-administrator-sign-in).

Opening `/setup` from a computer other than the server itself, or through a
reverse proxy (a request with an `X-Forwarded-For`, `Forwarded` or
`X-Real-IP` header), requires the setup token from
`<data directory>/setup-token.txt`. The browser remembers a correct token for
an hour, so it is entered only once. When the administrator account has been
created the token file is deleted and `/setup` is closed.

The wizard then signs you in as the local administrator and continues with
**Admin → Settings**.

## Using the status page

- **Status page** (`/`): overall status, open incidents, maintenance in
  progress and upcoming, every service with its status and a 90-day history
  bar, and the incidents of the last 7 days. It refreshes itself every
  minute. **History** (`/history`) lists incidents and maintenance per month;
  each incident and maintenance window has its own page.
- **Editors** use **Manage** (`/manage`):
  - *Report incident* (title, impact per affected service, status, message),
    then post updates on the incident's page until it is *Resolved*.
  - *Schedule maintenance* (title, description, start and end, affected
    services). A maintenance window can be edited until it is over. Before
    it starts it can be **cancelled**; once it is in progress it can be
    ended early with **End now** instead.
  - Titles are limited to 200 characters, messages and descriptions to
    10,000 characters.
  - With *Send e-mail notifications* ticked, the members of the groups
    assigned to the affected services are e-mailed (see
    [Services and notifications](#services-and-notifications)).
  - Administrators can also delete incidents and maintenance windows that
    were created by mistake.
- **Your account** (click your name in the header) shows your role, your
  groups and the services you receive e-mails about. The local administrator
  can change its display name and user name there (same rules as in the
  setup; afterwards sign in with the new user name – sessions that are open
  stay signed in) and its password.

| | |
|---|---|
| ![Report incident](docs/screenshots/report-incident.png) | ![Incident page](docs/screenshots/incident.png) |
| ![Dark mode](docs/screenshots/status-dark.png) | ![Mobile](docs/screenshots/mobile.png) |

### JSON API

`GET /api/status` returns the current status (it needs a signed-in session
when public access is off):

```json
{
  "status": "major_outage",
  "status_label": "Major system outage",
  "updated_at": "2026-11-12T23:26:09Z",
  "services": [
    { "id": 5, "name": "VPN", "category": "Infrastructure", "status": "major_outage", "status_label": "Major outage" }
  ],
  "incidents": [
    { "id": 1, "title": "VPN connections dropping", "status": "identified", "impact": "major_outage",
      "started_at": "2026-11-12T22:35:42Z", "url": "https://status.example.com/incidents/1" }
  ],
  "maintenances": [
    { "id": 1, "title": "ERP kernel upgrade", "state": "scheduled",
      "starts_at": "2026-11-14T23:00:00Z", "ends_at": "2026-11-15T02:00:00Z", "url": "https://status.example.com/maintenance/1" }
  ]
}
```

`status` is one of `operational`, `maintenance`, `degraded`,
`partial_outage`, `major_outage`. Only open incidents (`status`
`investigating`, `identified` or `monitoring`) and maintenance in progress or
upcoming (`state` `in_progress` or `scheduled`) are listed. Times are UTC
(RFC 3339); the URLs use the configured base URL.

## Configuration in the web interface

Administrators configure the status page under **Admin** (`/admin`):
**Settings**, **Branding**, **Services**, **Groups**, **Users** and
**Notifications**. Only the listen address, the TLS certificate, the database
connection and the log level are set in [`config.json`](#configjson).

### General settings

The first section of **Admin → Settings**:

- **Site name** – shown in the header and in e-mails.
- **Base URL** – the address users open, without trailing slash, e.g.
  `https://status.example.com`. It is used for links in e-mails, for the
  single sign-on redirect URI and for the provisioning Tenant URL, so set it
  first.
- **Time zone** – chosen from a list grouped by region (e.g. *Berlin
  (UTC+01:00)*; UTC comes first). Times on the status page and in e-mails
  are shown in this time zone, and times you enter (e.g. the start and end of
  a maintenance window) are read in it. A time zone that was set earlier and
  is not in the list stays selected.
- **Public access** – when on, the status pages can be viewed without signing
  in; when off, everybody has to sign in.

The other sections of the settings page are described below:
[single sign-on](#single-sign-on), [provisioning](#set-up-provisioning-in-microsoft-entra-id)
and [e-mail](#e-mail).

### Branding

**Admin → Branding** adapts the status page to your company: logos, favicon,
the colours of light and dark mode, and the appearance. Every field is
optional; whatever is left empty keeps the default look.

- **Logo for light mode** – a PNG, JPEG, GIF, WebP or SVG image of at most
  512 KB. It replaces the default mark in the header of every page: status
  pages, sign-in page, administration, account and error pages. It is shown
  32 pixels high (and at most 240 pixels wide), so use an SVG or an image at
  least 64 pixels high, which stays sharp on high-resolution screens.
- **Logo for dark mode** – optional, same formats, e.g. a version with light
  lettering. Without it the light-mode logo is shown in dark mode too, so it
  should then be readable on a dark header as well (and a logo uploaded only
  for dark mode is shown in both modes).
- **Show the site name next to the logo** – on by default. Turn it off when
  the logo already contains the name; screen readers then read the
  [site name](#general-settings) as the logo's text. Next to a logo the name
  is shown only where it fits completely: on phones, or with a long name or a
  wide logo, often only the logo is shown. A name that must always be visible
  therefore belongs in the logo.
- **Favicon** – the icon in the browser tab: PNG, GIF, SVG or ICO, at most
  512 KB. Without one the default icon is used, drawn in the light-mode
  accent colour.
- **Appearance** – *Follow the device setting* (the default: light or dark
  mode, as each viewer's operating system or browser is set), *Always light*
  or *Always dark*. Printed pages always use the light colours.
- **Colours** – hex codes such as `#1f5bd8` (`#abc` is short for `#aabbcc`),
  typed in or chosen with the colour picker next to the field. An empty
  field shows the colour it falls back to:

  | Colour | Used for | When empty |
  |--------|----------|------------|
  | Light mode: **Accent colour** | buttons, links, focus rings, the active tab and the default mark in the header | the default blue `#1f5bd8` |
  | Light mode: **Header background** | the header at the top of every page | the default header (white) |
  | Dark mode: **Accent colour** | the same in dark mode | the light-mode accent colour, lightened where needed so that it stands out on the dark background (contrast at least 3:1); without a light-mode accent colour either, the default blue `#2f6be0` |
  | Dark mode: **Header background** | the header in dark mode | the default dark header (the light-mode header colour is not used) |

**Readability.** Whatever colours you choose, text stays readable and focus
outlines stay visible:

- Buttons get white or near-black text, whichever contrasts more with the
  accent colour (black where neither reaches 4.5:1). Their hover shade is a
  little darker behind white text and a little lighter behind dark text, so
  the text never gets harder to read.
- Links are text on the page background, so they need a contrast of at least
  4.5:1 (WCAG AA). A light accent colour in light mode, or a dark one in dark
  mode, often does not reach it; links then use a darker (light mode) or
  lighter (dark mode) shade of it, which the branding page names.
- Focus outlines, the border of the field being edited and the marker of the
  active tab need a contrast of at least 3:1 on the page. Where the accent
  colour does not reach it, they use a darker (light mode) or lighter (dark
  mode) shade of it.
- A header background gets white or dark text, whichever contrasts more (at
  least 4.5:1). The muted text is mixed from the two and keeps at least 4.5:1
  on the background; the hover and the highlight of the current page are
  slightly lighter or darker shades of the background, on which the text
  keeps at least 4.5:1 too.

The branding page shows a **preview** of the header, a button, a link and some
text in light and in dark mode, with the saved settings.

The **status colours** – green *operational*, blue *maintenance*, yellow
*degraded*, orange *partial outage*, red *major outage* – cannot be changed:
on the status page and in e-mails they must mean the same to everybody, and
viewers recognise a status by its colour at a glance.

**Saving.** Files are checked by their content, not by their name. SVG files
must be plain images. Their `svg` element needs
`xmlns="http://www.w3.org/2000/svg"`, without which browsers do not show the
file. Files with active content are refused: scripts, event handlers,
embedded documents (e.g. web pages), entity declarations, a document type
declaration with its own definitions (`<!DOCTYPE svg [ … ]>`; a plain
`<!DOCTYPE svg PUBLIC …>` is fine) and processing instructions such as style
sheet links (`<?xml-stylesheet …?>`). Export such a file again as a plain
SVG, or use a PNG. If anything is wrong, nothing is saved and the page shows
what to fix; files must then be chosen again. To remove an image, tick
*Remove* next to it; a newly chosen file replaces the old one.

**The images are public.** Logos and favicon are served at
`/brand/logo-light`, `/brand/logo-dark` and `/brand/favicon` to anybody who
can reach the status page, even when public access is off, because the
sign-in page shows them. Browsers keep them in their cache; a new image gets
a new address, so it appears at once. The images are stored in the database,
so they are part of its [backup](#data-directory-backup-and-upgrade).

**E-mails** use only the light-mode accent colour, for the *View incident*
and *View maintenance* buttons and the *Open the status page* button of the
test e-mail (with the same text colour as the buttons of the status page);
without one the buttons stay blue. Everything else in the e-mails stays as
it is. They contain no logo, because mail clients block or proxy images from
the internet.

**Restore defaults** (below the form, after a confirmation) removes the logos
and the favicon, empties the colours, sets the appearance back to *Follow the
device setting* and turns *Show the site name next to the logo* on again.

## Single sign-on

The redirect (callback) URI to register at the identity provider is always

```
<Base URL>/auth/oidc/callback      e.g. https://status.example.com/auth/oidc/callback
```

In **Admin → Settings → Single sign-on (OpenID Connect)** tick *Enable sign-in
with OpenID Connect* and enter the **issuer URL**, **client ID** and **client
secret** (these three are shown also while sign-in is off, because
[e-mail with Microsoft Graph](#microsoft-365-microsoft-graph) uses them too).
**Scopes** default to `openid profile email`, the **groups claim** to
`groups`; the **button label** is the text of the sign-in button.

**Group memberships at sign-in.** At every sign-in the user's group
memberships are replaced by the groups in the groups claim of the ID token
(or of the userinfo endpoint, if the ID token has no such claim); groups the
status page does not know yet are created automatically. **If the claim is
missing, the user is in no group any more**, so configure the identity
provider to send it (see below). Memberships stay unchanged only when Entra
ID reports that the user has too many groups for the token, or when the
userinfo endpoint cannot be read. Memberships of
[provisioned groups](#how-users-and-groups-get-into-the-status-page) are
never changed by a sign-in. Group IDs that are GUIDs (Entra ID object IDs)
are compared regardless of upper/lower case.

The user's e-mail address is taken from the `email` claim (or
`preferred_username`/`upn` if they contain an address). An address the
provider marks with `email_verified: false` is ignored, so such users get no
notification e-mails. The user name (`preferred_username`, or `upn`) is
stored as well: it identifies a provisioned user at their first sign-in. The
name, e-mail address and user name of provisioned users come from
provisioning, not from the sign-in.

### Microsoft Entra ID

1. In the [Microsoft Entra admin center](https://entra.microsoft.com) open
   **App registrations → New registration**. Name it e.g. "Status Page",
   choose *Accounts in this organizational directory only*, and add the
   redirect URI with platform **Web**:
   `https://status.example.com/auth/oidc/callback`.
2. On the **Overview** page copy the **Application (client) ID** and the
   **Directory (tenant) ID**.
3. **Certificates & secrets → New client secret**; copy the secret's
   **Value** (it is shown only once; note the expiry date).
4. **Token configuration → Add groups claim** → tick **Security groups** →
   for the ID token choose **Group ID** → Add.
5. In the status page settings enter:
   - Issuer: `https://login.microsoftonline.com/<tenant ID>/v2.0`
   - Client ID and client secret from steps 2 and 3
   - Groups claim: `groups`

   Group IDs in Entra ID are object IDs (GUIDs). Provisioning fills in the
   names of the groups it provisions; for other groups you can type a display
   name when you add or edit a group.

This app registration is also used to
[send e-mail with Microsoft Graph](#microsoft-365-microsoft-graph).
[Provisioning](#set-up-provisioning-in-microsoft-entra-id) needs a second,
separate enterprise application.

Users who are members of more than 200 groups get no `groups` claim from
Entra ID (their memberships are then left unchanged at sign-in). Provisioning
keeps the members of the provisioned groups up to date regardless. Without
provisioning, choose *Groups assigned to the application* in step 4 and
assign the relevant groups to the enterprise application of the app
registration; note that this option only sends *direct* memberships, not
nested groups.

### Keycloak

1. In your realm: **Clients → Create client**, type *OpenID Connect*, client ID
   e.g. `statuspage`. Turn **Client authentication** on and keep **Standard
   flow** enabled. **Valid redirect URIs**:
   `https://status.example.com/auth/oidc/callback`.
2. Copy the client secret from the **Credentials** tab.
3. Add the groups to the token: **Clients → statuspage → Client scopes →
   statuspage-dedicated → Configure a new mapper → Group Membership**. Token
   claim name `groups`, **Full group path off**, *Add to ID token* on.
4. In the status page settings enter issuer
   `https://keycloak.example.com/realms/<realm>`, client ID, client secret and
   groups claim `groups`.
5. Make sure the users' e-mail addresses are **verified**: turn on *Email
   verified* for users created in the admin console, and *Trust Email* for an
   LDAP / Active Directory user federation. Users with an unverified address
   get no e-mails.

With this mapper the groups are identified by their names. Provisioning is
made for Microsoft Entra ID; with Keycloak, users are known once they have
signed in.

### Other OpenID Connect providers

Any provider that supports discovery (`<issuer>/.well-known/openid-configuration`)
and the authorization code flow works. Register a confidential client with
the redirect URI above and make the provider put the user's groups into a
claim of the ID token (or the userinfo response); enter that claim's name as
the groups claim. Whatever values appear in the claim (IDs or names) identify
the groups in the status page. If the provider needs an extra scope for
groups, add it to the scopes.

### Local administrator sign-in

The local administrator signs in on the sign-in page; with single sign-on
enabled, its form is folded below the single sign-on button
(`/login?local=1` opens it). The checkbox **Allow sign-in with the local
administrator account** in **Admin → Settings → Single sign-on** (on by
default) turns this sign-in off:

- It can only be turned off while single sign-on is enabled and configured
  (issuer URL, client ID and client secret) and at least one group has the
  Administrator role (**Admin → Groups**); otherwise saving shows an error.
  For the same reason single sign-on cannot be turned off while the local
  sign-in is off.
- While it is off, the sign-in page shows only the single sign-on button,
  `/login?local=1` shows a note instead of the form, local sign-in attempts
  are refused, and sessions of the local administrator stop working at once.
  If you turn it off while signed in as the local administrator, you are
  signed out and the sign-in page tells you why. Turning it on again, in
  Admin → Settings or on the command line, does not bring those sessions
  back: the local administrator signs in again.
- While it is off, the only group with the Administrator role cannot be
  given another role or deleted in **Admin → Groups**; give another group
  the Administrator role first.
- **When single sign-on fails** (identity provider outage, expired client
  secret, no administrator group left), turn the local sign-in on again on
  the server, as administrator or root, e.g.
  `sudo statuspage local-login enable -data /var/lib/statuspage`, or on
  Windows `"C:\Program Files\Status Page\statuspage.exe" local-login enable`
  in an administrator command prompt. The help below the checkbox shows the
  exact command for your server (with its data directory): note it down
  before you turn the local sign-in off. A running status page applies it
  within about 10 seconds, without a restart; then sign in at
  `/login?local=1`. `statuspage local-login status` shows the user name of
  the local administrator account (it may have been changed on the account
  page).

## How users and groups get into the status page

Apart from the local administrator the status page has no accounts of its
own: users and their group memberships come from the identity provider, in
two ways that work together.

1. **SCIM provisioning from Microsoft Entra ID** (recommended with Entra ID).
   Entra ID sends ("provisions") the groups assigned to a provisioning
   enterprise application, with their members, over SCIM (System for
   Cross-domain Identity Management). Everybody in these groups is known –
   and notified – even if they never signed in. Entra ID sends changes about
   every 40 minutes (the interval cannot be changed; *Provision on demand* in
   Entra ID applies a change for one user at once). Users that Entra ID
   deactivates – disabled, deleted or no longer assigned – get no e-mails and
   cannot sign in; users it deletes for good are removed (see
   [What provisioning changes](#what-provisioning-changes)). The members of
   provisioned groups are changed only by provisioning.
2. **Sign-in with the groups claim** (any OpenID Connect provider). At every
   sign-in the groups claim replaces the user's group memberships (see
   [Single sign-on](#single-sign-on)). On its own, it knows (and notifies)
   only users who have signed in at least once, with the groups of their last
   sign-in; people who left stay until an administrator removes them on
   **Admin → Users**.
   With providers other than Entra ID, consider turning public access off so
   that everybody signs in.

With provisioning, sign-ins still update the memberships of all groups that
are not provisioned.

![Provisioning settings](docs/screenshots/settings-provisioning.png)

### Set up provisioning in Microsoft Entra ID

You need the [base URL](#general-settings) set and saved, an HTTPS address
that Entra ID can reach (see [Connectivity](#connectivity)), and Microsoft
Entra ID P1 or P2 to assign groups to an application.

1. **Token.** In the status page open **Admin → Settings**, section *Users
   and groups from Microsoft Entra ID (SCIM provisioning)*. Note the **Tenant
   URL**, `<Base URL>/scim/v2` (e.g. `https://status.example.com/scim/v2`),
   click **Generate token** and copy the token now: it is shown only once.
   The status page keeps only a hash of it; if it is lost, generate a new
   one. The button reloads the page with the saved settings, so save other
   changes first (the page asks before it discards them).
2. **Application.** In the [Microsoft Entra admin center](https://entra.microsoft.com)
   open **Entra ID → Enterprise apps → New application → Create your own
   application**, enter a name (e.g. "Status Page provisioning"), choose
   *Integrate any other application you don't find in the gallery
   (Non-gallery)* and click **Create**. It must be a separate application:
   the app registration used for single sign-on cannot be used for
   provisioning (Entra ID does not offer automatic provisioning for it).
3. **Connection.** In the new application open **Provisioning → New
   configuration**, enter the Tenant URL and the token as **Secret Token**,
   click **Test Connection** (Entra ID looks up a user that does not exist
   and expects an empty answer) and then **Create**. If Entra ID reaches the
   server under another name than the base URL, enter that name followed by
   `/scim/v2` as Tenant URL.
4. **Groups.** Under **Users and groups → Add user/group** assign the groups
   whose members the status page should know, with the provisioning scope
   *Sync only assigned users and groups*. Assigning groups requires Microsoft
   Entra ID P1 or P2. The groups must be security-enabled, and only their
   direct members are provisioned: nested groups are not supported, so
   assign them as well.
5. **Attribute mapping.** Keep the default mappings for users and groups.
   The status page uses:

   | Entra ID | SCIM attribute | Used for |
   |---|---|---|
   | userPrincipalName | `userName` | user name; it identifies the user at their first sign-in |
   | mail | `emails[type eq "work"].value` | the address notifications are sent to |
   | displayName; givenName, surname | `displayName`; `name.givenName`, `name.familyName` | the name shown (the display name, else given name and surname) |
   | not IsSoftDeleted | `active` | false (disabled, deleted or unassigned): no e-mails, no sign-in |
   | mailNickname | `externalId` | stored only |
   | group displayName | `displayName` | group name (unique among provisioned groups) |
   | group objectId | `externalId` | group ID – the same value as in the groups claim; set when the group is created or taken over and never changed by provisioning later (it decides who gets the group's role) |
   | group members | `members` | group members |

   Other attributes (job title, phone numbers, manager, …) are ignored.
   Without a `mail` value, a user name that is an e-mail address is used,
   and it changes with the user name (not for guests, whose user names
   contain `#EXT#`). The Tenant URL flag `?aadOptscim062020` is not needed:
   both styles of requests are understood.
6. **Optional:** test a single user with **Provision on demand**, and under
   **Overview → Properties** enter a notification e-mail address for failures
   and set up the accidental deletion prevention.
7. **Start provisioning.** The first cycle takes a while for large groups;
   then Entra ID sends changes about every 40 minutes. The application's
   **Provisioning logs** in Entra ID show every action and, for a failed
   one, the reason the status page gave; the status page logs the changes in
   `statuspage.log`.
8. **Roles and services.** The provisioned groups appear in **Admin →
   Groups** and in the service form with the badge *Provisioned*, even
   before they have a role or services. Open each one and choose its role
   and the services its members are notified about (see
   [Roles and groups](#roles-and-groups)).

Users and groups the status page knows already are taken over instead of
being created twice: users who signed in before (found by user name, then by
e-mail address), and groups recorded from sign-ins or added under **Admin →
Groups** with the same object ID. Such a group keeps its role and services;
its members are replaced by the provisioned ones. So you can prepare the
roles and services of groups before you start provisioning.

### Connectivity

The Microsoft Entra provisioning service calls the Tenant URL from
Microsoft's cloud:

- over HTTPS (TLS 1.2) with a valid certificate from a public certification
  authority;
- from the IP ranges of the Azure service tag **AzureActiveDirectory** –
  allow them in your firewall;
- publish only the path `/scim/v2/` to the internet, e.g. with a reverse
  proxy that forwards only requests whose path starts with `/scim/v2/`, and
  keep the web interface internal (see [Security notes](#security-notes)).

If the server is in a private network that Entra ID cannot reach, use the
**Microsoft Entra provisioning agent**
([on-premises application provisioning to SCIM apps](https://learn.microsoft.com/entra/identity/app-provisioning/on-premises-scim-provisioning)).
It runs on a Windows Server 2016 or later machine with 3 GB RAM in your
network that can reach the status page, needs only outbound connections, and
requires Microsoft Entra ID P1. In step 2 add the gallery application
**On-premises SCIM app** instead; under **Provisioning** choose **Get
started**, the mode **Automatic**, install and assign the agent under
**On-Premises Connectivity**, and then enter the Tenant URL and the token as
above.

### What provisioning changes

- **Users.** Provisioning sets the user name, name, e-mail address and the
  *active* state; sign-ins no longer change them. At their first sign-in,
  provisioned users are recognised by the user name (`preferred_username`,
  or `upn`; upper and lower case do not matter).
- **Deactivated users** – disabled, deleted or unassigned in Entra ID – get
  no e-mails, are signed out at once and cannot sign in ("Sign-in failed:
  your account has been deactivated by your organisation's user
  provisioning"). Assigning or enabling them again activates them; their
  next sign-in links them again by user name or e-mail address. Users that
  Entra ID deletes for good (30 days after deletion) are removed from the
  status page. Provisioned users cannot be removed on **Admin → Users**:
  Entra ID manages them.
- **Groups.** Only provisioning changes the members of a provisioned group:
  sign-ins neither add nor remove these memberships. The group name comes
  from Entra ID. When Entra ID deletes a group, the group loses its members
  but stays in the status page with its role and services, and gets its
  members back when Entra ID provisions it again.
- **The local administrator** is never visible to provisioning.

### New token, turning provisioning off and on again

- **Generate new token** replaces the token at once, e.g. when the old one
  may have been exposed. Enter the new token as *Secret Token* in the
  provisioning configuration of the Entra application. Reloading the page
  that shows the token does not generate another one, but it no longer
  shows the token either.
- **Turn off provisioning** deletes the token, so the status page refuses
  Entra ID's requests from then on, including those it is processing at
  that moment; stop provisioning in Entra ID as well. All groups and users
  stop being provisioned: sign-ins manage group memberships, names and
  e-mail addresses again, and existing memberships stay until the user's
  next sign-in. Deactivated users stay deactivated; remove them on
  **Admin → Users** if they should be able to sign in again.
- **Turning provisioning on again:** generate a token, enter it as *Secret
  Token* in the Entra application and then use **Restart provisioning** on
  the application's provisioning overview, so that Entra ID sends all
  users, groups and members again. Without a restart, the groups that were
  provisioned before stay incomplete: Entra ID sends only changes.

## Roles and groups

Roles are given to identity provider groups, not to individual users:

| Role          | Can                                                                 |
|---------------|---------------------------------------------------------------------|
| Viewer        | see the status pages (every signed-in user)                         |
| Editor        | additionally create and update incidents and maintenance windows (`/manage`) |
| Administrator | additionally change settings, services and groups, see users and notifications, delete incidents and maintenance windows |

A user's role is the highest role of their groups. **Admin → Groups** lists
the groups that have a role or services, and all provisioned groups (badge
*Provisioned*); the other groups – usually recorded from the members'
sign-ins – are in the collapsed list *Other groups*. Open a group to choose
its role and the services its members are notified about. You can also add a
group in advance by its ID as it appears in the groups claim (Entra ID object
ID, Keycloak group name); provisioning takes such a group over.

![Group page](docs/screenshots/admin-group.png)

**Admin → Users** lists the known users with their groups, role, sign-in
(*SSO*, *Never signed in* or *Local administrator*) and last sign-in, marked
*Provisioned* or *Deactivated* where it applies. Administrators can remove a
user who is not provisioned, e.g. someone who left the company, to stop their
e-mails; a removed user comes back at their next sign-in. Provisioned users
are managed in Microsoft Entra ID and cannot be removed here (*Managed in
Entra ID*): Entra ID deactivates them when they are disabled, deleted or
unassigned, and removes them when it deletes them for good. The local
administrator cannot be removed.

## Services and notifications

Create services in **Admin → Services**: name, description, category, sort
order and the **groups to notify** (or assign services on a group's page).
For the **category** choose *No category*, an existing category or *New
category…* and enter its name. Services with the same category are shown
together on the status page (upper and lower case do not matter), ordered by
the sort order.

When an editor creates or updates an incident or a maintenance window and
ticks *notify*, the known members of the groups assigned to the affected
services who have an e-mail address receive an e-mail; deactivated users do
not. Maintenance windows with notifications enabled also send an e-mail when
they are cancelled, when they start and when they are completed; a window
entered when it has already started is announced with the "started" e-mail.
Users see on their account page which services they are notified about.
Who is known depends on
[how users get into the status page](#how-users-and-groups-get-into-the-status-page):
with provisioning, all members of the provisioned groups are known; without
it, only users who have signed in at least once.

While [e-mail](#e-mail) is configured, e-mails are queued in the database and
delivered in the background with the chosen method (while it is not, editors
are told that no e-mails were sent). A failed delivery is retried after
1 minute, 5 minutes, 15 minutes and 1 hour; after that the e-mail is marked
as *failed*. When the
server throttles the delivery, it pauses (SMTP: 1 minute; Microsoft Graph: as
long as it asks) and the e-mails that were not tried yet wait without using
up their retries. When the server cannot be used at all (unreachable, sign-in
or access token rejected, permission missing), every e-mail of the batch
being sent (up to 50) counts a failed attempt, so e-mails are marked as
*failed* after about 80 minutes of outage: outdated status e-mails are of
little use. An e-mail the server has accepted is never sent again.
**Admin → Notifications** shows the latest 200 e-mails with their delivery
status, attempts and last error. Sent and failed e-mails are deleted after
90 days.

## E-mail

In **Admin → Settings → E-mail**, *Send notifications with* offers
**Microsoft 365 (Microsoft Graph) — recommended**, the default, and **SMTP
server**. Only the fields of the chosen method are shown and checked; the
values of the other method are kept. An empty sender mailbox (Microsoft
Graph) or an empty SMTP server turns e-mail off. **Send test e-mail** (below
the settings) uses the saved settings and shows the error in plain words if
it fails.

**Upgrade:** an installation that already had an SMTP server configured keeps
sending with SMTP after the upgrade; new installations start with Microsoft
Graph.

### Microsoft 365 (Microsoft Graph)

Microsoft Graph uses the [single sign-on app registration](#microsoft-entra-id):
its issuer (`https://login.microsoftonline.com/<tenant ID>/v2.0`), client ID
and client secret from the single sign-on settings – sign-in itself does not
have to be enabled (the three fields are shown also while it is off). With
another identity provider, use SMTP.

1. **Sender mailbox:** a user or shared mailbox in Exchange Online, e.g.
   `status@example.com` (a shared mailbox needs no licence). E-mails are sent
   from it; non-delivery reports and out-of-office replies arrive there.
2. **Permission**, one of:
   - **Mail.Send:** in the app registration open **API permissions → Add a
     permission → Microsoft Graph → Application permissions**, add
     **Mail.Send** and click **Grant admin consent for &lt;your
     organization&gt;**. The application can then send as any mailbox of
     your organisation.
   - **RBAC for Applications** (recommended): allow only the sender mailbox,
     see below.
3. Enter the sender mailbox in **Admin → Settings → E-mail**, save and send a
   test e-mail. A test e-mail always requests a new access token, so it never
   uses one from before a permission change.

**Allow sending from the sender mailbox only.** Grant the permission with
Exchange Online *RBAC for Applications* instead of Mail.Send in Entra ID. In
Exchange Online PowerShell (`Connect-ExchangeOnline`, as Exchange
Administrator or member of Organization Management), with the **Application
(client) ID** and the **Object ID** from **Entra ID → Enterprise apps → the
status page application** (not from App registrations, whose Object ID is a
different one):

```powershell
New-ServicePrincipal -AppId <Application (client) ID> -ObjectId <Object ID of the enterprise application (service principal)> -DisplayName "Status Page"
New-ManagementScope -Name "Status Page sender" -RecipientRestrictionFilter "PrimarySmtpAddress -eq 'status@example.com'"
New-ManagementRoleAssignment -App <Application (client) ID> -Role "Application Mail.Send" -CustomResourceScope "Status Page sender"
Test-ServicePrincipalAuthorization -Identity <Application (client) ID> -Resource status@example.com
```

The test shows `InScope` `True` for *Application Mail.Send*. **Do not also
grant Mail.Send in Entra ID**, and remove it if it was granted: the
permissions of both are combined, and an unscoped Entra ID grant makes the
restriction ineffective. Changes take 30 minutes to 2 hours to apply.
(Application Access Policies are the older way to do this.)

How Microsoft Graph delivers:

- Every e-mail is a separate request (`sendMail`) with the HTML body; no copy
  is kept in *Sent Items*. *Sent* means that Microsoft 365 accepted the
  e-mail (HTTP 202); problems after that arrive as non-delivery reports in
  the sender mailbox.
- Exchange Online allows a mailbox 10,000 recipients per 24 hours. The limit
  of 30 messages per minute applies to SMTP, not to Microsoft Graph.
- **Throttled** (HTTP 429, or 503/504 when Microsoft Graph is busy): the
  delivery pauses as long as Microsoft Graph asks (*Retry-After*; 1 minute if
  it does not say, at most 10 minutes), and the e-mails wait without using up
  their retries.
- **Microsoft Graph cannot be used** – no access token (wrong issuer, client
  ID or client secret, expired secret), HTTP 401, 403 (permission missing),
  404 (unknown sender or no Exchange Online mailbox), unreachable or a server
  error: every e-mail of the batch counts one failed attempt, as with an
  unreachable SMTP server. The error message says what to check: it names
  Microsoft Entra ID (`login.microsoftonline.com`) when no access token
  could be obtained, and Microsoft Graph (`graph.microsoft.com`) when
  sending failed.
- An e-mail Microsoft Graph rejects on its own (e.g. an invalid recipient
  address, HTTP 400) fails alone and is retried like any other.
- The server needs outbound HTTPS to `login.microsoftonline.com` and
  `graph.microsoft.com` (Microsoft's global cloud). A proxy can be set with
  the environment variable `HTTPS_PROXY`; for the Windows service as a
  system environment variable, which takes effect after a reboot.

### SMTP server

Enter the SMTP server, port (empty: 587 for STARTTLS, 465 for TLS, 25 without
encryption), security (*STARTTLS*, *TLS* or *none*), optional user name and
password, and the sender, e.g. `IT Status <status@example.com>`. When the
server throttles the delivery (a temporary error) the delivery pauses for a
minute; when it closes the connection, the status page reconnects at once.

For Microsoft 365 prefer Microsoft Graph. With SMTP AUTH: server
`smtp.office365.com`, port `587`, security *STARTTLS*, user name and password
of the sending mailbox, sender = that mailbox's address; SMTP AUTH must be
enabled for the mailbox. Microsoft limits SMTP AUTH to **30 messages per
minute**, so e-mails to a large group go out over several minutes, and it is
retiring basic authentication for SMTP AUTH. For large groups, or if basic
authentication is disabled in your tenant, use Microsoft Graph, an internal
SMTP relay or an Exchange Online connector (relay) instead.

## config.json

`config.json` in the [data directory](#data-directory-backup-and-upgrade)
holds the settings needed before the database can be reached. It is written
by the installer, the web setup or `statuspage setup`; edit it by hand only
for the listen address, TLS and the log level, and restart the service
afterwards (`statuspage service restart`).

```json
{
  "listen": ":8080",
  "db_driver": "sqlite",
  "db_dsn": "statuspage.db",
  "secret_key": "…generated…",
  "tls_cert": "status.pem",
  "tls_key": "status-key.pem",
  "log_level": "info"
}
```

| Field | Meaning |
|-------|---------|
| `listen` | address of the web server, e.g. `:8080` (all interfaces) or `127.0.0.1:8080`; default `:8080`. `statuspage run -listen` overrides it for one run. |
| `db_driver`, `db_dsn` | `sqlite` and the database file (relative to the data directory), or `postgres` and a connection URL. Change them with `statuspage setup`. |
| `secret_key` | generated; encrypts the OIDC client secret (also used for Microsoft Graph) and the SMTP password in the database. Keep it with the database backup – without it these secrets must be entered again. |
| `tls_cert`, `tls_key` | optional PEM certificate and key, absolute or relative to the data directory. When both are set the server speaks HTTPS itself; setting only one is an error. |
| `log_level` | optional: `debug`, `info` (default), `warn` or `error`. The environment variable `STATUSPAGE_LOG_LEVEL` overrides it (it does not reach the Windows service until the next reboot, so use `log_level` there). |

A UTF-8 byte order mark (as written by Notepad or Windows PowerShell 5) is
accepted.

## PostgreSQL

The status page creates and upgrades its tables itself, but the database must
exist. For example, as a PostgreSQL superuser:

```sql
CREATE ROLE statuspage LOGIN PASSWORD 'change-me';
CREATE DATABASE statuspage OWNER statuspage ENCODING 'UTF8';
```

The user must be allowed to create tables in the database's default schema;
being the owner of the database is enough (PostgreSQL 15+ no longer allows
this for other users by default). Also make sure `pg_hba.conf` allows
connections from the status page server.

The connection is entered in the Windows installer, the web setup or with
`statuspage setup -db postgres …`, and stored in `config.json`. SSL modes:
`disable`, `allow`, `prefer` (default), `require`, `verify-ca`,
`verify-full`.

To change the connection later (e.g. a new password), run `statuspage setup`
again with all PostgreSQL options; the secret key and the other settings in
`config.json` are kept. Then restart the service.

## Data directory, backup and upgrade

Default location: `C:\ProgramData\StatusPage` on Windows, otherwise `data`
next to the executable. Override with `-data DIR` or the environment variable
`STATUSPAGE_DATA`.

| File | Content |
|------|---------|
| `config.json` | listen address, database connection, secret key, optional TLS certificate and log level ([details](#configjson)) |
| `statuspage.db` (+ `-wal`, `-shm`) | SQLite database |
| `statuspage.log`, `statuspage.log.1` | log file (rotated at start when larger than 10 MB) |
| `setup-token.txt` | setup token, only until the setup is complete |
| `setup-listen.txt` | port for setup mode (and for `statuspage setup` without `-listen`), only if the database setup in the installer failed |

**Backup.** Back up `config.json` and the database:
- SQLite: stop the service and copy `statuspage.db`, or copy it online with
  `sqlite3 statuspage.db ".backup backup.db"`.
- PostgreSQL: `pg_dump`.

**Upgrade.** Make a backup, then:
- Windows: run the new installer.
- Linux/macOS: replace the binary and run
  `statuspage service restart -data /var/lib/statuspage`.

Database changes are applied automatically at start. An installation that
sends e-mail with SMTP keeps doing so (see [E-mail](#e-mail)).

## Command line reference

```
statuspage [run] [-data DIR] [-listen ADDR]
    Run the server (default command). Started by the Windows service manager
    or systemd it runs as a service. -listen overrides "listen" in
    config.json for this run.

statuspage setup [-data DIR] (-params-file FILE | -db sqlite|postgres ...) [-check-only] [-result-file FILE]
    Test the database connection, create the tables and write config.json.
    Run again on an existing installation, it keeps the secret key, the TLS
    and log settings and, unless -listen is given, the listen address.
      -db sqlite|postgres     database type
      -sqlite-path FILE       SQLite file (default statuspage.db in the data directory)
      -pg-host HOST           default localhost
      -pg-port PORT           default 5432
      -pg-database NAME       default statuspage (must exist)
      -pg-user NAME           default statuspage
      -pg-password PASSWORD   or environment variable STATUSPAGE_PG_PASSWORD
      -pg-sslmode MODE        disable, allow, prefer (default), require, verify-ca, verify-full
      -listen ADDR            web server address (default: the address in an existing
                              config.json, else the port in setup-listen.txt left by
                              the installer, otherwise :8080)
      -params-file FILE       read the settings from a JSON file instead (used by the installer)
      -check-only             only test, write nothing
      -result-file FILE       write "OK" or the error message to FILE

statuspage service install|uninstall|start|stop|restart|status [-data DIR]
    Manage the "Status Page" Windows service / systemd unit (needs
    administrator or root rights). The service runs "statuspage run -data DIR".
      install    registers the service; on Windows an existing StatusPage
                 service is changed to run this program with DIR
      start, restart
                 wait up to 15 seconds until the status page answers (another
                 program on the port does not count) and fail (with the log
                 file name) if it does not or the service stopped
      status     prints running, stopped or not installed

statuspage reset-password -username NAME [-data DIR]
    Set a new password for a local administrator account and sign out all
    its sessions. The password is read from standard input (asked twice,
    hidden, in a terminal). If there is no local account named NAME, the
    error names the local administrator account. If the sign-in with the
    local administrator account is turned off, it reminds you to turn it on
    with "statuspage local-login enable".

statuspage local-login enable|disable|status [-data DIR]
    Turn the sign-in with the local administrator account on or off, or show
    whether it is on and the user name of the local administrator account.
    "enable" always works: use it when single sign-on is unavailable.
    "disable" works only while single sign-on is enabled and configured and
    at least one group has the Administrator role. Changing it signs out all
    sessions of the local administrator account. A running status page
    applies the change within about 10 seconds; no restart is needed.

statuspage version
```

`reset-password` and `local-login` work directly on the database: run them on
the server, as administrator or root, with the same `-data` as the service
(in a data directory without `config.json`, e.g. the default one when the
service uses another, they say so and ask for `-data`). Write the action of
`local-login` first: `statuspage local-login enable -data DIR`.

Exit codes: 0 on success (also when `local-login` finds the setting as
requested already), 1 on an error (e.g. `local-login disable` refused), 2 for
an invalid command line (unknown command or option, missing or unknown
action; `setup` then writes no result file).

Examples:

```sh
echo 'new-long-password' | statuspage reset-password -username it-admin -data /var/lib/statuspage
statuspage local-login status -data /var/lib/statuspage
```

## Security notes

- **Use HTTPS.** Put the status page behind a reverse proxy that terminates
  TLS (IIS, nginx, Caddy, …) and forwards `X-Forwarded-Proto: https`, or set
  `tls_cert`/`tls_key` in `config.json`. Set the **Base URL** to the HTTPS
  address. Only expose the HTTP port to the proxy.
- **Setup token.** Until the local administrator exists, `/setup` can only be
  used from the server itself or with the token from `setup-token.txt`.
  Requests with proxy headers (`X-Forwarded-For`, `Forwarded`, `X-Real-IP`)
  always need the token; a reverse proxy on the same server that sends none
  of them looks like a local request, so finish the setup before publishing
  the page through it.
- **Local administrator.** Use a long password and keep it for emergencies;
  daily work should use single sign-on. Reset it with
  `statuspage reset-password`. Failed local sign-ins are limited to 10 per
  15 minutes per client address and user name; after that, the name is
  refused from that address for up to 15 minutes, even with the right
  password (restarting the service clears the counter). Behind a reverse
  proxy everyone has the proxy's address, so anybody who knows the
  administrator's user name can lock it out: choose a name that is not easy
  to guess (not `admin`). Once single sign-on works and a group has the
  Administrator role, consider [turning the local sign-in off](#local-administrator-sign-in);
  anybody who can run `statuspage local-login enable` on the server with
  access to the data directory can turn it on again.
- **Provisioning token.** The token allows creating, changing and deleting
  users and the members of provisioned groups – and with that the roles of
  users – so treat it like an administrator password. The status page shows
  it only once and stores only its SHA-256 hash; requests with another token
  are refused and logged. Generate a new token if it may have been exposed,
  and turn provisioning off when you no longer use it: without a token the
  endpoint refuses every request.
- **Exposing provisioning.** If Entra ID must reach the status page from the
  internet, publish only paths starting with `/scim/v2/` (e.g. a reverse
  proxy rule), only for the IP ranges of the AzureActiveDirectory service
  tag, and keep the web interface internal. The provisioning endpoint uses
  only its token: no cookies, no sessions.
- **Logos and favicon** (`/brand/…`) can be read without signing in, even
  when public access is off (see [Branding](#branding)), so upload nothing
  confidential. Only images are accepted (SVG files only without scripts,
  event handlers or other active content), and SVG files are served with a
  Content Security Policy that sandboxes them
  (`default-src 'none'; style-src 'unsafe-inline'; sandbox`): even one opened
  directly in the browser cannot run scripts.
- **Data directory.** `config.json` contains the secret key and, for
  PostgreSQL, the database password. On Windows the installer and
  `statuspage.exe` itself (server, `setup` and `service install`) limit the
  folder to SYSTEM and Administrators – plus the current user when it runs
  without administrator rights, e.g. a portable copy. On Linux the folder is
  created with mode 0700; keep it readable by root (or the service user)
  only.
- Sessions expire after 12 hours; all forms are protected against CSRF; the
  local password is stored with bcrypt; secrets in the database are
  encrypted.

## Building from source

Requires Go (see `go.mod`); no C compiler is needed.

```sh
go build -o dist/statuspage ./cmd/statuspage          # this platform
make build-windows                                     # dist/statuspage.exe
make test                                              # go vet + go test
make installer                                         # dist/StatusPageSetup-<version>.exe
```

`make installer` uses the Docker image `amake/innosetup` (or `iscc` from
[Inno Setup](https://jrsoftware.org/isinfo.php) 6.3+ on the PATH). On
Windows: `iscc /DAppVersion=1.2.3 installer\statuspage.iss`. The version is
set with `-ldflags "-X main.version=1.2.3"`.

PostgreSQL tests run when `STATUSPAGE_TEST_PG_DSN` is set, e.g.
`postgres://statuspage:statuspage@localhost:5432/statuspage_test?sslmode=disable`
(the tests drop and recreate the `public` schema of that database; run them
with `go test -p 1 ./...`, as `make test` does, because the packages share
the database). The end-to-end tests in `internal/e2e` run the whole
application like a browser – setup wizard, single sign-on against a mock
OpenID provider, SCIM provisioning the way Microsoft Entra ID sends it,
e-mails to a fake SMTP server and to a fake Microsoft Graph, restart – with
SQLite and, if set, PostgreSQL (the Microsoft Graph journey with SQLite
only), and smoke-test the command line tool.

`go run ./tools/devseed` prepares a data directory with sample data for
development (see the comment in `tools/devseed/main.go`).

GitHub Actions (`.github/workflows/build.yml`) runs the tests, builds binaries
for Windows, Linux (amd64, arm64) and macOS (arm64) and the installer, and
publishes them as a release for tags `v*`; the release notes start with the
version's section of [`CHANGELOG.md`](CHANGELOG.md). To release without
pushing a tag, run the workflow by hand (**Actions → build → Run workflow**)
on `main` with the version, e.g. `1.0.0`: it creates the tag `v1.0.0` on that
commit and the release.
