package db

import (
	"context"
	"fmt"
	"strings"
)

// migrations are applied in order; never edit an existing entry once
// released, append a new one instead. Placeholders:
//
//	{{PK}}    auto-increment primary key column definition
//	{{BOOL}}  boolean column type
//	{{FALSE}} / {{TRUE}} boolean literals for DEFAULT clauses
//	{{BLOB}}  binary data column type (BLOB, BYTEA)
var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE users (
	id             {{PK}},
	email          TEXT NOT NULL DEFAULT '',
	name           TEXT NOT NULL DEFAULT '',
	oidc_subject   TEXT UNIQUE,
	directory_id   TEXT UNIQUE,
	username       TEXT UNIQUE,
	password_hash  TEXT,
	is_local_admin {{BOOL}} NOT NULL DEFAULT {{FALSE}},
	last_login_at  BIGINT,
	created_at     BIGINT NOT NULL
);
CREATE UNIQUE INDEX users_email_idx ON users (email) WHERE email <> '';

CREATE TABLE idp_groups (
	id             {{PK}},
	external_id    TEXT NOT NULL UNIQUE,
	name           TEXT NOT NULL DEFAULT '',
	role           TEXT NOT NULL DEFAULT '',
	last_synced_at BIGINT,
	created_at     BIGINT NOT NULL
);

CREATE TABLE user_groups (
	user_id  BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	group_id BIGINT NOT NULL REFERENCES idp_groups (id) ON DELETE CASCADE,
	PRIMARY KEY (user_id, group_id)
);
CREATE INDEX user_groups_group_idx ON user_groups (group_id);

CREATE TABLE services (
	id          {{PK}},
	name        TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	category    TEXT NOT NULL DEFAULT '',
	sort_order  INTEGER NOT NULL DEFAULT 0,
	created_at  BIGINT NOT NULL
);

CREATE TABLE service_groups (
	service_id BIGINT NOT NULL REFERENCES services (id) ON DELETE CASCADE,
	group_id   BIGINT NOT NULL REFERENCES idp_groups (id) ON DELETE CASCADE,
	PRIMARY KEY (service_id, group_id)
);
CREATE INDEX service_groups_group_idx ON service_groups (group_id);

CREATE TABLE incidents (
	id          {{PK}},
	title       TEXT NOT NULL,
	status      TEXT NOT NULL,
	started_at  BIGINT NOT NULL,
	resolved_at BIGINT,
	created_by  BIGINT REFERENCES users (id) ON DELETE SET NULL,
	created_at  BIGINT NOT NULL,
	updated_at  BIGINT NOT NULL
);
CREATE INDEX incidents_started_idx ON incidents (started_at);

CREATE TABLE incident_services (
	incident_id BIGINT NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
	service_id  BIGINT NOT NULL REFERENCES services (id) ON DELETE CASCADE,
	impact      TEXT NOT NULL,
	PRIMARY KEY (incident_id, service_id)
);
CREATE INDEX incident_services_service_idx ON incident_services (service_id);

CREATE TABLE incident_updates (
	id          {{PK}},
	incident_id BIGINT NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
	status      TEXT NOT NULL,
	message     TEXT NOT NULL,
	created_by  BIGINT REFERENCES users (id) ON DELETE SET NULL,
	created_at  BIGINT NOT NULL
);
CREATE INDEX incident_updates_incident_idx ON incident_updates (incident_id);

CREATE TABLE maintenances (
	id             {{PK}},
	title          TEXT NOT NULL,
	description    TEXT NOT NULL DEFAULT '',
	starts_at      BIGINT NOT NULL,
	ends_at        BIGINT NOT NULL,
	cancelled      {{BOOL}} NOT NULL DEFAULT {{FALSE}},
	notify         {{BOOL}} NOT NULL DEFAULT {{TRUE}},
	notified_start {{BOOL}} NOT NULL DEFAULT {{FALSE}},
	notified_end   {{BOOL}} NOT NULL DEFAULT {{FALSE}},
	created_by     BIGINT REFERENCES users (id) ON DELETE SET NULL,
	created_at     BIGINT NOT NULL,
	updated_at     BIGINT NOT NULL
);
CREATE INDEX maintenances_window_idx ON maintenances (starts_at, ends_at);

CREATE TABLE maintenance_services (
	maintenance_id BIGINT NOT NULL REFERENCES maintenances (id) ON DELETE CASCADE,
	service_id     BIGINT NOT NULL REFERENCES services (id) ON DELETE CASCADE,
	PRIMARY KEY (maintenance_id, service_id)
);
CREATE INDEX maintenance_services_service_idx ON maintenance_services (service_id);

CREATE TABLE notifications (
	id              {{PK}},
	user_id         BIGINT REFERENCES users (id) ON DELETE SET NULL,
	email           TEXT NOT NULL,
	subject         TEXT NOT NULL,
	body_text       TEXT NOT NULL,
	body_html       TEXT NOT NULL DEFAULT '',
	status          TEXT NOT NULL DEFAULT 'pending',
	attempts        INTEGER NOT NULL DEFAULT 0,
	last_error      TEXT NOT NULL DEFAULT '',
	next_attempt_at BIGINT NOT NULL,
	created_at      BIGINT NOT NULL,
	sent_at         BIGINT
);
CREATE INDEX notifications_pending_idx ON notifications (status, next_attempt_at);

CREATE TABLE sessions (
	id         TEXT PRIMARY KEY,
	user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	expires_at BIGINT NOT NULL,
	created_at BIGINT NOT NULL
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
`,
	// 2: user provisioning (SCIM) and the end of the Graph directory sync.
	//   users.principal_name   user name at the identity provider (Entra
	//                          userPrincipalName / preferred_username,
	//                          SCIM userName), as sent
	//   users.scim_external_id SCIM externalId (Entra: mailNickname by default)
	//   users.active           false = deactivated by provisioning: no
	//                          e-mails, no sign-in
	//   users.provisioned      created or updated by SCIM provisioning
	//   idp_groups.provisioned memberships are managed by SCIM provisioning
	`
ALTER TABLE users ADD COLUMN principal_name TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN scim_external_id TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN active {{BOOL}} NOT NULL DEFAULT {{TRUE}};
ALTER TABLE users ADD COLUMN provisioned {{BOOL}} NOT NULL DEFAULT {{FALSE}};
CREATE INDEX users_principal_name_idx ON users (LOWER(principal_name));
CREATE INDEX users_scim_external_id_idx ON users (scim_external_id);
ALTER TABLE idp_groups ADD COLUMN provisioned {{BOOL}} NOT NULL DEFAULT {{FALSE}};
DELETE FROM settings WHERE key = 'directory_sync_enabled';
`,
	// 3: users.email_from_user_name – provisioning sent no e-mail address, so
	//    the address was derived from the SCIM userName and follows renames.
	`
ALTER TABLE users ADD COLUMN email_from_user_name {{BOOL}} NOT NULL DEFAULT {{FALSE}};
`,
	// 4: brand_images – logos and favicon uploaded on Admin → Branding.
	//   name    logo-light, logo-dark or favicon
	//   version first 16 hex characters of the SHA-256 of data; also stored
	//           in the settings brand_logo_light, brand_logo_dark and
	//           brand_favicon, so that pages can link to the image
	//           (/brand/{name}?v=version) without reading it
	`
CREATE TABLE brand_images (
	name         TEXT PRIMARY KEY,
	content_type TEXT NOT NULL,
	data         {{BLOB}} NOT NULL,
	version      TEXT NOT NULL,
	updated_at   BIGINT NOT NULL
);
`,
	// 5: group names from Microsoft Entra ID, and app roles.
	//   idp_groups.directory_name       the group's display name in Microsoft
	//                                   Entra ID, looked up with Microsoft Graph
	//                                   (package groupnames). idp_groups.name is
	//                                   from now on only a name given by an
	//                                   administrator or by provisioning ('' =
	//                                   none: the directory name, else the
	//                                   external id is shown), so the names that
	//                                   just repeat the external id are cleared
	//   idp_groups.directory_checked_at when the directory name was last looked up
	//   idp_groups.directory_found_at   when the lookup last found the group (a
	//                                   group not found at the last lookup keeps
	//                                   its last name)
	//   users.app_roles                 the values of the roles claim at the
	//                                   user's last sign-in (a JSON array of
	//                                   strings; '' = none)
	`
ALTER TABLE idp_groups ADD COLUMN directory_name TEXT NOT NULL DEFAULT '';
ALTER TABLE idp_groups ADD COLUMN directory_checked_at BIGINT;
ALTER TABLE idp_groups ADD COLUMN directory_found_at BIGINT;
UPDATE idp_groups SET name = '' WHERE name = external_id AND provisioned = {{FALSE}};
ALTER TABLE users ADD COLUMN app_roles TEXT NOT NULL DEFAULT '';
`,
}

// columnRepairs are columns that a migration gained after builds of an
// unreleased branch had already applied it: such a database records the
// migration as applied but lacks the column. Migrate adds each column where
// it is missing, once the schema has reached the migration (version).
var columnRepairs = []struct {
	version             int
	table, column, kind string
}{
	// Added to migration 5 after the build of commit c22fd87 (branch
	// builds only; 5 was first released with the column).
	{5, "idp_groups", "directory_found_at", "BIGINT"},
}

// repairColumns adds the columns of columnRepairs that are missing.
func (d *DB) repairColumns(ctx context.Context, version int) error {
	for _, c := range columnRepairs {
		if version < c.version {
			continue
		}
		if d.IsPostgres() {
			if _, err := d.ExecContext(ctx, `ALTER TABLE `+c.table+` ADD COLUMN IF NOT EXISTS `+c.column+` `+c.kind); err != nil {
				return fmt.Errorf("add column %s.%s: %w", c.table, c.column, err)
			}
			continue
		}
		var n int
		if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('`+c.table+`') WHERE name = ?`, c.column).Scan(&n); err != nil {
			return fmt.Errorf("check column %s.%s: %w", c.table, c.column, err)
		}
		if n == 0 {
			if _, err := d.ExecContext(ctx, `ALTER TABLE `+c.table+` ADD COLUMN `+c.column+` `+c.kind); err != nil {
				return fmt.Errorf("add column %s.%s: %w", c.table, c.column, err)
			}
		}
	}
	return nil
}

func (d *DB) dialect(sql string) string {
	r := strings.NewReplacer(
		"{{PK}}", "INTEGER PRIMARY KEY AUTOINCREMENT",
		"{{BOOL}}", "INTEGER",
		"{{FALSE}}", "0",
		"{{TRUE}}", "1",
		"{{BLOB}}", "BLOB",
	)
	if d.IsPostgres() {
		r = strings.NewReplacer(
			"{{PK}}", "BIGSERIAL PRIMARY KEY",
			"{{BOOL}}", "BOOLEAN",
			"{{FALSE}}", "FALSE",
			"{{TRUE}}", "TRUE",
			"{{BLOB}}", "BYTEA",
		)
	}
	return r.Replace(sql)
}

// SchemaVersion returns the latest schema version known to this binary.
func SchemaVersion() int { return len(migrations) }

// Migrate creates or upgrades all tables. It is safe to call on every start.
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at BIGINT NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var current int
	if err := d.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this program supports (%d); please upgrade", current, len(migrations))
	}
	for v := current + 1; v <= len(migrations); v++ {
		stmts := splitStatements(d.dialect(migrations[v-1]))
		err := d.InTx(ctx, func(tx *Tx) error {
			for _, s := range stmts {
				if _, err := tx.ExecContext(ctx, s); err != nil {
					return fmt.Errorf("%w\nstatement: %s", err, s)
				}
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, v, Now())
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %d: %w", v, err)
		}
	}
	return d.repairColumns(ctx, len(migrations))
}

func splitStatements(sql string) []string {
	var out []string
	for _, s := range strings.Split(sql, ";\n") {
		if s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ";")); s != "" {
			out = append(out, s)
		}
	}
	return out
}
