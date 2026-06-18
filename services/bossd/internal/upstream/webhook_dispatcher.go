package upstream

import (
	"context"
	"fmt"
	"strings"

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
// report review summary state. Providers also classify bot COMMENTED reviews
// with actionable inline feedback as changes requested.
type ReviewCommentProvider interface {
	GetReviewComments(ctx context.Context, repoPath string, prID int) ([]vcs.ReviewComment, error)
}

// ReviewInlineCommentProvider fetches comments attached to one submitted review.
type ReviewInlineCommentProvider interface {
	GetReviewInlineComments(ctx context.Context, repoPath string, prID int, reviewID int64) ([]vcs.ReviewComment, error)
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

// eventTypeRequiresPR reports whether a GitHub webhook event type is
// intrinsically scoped to a pull request, so a missing PR number signals a
// resolution bug rather than routine non-PR traffic. The values match the
// X-GitHub-Event header strings forwarded by bosso.
func eventTypeRequiresPR(eventType string) bool {
	switch eventType {
	case "pull_request",
		"pull_request_review",
		"pull_request_review_comment",
		"pull_request_review_thread":
		return true
	default:
		return false
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
		// Only PR-scoped event types are expected to always carry a PR
		// number; for those a zero here points at a real resolution
		// bug worth a WARN. Events like ping, check_suite/check_run on
		// non-PR refs, and issue_comment on plain issues legitimately
		// resolve to zero, so they log at Debug to avoid drowning the
		// signal in routine traffic.
		level := zerolog.DebugLevel
		if eventTypeRequiresPR(ev.GetEventType()) {
			level = zerolog.WarnLevel
		}
		d.logger.WithLevel(level).
			Str("event_type", ev.GetEventType()).
			Str("repo_origin_url", ev.GetRepoOriginUrl()).
			Int("envelope_pull_request", int(ev.GetPullRequest())).
			Msg("webhook: no PR number resolved; skipping dispatch")
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
		if !ok || !reviewNeedsCommentEnrichment(review.State) {
			continue
		}
		if d.reviewComments == nil {
			if review.State == vcs.ReviewStateCommented || len(review.Comments) > 0 {
				continue
			}
			d.logger.Warn().
				Str("repo_origin_url", repoOriginURL).
				Int("pull_request", prNumber).
				Msg("skipping realtime review emission without review comment provider")
			return nil, false
		}
		comments, err := d.getSubmittedReviewComments(ctx, repoOriginURL, prNumber, review)
		if err != nil {
			d.logger.Warn().
				Err(err).
				Str("repo_origin_url", repoOriginURL).
				Int("pull_request", prNumber).
				Msg("failed to fetch review comments for realtime emission")
			return nil, false
		}
		review.Comments = append(review.Comments, comments...)
		// Realtime-path promotion: handleReviewSubmitted gates the fix loop on the
		// review's summary State, so a bot COMMENTED review with actionable inline
		// feedback must be promoted here. This mirrors the provider-side promotion
		// in github.Provider.GetReviewComments (which feeds the display-poller path
		// that keys off per-comment State); keep both in sync.
		if review.State == vcs.ReviewStateCommented && hasActionableBotReviewComments(review, comments) {
			review.State = vcs.ReviewStateChangesRequested
		}
		enriched[i] = review
	}
	return enriched, true
}

func (d *WebhookDispatcher) getSubmittedReviewComments(ctx context.Context, repoOriginURL string, prNumber int, review vcs.ReviewSubmitted) ([]vcs.ReviewComment, error) {
	if review.ReviewID != 0 {
		if provider, ok := d.reviewComments.(ReviewInlineCommentProvider); ok {
			return provider.GetReviewInlineComments(ctx, repoOriginURL, prNumber, review.ReviewID)
		}
	}
	return d.reviewComments.GetReviewComments(ctx, repoOriginURL, prNumber)
}

func reviewNeedsCommentEnrichment(state vcs.ReviewState) bool {
	return state == vcs.ReviewStateChangesRequested || state == vcs.ReviewStateCommented
}

func hasActionableBotReviewComments(review vcs.ReviewSubmitted, comments []vcs.ReviewComment) bool {
	if !strings.HasSuffix(review.Author, "[bot]") {
		return false
	}
	for _, comment := range comments {
		if comment.State == vcs.ReviewStateChangesRequested {
			return true
		}
	}
	return false
}
