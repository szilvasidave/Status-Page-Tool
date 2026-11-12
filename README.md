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
  setup and emergencies.
- **Groups come from the identity provider.** Assign groups to services and
  their members are e-mailed when an incident or maintenance affects the
  service. With Microsoft Entra ID, group members can optionally be synced
  hourly, so people are notified even if they never signed in.
- **Roles per group:** *Viewer* (every signed-in user), *Editor* (posts
  incidents and maintenance windows) and *Administrator* (configuration).
- **Storage:** built-in SQLite or PostgreSQL.
- All configuration is done in the web interface by administrators.

It is deliberately simple: no approval workflows, no plugins, e-mail is the
only notification channel.

## Contents

- [Quick start](#quick-start)
- [First-run setup](#first-run-setup)
- [Using the status page](#using-the-status-page)
- [Configuration](#configuration)
- [PostgreSQL](#postgresql)
- [Data directory, backup and upgrade](#data-directory-backup-and-upgrade)
- [Command line reference](#command-line-reference)
- [Security notes](#security-notes)
- [Building from source](#building-from-source)

## Quick start

### Windows (installer)

1. Download `StatusPageSetup-<version>.exe` from the releases page and run it
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
all data in `C:\ProgramData\StatusPage` (readable only by SYSTEM and
Administrators) and registers the Windows service **Status Page**
(`StatusPage`, automatic delayed start, restarted automatically if it fails).

Unattended installation: `StatusPageSetup-<version>.exe /VERYSILENT` installs
with SQLite on port 8080.

Running the installer again on a machine that already has a configuration
performs an **upgrade**: it stops the service, replaces the program and starts
the service again. Database changes are applied automatically at start.

Uninstalling removes the service and the firewall rule but **keeps the data
directory**; delete `C:\ProgramData\StatusPage` yourself if you no longer
need it.

### Windows (portable exe)

Double-click `statuspage.exe` (or run it in a command prompt). It starts in
setup mode, opens `http://localhost:8080/setup` in your browser and stores its
data in `C:\ProgramData\StatusPage`. Use `statuspage.exe -data .\data` to keep
the data next to the exe and `-listen :9090` for another port. Press Ctrl+C to
stop it. Install it as a service later with
`statuspage.exe service install` (in an administrator command prompt).

### Linux (systemd)

```sh
sudo install -m 755 statuspage-linux-amd64 /usr/local/bin/statuspage
sudo statuspage service install -data /var/lib/statuspage
sudo statuspage service start
```

This creates and starts the systemd unit `StatusPage.service`, which runs
`statuspage run -data /var/lib/statuspage` as root and restarts it if it
stops. Logs go to `/var/lib/statuspage/statuspage.log` and to the journal
(`journalctl -u StatusPage`).

Without `-data` the data directory is `data` next to the binary. The same
commands work on macOS (launchd).

Then open `http://<server>:8080/setup`. When the setup page is opened from
another computer it asks for the **setup token** from
`/var/lib/statuspage/setup-token.txt` (also shown in the log when it is
created). You can
also configure the database on the command line first, e.g.
`sudo statuspage setup -data /var/lib/statuspage -db sqlite`.

## First-run setup

`/setup` is a short wizard:

1. **Database** – only if it was not configured by the installer or
   `statuspage setup`: choose SQLite or PostgreSQL. The tables are created
   immediately.
2. **Local administrator** – user name and password (at least 10
   characters), plus site name, base URL and time zone. This account always
   has the Administrator role. It is meant for the initial configuration and
   as a break-glass login when single sign-on is unavailable.

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
- **Editors** use **Manage** (`/manage`): *Report incident* (title, impact per
  affected service, status, message), then post updates on the incident's
  page until it is *Resolved*; *Schedule maintenance* (title, description,
  start and end, affected services). Maintenance windows can be edited or
  cancelled until they are over and ended early while they are in progress.
  With *Send e-mail notifications* ticked, the members of the groups assigned
  to the affected services are e-mailed (see
  [Services and notifications](#services-and-notifications)).
  Administrators can also delete incidents and maintenance windows that were
  created by mistake.
- **Your account** (click your name in the header) shows your role, your
  groups from the identity provider and the services you receive e-mails
  about. The local administrator changes its password there.

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
upcoming (`state` `in_progress` or `scheduled`) are listed. Times are UTC (RFC 3339); the URLs use the configured base URL.

## Configuration

Everything below is done in the web interface by administrators (`/admin`).
Only the database connection lives in `config.json` (see
[Data directory](#data-directory-backup-and-upgrade)).

### General settings (Admin → Settings)

- **Site name** – shown in the header and in e-mails.
- **Base URL** – the address users open, without trailing slash, e.g.
  `https://status.example.com`. It is used for the OIDC redirect URI and for
  links in e-mails, so set it before configuring single sign-on.
- **Public access** – when on, the status pages can be viewed without signing
  in; when off, everybody has to sign in.
- **Time zone** – times are shown and entered in this time zone.

### Single sign-on with OpenID Connect

The redirect (callback) URI to register at the identity provider is always

```
<Base URL>/auth/oidc/callback      e.g. https://status.example.com/auth/oidc/callback
```

In **Admin → Settings** enable OpenID Connect and enter the **issuer URL**,
**client ID** and **client secret**. **Scopes** default to
`openid profile email`, the **groups claim** to `groups` (the ID token claim
that lists the user's groups). At every sign-in the user's group memberships
are replaced by the groups in that claim; unknown groups are created
automatically. The user's e-mail address is taken from the `email` claim (or
`preferred_username`/`upn` if they contain an address); an address the
provider marks with `email_verified: false` is ignored, so such users get no
notification e-mails.

#### Microsoft Entra ID

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

   Group IDs in Entra ID are object IDs (GUIDs); the group names are filled in
   by the directory sync, or you can type a display name when you add or edit
   a group.

Users who are members of more than 200 groups get no `groups` claim from
Entra ID. In that case choose *Groups assigned to the application* in step 4
(and assign the relevant groups to the enterprise application), and/or enable
the directory sync.

**Optional: directory sync.** To notify members who never signed in, and to
search groups by name when adding them:

1. In the app registration open **API permissions → Add a permission →
   Microsoft Graph → Application permissions** and add
   **GroupMember.Read.All** and **User.ReadBasic.All**.
2. Click **Grant admin consent for <your organization>**.
3. Enable **directory sync** in Admin → Settings. It uses the same client ID
   and secret. The members of all groups that have a role or a service
   assigned are synchronised hourly; **Admin → Groups** also has a button to
   sync now.

#### Keycloak

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

With this mapper the groups are identified by their names.

#### Other OpenID Connect providers

Any provider that supports discovery (`<issuer>/.well-known/openid-configuration`)
and the authorization code flow works. Register a confidential client with
the redirect URI above and make the provider put the user's groups into a
claim of the ID token; enter that claim's name as the groups claim. Whatever
values appear in the claim (IDs or names) identify the groups in the status
page. If the provider needs an extra scope for groups, add it to the scopes.

### Roles and groups (Admin → Groups)

Roles are given to identity provider groups, not to individual users:

| Role          | Can                                                                 |
|---------------|---------------------------------------------------------------------|
| Viewer        | see the status pages (every signed-in user)                         |
| Editor        | additionally create and update incidents and maintenance windows (`/manage`) |
| Administrator | additionally change settings, services and groups, see users and notifications, delete incidents and maintenance windows |

A user's role is the highest role of their groups. Groups appear in the list
after a member has signed in (or after a directory sync); you can also add a
group in advance by its ID as it appears in the groups claim (Entra ID object
ID, Keycloak group name). **Admin → Users** lists the known users (signed in
or synced from the directory) with their groups and resulting role.

![Group settings](docs/screenshots/admin-group.png)

### Services and notifications

Create services in **Admin → Services** (name, description, optional
category and sort order) and choose the **groups to notify** for each service
(or assign services on a group's page). When an editor creates or updates an
incident or a maintenance window and ticks *notify*, every member of the
groups assigned to the affected services who has an e-mail address receives
an e-mail. Maintenance windows with notifications enabled also send an e-mail
when they are cancelled, when they start and when they are completed. Users see on their account page which services they are notified
about.

E-mails are queued in the database and delivered in the background. A failed
delivery is retried after 1 minute, 5 minutes, 15 minutes and 1 hour; after
that the e-mail is marked as *failed*. **Admin → Notifications** shows the
latest e-mails with their delivery status and errors. Sent and failed e-mails
are deleted after 90 days.

### E-mail (SMTP)

In **Admin → Settings** enter the SMTP server, port, security (*STARTTLS*,
*TLS* or *none*), optional user name and password, and the sender, e.g.
`IT Status <status@example.com>`. Use **Send test e-mail** to check it.

Microsoft 365 / Office 365: server `smtp.office365.com`, port `587`, security
*STARTTLS*, user name and password of the sending mailbox, sender = that
mailbox's address. SMTP AUTH must be enabled for the mailbox. Microsoft is
retiring basic authentication for SMTP AUTH; if it is disabled in your tenant,
use an internal SMTP relay or an Exchange Online connector instead.

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

SSL modes: `disable`, `allow`, `prefer` (default), `require`, `verify-ca`,
`verify-full`.

The connection is entered in the Windows installer, the web setup or with
`statuspage setup -db postgres …`, and stored in `config.json`.

## Data directory, backup and upgrade

Default location: `C:\ProgramData\StatusPage` on Windows, otherwise `data`
next to the executable. Override with `-data DIR` or the environment variable
`STATUSPAGE_DATA`.

| File | Content |
|------|---------|
| `config.json` | listen address, database connection, secret key, optional TLS certificate |
| `statuspage.db` (+ `-wal`, `-shm`) | SQLite database |
| `statuspage.log`, `statuspage.log.1` | log file (rotated at start when larger than 10 MB) |
| `setup-token.txt` | setup token, only until the setup is complete |

`config.json`:

```json
{
  "listen": ":8080",
  "db_driver": "sqlite",
  "db_dsn": "statuspage.db",
  "secret_key": "…generated…",
  "tls_cert": "C:\\certs\\status.pem",
  "tls_key": "C:\\certs\\status-key.pem"
}
```

`tls_cert`/`tls_key` are optional: when both are set (absolute paths to PEM
files) the server speaks HTTPS itself. The **secret key** encrypts the OIDC
client secret and the SMTP password in the database – keep it together with
the database backup. Changes to `config.json` need a service restart. The log
level can be set with the environment variable `STATUSPAGE_LOG_LEVEL`
(`debug`, `info`, `warn`, `error`).

**Backup.** Back up `config.json` and the database:
- SQLite: stop the service and copy `statuspage.db`, or copy it online with
  `sqlite3 statuspage.db ".backup backup.db"`.
- PostgreSQL: `pg_dump`.

**Upgrade.** Make a backup, then:
- Windows: run the new installer.
- Linux/macOS: replace the binary and run `statuspage service restart`.

Database changes are applied automatically at start.

## Command line reference

```
statuspage [run] [-data DIR] [-listen ADDR]
    Run the server (default command). Started by the Windows service manager
    or systemd it runs as a service.

statuspage setup [-data DIR] (-params-file FILE | -db sqlite|postgres ...) [-check-only] [-result-file FILE]
    Test the database connection, create the tables and write config.json.
      -db sqlite|postgres     database type
      -sqlite-path FILE       SQLite file (default statuspage.db in the data directory)
      -pg-host, -pg-port, -pg-database, -pg-user, -pg-sslmode
      -pg-password            or environment variable STATUSPAGE_PG_PASSWORD
      -listen ADDR            web server address (default :8080)
      -params-file FILE       read all of the above from a JSON file instead
      -check-only             only test, write nothing
      -result-file FILE       write "OK" or the error message to FILE
    Exit code 0 on success, 1 on error.

statuspage service install|uninstall|start|stop|restart|status [-data DIR]
    Manage the "Status Page" Windows service / systemd unit (needs
    administrator or root rights). The service runs "statuspage run -data DIR".

statuspage reset-password -username NAME [-data DIR]
    Set a new password for a local administrator account. The password is
    read from standard input (asked twice, hidden, in a terminal).

statuspage version
```

Example: `echo 'new-long-password' | statuspage reset-password -username admin -data /var/lib/statuspage`

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
  `statuspage reset-password`.
- **Data directory.** `config.json` contains the secret key and, for
  PostgreSQL, the database password. The Windows installer limits the folder
  to SYSTEM and Administrators; on Linux keep it readable by root (or the
  service user) only.
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
(the tests drop and recreate the `public` schema of that database).

GitHub Actions (`.github/workflows/build.yml`) runs the tests, builds binaries
for Windows, Linux (amd64, arm64) and macOS (arm64) and the installer, and
publishes them as a release for tags `v*`.
