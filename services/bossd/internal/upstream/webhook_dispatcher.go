package upstream

import (
	"context"
	"fmt"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// PRRefresher refreshes display state for sessions associated with one PR.
type PRRefresher interface {
	RefreshPR(ctx context.Context, repoOriginURL string, prNumber int) error
}

// WebhookDispatcher routes PR-scoped webhook events to the display poller.
type WebhookDispatcher struct {
	refresher PRRefresher
	logger    zerolog.Logger
}

func NewWebhookDispatcher(refresher PRRefresher, logger zerolog.Logger) *WebhookDispatcher {
	return &WebhookDispatcher{
		refresher: refresher,
		logger:    logger,
	}
}

func (d *WebhookDispatcher) Dispatch(ctx context.Context, ev *pb.WebhookEvent) error {
	if ev == nil {
		return fmt.Errorf("webhook event is nil")
	}
	if ev.PullRequest == 0 {
		d.logger.Debug().
			Str("event_type", ev.GetEventType()).
			Str("repo_origin_url", ev.GetRepoOriginUrl()).
			Msg("skipping webhook event without PR scope")
		return nil
	}
	if ev.RepoOriginUrl == "" {
		return fmt.Errorf("webhook event for PR %d missing repo origin URL", ev.PullRequest)
	}
	if d.refresher == nil {
		return fmt.Errorf("webhook dispatcher refresher not wired")
	}

	prNumber := int(ev.PullRequest)
	if err := d.refresher.RefreshPR(ctx, ev.RepoOriginUrl, prNumber); err != nil {
		return fmt.Errorf("refresh PR %s#%d from webhook: %w", ev.RepoOriginUrl, prNumber, err)
	}

	d.logger.Info().
		Str("event_type", ev.GetEventType()).
		Str("repo_origin_url", ev.GetRepoOriginUrl()).
		Int("pull_request", prNumber).
		Msg("refreshed PR from webhook")
	return nil
}
