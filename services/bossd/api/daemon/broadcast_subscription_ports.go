package server

import (
	"context"

	"github.com/recurser/bossd/internal/domain"
)

// broadcastSubscriptionStore excludes firing operations owned by the evaluator.
type broadcastSubscriptionStore interface {
	Create(ctx context.Context, sub *domain.BroadcastSubscription) error
	Get(ctx context.Context, id string) (*domain.BroadcastSubscription, error)
	List(ctx context.Context, filter domain.ListBroadcastSubscriptionsFilter) ([]*domain.BroadcastSubscription, error)
	MarkCanceled(ctx context.Context, id string) (bool, error)
}
