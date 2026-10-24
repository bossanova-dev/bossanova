package server

import (
	"context"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

// githubCallbackStore contains callback registration and listing operations.
type githubCallbackStore interface {
	Create(context.Context, domain.CreateGitHubCallbackParams) (*domain.GitHubCallback, *domain.GitHubCallbackConflict, error)
	List(context.Context, domain.ListGitHubCallbacksFilter) ([]*domain.GitHubCallback, error)
	Delete(context.Context, string, string) (domain.DeleteGitHubCallbackOutcome, error)
	MarkExpiredCallbacks(context.Context, time.Time) (int, []domain.ExpiredGitHubCallback, error)
}
