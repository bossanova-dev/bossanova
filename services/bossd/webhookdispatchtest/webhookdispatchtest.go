// Package webhookdispatchtest exposes bossd's internal webhook dispatcher to
// cross-service integration tests.
package webhookdispatchtest

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/upstream"
	"github.com/rs/zerolog"
)

type PRRefresher interface {
	RefreshPR(ctx context.Context, repoOriginURL string, prNumber int) error
}

func Dispatch(ctx context.Context, refresher PRRefresher, ev *pb.WebhookEvent) error {
	return upstream.NewWebhookDispatcher(refresher, zerolog.Nop()).Dispatch(ctx, ev)
}
