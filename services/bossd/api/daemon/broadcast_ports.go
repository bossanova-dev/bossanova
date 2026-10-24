package server

import (
	"context"

	"github.com/recurser/bossd/internal/domain"
)

// broadcastStore contains the broadcast operations consumed by the daemon RPCs.
type broadcastStore interface {
	Create(ctx context.Context, b *domain.Broadcast) error
	CreateDeliveries(ctx context.Context, broadcastID string, targets []domain.BroadcastDelivery) error
	MarkCompletedIfSettled(ctx context.Context, broadcastID string) error
	Get(ctx context.Context, id string) (*domain.Broadcast, error)
	List(ctx context.Context, filter domain.ListBroadcastsFilter) ([]*domain.Broadcast, error)
	Delete(ctx context.Context, id string) error
}
