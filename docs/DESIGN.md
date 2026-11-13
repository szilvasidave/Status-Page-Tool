# Status Page — design notes

A deliberately small status page for internal IT services. One self-contained
executable (`statuspage.exe`), no external runtime dependencies.

## Requirements (from the product owner)

1. Simply installable with an exe (Windows installer that registers a service).
2. Login via OpenID Connect.
3. Groups and their members originate from the identity provider (IdP).
   Groups can be assigned to services; individual members of those groups get
   notified (e-mail) when something happens to the service.
4. A role for people who post status updates / maintenance windows (Editor).
5. An Admin role; all configuration is done in the web UI by admins.
6. Storage in local SQLite, or PostgreSQL. With PostgreSQL the installer asks
   for the credentials and creates the tables.
7. Keep it simple: no review workflows, no extensive features.

## Architecture

```
cmd/statuspage         CLI entry point: run (default), setup, service install/…, reset-password, version
internal/app           lifecycle: setup mode → normal mode, TLS, background workers
internal/config        config.json (listen, db driver/dsn, secret key, optional TLS, log level),
                       data directory creation and permissions (icacls on Windows)
internal/setup         database setup shared by the CLI/installer and the web wizard:
                       connection and privilege checks, migrations, writing config.json
internal/db            database open (SQLite via modernc.org/sqlite, PostgreSQL via pgx), schema migrations
internal/store         all application SQL; models; pure status computation (status.go)
internal/secret        AES-GCM encryption of stored secrets and cookies, random tokens
internal/auth          sessions, CSRF, local admin login, OIDC login + group memberships at login
internal/notify        e-mail outbox, rendering, SMTP delivery (go-mail), maintenance start/end mails
internal/directory     optional Microsoft Entra ID group member sync via Microsoft Graph
internal/web           HTTP handlers + html/template templates + static assets (embedded)
internal/e2e           end-to-end tests: the real app driven like a browser, plus the CLI
internal/testutil      test helpers: mock OpenID provider (mockidp), fake SMTP server (fakesmtp)
tools/devseed          development only: data directory with sample data, sessions for test users
installer/             Inno Setup script → StatusPageSetup.exe
```

SQL outside `internal/store` is limited to the migrations (`internal/db`),
the connection and privilege checks of `internal/setup` and `tools/devseed`.

## Key decisions

* **Single binary**, templates and static files embedded with `embed`. Pure Go
  (no CGO) so `GOOS=windows go build` works anywhere.
* **Configuration split**: `config.json` in the data directory holds what is
  needed before the database can be reached: listen address, optional TLS
  certificate/key (paths relative to the data directory), database
  driver/DSN, secret key and log level. These are not editable in the web
  UI. Everything else (site name, OIDC, SMTP, …) lives in the `settings`
  table and is edited at `/admin/settings`. Re-running `statuspage setup`
  only replaces the database settings (and the listen address if `-listen`
  is given).
* **Data directory**: `%ProgramData%\StatusPage` on Windows, `./data` next to
  the binary elsewhere; override with `-data` or `STATUSPAGE_DATA`. It holds
  secrets, so `config.EnsureDataDir` (used by `app.Run`, `setup.Apply` and
  `service install`) creates it with mode 0700 and, on Windows, replaces its
  inherited ACL with SYSTEM + Administrators (+ the current user when it runs
  without administrator rights) via `icacls`, like the installer.
* **First run**: without `config.json` the server starts in *setup mode* and
  serves only `/setup` (choose SQLite or PostgreSQL, test connection, create
  tables). The Windows installer performs this step itself (custom wizard
  pages) by calling `statuspage.exe setup …`; if that fails it writes the
  chosen port to `<data>/setup-listen.txt`, which setup mode listens on and
  `statuspage setup` without `-listen` writes to `config.json`
  (`config.SetupListen`).
  Afterwards `/setup` asks for the local administrator account (break-glass
  login, also used to configure OIDC). Opening `/setup` from a non-loopback
  address requires the setup token from `<data>/setup-token.txt`.
* **Service**: `kardianos/service` (Windows service, systemd, launchd).
  `service install` on Windows repoints an existing `StatusPage` service to
  the current exe and data directory (installer upgrades over a portable
  install). `service start`/`restart` poll `/healthz` for up to 15 s, because
  the service manager reports "running" before the server has opened the
  database and the port; the installer relies on their exit code. Only a 200
  with the header `X-Status-Page: ok` counts (redirects are not followed),
  so that another program on the port does not, and a stopped service fails.
* **Roles**: `viewer` (any signed-in user), `editor`, `admin`. Roles are
  assigned to IdP *groups* in the admin UI (Groups page). A user's role is the
  highest role of their groups, computed on every request. The local admin
  account is always admin. Failed local sign-ins are limited (in memory) to
  10 per 15 minutes per client address and user name.
* **Groups & members from the IdP**:
  * at every OIDC login the configured groups claim (default `groups`, from
    the ID token, else from userinfo) replaces the user's memberships; unknown
    groups are created automatically. An absent claim means *no groups*
    (the IdP must be configured to send it). Memberships stay unchanged only
    on Entra group overage (`_claim_names`/`hasgroups`) or when the userinfo
    request failed. A claim of an unexpected type is logged and treated as no
    groups;
  * optionally (Microsoft Entra ID) a background job syncs the (transitive)
    members of all groups with a role or services via Microsoft Graph every
    hour (permissions GroupMember.Read.All + User.Read.All; disabled accounts
    are skipped), so members are notified even if they never signed in. The
    sync is authoritative for the groups it manages (role or services, and
    `last_synced_at` set): sign-in leaves those memberships alone, the sync
    refreshes e-mail/name of users that never signed in, and a group deleted
    in the directory (404) loses its members and is reported;
  * without the sync only users who signed in at least once are known (and
    notified), with the groups of their last sign-in. Admins can remove an
    SSO/directory user on Admin → Users (not the local admin); it is
    recreated at the next sign-in or sync;
  * group external ids that look like GUIDs are lower-cased on write and
    lookup;
  * admins can pre-register a group by its id/name before anybody signed in.
* **Service status** is derived, never set by hand: worst impact of all open
  incidents on the service, or "maintenance" while a maintenance window is in
  progress. Severity: operational < maintenance < degraded < partial outage <
  major outage.
* **Incidents** have a status (investigating → identified → monitoring →
  resolved), affected services with an impact each, and a timeline of
  updates. **Maintenance windows** have a start/end and affected services;
  their state (scheduled/in progress/completed/cancelled) is derived from time.
  Only a scheduled window can be cancelled; one in progress is ended early
  ("End now", which moves its end to now). Editor text limits: title 200,
  message/description 10,000 characters.
* **Notifications**: e-mail only. Web handlers enqueue one row per recipient
  in the `notifications` outbox table; a worker delivers them and retries a
  failed delivery after 1, 5, 15 and 60 minutes before marking it failed.
  When the server throttles (temporary error: 1 minute pause) or closes the
  connection (reconnect at once), messages of the batch (50 per connection)
  that were not tried keep their attempts. A connection or sign-in failure
  counts a failed attempt for every message of the batch, so after about 80
  minutes of outage they are marked failed (outdated status mails are of
  little value). A message the server accepted is never marked failed or
  re-sent; results are recorded per message. Sent and failed e-mails are
  deleted after 90 days; the admin list does not load the bodies.
  Recipients = known users with an e-mail who are members of a group
  assigned to an affected service. Sent on: incident created/updated (if
  "notify" ticked), maintenance scheduled/changed/cancelled (if ticked; a
  window created while already in progress gets the "started" e-mail) and
  started/completed (if the maintenance has notify enabled).
* **Web UI**: server-rendered `html/template` pages in one layout, one
  stylesheet (`static/style.css`, its class vocabulary is documented at the
  top and checked by a test), light and dark mode via `prefers-color-scheme`.
  `static/app.js` only adds delete confirmations and field toggling; pages
  stay usable without JavaScript. Flash messages: success, info, warning
  (done, but e.g. the e-mails could not be queued), error.
* **Times** are stored as unix seconds (UTC) and displayed/entered in the
  configured time zone.
* **Security**: sessions in DB (cookie holds a random token, DB stores its
  SHA-256), 12h lifetime, HttpOnly + SameSite=Lax cookies; CSRF double-submit
  token + Origin check on all POSTs; bcrypt for the local password; OIDC with
  state, nonce and PKCE; secrets in the DB are AES-GCM encrypted with the key
  from `config.json`; strict CSP (no inline scripts); data directory
  restricted to SYSTEM/Administrators on Windows.

## URL map

Public (login required if "public access" is off): `/`, `/history`,
`/incidents/{id}`, `/maintenance/{id}`, `/api/status` (JSON).
Auth: `/login`, `/auth/oidc/login`, `/auth/oidc/callback`, `/logout`, `/account`.
Editors: `/manage…`. Admins: `/admin/settings`, `/admin/services`,
`/admin/groups`, `/admin/users`, `/admin/notifications`.
See `internal/web/routes.go` for the authoritative list.
