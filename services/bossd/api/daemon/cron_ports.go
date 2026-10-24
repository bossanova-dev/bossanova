package server

import (
	"context"

	"github.com/recurser/bossd/internal/domain"
)

// cronJobStore contains the scheduling records consumed by the daemon RPCs.
type cronJobStore interface {
	Create(context.Context, domain.CreateCronJobParams) (*domain.CronJob, error)
	Get(context.Context, string) (*domain.CronJob, error)
	List(context.Context) ([]*domain.CronJob, error)
	ListByRepo(context.Context, string) ([]*domain.CronJob, error)
	Update(context.Context, string, domain.UpdateCronJobParams) (*domain.CronJob, error)
	Delete(context.Context, string) error
}
