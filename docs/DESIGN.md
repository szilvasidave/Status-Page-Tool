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
cmd/statuspage         CLI entry point: run (default), setup, service install/…, admin password reset
internal/app           lifecycle: setup mode → normal mode, background workers
internal/config        config.json (listen, db driver/dsn, secret key, optional TLS)
internal/db            database open (SQLite via modernc.org/sqlite, PostgreSQL via pgx), schema migrations
internal/store         ALL SQL lives here; models; pure status computation (status.go)
internal/auth          sessions, CSRF, local admin login, OIDC login + group sync at login
internal/notify        e-mail outbox, rendering, SMTP delivery (go-mail), maintenance start/end mails
internal/directory     optional Microsoft Entra ID group member sync via Microsoft Graph
internal/web           HTTP handlers + html/template templates + static assets (embedded)
internal/e2e           end-to-end tests: the real app driven like a browser, plus the CLI
internal/testutil      test helpers: mock OpenID provider (mockidp), fake SMTP server (fakesmtp)
installer/             Inno Setup script → StatusPageSetup.exe
```

## Key decisions

* **Single binary**, templates and static files embedded with `embed`. Pure Go
  (no CGO) so `GOOS=windows go build` works anywhere.
* **Configuration split**: `config.json` in the data directory holds only what
  is needed to reach the database (+ secret key). Everything else (site name,
  OIDC, SMTP, …) lives in the `settings` table and is edited at
  `/admin/settings`.
* **Data directory**: `%ProgramData%\StatusPage` on Windows, `./data` next to
  the binary elsewhere; override with `-data` or `STATUSPAGE_DATA`.
* **First run**: without `config.json` the server starts in *setup mode* and
  serves only `/setup` (choose SQLite or PostgreSQL, test connection, create
  tables). The Windows installer performs this step itself (custom wizard
  pages) by calling `statuspage.exe setup …`. Afterwards `/setup` asks for the
  local administrator account (break-glass login, also used to configure OIDC).
  Opening `/setup` from a non-loopback address requires the setup token from
  `<data>/setup-token.txt`.
* **Roles**: `viewer` (any signed-in user), `editor`, `admin`. Roles are
  assigned to IdP *groups* in the admin UI (Groups page). A user's role is the
  highest role of their groups, computed on every request. The local admin
  account is always admin.
* **Groups & members from the IdP**:
  * at every OIDC login the configured groups claim (default `groups`) is read
    and the user's memberships are replaced with it (groups are created
    automatically);
  * optionally (Microsoft Entra ID) a background job syncs the members of all
    relevant groups via Microsoft Graph every hour, so members are notified
    even if they never signed in;
  * admins can pre-register a group by its id/name before anybody signed in.
* **Service status** is derived, never set by hand: worst impact of all open
  incidents on the service, or "maintenance" while a maintenance window is in
  progress. Severity: operational < maintenance < degraded < partial outage <
  major outage.
* **Incidents** have a status (investigating → identified → monitoring →
  resolved), affected services with an impact each, and a timeline of
  updates. **Maintenance windows** have a start/end and affected services;
  their state (scheduled/in progress/completed/cancelled) is derived from time.
* **Notifications**: e-mail only. Web handlers enqueue one row per recipient
  in the `notifications` outbox table; a worker delivers them and retries a
  failed delivery after 1, 5, 15 and 60 minutes before marking it failed.
  Sent and failed e-mails are deleted after 90 days.
  Recipients = users with an e-mail who are members of a group assigned to an
  affected service. Sent on: incident created/updated (if "notify" ticked),
  maintenance scheduled/changed/cancelled (if ticked) and started/completed
  (if the maintenance has notify enabled).
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
  from `config.json`; strict CSP (no inline scripts).

## URL map

Public (login required if "public access" is off): `/`, `/history`,
`/incidents/{id}`, `/maintenance/{id}`, `/api/status` (JSON).
Auth: `/login`, `/auth/oidc/login`, `/auth/oidc/callback`, `/logout`, `/account`.
Editors: `/manage…`. Admins: `/admin/settings`, `/admin/services`,
`/admin/groups`, `/admin/users`, `/admin/notifications`.
See `internal/web/routes.go` for the authoritative list.
