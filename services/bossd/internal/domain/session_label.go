package domain

import (
	"context"

	"github.com/recurser/bossalib/models"
)

// CreateSessionLabelParams holds inputs for creating a new session label.
type CreateSessionLabelParams struct {
	Name        string
	Color       string
	Description string
}

// UpdateSessionLabelParams holds optional fields for updating a session label.
type UpdateSessionLabelParams struct {
	Name        *string
	Color       *string
	Description *string
}

// SessionLabelStore manages session labels and session-to-label assignments.
type SessionLabelStore interface {
	Create(ctx context.Context, params CreateSessionLabelParams) (*models.SessionLabel, error)
	Get(ctx context.Context, id string) (*models.SessionLabel, error)
	GetByName(ctx context.Context, name string) (*models.SessionLabel, error)
	List(ctx context.Context) ([]*models.SessionLabel, error)
	Update(ctx context.Context, id string, params UpdateSessionLabelParams) (*models.SessionLabel, error)
	Delete(ctx context.Context, id string) ([]string, error)
	SetForSession(ctx context.Context, sessionID string, labelIDs []string) ([]*models.SessionLabel, error)
	ListForSession(ctx context.Context, sessionID string) ([]*models.SessionLabel, error)
	ListForSessions(ctx context.Context, sessionIDs []string) (map[string][]*models.SessionLabel, error)
	ListSessionIDsByLabel(ctx context.Context, labelID string) ([]string, error)
}
