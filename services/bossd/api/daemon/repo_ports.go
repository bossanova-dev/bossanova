package server

import (
	"context"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

type repoReader interface {
	Get(context.Context, string) (*domain.Repo, error)
	List(context.Context) ([]*domain.Repo, error)
}
type repoWriter interface {
	Create(context.Context, domain.CreateRepoParams) (*domain.Repo, error)
	Update(context.Context, string, domain.UpdateRepoParams) (*domain.Repo, error)
	Delete(context.Context, string) error
}
type repoStore interface {
	repoReader
	repoWriter
}
type sessionStartReadiness interface{ SessionStartReadyDeadline() time.Duration }
