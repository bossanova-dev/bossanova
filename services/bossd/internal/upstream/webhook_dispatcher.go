package upstream

import (
	"context"
	"fmt"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"github.com/rs/zerolog"
)

// PRRefresher refreshes display state for sessions associated with one PR.
type PRRefresher interface {
	RefreshPR(ctx context.Context, repoOriginURL string, prNumber int) error
}

// SessionEventEmitter emits webhook-derived events to matching sessions.
type SessionEventEmitter interface {
	EmitForPR(ctx context.Context, repoOriginURL string, prNumber int, events []vcs.Event) error
}

// ReviewCommentProvider fetches review feedback when webhook payloads only
// report that changes were requested.
type ReviewCommentProvider interface {
	GetReviewComments(ctx context.Context, repoPath string, prID int) ([]vcs.ReviewComment, error)
}

// WebhookDispatcher routes PR-scoped webhook events to the display poller.
type WebhookDispatcher struct {
	refresher      PRRefresher
	emitter        SessionEventEmitter
	reviewComments ReviewCommentProvider
	logger         zerolog.Logger
}

func NewWebhookDispatcher(refresher PRRefresher, logger zerolog.Logger) *WebhookDispatcher {
	return NewWebhookDispatcherWithEmitter(refresher, nil, logger)
}

func NewWebhookDispatcherWithEmitter(refresher PRRefresher, emitter SessionEventEmitter, logger zerolog.Logger) *WebhookDispatcher {
	return NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, nil, logger)
}

func NewWebhookDispatcherWithEmitterAndReviewComments(refresher PRRefresher, emitter SessionEventEmitter, reviewComments ReviewCommentProvider, logger zerolog.Logger) *WebhookDispatcher {
	return &WebhookDispatcher{
		refresher:      refresher,
		emitter:        emitter,
		reviewComments: reviewComments,
		logger:         logger,
	}
}

func (d *WebhookDispatcher) Dispatch(ctx context.Context, ev *pb.WebhookEvent) error {
	if ev == nil {
		return fmt.Errorf("webhook event is nil")
	}
	if ev.RepoOriginUrl == "" {
		return fmt.Errorf("webhook event for PR %d missing repo origin URL", ev.PullRequest)
	}
	if d.refresher == nil {
		return fmt.Errorf("webhook dispatcher refresher not wired")
	}

	payloadPR := d.maybeEmitRealtime(ctx, ev)

	prNumber := int(ev.PullRequest)
	if prNumber == 0 {
		prNumber = payloadPR
	}
	if prNumber == 0 {
		return nil
	}

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

func (d *WebhookDispatcher) maybeEmitRealtime(ctx context.Context, ev *pb.WebhookEvent) int {
	if len(ev.GetPayload()) == 0 {
		return 0
	}

	events, prNumber, err := TranslateWebhook(ev.GetEventType(), ev.GetPayload())
	if err != nil {
		d.logger.Warn().
			Err(err).
			Str("event_type", ev.GetEventType()).
			Str("repo_origin_url", ev.GetRepoOriginUrl()).
			Int("pull_request", int(ev.GetPullRequest())).
			Msg("failed to translate webhook payload for realtime emission")
		return 0
	}
	envelopePR := int(ev.GetPullRequest())
	if envelopePR != 0 && prNumber != 0 && prNumber != envelopePR {
		d.logger.Warn().
			Str("event_type", ev.GetEventType()).
			Str("repo_origin_url", ev.GetRepoOriginUrl()).
			Int("payload_pull_request", prNumber).
			Int("envelope_pull_request", envelopePR).
			Msg("skipping realtime webhook emission for mismatched PR scope")
		return 0
	}
	if prNumber == 0 {
		prNumber = envelopePR
	}
	if d.emitter == nil || len(events) == 0 {
		return prNumber
	}
	events, ok := d.enrichReviewComments(ctx, ev.GetRepoOriginUrl(), prNumber, events)
	if !ok {
		return prNumber
	}

	if err := d.emitter.EmitForPR(ctx, ev.GetRepoOriginUrl(), prNumber, events); err != nil {
		d.logger.Warn().
			Err(err).
			Str("event_type", ev.GetEventType()).
			Str("repo_origin_url", ev.GetRepoOriginUrl()).
			Int("pull_request", prNumber).
			Msg("failed to emit realtime webhook events")
	}
	return prNumber
}

func (d *WebhookDispatcher) enrichReviewComments(ctx context.Context, repoOriginURL string, prNumber int, events []vcs.Event) ([]vcs.Event, bool) {
	enriched := make([]vcs.Event, len(events))
	copy(enriched, events)
	for i, event := range enriched {
		review, ok := event.(vcs.ReviewSubmitted)
		if !ok || review.State != vcs.ReviewStateChangesRequested {
			continue
		}
		if d.reviewComments == nil {
			if len(review.Comments) > 0 {
				continue
			}
			d.logger.Warn().
				Str("repo_origin_url", repoOriginURL).
				Int("pull_request", prNumber).
				Msg("skipping realtime review emission without review comment provider")
			return nil, false
		}
		comments, err := d.reviewComments.GetReviewComments(ctx, repoOriginURL, prNumber)
		if err != nil {
			d.logger.Warn().
				Err(err).
				Str("repo_origin_url", repoOriginURL).
				Int("pull_request", prNumber).
				Msg("failed to fetch review comments for realtime emission")
			return nil, false
		}
		review.Comments = append(review.Comments, comments...)
		enriched[i] = review
	}
	return enriched, true
}
