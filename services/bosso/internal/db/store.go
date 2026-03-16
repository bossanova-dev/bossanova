package db

import (
	"context"
	"time"
)

// User represents an authenticated user from OIDC.
type User struct {
	ID        string
	Sub       string // OIDC subject identifier
	Email     string
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Daemon represents a registered daemon instance.
type Daemon struct {
	ID             string
	UserID         string
	Hostname       string
	Endpoint       string // optional ConnectRPC endpoint URL for proxy access
	SessionToken   string
	RepoIDs        []string
	ActiveSessions int
	LastHeartbeat  *time.Time
	Online         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SessionEntry is a lightweight registry entry for routing sessions to daemons.
type SessionEntry struct {
	SessionID string
	DaemonID  string
	Title     string
	State     int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// WebhookConfig maps a repo origin URL to an HMAC secret for webhook verification.
type WebhookConfig struct {
	ID            string
	RepoOriginURL string
	Provider      string
	Secret        string
	CreatedAt     time.Time
}

// AuditEntry is an append-only audit log record.
type AuditEntry struct {
	ID        string
	UserID    *string
	Action    string
	Resource  string
	Detail    *string
	CreatedAt time.Time
}

// --- Store interfaces ---

// CreateUserParams holds parameters for creating a user.
type CreateUserParams struct {
	Sub   string
	Email string
	Name  string
}

// UpdateUserParams holds optional fields for updating a user.
type UpdateUserParams struct {
	Email *string
	Name  *string
}

// UserStore defines the interface for user persistence.
type UserStore interface {
	Create(ctx context.Context, params CreateUserParams) (*User, error)
	Get(ctx context.Context, id string) (*User, error)
	GetBySub(ctx context.Context, sub string) (*User, error)
	List(ctx context.Context) ([]*User, error)
	Update(ctx context.Context, id string, params UpdateUserParams) (*User, error)
	Delete(ctx context.Context, id string) error
}

// CreateDaemonParams holds parameters for registering a daemon.
type CreateDaemonParams struct {
	ID           string // daemon-provided ID
	UserID       string
	Hostname     string
	Endpoint     string // optional ConnectRPC endpoint URL
	SessionToken string
	RepoIDs      []string
}

// UpdateDaemonParams holds optional fields for updating a daemon.
type UpdateDaemonParams struct {
	Hostname       *string
	ActiveSessions *int
	LastHeartbeat  *string // ISO 8601
	Online         *bool
}

// DaemonStore defines the interface for daemon registry persistence.
type DaemonStore interface {
	Create(ctx context.Context, params CreateDaemonParams) (*Daemon, error)
	Get(ctx context.Context, id string) (*Daemon, error)
	GetByToken(ctx context.Context, token string) (*Daemon, error)
	ListByUser(ctx context.Context, userID string) ([]*Daemon, error)
	ListByRepoID(ctx context.Context, repoID string) ([]*Daemon, error)
	Update(ctx context.Context, id string, params UpdateDaemonParams) (*Daemon, error)
	UpdateRepos(ctx context.Context, daemonID string, repoIDs []string) error
	Delete(ctx context.Context, id string) error
}

// CreateSessionEntryParams holds parameters for registering a session.
type CreateSessionEntryParams struct {
	SessionID string
	DaemonID  string
	Title     string
	State     int
}

// UpdateSessionEntryParams holds optional fields for updating a session entry.
type UpdateSessionEntryParams struct {
	DaemonID *string
	Title    *string
	State    *int
}

// SessionRegistryStore defines the interface for session routing persistence.
type SessionRegistryStore interface {
	Create(ctx context.Context, params CreateSessionEntryParams) (*SessionEntry, error)
	Get(ctx context.Context, sessionID string) (*SessionEntry, error)
	ListByDaemon(ctx context.Context, daemonID string) ([]*SessionEntry, error)
	Update(ctx context.Context, sessionID string, params UpdateSessionEntryParams) (*SessionEntry, error)
	Delete(ctx context.Context, sessionID string) error
}

// CreateAuditParams holds parameters for creating an audit entry.
type CreateAuditParams struct {
	UserID   *string
	Action   string
	Resource string
	Detail   *string
}

// AuditStore defines the interface for audit log persistence.
type AuditStore interface {
	Create(ctx context.Context, params CreateAuditParams) (*AuditEntry, error)
	List(ctx context.Context, opts AuditListOpts) ([]*AuditEntry, error)
}

// AuditListOpts configures audit log queries.
type AuditListOpts struct {
	UserID *string
	Action *string
	Limit  int
}

// CreateWebhookConfigParams holds parameters for creating a webhook config.
type CreateWebhookConfigParams struct {
	RepoOriginURL string
	Provider      string
	Secret        string
}

// WebhookConfigStore defines the interface for webhook config persistence.
type WebhookConfigStore interface {
	Create(ctx context.Context, params CreateWebhookConfigParams) (*WebhookConfig, error)
	Get(ctx context.Context, id string) (*WebhookConfig, error)
	GetByRepo(ctx context.Context, repoOriginURL, provider string) (*WebhookConfig, error)
	List(ctx context.Context) ([]*WebhookConfig, error)
	Delete(ctx context.Context, id string) error
}
