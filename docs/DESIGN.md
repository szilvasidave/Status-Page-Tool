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

Later feedback:

8. Group members come from Microsoft Entra ID by SCIM provisioning (pushed by
   Entra ID), not from a Microsoft Graph pull.
9. E-mail goes through Microsoft Graph by default; SMTP stays optional.
10. The local (break-glass) sign-in can be turned off once single sign-on
    works, and turned on again from the command line.
11. No local user management: the one local administrator can only change its
    display name, user name and password; everybody else comes from the IdP.
12. Easier forms: a time zone select, a category select for services.

## Architecture

```
cmd/statuspage         CLI entry point: run (default), setup, service install/…, reset-password, local-login, version
internal/app           lifecycle: setup mode → normal mode, TLS, background workers
internal/config        config.json (listen, db driver/dsn, secret key, optional TLS, log level),
                       data directory creation and permissions (icacls on Windows)
internal/setup         database setup shared by the CLI/installer and the web wizard:
                       connection and privilege checks, migrations, writing config.json
internal/db            database open (SQLite via modernc.org/sqlite, PostgreSQL via pgx), schema migrations
internal/store         all application SQL; models; pure status computation (status.go);
                       SCIM data access (scim.go); local administrator details and sign-in switch (local_admin.go)
internal/secret        AES-GCM encryption of stored secrets and cookies, random tokens
internal/auth          sessions, CSRF, local admin login (and its switch), OIDC login + group memberships at login
internal/scim          SCIM 2.0 endpoint /scim/v2 for user and group provisioning from Microsoft Entra ID
internal/msgraph       Microsoft Graph client: client-credentials token of the SSO app registration, sendMail
internal/notify        e-mail outbox, rendering, delivery with Microsoft Graph (default) or SMTP (go-mail),
                       maintenance start/end mails
internal/web           HTTP handlers + html/template templates + static assets (embedded)
internal/e2e           end-to-end tests: the real app driven like a browser, plus the CLI
internal/testutil      test helpers: mock OpenID provider (mockidp), fake SMTP server (fakesmtp),
                       fake Microsoft Graph token and sendMail endpoints with injectable failures (fakegraph)
tools/devseed          development only: data directory with sample data, sessions for test users
installer/             Inno Setup script → StatusPageSetup.exe
```

SQL outside `internal/store` is limited to the migrations (`internal/db`),
the connection and privilege checks of `internal/setup` and `tools/devseed`.
The former `internal/directory` (a Microsoft Graph pull of group members) was
removed; migration 2 deletes its setting.

## Key decisions

* **Single binary**, templates and static files embedded with `embed`. Pure Go
  (no CGO) so `GOOS=windows go build` works anywhere.
* **Configuration split**: `config.json` in the data directory holds what is
  needed before the database can be reached: listen address, optional TLS
  certificate/key (paths relative to the data directory), database
  driver/DSN, secret key and log level. These are not editable in the web
  UI. Everything else (site name, OIDC, e-mail, provisioning token, …) lives
  in the `settings` table and is edited at `/admin/settings`; `GetSettings`
  caches it for 10 seconds, so a change made by another process (the CLI) is
  picked up within that time. Re-running `statuspage setup` only replaces the
  database settings (and the listen address if `-listen` is given).
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
  login, also used to configure OIDC) and the basic settings. No user name is
  suggested (a guessable name can be locked out by failed sign-ins); local
  user names are at most 64 letters, digits and `. _ - @`, stored in lower
  case (`localUsernameProblem`, shared with the account page). Opening
  `/setup` from a non-loopback address requires the setup token from
  `<data>/setup-token.txt`.
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
* **Local administrator**: exactly one break-glass account; there is no other
  local user management. On `/account` it can change its display name
  (0–100 characters) and user name (`store.UpdateLocalAdmin`, unique among
  local accounts regardless of case; open sessions stay valid) and its
  password (other sessions are signed out).
* **Local sign-in switch**: `Settings.LocalLoginEnabled` (settings key
  `local_login_enabled`, default true). It can only be turned off while OIDC
  is enabled and configured (issuer, client ID, client secret) and a group
  has the admin role (`store.CheckLocalLoginOff`, used by the settings page
  and `statuspage local-login disable`); the same check stops OIDC from being
  turned off while the switch is off. While off: `auth.LoginLocal` returns
  `ErrLocalLoginOff` (POST /login → 403) before the limiter and the password
  check; `loadIdentity` ignores sessions of local admins; the sign-in page
  shows only SSO (`?local=1` shows a note), and when it sees a browser that
  holds a local-admin session it ends that session and explains why.
  `statuspage local-login enable|disable|status` writes only that key
  (`store.SetLocalLoginEnabled`) and deletes the local-admin sessions when
  the value changes; a running server picks it up through the settings cache.
  Turning it on again in the web page does not delete sessions, so a
  local-admin session that is less than 12 hours old works again.
* **Identity: users and groups from the IdP.** Two sources that work
  together:
  * **SCIM provisioning** (Microsoft Entra ID, `internal/scim`): Entra ID
    pushes the groups assigned to a provisioning enterprise application (a
    non-gallery app, separate from the SSO app registration) with their
    members, about every 40 minutes, so members are known and notified even
    if they never signed in. Users: SCIM `userName` ↔ `users.principal_name`
    (as sent), `externalId` ↔ `scim_external_id`, e-mail = the work address,
    else the primary, else the first, else a `userName` that is an address
    (not for guests' `#EXT#` names), lower-cased; name = `displayName`, else
    `name.formatted`, else given + family name; `active` ↔ `active`. Every
    SCIM write sets `provisioned`. `active = false` means no e-mails
    (`RecipientsForServices` skips the user), no sign-in, and the user's
    sessions are deleted; DELETE removes the user (memberships and sessions
    cascade). POST /Users takes over a user who is not provisioned yet (e.g.
    created by a sign-in), matched by user name (case-insensitive), then by
    e-mail, instead of creating a duplicate. Groups: SCIM `id` =
    `idp_groups.id`, `displayName` ↔ name, `externalId` ↔ `external_id` (the
    Entra object ID, the same value as in the groups claim; GUIDs
    lower-cased; a group without one gets a generated `scim:…` id), members ↔
    `user_groups`. Every write sets `provisioned` and `last_synced_at`. POST
    /Groups takes over an existing group with the same external id (from
    sign-ins or added by an admin) with its role and services and replaces
    its members; DELETE removes all memberships and clears `provisioned` but
    keeps the group with its role and services (re-provisioning restores
    it). Provisioning sees every user except local admins (so it can take
    over users who signed in before), but only provisioned groups (a group
    is taken over only by POST, which replaces the members recorded from
    sign-ins). Display names are unique among provisioned groups, user
    names among provisioned users (409 `uniqueness`).
  * **Sign-in** (`internal/auth/oidc.go`, any OIDC provider): the user is
    matched by `oidc_subject`, then `directory_id` (Entra `oid`), then
    `principal_name` (`preferred_username`, else `upn`; case-insensitive),
    then e-mail; with an `oid`, name and e-mail matches only take rows
    without a directory id. A sign-in never overwrites the name, e-mail or
    principal name of a provisioned user and never takes a provisioned
    user's e-mail address; a deactivated user is refused ("your account has
    been deactivated by your organisation's user provisioning"). The
    configured groups claim (default `groups`, from the ID token, else from
    userinfo) replaces the user's memberships, except memberships of
    provisioned groups, which only SCIM changes; unknown groups are created
    (`ON CONFLICT DO NOTHING`). An absent claim means *no groups* (the IdP
    must be configured to send it). Memberships stay unchanged only on Entra
    group overage (`_claim_names`/`hasgroups`) or when the userinfo request
    failed. A claim of an unexpected type is logged and treated as no
    groups.
  * Without provisioning only users who signed in at least once are known
    (and notified), with the groups of their last sign-in. Admins can remove
    an SSO/provisioned user on Admin → Users (not the local admin); it is
    recreated at the next sign-in or when provisioning creates it again.
  * Admins can pre-register a group by its id/name before anybody signed in
    or before provisioning takes it over.
* **SCIM endpoint** (`/scim/v2`, Tenant URL `<Base URL>/scim/v2`): bearer
  token only – 32 random bytes base64url, generated in Admin → Settings and
  shown once on the rendered response (never in a redirect, cookie or log),
  stored as SHA-256 hex (`Settings.SCIMTokenHash`) and compared in constant
  time. No token = provisioning off = 401 (with `WWW-Authenticate`). The auth
  middleware and the setup redirect let `/scim/` through untouched (no
  cookies, sessions or CSRF). Responses are `application/scim+json`;
  requests may be `application/scim+json` or `application/json` (≤ 2 MiB,
  values ≤ 256 characters). Filters: `eq` and `and` only – users by
  `userName`, `externalId`, `id`, `emails.value` and
  `emails[type eq "work"].value`, groups by `displayName`, `externalId` and
  `id`; anything else is 400 `invalidFilter`; no match is an empty
  ListResponse. Paging: `startIndex` (1-based), `count` (default 100, max
  500). PATCH accepts both request styles of Entra ID (with and without
  `aadOptscim062020`): case-insensitive `op`, operations with or without
  `path`, `active` as bool or `"True"`/`"False"`, members added, removed by
  value list or `members[value eq "id"]`, or replaced; member values that
  are not users are skipped; unknown and extension attributes are ignored.
  User PATCH/PUT answer 200 with the resource, group PATCH 204, POST 201
  with `Location` and `meta`, DELETE 204; errors use the SCIM error schema
  (`status` as a string, `scimType` where defined, readable `detail` for the
  Entra provisioning logs). Discovery: `ServiceProviderConfig`,
  `ResourceTypes`, `Schemas`. On PostgreSQL the transactions that look users
  or groups up before creating or changing them (SCIM writes, sign-ins, group
  claims) take an advisory lock first, so that two of them cannot both decide
  that a user does not exist yet (SQLite serialises writers anyway).
  Revoking the token clears `provisioned` on all groups and
  users, so sign-ins manage memberships, names and e-mail addresses again;
  deactivated users stay deactivated.
* **Service status** is derived, never set by hand: worst impact of all open
  incidents on the service, or "maintenance" while a maintenance window is in
  progress. Severity: operational < maintenance < degraded < partial outage <
  major outage.
* **Services** have an optional free-text category; the form offers the
  existing categories (compared case-insensitively) and "New category…"
  (field `category_new`, option value `(new)`). The status page groups
  services by category, case-insensitively.
* **Incidents** have a status (investigating → identified → monitoring →
  resolved), affected services with an impact each, and a timeline of
  updates. **Maintenance windows** have a start/end and affected services;
  their state (scheduled/in progress/completed/cancelled) is derived from time.
  Only a scheduled window can be cancelled; one in progress is ended early
  ("End now", which moves its end to now). Editor text limits: title 200,
  message/description 10,000 characters.
* **Notifications**: e-mail only. Web handlers enqueue one row per recipient
  in the `notifications` outbox table (only while e-mail is configured); a
  worker delivers them and retries a failed delivery after 1, 5, 15 and 60
  minutes before marking it failed. A connection, sign-in or access-token
  failure counts a failed attempt for every remaining message of the batch
  (up to 50), so
  after about 80 minutes of outage they are marked failed (outdated status
  mails are of little value); messages that were not tried because the
  server throttled (or an SMTP server closed the connection) keep their
  attempts. A message the server accepted is never marked failed or
  re-sent; results are recorded per message. Sent and failed e-mails are
  deleted after 90 days; the admin list does not load the bodies.
  Recipients = known, active users with an e-mail who are members of a group
  assigned to an affected service. Sent on: incident created/updated (if
  "notify" ticked), maintenance scheduled/changed/cancelled (if ticked; a
  window created while already in progress gets the "started" e-mail) and
  started/completed (if the maintenance has notify enabled).
* **E-mail methods**: `Settings.MailMethod` (`graph`, the default, or
  `smtp`) is read for every batch; an installation that had an SMTP server
  before the setting existed loads as `smtp`. Only the chosen method's form
  fields are validated (the others are kept); an empty Graph sender or SMTP
  server means e-mail off; a POST without `mail_method` keeps the stored
  method.
  * **Microsoft Graph** (`internal/msgraph`, `notify/graph.go`): uses the SSO
    app registration (issuer, client ID, client secret – SSO itself need not
    be enabled); the tenant comes from the Entra issuer
    (`login.microsoftonline.com/{tenant}/v2.0` or `sts.windows.net/{tenant}/`),
    other issuers are rejected when saving. Client-credentials token for
    `https://graph.microsoft.com/.default` (cached by the oauth2 token
    source), 30 s timeouts, redirects never followed. `POST
    /v1.0/users/{sender}/sendMail` (sender path-escaped) with one recipient,
    the HTML body and `saveToSentItems: false`; 202 = accepted. Needs the
    application permission Mail.Send, or Exchange Online RBAC for
    Applications scoped to the sender mailbox. Error classes: 429/503/504
    pause the delivery for Retry-After (1 minute without one, at most
    10 minutes) and the throttled message keeps its attempts; token and
    network errors, 401, 403, 404 and other 5xx count one attempt for every
    remaining message of the batch; any other 4xx (e.g. 400 invalid
    recipient) fails only that message. Errors carry hints (Mail.Send/RBAC
    for 403, the mailbox for 404). The worker keeps its client and access
    token until the settings change or a failure other than throttling; a
    test e-mail always uses a new token.
  * **SMTP** (go-mail): one connection per batch; a temporary error that is
    not about the recipient pauses the delivery for a minute; a closed
    connection is reopened at once.
* **Web UI**: server-rendered `html/template` pages in one layout, one
  stylesheet (`static/style.css`, its class vocabulary is documented at the
  top and checked by a test), light and dark mode via `prefers-color-scheme`.
  `static/app.js` only adds delete confirmations (`data-confirm`), field
  toggling (`data-show-when`) and, in the setup wizard, the browser's time
  zone as the default (`data-browser-timezone`, with a small map of renamed
  zones that browsers report under old names); pages stay usable without
  JavaScript. The settings page has one handler file and one template
  partial per section (`admin_settings_{general,sso,sync,mail}.go`,
  `partials/settings_*.html`), each with a view and a save function.
  Flash messages: success, info, warning (done, but e.g. the e-mails could
  not be queued), error.
* **Times** are stored as unix seconds (UTC) and displayed/entered in the
  configured time zone. It is chosen from a select of the zone.tab zones
  (IANA tzdata 2025c, 418 zones) grouped by region, labelled with the current
  UTC offset; any IANA name is accepted and a stored value outside the list
  is always offered (Go's embedded tzdata makes this work on Windows).
* **Security**: sessions in DB (cookie holds a random token, DB stores its
  SHA-256), 12h lifetime, HttpOnly + SameSite=Lax cookies; CSRF double-submit
  token + Origin check on all POSTs (not on `/scim/`, which has no cookies and
  its own bearer token); bcrypt for the local password; OIDC with state,
  nonce and PKCE; secrets in the DB are AES-GCM encrypted with the key from
  `config.json` (the SCIM token is only hashed); strict CSP (no inline
  scripts); data directory restricted to SYSTEM/Administrators on Windows.

## URL map

Public (login required if "public access" is off): `/`, `/history`,
`/incidents/{id}`, `/maintenance/{id}`, `/api/status` (JSON). Health:
`/healthz`. First run: `/setup`.
Auth: `/login`, `/auth/oidc/login`, `/auth/oidc/callback`, `/logout`,
`/account`, `POST /account/profile` (local administrator: display name and
user name), `POST /account/password`.
Editors: `/manage…`. Admins: `/admin/settings` (also `POST
/admin/settings/test-email`, `POST /admin/settings/scim-token` – generate and
show a provisioning token – and `POST /admin/settings/scim-token/delete` –
turn provisioning off), `/admin/services`, `/admin/groups`, `/admin/users`,
`/admin/notifications`.
Provisioning: `/scim/v2/…` (`Users`, `Groups`, `ServiceProviderConfig`,
`ResourceTypes`, `Schemas`) with the bearer token, outside the session, CSRF
and setup redirect.
See `internal/web/routes.go` for the authoritative list.
