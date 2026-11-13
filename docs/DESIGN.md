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
13. Each company can brand it: logos and colours for light and dark mode; no
    e-mail templating.
14. Groups from Microsoft Entra ID show their names, not just their object
    IDs, so that admins can assign roles and services; roles can also be
    assigned in Entra ID with app roles.

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
                       SCIM data access (scim.go); local administrator details and sign-in switch (local_admin.go);
                       logos and favicon (brand.go)
internal/secret        AES-GCM encryption of stored secrets and cookies, random tokens
internal/auth          sessions, CSRF, local admin login (and its switch), OIDC login + group memberships at login
internal/scim          SCIM 2.0 endpoint /scim/v2 for user and group provisioning from Microsoft Entra ID
internal/msgraph       Microsoft Graph client: client-credentials token of the SSO app registration, sendMail,
                       group names
internal/groupnames    worker that looks up the names of groups (Entra ID object IDs) with Microsoft Graph
internal/notify        e-mail outbox, rendering, delivery with Microsoft Graph (default) or SMTP (go-mail),
                       maintenance start/end mails
internal/theme         branding (leaf package, pure functions): colour tokens of light and dark mode derived
                       from the chosen colours, their CSS, e-mail button colour, default favicon, image checks
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
removed; migration 2 deletes its setting and adds the provisioning columns,
migration 3 adds `users.email_from_user_name`, migration 4 the table
`brand_images` (logos and favicon; `{{BLOB}}` is `BLOB` or `BYTEA`),
migration 5 `idp_groups.directory_name` and `directory_checked_at` (group
names from Microsoft Entra ID; it clears the names that only repeat the
external id, except those of provisioned groups) and `users.app_roles`.

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
  picked up within that time. The provisioning token settings are written
  only by `SetSCIMToken`; `SaveSettings` skips them, so that saving an older
  copy of the settings cannot bring back a replaced or deleted token.
  Re-running `statuspage setup` only replaces the database settings (and the
  listen address if `-listen` is given).
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
  assigned to IdP *groups* in the admin UI (Groups page), or with *app
  roles*: every sign-in stores the values of the roles claim
  (`Settings.OIDCRolesClaim`, default `roles`) in `users.app_roles` (JSON
  array; trimmed, without duplicates, at most 100 values of at most 256
  characters), and while `Settings.OIDCAppRoles` is on, `Settings.AppRole`
  maps them to a role (`OIDCAdminRole`, default `Admin`, then
  `OIDCEditorRole`, default `Editor`; case-insensitive; "" = none). A user's
  role is the highest role of their groups and app roles, computed on every
  request, so turning app roles off or changing the values applies at once;
  an assignment in the IdP applies at the next sign-in. The local admin
  account is always admin. Failed local sign-ins are limited (in memory) to
  10 per 15 minutes per client address and user name.
* **Local administrator**: exactly one break-glass account; there is no other
  local user management. On `/account` it can change its display name
  (0–100 characters) and user name (`store.UpdateLocalAdmin`, unique among
  local accounts regardless of case; open sessions stay valid) and its
  password (other sessions are signed out).
* **Local sign-in switch**: `Settings.LocalLoginEnabled` (settings key
  `local_login_enabled`, default true). It can only be turned off while OIDC
  is enabled and configured (issuer, client ID, client secret) and somebody
  can sign in as admin with it (`store.HasSSOAdmin`: a group has the admin
  role, or, with app roles on, an active user's stored app roles give admin;
  `store.CheckLocalLoginOff`, used by the settings page and `statuspage
  local-login disable`); the same check stops OIDC, or app roles, from being
  turned off while the switch is off. While off: `auth.LoginLocal` returns
  `ErrLocalLoginOff` (POST /login → 403) before the limiter and the password
  check; `loadIdentity` ignores sessions of local admins; the sign-in page
  shows only SSO (`?local=1` shows a note with the general form of the
  command; Admin → Settings shows the exact one, with the data directory
  and, on Windows, the program path), and when it sees a browser that holds
  a local-admin session it ends that session and explains why. While it is
  off, Admin → Groups refuses to take the admin role from, or delete, the
  only group that has it unless an app role admin exists
  (`store.HasSSOAdmin` without that group; 422 on save, error flash on
  delete). `statuspage local-login enable|disable|status` writes only
  that key (`store.SetLocalLoginEnabled`) and deletes the local-admin
  sessions when the value changes; a running server picks it up through the
  settings cache. The settings page deletes them too when it turns the
  switch on again (`store.DeleteLocalAdminSessions`, before saving), so no
  session from before the switch-off comes back; while it is off they are
  only ignored, so that the sign-in page can explain the sign-out to each
  browser. `local-login status` also prints the user name of the local
  administrator account, and `reset-password` names it when the given name
  is unknown (`store.LocalAdminUsernames`), because the name can be changed
  on `/account`. Without `config.json` in the data directory, the commands
  ask for the `-data` of the service.
* **Identity: users and groups from the IdP.** Two sources that work
  together:
  * **SCIM provisioning** (Microsoft Entra ID, `internal/scim`): Entra ID
    pushes the groups assigned to a provisioning enterprise application (a
    non-gallery app, separate from the SSO app registration) with their
    members, about every 40 minutes, so members are known and notified even
    if they never signed in. Users: SCIM `userName` ↔ `users.principal_name`
    (as sent), `externalId` ↔ `scim_external_id`, e-mail = the work address,
    else the primary, else the first, else a `userName` that is an address
    (not for guests' `#EXT#` names), lower-cased; an address derived from
    the `userName` follows its renames (`users.email_from_user_name`), one
    that was sent does not; name = `displayName`, else `name.formatted`,
    else given + family name; `active` ↔ `active`. Every SCIM write sets
    `provisioned`. `active = false` means no e-mails
    (`RecipientsForServices` skips the user), no sign-in, and the user's
    sessions are deleted; `loadIdentity` also refuses and deletes any later
    session of a deactivated user (e.g. of a sign-in that finished right
    after the deactivation). Activating a user again also clears
    `oidc_subject` and `directory_id` and deletes the sessions: the next
    sign-in links the user again by user name or e-mail (the user name may
    belong to another person by now). DELETE removes the user (memberships
    and sessions cascade). POST /Users takes over a user who is not
    provisioned yet (e.g. created by a sign-in), matched by user name
    (case-insensitive), then by e-mail, instead of creating a duplicate.
    Groups: SCIM `id` = `idp_groups.id`, `displayName` ↔ name, `externalId`
    ↔ `external_id` (the Entra object ID, the same value as in the groups
    claim; GUIDs lower-cased; a group without one gets a generated `scim:…`
    id, which the first real one replaces; provisioning never changes a real
    one, because it decides who gets the group's role), members ↔
    `user_groups`. Every write sets `provisioned` and `last_synced_at` (the
    group page shows it only for provisioned groups: an upgraded
    installation may still hold the time of the removed directory sync). POST
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
    `principal_name` (`preferred_username`, else `upn`; case-insensitive;
    only rows without a subject, or provisioned rows, because user names
    are reused), then e-mail; with an `oid`, name and e-mail matches only
    take rows without a directory id. A sign-in never overwrites the name,
    e-mail or principal name of a provisioned user and never takes a
    provisioned user's e-mail address; a deactivated user is refused ("your
    account has been deactivated by your organisation's user
    provisioning"). The configured groups claim (default `groups`, from the
    ID token, else from userinfo) replaces the user's memberships, except
    memberships of provisioned groups, which only SCIM changes; unknown
    groups are created (`ON CONFLICT DO NOTHING`). An absent claim means *no
    groups* (the IdP must be configured to send it). Memberships stay
    unchanged only on Entra group overage (`_claim_names`/`hasgroups`) or
    when the userinfo request failed. A claim of an unexpected type is
    logged and treated as no groups. The roles claim is read the same way
    and replaces the user's app roles at every sign-in, also while app roles
    are off (so that turning them on uses those of the last sign-in, not
    older ones); userinfo is asked for it only while they are on. When the
    userinfo request fails, the user has no app roles until the next
    sign-in (fail closed), while the group memberships stay.
  * Without provisioning only users who signed in at least once are known
    (and notified), with the groups of their last sign-in. Admins can remove
    a user who is not provisioned on Admin → Users (not the local admin); it
    is recreated at the next sign-in. Provisioned users cannot be removed
    there (the page shows "Managed in Entra ID", the handler refuses): Entra
    ID deactivates and deletes them, and one removed by hand would come back
    at its next sign-in without the memberships of provisioned groups, which
    Entra ID sends again only after *Restart provisioning*. After turning
    provisioning off nobody is provisioned, so all can be removed.
  * Admins can pre-register a group by its id/name before anybody signed in
    or before provisioning takes it over. The Groups page and the service
    form list provisioned groups even without a role or services; group
    display names have at most 256 characters, like in Entra ID and SCIM.
  * **Group names** (`internal/groupnames`): the Entra groups claim holds
    object IDs only (names only for groups assigned to the app, and then no
    IDs), so groups keep the object ID as `external_id` and get their name
    from Microsoft Graph as `directory_name` while `Settings.EntraGroupNames`
    is on and OIDC is configured with a client secret
    (`Settings.GroupNamesActive`). `GET /v1.0/groups?$filter=id in
    (…)&$select=id,displayName` with up to 15 IDs per request (the limit of
    `in`; only GUIDs go into the filter; spaces as `%20`), application
    permission GroupMember.Read.All. `idp_groups.name` holds only a name
    given by an admin or by provisioning: `Group.DisplayName` = name, else
    directory name, else external id (also the SQL order); the edit page
    stores a name equal to the directory name or the id as "", so that the
    group follows renames. The worker (`Resolver.Run`) looks up the groups
    that are not provisioned, have a GUID as external id and were not looked
    up for 24 hours (`store.GroupsForNameLookup`): at its start, hourly and
    2 s after `Wake` (after every OIDC sign-in and settings save), and
    stores the names after each request (`SetGroupDirectoryNames`; a group
    that is not found keeps its last name). After a failure it pauses for an
    hour (Retry-After, at most an hour, when throttled) while issuer, client
    ID and client secret stay the same; the last outcome is shown on Admin →
    Groups, whose *Look up names now* (`LookUpAll`, 25 s, then the worker
    goes on) ignores the pause and the 24 hours. Adding a group by object ID
    looks it up at once (`LookUpGroup`, 10 s) and warns if Entra ID does not
    know it. A lookup whose context ends (shutdown, request time) records
    neither a success nor a failure. One lookup runs at a time.
* **SCIM endpoint** (`/scim/v2`, Tenant URL `<Base URL>/scim/v2`): bearer
  token only – 32 random bytes base64url, generated in Admin → Settings,
  stored as SHA-256 hex (`Settings.SCIMTokenHash`) and compared in constant
  time. It is shown once with Post/Redirect/Get: `POST
  /admin/settings/scim-token` keeps it in memory for at most 5 minutes, for
  the administrator who generated it, and redirects to
  `/admin/settings?new-token=<slot id>#provisioning`, which shows it and
  forgets it; reloading neither shows it again nor generates another one.
  It is never in a URL, cookie or log. No token = provisioning off = 401
  (with `WWW-Authenticate`). The auth middleware and the setup redirect let
  `/scim/` through untouched (no cookies, sessions or CSRF). Responses are
  `application/scim+json`; requests may be `application/scim+json` or
  `application/json` (≤ 2 MiB, values ≤ 256 characters). Filters: `eq` and
  `and` only – users by `userName`, `externalId`, `id`, `emails.value` and
  `emails[type eq "work"].value`, groups by `displayName`, `externalId`,
  `id` and `members`/`members.value` (a user id: a membership check on the
  real state); comparisons of the manager (`manager`, `manager.value`, the
  enterprise extension's) match no user, because Entra ID may check the
  manager before changing it and it is not stored; anything else is 400
  `invalidFilter`, as are values that are not valid UTF-8 or contain
  control characters; no match is an empty ListResponse. Paging:
  `startIndex` (1-based), `count` (default 100, max 500). PATCH accepts
  both request styles of Entra ID (with and without
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
  Revoking the token clears `provisioned` on all groups and users, so
  sign-ins manage memberships, names and e-mail addresses again;
  deactivated users stay deactivated. `SetSCIMToken` takes the same lock,
  and every SCIM write checks the token of its request again inside its
  transaction (`store.WithSCIMToken`; a deleted or replaced token gives
  401), so no write that was authenticated before provisioning was turned
  off can set the provisioned flags again. Turning provisioning on again
  needs *Restart provisioning* in Entra ID: Entra ID keeps the ids of the
  groups it provisioned before and sends only changes, while the status
  page no longer treats those groups as provisioned (a group is found again
  by its display name and taken over by POST, which replaces its members);
  a restart sends all users, groups and members again.
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
    be enabled, so the settings page shows these three fields also while
    sign-in is off and checks them for Graph); the tenant comes from the
    Entra issuer
    (`login.microsoftonline.com/{tenant}/v2.0` or `sts.windows.net/{tenant}/`),
    other issuers are rejected when saving. Client-credentials token for
    `https://graph.microsoft.com/.default` (cached by the client and renewed
    10 s before it expires; requested with the caller's context, so stopping
    the service does not wait for it), 30 s timeouts, redirects never
    followed. `POST /v1.0/users/{sender}/sendMail` (sender path-escaped; a
    sender that begins with `$` is addressed as `/users('{sender}')`,
    because Microsoft Graph answers `/users/$…` with HTTP 400) with one
    recipient, the HTML body and `saveToSentItems: false`; 202 = accepted.
    Needs the application permission Mail.Send, or Exchange Online RBAC for
    Applications scoped to the sender mailbox. Error classes: 429/503/504
    pause the delivery for Retry-After (1 minute without one, at most
    10 minutes) and the throttled message keeps its attempts; token and
    network errors, 401, 403, 404 and other 5xx count one attempt for every
    remaining message of the batch; any other 4xx (e.g. 400 invalid
    recipient) fails only that message. Errors carry hints (Mail.Send/RBAC
    for 403, the mailbox for 404); a failed access token request names
    Microsoft Entra ID ("could not reach Microsoft Entra ID to get an access
    token", "… did not respond within 30s …"), a failed e-mail request
    Microsoft Graph. The worker keeps its client and access token until the
    settings change or a failure other than throttling; a test e-mail always
    uses a new token.
  * **SMTP** (go-mail): one connection per batch; a temporary error that is
    not about the recipient pauses the delivery for a minute; a closed
    connection is reopened at once.
* **Web UI**: server-rendered `html/template` pages in one layout, one
  stylesheet (`static/style.css`, its class vocabulary is documented at the
  top and checked by a test), light and dark mode via `prefers-color-scheme`
  (the branding can force one, see below), on screen only: printing always
  uses the light colours.
  `static/app.js` only adds delete confirmations (`data-confirm`), a
  confirmation when a button that submits another form (`form="…"`, e.g.
  the provisioning token buttons) would lose changes made in the settings
  form (`data-unsaved`), field toggling (`data-show-when`), on the branding
  page colour pickers synced with the hex fields (`data-color-for`), the
  default of the empty dark accent field following the light accent
  (`data-fallback-for`; a port of the colour maths of `internal/theme`, keep
  the two in sync) and a size check of chosen files (`data-max-bytes`: a
  file that is too large is cleared and a message shown below the field),
  and, in the setup wizard, the browser's time zone as the default
  (`data-browser-timezone`, with a small map of renamed zones that browsers
  report under old names); pages stay usable without JavaScript. The
  settings page has one handler file and one template
  partial per section (`admin_settings_{general,sso,sync,mail}.go`,
  `partials/settings_*.html`), each with a view and a save function.
  Flash messages: success, info, warning (done, but e.g. the e-mails could
  not be queued), error.
* **Branding** (Admin → Branding, `internal/theme`): logos for light and
  dark mode, a favicon, the accent and header colour of each mode and the
  appearance (follow the device, always light, always dark). Status colours
  are not customisable (they keep their meaning), and there is no e-mail
  templating.
  * **Storage**: images in the table `brand_images` (name `logo-light`,
    `logo-dark` or `favicon`, content type, data, version = the first 16 hex
    characters of the SHA-256). The versions are also settings
    (`brand_logo_light`, `brand_logo_dark`, `brand_favicon`), written in the
    same transaction and only by `SetBrandImage`/`DeleteBrandImage`
    (`SaveSettings` skips them, like the provisioning token), so pages link
    to `/brand/{name}?v=<version>` from the cached settings without reading
    the image, and saving the settings never brings back a replaced or
    removed image. Colours, mode and "show the site name" are ordinary
    settings (`theme_*`, colours `#rrggbb` or empty for the default;
    normalised on load and save). The branding page saves them with
    `SaveTheme`, which writes only these six keys: the rest of its copy
    comes from the settings cache and may be older (e.g. a local-login
    switch the command line has just changed), so it is never written back.
  * **Uploads**: at most 512 KB per image; `POST /admin/branding` is the
    only multipart form (body ≤ 4 MiB). `theme.CheckImage` takes the type
    from the content, never from the file name: logos PNG, JPEG, GIF, WebP
    or SVG, favicons PNG, GIF, SVG or ICO; PNG, JPEG and GIF headers must
    decode, WebP is recognised by its signature only. An SVG must be
    well-formed XML with an `svg` root in the SVG namespace (without
    `xmlns="http://www.w3.org/2000/svg"` browsers do not draw it:
    `ErrSVGNamespace`, with its own message); before the root there may be
    only a byte order mark, the XML declaration, comments and a document
    type declaration without an internal subset. Refused as active content
    (`ErrSVGActive`, "scripts, event handlers, embedded documents, entity or
    document type declarations, or style sheet links"): entity
    declarations, a document type declaration with an internal subset (`[`
    or `<` in it: it can declare default attributes such as `onload`, and
    `encoding/xml` and browsers read quotes in it differently), processing
    instructions other than the XML declaration (e.g. `<?xml-stylesheet?>`),
    `script`, `foreignObject`, XHTML and other embedding elements, `on…`
    attributes and values with `javascript:`, `vbscript:` or
    `data:text/html`. Everything is validated first; on any error nothing is
    stored (422) and files must be chosen again.
  * **Serving** (`GET /brand/{name}`): public, also while public access is
    off, because the sign-in page shows the logo; like `/static/` it has no
    session lookup, no setup redirect and no request log, and a GET or HEAD
    sets no CSRF cookie (the response may be kept in shared caches). `ETag` = the
    version (304 on a match), `Cache-Control: public, max-age=31536000,
    immutable` when `?v=` is the current version, else `no-cache`; SVG gets
    its own CSP `default-src 'none'; style-src 'unsafe-inline'; sandbox`.
    Without an upload `favicon` is `theme.FaviconSVG` in the light accent
    (version `c` + its hex digits; pages use it only when the light accent
    is customised, else `/static/favicon.svg`); a missing logo is 404.
  * **Colours** (`theme.Resolve`): the page tokens (background, surfaces,
    text, border) are fixed; `theme` holds the defaults of `style.css`, and
    a test keeps them equal. A custom accent gets white or #111111 text,
    whichever contrasts more (white or black if neither reaches 4.5; one of
    them always does), a hover shade with 14 % black (light) or white (dark)
    mixed in, or 14 % of the other one where that would lower the contrast
    with the button text (in effect darker behind white text, lighter behind
    dark text), and a link colour moved towards black (light) or white
    (dark) until its contrast on `--surface-2`, the worst page surface, is
    at least 4.5 (the branding page names the adjusted shade). Focus
    outlines, the border of a focused field and the marker of the active
    tab are not text and use `--primary-ui` (`Palette.PrimaryUI`): the
    accent moved the same way until its contrast on `--surface-2` is at
    least 3. The default accents reach it unchanged, so `style.css` sets it
    to `var(--primary)`; a test checks that these rules use it. Without a
    dark accent, dark mode uses the light accent, lightened until its
    contrast on the dark background is at least 3. A custom header gets
    white or #17202c text, whichever contrasts more (black if neither
    reaches 4.5), muted text mixed until it reaches 4.5, hover and current
    shades with 10 and 16 % of the text colour (or of the opposite colour,
    black for white text and white for dark text, where the text would drop
    below 4.5 on them) and a line shade with 20 %; the dark header has no
    fallback to the light one.
  * **CSS and appearance**: when a colour is set, `theme.CSS` gives a
    `<style>` block after the stylesheet with the complete set of accent and
    header variables (`--primary`, `--primary-ui`, `--link`, `--focus`,
    `--header-*`, …; defaults included) for both modes, with the selectors
    and media queries of `style.css`, so a colour customised for one mode
    never leaks into the other. The dark tokens of `style.css` are in
    `@media screen and (prefers-color-scheme: dark) { :root:not([data-theme="light"]) }`
    and in an identical `@media screen { :root[data-theme="dark"] }` block
    (checked by a test). They apply only on screen, so printing always uses
    the light tokens, also when dark mode is forced (the print styles set
    `color-scheme: light` too). A forced appearance sets `data-theme` on
    `<html>` and the matching `color-scheme` meta tag. The header shows the
    logo (in auto mode a `<picture>` with the dark logo for
    `prefers-color-scheme: dark`), scaled down (at least 32 px wide) where
    the header is too narrow, with an empty `alt` while the site name is
    shown next to it and the site name otherwise. Next to a logo the name
    is shown only where it fits completely, else it wraps into a hidden
    second line and stays the link text for screen readers (on phones, or
    with a long name or a wide logo, often only the logo is visible). Setup
    mode and a failed settings load use the defaults.
  * **E-mails**: only the link button takes the light accent
    (`theme.EmailButton`, with the button text colour of the web interface):
    *View incident*, *View maintenance* and *Open the status page* of the
    test e-mail. Without one the HTML is unchanged (a test compares the test
    e-mail with the one from before branding). No logo, because mail clients
    block or proxy remote images.
* **Times** are stored as unix seconds (UTC) and displayed/entered in the
  configured time zone. It is chosen from a select of the zone.tab zones
  (IANA tzdata 2025c, 418 zones) grouped by region, labelled with the current
  UTC offset; any IANA name is accepted and a stored value outside the list
  is always offered (Go's embedded tzdata makes this work on Windows).
* **Security**: sessions in DB (cookie holds a random token, DB stores its
  SHA-256), 12h lifetime, HttpOnly + SameSite=Lax cookies; CSRF double-submit
  token + Origin check on all POSTs (not on `/scim/`, which has no cookies and
  its own bearer token); request bodies ≤ 1 MiB (`auth.MaxFormBytes`;
  `auth.MaxUploadBytes`, 4 MiB, for `POST /admin/branding`), and without a
  CSRF header the middleware parses the form itself (multipart in memory),
  so a larger body gets 413 instead of a misleading CSRF error; uploaded
  SVGs are checked and served sandboxed (see Branding); bcrypt for the
  local password; OIDC with state,
  nonce and PKCE; secrets in the DB are AES-GCM encrypted with the key from
  `config.json` (the SCIM token is only hashed); strict CSP (no inline
  scripts); data directory restricted to SYSTEM/Administrators on Windows.

## URL map

Public (login required if "public access" is off): `/`, `/history`,
`/incidents/{id}`, `/maintenance/{id}`, `/api/status` (JSON). Always public:
`/static/…`, `/brand/{logo-light,logo-dark,favicon}` (uploaded logos and
favicon). Health: `/healthz`. First run: `/setup`.
Auth: `/login`, `/auth/oidc/login`, `/auth/oidc/callback`, `/logout`,
`/account`, `POST /account/profile` (local administrator: display name and
user name), `POST /account/password`.
Editors: `/manage…`. Admins: `/admin/settings` (also `POST
/admin/settings/test-email`, `POST /admin/settings/scim-token` – generate a
provisioning token and redirect to the settings page, which shows it once –
and `POST /admin/settings/scim-token/delete` – turn provisioning off),
`/admin/branding` (`POST` multipart; `POST /admin/branding/reset` restores
the defaults), `/admin/services`, `/admin/groups` (`POST /admin/groups/names`
looks up the names of all groups in Entra ID), `/admin/users` (`POST
/admin/users/{id}/delete` removes a user who is not provisioned),
`/admin/notifications`.
Provisioning: `/scim/v2/…` (`Users`, `Groups`, `ServiceProviderConfig`,
`ResourceTypes`, `Schemas`) with the bearer token, outside the session, CSRF
and setup redirect.
See `internal/web/routes.go` for the authoritative list.
