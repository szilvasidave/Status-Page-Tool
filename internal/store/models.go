package store

import (
	"errors"
	"slices"
	"time"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// ---------------------------------------------------------------- roles

// Role is a user's permission level. Higher values include lower ones.
type Role int

const (
	RoleNone   Role = 0 // anonymous
	RoleViewer Role = 1 // any signed-in user
	RoleEditor Role = 2 // may post incidents and maintenance windows
	RoleAdmin  Role = 3 // may change configuration
)

// String returns "admin", "editor", "viewer" or "" (RoleNone); ParseRole
// reverses it. idp_groups.role stores "editor", "admin" or "" (no special
// role, members are viewers); UpdateGroup stores "" for RoleViewer.
func (r Role) String() string {
	switch r {
	case RoleAdmin:
		return "admin"
	case RoleEditor:
		return "editor"
	case RoleViewer:
		return "viewer"
	}
	return ""
}

// Label is a human readable role name.
func (r Role) Label() string {
	switch r {
	case RoleAdmin:
		return "Administrator"
	case RoleEditor:
		return "Editor"
	case RoleViewer:
		return "Viewer"
	}
	return "None"
}

// ParseRole parses "admin", "editor", "viewer"; anything else is RoleNone.
func ParseRole(s string) Role {
	switch s {
	case "admin":
		return RoleAdmin
	case "editor":
		return RoleEditor
	case "viewer":
		return RoleViewer
	}
	return RoleNone
}

// ---------------------------------------------------------------- settings

// Settings are the application settings stored in the settings table and
// edited by administrators in the web UI. Secrets are stored encrypted and
// returned decrypted.
type Settings struct {
	SiteName     string // shown in the header and e-mails
	BaseURL      string // public URL without trailing slash, e.g. https://status.example.com
	PublicAccess bool   // status pages visible without signing in
	Timezone     string // IANA name used to display and enter times

	OIDCEnabled      bool
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string // secret
	OIDCScopes       string // space separated
	OIDCGroupsClaim  string // name of the ID token claim containing group ids/names
	OIDCButtonLabel  string

	// DirectorySyncEnabled turns on group member synchronisation from
	// Microsoft Entra ID via Microsoft Graph using the OIDC client
	// credentials.
	DirectorySyncEnabled bool

	SMTPHost     string
	SMTPPort     int
	SMTPSecurity string // "starttls", "tls" or "none"
	SMTPUsername string
	SMTPPassword string // secret
	SMTPFrom     string // e.g. "IT Status <status@example.com>"
}

// DefaultSettings returns the settings used for keys that are not stored yet.
func DefaultSettings() Settings {
	return Settings{
		SiteName:        "Status",
		PublicAccess:    true,
		Timezone:        "UTC",
		OIDCScopes:      "openid profile email",
		OIDCGroupsClaim: "groups",
		OIDCButtonLabel: "Sign in with SSO",
		SMTPPort:        587,
		SMTPSecurity:    "starttls",
	}
}

// Location returns the configured time zone (UTC if invalid).
func (s *Settings) Location() *time.Location {
	if s == nil || s.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// SMTPConfigured reports whether e-mail can be sent.
func (s *Settings) SMTPConfigured() bool {
	return s != nil && s.SMTPHost != "" && s.SMTPFrom != ""
}

// OIDCConfigured reports whether single sign-on is enabled and complete.
func (s *Settings) OIDCConfigured() bool {
	return s != nil && s.OIDCEnabled && s.OIDCIssuer != "" && s.OIDCClientID != ""
}

// ---------------------------------------------------------------- users & groups

type User struct {
	ID           int64
	Email        string // lower case, may be empty for local accounts
	Name         string
	OIDCSubject  string // empty if the user never signed in via OIDC
	DirectoryID  string // identity provider object id (e.g. Entra "oid"), may be empty
	Username     string // local accounts only
	PasswordHash string // local accounts only
	IsLocalAdmin bool
	LastLoginAt  *time.Time
	CreatedAt    time.Time
}

// DisplayName returns the best available human readable name.
func (u *User) DisplayName() string {
	switch {
	case u == nil:
		return ""
	case u.Name != "":
		return u.Name
	case u.Email != "":
		return u.Email
	}
	return u.Username
}

// UserWithGroups is used by the admin users list.
type UserWithGroups struct {
	User
	Role   Role
	Groups []GroupRef
}

// Group is a group that originates from the identity provider.
type Group struct {
	ID           int64
	ExternalID   string // value as it appears in the groups claim / directory object id
	Name         string // display name (falls back to ExternalID)
	Role         Role   // RoleNone (members are viewers), RoleEditor or RoleAdmin
	MemberCount  int
	ServiceIDs   []int64
	LastSyncedAt *time.Time
	CreatedAt    time.Time
}

// DisplayName returns Name or ExternalID.
func (g *Group) DisplayName() string {
	if g.Name != "" {
		return g.Name
	}
	return g.ExternalID
}

// GroupRef identifies a group by its identity provider id with an optional
// display name.
type GroupRef struct {
	ExternalID string
	Name       string
}

// OIDCUser is the identity extracted from a successful OIDC login.
type OIDCUser struct {
	Subject     string
	Email       string
	Name        string
	DirectoryID string // e.g. Entra ID "oid" claim, may be empty
}

// DirectoryUser is a group member returned by a directory sync.
type DirectoryUser struct {
	DirectoryID string
	Email       string
	Name        string
}

// Recipient is a user that should receive a notification e-mail.
type Recipient struct {
	UserID int64
	Email  string
	Name   string
}

// Session is a signed-in browser session. ID is the SHA-256 hash (hex) of the
// token stored in the cookie.
type Session struct {
	ID        string
	UserID    int64
	ExpiresAt time.Time
	CreatedAt time.Time
}

// ---------------------------------------------------------------- services & status

type Service struct {
	ID          int64
	Name        string
	Description string
	Category    string // optional heading on the status page
	SortOrder   int
	GroupIDs    []int64 // groups notified about this service
	CreatedAt   time.Time
}

// ServiceRef is a service id with its name.
type ServiceRef struct {
	ID   int64
	Name string
}

// Status is the status of a service, also used as the impact of an incident
// on a service (degraded, partial_outage, major_outage).
type Status string

const (
	StatusOperational   Status = "operational"
	StatusMaintenance   Status = "maintenance"
	StatusDegraded      Status = "degraded"
	StatusPartialOutage Status = "partial_outage"
	StatusMajorOutage   Status = "major_outage"
)

// Impacts lists the statuses an incident can assign to a service, mildest first.
var Impacts = []Status{StatusDegraded, StatusPartialOutage, StatusMajorOutage}

// ValidImpact reports whether s can be used as an incident impact.
func ValidImpact(s Status) bool { return slices.Contains(Impacts, s) }

// Severity orders statuses: operational < maintenance < degraded < partial < major.
func (s Status) Severity() int {
	switch s {
	case StatusMaintenance:
		return 1
	case StatusDegraded:
		return 2
	case StatusPartialOutage:
		return 3
	case StatusMajorOutage:
		return 4
	}
	return 0
}

// Label returns a human readable label.
func (s Status) Label() string {
	switch s {
	case StatusMaintenance:
		return "Under maintenance"
	case StatusDegraded:
		return "Degraded performance"
	case StatusPartialOutage:
		return "Partial outage"
	case StatusMajorOutage:
		return "Major outage"
	}
	return "Operational"
}

// Worst returns the more severe of a and b.
func Worst(a, b Status) Status {
	if b.Severity() > a.Severity() {
		return b
	}
	if a == "" {
		return StatusOperational
	}
	return a
}

// ---------------------------------------------------------------- incidents

type IncidentStatus string

const (
	IncidentInvestigating IncidentStatus = "investigating"
	IncidentIdentified    IncidentStatus = "identified"
	IncidentMonitoring    IncidentStatus = "monitoring"
	IncidentResolved      IncidentStatus = "resolved"
)

// IncidentStatuses lists all incident statuses in workflow order.
var IncidentStatuses = []IncidentStatus{IncidentInvestigating, IncidentIdentified, IncidentMonitoring, IncidentResolved}

// ValidIncidentStatus reports whether s is a known incident status.
func ValidIncidentStatus(s IncidentStatus) bool { return slices.Contains(IncidentStatuses, s) }

func (s IncidentStatus) Label() string {
	switch s {
	case IncidentInvestigating:
		return "Investigating"
	case IncidentIdentified:
		return "Identified"
	case IncidentMonitoring:
		return "Monitoring"
	case IncidentResolved:
		return "Resolved"
	}
	return string(s)
}

type Incident struct {
	ID         int64
	Title      string
	Status     IncidentStatus
	StartedAt  time.Time
	ResolvedAt *time.Time
	CreatedBy  int64 // 0 if unknown/deleted user
	CreatedAt  time.Time
	UpdatedAt  time.Time

	// Services affected by the incident (ordered like the status page).
	Services []ServiceImpact
	// Updates, newest first. Populated by GetIncident; list methods only
	// populate the latest update.
	Updates []IncidentUpdate
}

// ServiceImpact is the impact of an incident on one service.
type ServiceImpact struct {
	ServiceID   int64
	ServiceName string // filled when read from the database
	Impact      Status
}

type IncidentUpdate struct {
	ID         int64
	IncidentID int64
	Status     IncidentStatus
	Message    string
	CreatedBy  int64
	AuthorName string // display name of CreatedBy, filled when read
	CreatedAt  time.Time
}

// Resolved reports whether the incident is resolved.
func (i *Incident) Resolved() bool { return i.Status == IncidentResolved }

// Impact returns the worst impact over all affected services.
func (i *Incident) Impact() Status {
	worst := StatusOperational
	for _, s := range i.Services {
		worst = Worst(worst, s.Impact)
	}
	return worst
}

// ServiceIDs returns the ids of the affected services.
func (i *Incident) ServiceIDs() []int64 {
	ids := make([]int64, 0, len(i.Services))
	for _, s := range i.Services {
		ids = append(ids, s.ServiceID)
	}
	return ids
}

// ImpactOn returns the impact on service id ("" if not affected).
func (i *Incident) ImpactOn(serviceID int64) Status {
	for _, s := range i.Services {
		if s.ServiceID == serviceID {
			return s.Impact
		}
	}
	return ""
}

// ---------------------------------------------------------------- maintenance

type MaintenanceState string

const (
	MaintenanceScheduled  MaintenanceState = "scheduled"
	MaintenanceInProgress MaintenanceState = "in_progress"
	MaintenanceCompleted  MaintenanceState = "completed"
	MaintenanceCancelled  MaintenanceState = "cancelled"
)

func (s MaintenanceState) Label() string {
	switch s {
	case MaintenanceScheduled:
		return "Scheduled"
	case MaintenanceInProgress:
		return "In progress"
	case MaintenanceCompleted:
		return "Completed"
	case MaintenanceCancelled:
		return "Cancelled"
	}
	return string(s)
}

type Maintenance struct {
	ID            int64
	Title         string
	Description   string
	StartsAt      time.Time
	EndsAt        time.Time
	Cancelled     bool
	Notify        bool // send e-mails when scheduled, started and completed
	NotifiedStart bool
	NotifiedEnd   bool
	CreatedBy     int64
	CreatedAt     time.Time
	UpdatedAt     time.Time

	Services []ServiceRef // affected services (filled when read)
}

// State derives the maintenance state at time now.
func (m *Maintenance) State(now time.Time) MaintenanceState {
	switch {
	case m.Cancelled:
		return MaintenanceCancelled
	case now.Before(m.StartsAt):
		return MaintenanceScheduled
	case now.Before(m.EndsAt):
		return MaintenanceInProgress
	}
	return MaintenanceCompleted
}

// ServiceIDs returns the ids of the affected services.
func (m *Maintenance) ServiceIDs() []int64 {
	ids := make([]int64, 0, len(m.Services))
	for _, s := range m.Services {
		ids = append(ids, s.ID)
	}
	return ids
}

// ---------------------------------------------------------------- notifications

const (
	NotificationPending = "pending"
	NotificationSent    = "sent"
	NotificationFailed  = "failed"
)

// OutboundEmail is a message to enqueue in the notifications outbox.
type OutboundEmail struct {
	UserID   int64 // 0 if not tied to a user
	Email    string
	Subject  string
	BodyText string
	BodyHTML string
}

// Notification is a row of the notifications outbox.
type Notification struct {
	ID            int64
	UserID        int64
	Email         string
	Subject       string
	BodyText      string
	BodyHTML      string
	Status        string // pending, sent, failed
	Attempts      int
	LastError     string
	NextAttemptAt time.Time
	CreatedAt     time.Time
	SentAt        *time.Time
}
