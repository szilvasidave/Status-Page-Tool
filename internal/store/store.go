// Package store contains all database access. Every SQL statement of the
// application lives in this package; other packages only use the methods
// below. See package db for the SQL portability rules (SQLite + PostgreSQL).
package store

import (
	"context"
	"errors"
	"time"

	"github.com/szilvasidave/status-page-tool/internal/db"
)

var errNotImplemented = errors.New("not implemented")

// Store is safe for concurrent use.
type Store struct {
	db  *db.DB
	key []byte // secret key used to encrypt secret settings
}

// New creates a store. key is the 32 byte secret key from the config file.
func New(d *db.DB, key []byte) *Store { return &Store{db: d, key: key} }

// DB exposes the underlying database (used by tests and health checks).
func (s *Store) DB() *db.DB { return s.db }

// Ping checks database connectivity.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// ===================================================================== settings

// GetSettings returns the settings, using DefaultSettings() for keys that are
// not stored. Secret values are decrypted. The result may be cached in memory
// for a few seconds; SaveSettings invalidates the cache.
func (s *Store) GetSettings(ctx context.Context) (*Settings, error) {
	return nil, errNotImplemented
}

// SaveSettings stores all fields of st (secrets encrypted with the key).
func (s *Store) SaveSettings(ctx context.Context, st *Settings) error {
	return errNotImplemented
}

// ===================================================================== users

// GetUser returns the user with id or ErrNotFound.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	return nil, errNotImplemented
}

// GetUserByUsername returns the local account with the given username
// (case-insensitive) or ErrNotFound.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	return nil, errNotImplemented
}

// HasLocalAdmin reports whether at least one local administrator exists.
// The first-run setup is complete once this is true.
func (s *Store) HasLocalAdmin(ctx context.Context) (bool, error) {
	return false, errNotImplemented
}

// CreateLocalAdmin creates a local administrator account (username stored
// lower case). Returns an error if the username is taken.
func (s *Store) CreateLocalAdmin(ctx context.Context, username, name, passwordHash string) (*User, error) {
	return nil, errNotImplemented
}

// SetPasswordHash replaces the password hash of a local account.
func (s *Store) SetPasswordHash(ctx context.Context, userID int64, hash string) error {
	return errNotImplemented
}

// UpsertOIDCUser finds the user for a successful OIDC login and updates it,
// or creates it. Matching order: oidc_subject, then directory_id (if given),
// then email (if given, case-insensitive) among users that have no
// oidc_subject yet (e.g. created by a directory sync). The matched user gets
// oidc_subject, email (lower case), name, directory_id (if given) and
// last_login_at=now updated. Never matches or modifies local admin accounts.
func (s *Store) UpsertOIDCUser(ctx context.Context, in OIDCUser) (*User, error) {
	return nil, errNotImplemented
}

// SetUserGroups replaces the group memberships of a user with the given
// groups, as reported by the identity provider at login. Groups are created
// if they do not exist yet (keyed by ExternalID; Name defaults to ExternalID).
// An existing group's name is only overwritten when ref.Name is non-empty and
// differs from ExternalID.
func (s *Store) SetUserGroups(ctx context.Context, userID int64, groups []GroupRef) error {
	return errNotImplemented
}

// UserRole returns the effective role of a user: RoleAdmin for local admins,
// otherwise the highest role of the user's groups, but at least RoleViewer.
func (s *Store) UserRole(ctx context.Context, userID int64) (Role, error) {
	return RoleNone, errNotImplemented
}

// UserGroups returns the groups a user is a member of, ordered by name.
func (s *Store) UserGroups(ctx context.Context, userID int64) ([]Group, error) {
	return nil, errNotImplemented
}

// ListUsers returns all users with their groups and effective role, ordered
// by name/email.
func (s *Store) ListUsers(ctx context.Context) ([]UserWithGroups, error) {
	return nil, errNotImplemented
}

// ===================================================================== groups

// ListGroups returns all groups ordered by display name, with MemberCount and
// ServiceIDs filled.
func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	return nil, errNotImplemented
}

// GetGroup returns one group (MemberCount and ServiceIDs filled) or ErrNotFound.
func (s *Store) GetGroup(ctx context.Context, id int64) (*Group, error) {
	return nil, errNotImplemented
}

// GroupMembers returns the users in a group ordered by name/email.
func (s *Store) GroupMembers(ctx context.Context, groupID int64) ([]User, error) {
	return nil, errNotImplemented
}

// CreateGroup adds a group manually (before anyone of it signed in). If a
// group with the same external id exists it is returned unchanged (its name
// is set if it was empty/equal to the external id and name is given).
func (s *Store) CreateGroup(ctx context.Context, externalID, name string) (*Group, error) {
	return nil, errNotImplemented
}

// UpdateGroup sets the display name and role of a group.
func (s *Store) UpdateGroup(ctx context.Context, id int64, name string, role Role) error {
	return errNotImplemented
}

// SetGroupServices replaces the services assigned to a group.
func (s *Store) SetGroupServices(ctx context.Context, groupID int64, serviceIDs []int64) error {
	return errNotImplemented
}

// DeleteGroup deletes a group with its memberships and service assignments.
func (s *Store) DeleteGroup(ctx context.Context, id int64) error {
	return errNotImplemented
}

// GroupsToSync returns the groups that matter for directory synchronisation:
// groups with a role or with at least one service assigned.
func (s *Store) GroupsToSync(ctx context.Context) ([]Group, error) {
	return nil, errNotImplemented
}

// SyncGroupMembers applies a directory sync result to one group: sets the
// group name (if non-empty) and last_synced_at=now, finds or creates each
// member (match by directory_id, then by email case-insensitively; update
// directory_id/email/name if empty) and replaces the group's memberships with
// exactly these users. Members without an e-mail address are still recorded.
func (s *Store) SyncGroupMembers(ctx context.Context, groupID int64, name string, members []DirectoryUser) error {
	return errNotImplemented
}

// ===================================================================== services

// ListServices returns all services ordered by category, sort_order, name,
// with GroupIDs filled.
func (s *Store) ListServices(ctx context.Context) ([]Service, error) {
	return nil, errNotImplemented
}

// GetService returns one service (GroupIDs filled) or ErrNotFound.
func (s *Store) GetService(ctx context.Context, id int64) (*Service, error) {
	return nil, errNotImplemented
}

// CreateService inserts svc (ID and CreatedAt are set on svc) together with
// its GroupIDs.
func (s *Store) CreateService(ctx context.Context, svc *Service) error {
	return errNotImplemented
}

// UpdateService updates name, description, category, sort order and replaces
// the GroupIDs of svc.
func (s *Store) UpdateService(ctx context.Context, svc *Service) error {
	return errNotImplemented
}

// DeleteService deletes a service (and its incident/maintenance links).
func (s *Store) DeleteService(ctx context.Context, id int64) error {
	return errNotImplemented
}

// ===================================================================== incidents

// CreateIncident inserts inc (ID, CreatedAt, UpdatedAt set on inc; StartedAt
// defaults to now when zero; ResolvedAt is set when Status is resolved) with
// its Services and a first update (first.IncidentID/ID/CreatedAt are set).
// first.Status should equal inc.Status.
func (s *Store) CreateIncident(ctx context.Context, inc *Incident, first *IncidentUpdate) error {
	return errNotImplemented
}

// AddIncidentUpdate appends an update and sets the incident status to
// upd.Status (resolved_at=now when it becomes resolved, NULL when it is
// reopened) and updated_at=now. If impacts is non-nil the affected services
// are replaced by impacts. upd.ID/CreatedAt are set.
func (s *Store) AddIncidentUpdate(ctx context.Context, incidentID int64, upd *IncidentUpdate, impacts []ServiceImpact) error {
	return errNotImplemented
}

// UpdateIncidentTitle changes the title of an incident.
func (s *Store) UpdateIncidentTitle(ctx context.Context, id int64, title string) error {
	return errNotImplemented
}

// GetIncident returns an incident with Services and all Updates (newest
// first, AuthorName filled) or ErrNotFound.
func (s *Store) GetIncident(ctx context.Context, id int64) (*Incident, error) {
	return nil, errNotImplemented
}

// DeleteIncident deletes an incident with its updates.
func (s *Store) DeleteIncident(ctx context.Context, id int64) error {
	return errNotImplemented
}

// ListOpenIncidents returns unresolved incidents, newest first, with Services
// and the latest update.
func (s *Store) ListOpenIncidents(ctx context.Context) ([]Incident, error) {
	return nil, errNotImplemented
}

// ListIncidentsBetween returns incidents that overlap [from, to): started
// before to and not resolved before from. Newest first, with Services and the
// latest update.
func (s *Store) ListIncidentsBetween(ctx context.Context, from, to time.Time) ([]Incident, error) {
	return nil, errNotImplemented
}

// ===================================================================== maintenance

// CreateMaintenance inserts m with its Services (only IDs are used). ID,
// CreatedAt and UpdatedAt are set on m.
func (s *Store) CreateMaintenance(ctx context.Context, m *Maintenance) error {
	return errNotImplemented
}

// UpdateMaintenance updates title, description, starts_at, ends_at, notify
// and replaces the services. If the new window starts in the future,
// notified_start and notified_end are reset to false; if it ends in the
// future notified_end is reset to false.
func (s *Store) UpdateMaintenance(ctx context.Context, m *Maintenance) error {
	return errNotImplemented
}

// CancelMaintenance marks a maintenance as cancelled.
func (s *Store) CancelMaintenance(ctx context.Context, id int64) error {
	return errNotImplemented
}

// DeleteMaintenance deletes a maintenance.
func (s *Store) DeleteMaintenance(ctx context.Context, id int64) error {
	return errNotImplemented
}

// GetMaintenance returns a maintenance with Services or ErrNotFound.
func (s *Store) GetMaintenance(ctx context.Context, id int64) (*Maintenance, error) {
	return nil, errNotImplemented
}

// ListCurrentMaintenances returns non-cancelled maintenances that have not
// ended at now (in progress or upcoming), ordered by starts_at, with Services.
func (s *Store) ListCurrentMaintenances(ctx context.Context, now time.Time) ([]Maintenance, error) {
	return nil, errNotImplemented
}

// ListMaintenancesBetween returns maintenances (including cancelled) whose
// window overlaps [from, to), newest first, with Services.
func (s *Store) ListMaintenancesBetween(ctx context.Context, from, to time.Time) ([]Maintenance, error) {
	return nil, errNotImplemented
}

// MaintenancesToNotify returns non-cancelled maintenances with notify=true
// that started at or before now but have notified_start=false, or that ended
// at or before now but have notified_end=false. With Services.
func (s *Store) MaintenancesToNotify(ctx context.Context, now time.Time) ([]Maintenance, error) {
	return nil, errNotImplemented
}

// MarkMaintenanceNotified sets notified_start (start=true) or notified_end
// (start=false) to true.
func (s *Store) MarkMaintenanceNotified(ctx context.Context, id int64, start bool) error {
	return errNotImplemented
}

// ===================================================================== notifications

// RecipientsForServices returns the distinct users (with a non-empty e-mail)
// that are members of a group assigned to any of the given services, ordered
// by e-mail.
func (s *Store) RecipientsForServices(ctx context.Context, serviceIDs []int64) ([]Recipient, error) {
	return nil, errNotImplemented
}

// EnqueueEmails inserts pending notifications (next_attempt_at=now).
func (s *Store) EnqueueEmails(ctx context.Context, msgs []OutboundEmail) error {
	return errNotImplemented
}

// DueNotifications returns up to limit pending notifications whose
// next_attempt_at <= now, oldest first.
func (s *Store) DueNotifications(ctx context.Context, now time.Time, limit int) ([]Notification, error) {
	return nil, errNotImplemented
}

// MarkNotificationSent marks a notification as sent (sent_at=now, attempts+1).
func (s *Store) MarkNotificationSent(ctx context.Context, id int64) error {
	return errNotImplemented
}

// MarkNotificationAttemptFailed records a failed attempt: attempts+1,
// last_error=errMsg; if retryAt is nil the status becomes failed, otherwise it
// stays pending with next_attempt_at=*retryAt.
func (s *Store) MarkNotificationAttemptFailed(ctx context.Context, id int64, errMsg string, retryAt *time.Time) error {
	return errNotImplemented
}

// ListNotifications returns the most recent notifications, newest first.
func (s *Store) ListNotifications(ctx context.Context, limit int) ([]Notification, error) {
	return nil, errNotImplemented
}

// DeleteNotificationsBefore removes sent/failed notifications created before t
// (housekeeping).
func (s *Store) DeleteNotificationsBefore(ctx context.Context, t time.Time) error {
	return errNotImplemented
}

// ===================================================================== sessions

// CreateSession stores a session (ID is the token hash).
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	return errNotImplemented
}

// GetSession returns a non-expired session by id hash or ErrNotFound.
func (s *Store) GetSession(ctx context.Context, id string, now time.Time) (*Session, error) {
	return nil, errNotImplemented
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	return errNotImplemented
}

// DeleteUserSessions removes all sessions of a user.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	return errNotImplemented
}

// DeleteExpiredSessions removes sessions with expires_at <= now.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	return errNotImplemented
}
