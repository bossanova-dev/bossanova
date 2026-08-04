package upstream

import (
	"context"
	"fmt"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/vcs"
	"github.com/rs/zerolog"
)

// callbackEvalTimeout bounds a detached callback-evaluation goroutine so a hung
// GitHub round-trip cannot leak indefinitely. Generous relative to normal API
// latency; the acknowledgement path never waits on it regardless.
const callbackEvalTimeout = 30 * time.Second

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

// ReviewThreadFreshnessProvider reports which bot logins still own a blocking
// review thread on a PR. The realtime path uses it to establish that a bot
// COMMENTED review's feedback is still live before promoting it to
// CHANGES_REQUESTED: a delayed or redelivered webhook can arrive after the
// anchored hunk was rewritten, and promoting then restarts a fix loop over
// feedback that is already moot (BOS-669). Optional — see
// reviewAuthorHasBlockingThread for the behaviour when it is absent.
type ReviewThreadFreshnessProvider interface {
	BlockingThreadAuthors(ctx context.Context, repoPath string, prID int, botUsers map[string]bool) (map[string]bool, error)
}

// CallbackEvaluator verifies authoritative GitHub state for a PR and fires any
// matching durable callbacks. The dispatcher invokes it after resolving the
// affected PR from a webhook. Implemented by callback.Evaluator; optional.
type CallbackEvaluator interface {
	EvaluatePR(ctx context.Context, repoOwner, repoName string, prNumber int) error
}

// WebhookDispatcher routes PR-scoped webhook events to the display poller.
type WebhookDispatcher struct {
	refresher      PRRefresher
	emitter        SessionEventEmitter
	reviewComments ReviewCommentProvider
	evaluator      CallbackEvaluator
	logger         zerolog.Logger
	// evalDoneHook, when set (tests only), receives the done channel of each
	// detached callback-evaluation goroutine so a test can await completion.
	// The production acknowledgement path never blocks on it.
	evalDoneHook func(done <-chan struct{})
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

// WithEvaluator attaches a CallbackEvaluator invoked for the affected PR after a
// webhook resolves. Additive: existing call sites that never call this keep a
// nil evaluator (no callback evaluation). Returns d for chaining.
func (d *WebhookDispatcher) WithEvaluator(evaluator CallbackEvaluator) *WebhookDispatcher {
	d.evaluator = evaluator
	return d
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

	// Fire durable GitHub callbacks for the affected PR by verifying
	// authoritative state. Launched detached (see evaluateCallbacks): the
	// webhook ACK bosso returns to GitHub waits on this Dispatch completing, and
	// EvaluatePR makes GitHub round-trips, so it must never run on the
	// acknowledgement path. Returns immediately; failures are logged in the
	// background goroutine.
	d.evaluateCallbacks(ctx, ev.GetRepoOriginUrl(), prNumber)

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

// evaluateCallbacks verifies authoritative GitHub state and fires durable
// callbacks for repoOriginURL#prNumber. It derives owner/name from the origin
// URL (reusing vcs.GitHubNWO) and is a no-op when no evaluator is wired or the
// URL is not a parseable GitHub NWO.
//
// The evaluation is launched detached and returns immediately: EvaluatePR makes
// GitHub round-trips, and the webhook acknowledgement bosso returns to GitHub
// waits on Dispatch completing (command_dispatcher awaits Dispatch before
// WebhookAck), so running inline would put GitHub latency on the ACK path — the
// design forbids it. context.WithoutCancel keeps request-scoped values but
// survives Dispatch returning; a timeout bounds a hung call. safego.Go recovers
// panics so a callback bug can never crash the daemon.
func (d *WebhookDispatcher) evaluateCallbacks(ctx context.Context, repoOriginURL string, prNumber int) {
	if d.evaluator == nil || prNumber <= 0 {
		return
	}
	nwo := vcs.GitHubNWO(repoOriginURL)
	owner, name, ok := strings.Cut(nwo, "/")
	if !ok || owner == "" || name == "" {
		return
	}
	evalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callbackEvalTimeout)
	done := safego.Go(d.logger, func() {
		defer cancel()
		if err := d.evaluator.EvaluatePR(evalCtx, owner, name, prNumber); err != nil {
			d.logger.Warn().
				Err(err).
				Str("repo_origin_url", repoOriginURL).
				Int("pull_request", prNumber).
				Msg("callback evaluation failed")
		}
	})
	if d.evalDoneHook != nil {
		d.evalDoneHook(done)
	}
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
		// feedback must be promoted here. The condition ANDs a review-scoped signal
		// with an author-scoped one, which is structurally the same conjunction the
		// provider-side promotion applies (github.Provider.promoteBotCommentedReview,
		// feeding the display-poller path that keys off per-comment State):
		//
		//   - review-scoped: only a review carrying its own changes-requested inline
		//     comment(s) is a candidate, so an empty bot COMMENTED review (e.g.
		//     chatgpt-codex-connector[bot]'s per-commit boilerplate) or a benign
		//     body-only review ("LGTM", "No issues found") is never treated as
		//     changes-requested (BOS-254). The summary body is deliberately not
		//     consulted: benign prose is indistinguishable from a change request.
		//   - author-scoped: the author must still own a review thread that blocks
		//     — unresolved and not outdated. Since BOS-669 both paths read that from
		//     the same predicate (github.Provider.BlockingThreadAuthors), so a
		//     delayed or redelivered webhook whose anchored hunk was rewritten in the
		//     interim no longer restarts a fix loop over moot feedback.
		//
		// The switch evaluates its cases in order and stops at the first match, so
		// the freshness round-trip runs only for a review that would otherwise have
		// been promoted — the non-candidate case above absorbs everything else.
		// Keep that ordering: this runs on the webhook acknowledgement path (see
		// evaluateCallbacks), and the query is a `gh` subprocess, not a cheap call.
		switch {
		case review.State != vcs.ReviewStateCommented || !hasActionableBotReviewComments(review, comments):
			// Not a promote candidate: no freshness query, no decision log.
		case d.reviewAuthorHasBlockingThread(ctx, repoOriginURL, prNumber, review.Author):
			review.State = vcs.ReviewStateChangesRequested
			d.logger.Info().
				Str("repo_origin_url", repoOriginURL).
				Int("pull_request", prNumber).
				Int64("review_id", review.ReviewID).
				Str("author", review.Author).
				Msg("realtime review promoted to changes-requested")
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

// reviewAuthorHasBlockingThread reports whether this review's author still owns
// a blocking review thread on the PR — the realtime path's freshness check.
//
// It returns false, and thereby SUPPRESSES the promotion, whenever freshness
// cannot be established: the provider does not implement the capability, or the
// query fails. That is the same posture the poller path takes with
// vcs.ErrReviewThreadsUnverified — refuse to act on unverifiable review state —
// even though the resulting behaviour differs (there it blocks a merge; here it
// declines to start a fix loop). Both refuse to act.
//
// Suppressing is the safe direction because two independent lanes still see
// genuinely-live bot feedback: the merge gate (server.liveMergeBlock ->
// GetReviewComments) refuses the merge, and the display poller — which
// recomputes review state through the provider-side promotion — reaches
// DISPLAY_STATUS_REJECTED, off which the repair plugin dispatches. Neither
// depends on this promotion, so the cost of a wrong suppression is latency,
// while the cost of a wrong promotion is the BOS-665 livelock this exists to
// avoid (BOS-669).
func (d *WebhookDispatcher) reviewAuthorHasBlockingThread(ctx context.Context, repoOriginURL string, prNumber int, author string) bool {
	provider, ok := d.reviewComments.(ReviewThreadFreshnessProvider)
	if !ok {
		d.logger.Warn().
			Str("repo_origin_url", repoOriginURL).
			Int("pull_request", prNumber).
			Str("author", author).
			Msg("realtime review promotion suppressed: no review-thread freshness capability")
		return false
	}
	blocking, err := provider.BlockingThreadAuthors(ctx, repoOriginURL, prNumber, map[string]bool{author: true})
	if err != nil {
		d.logger.Warn().
			Err(err).
			Str("repo_origin_url", repoOriginURL).
			Int("pull_request", prNumber).
			Str("author", author).
			Msg("realtime review promotion suppressed: review-thread freshness unverifiable")
		return false
	}
	if !blocking[author] {
		d.logger.Info().
			Str("repo_origin_url", repoOriginURL).
			Int("pull_request", prNumber).
			Str("author", author).
			Msg("realtime review promotion suppressed: author has no blocking review thread")
		return false
	}
	return true
}

// hasActionableBotReviewComments reports whether a bot's COMMENTED review is
// review-scoped actionable: it carries its own changes-requested inline
// comment(s). An empty bot review, or a benign body-only one ("LGTM",
// "No issues found"), is never actionable, so it is not promoted to
// CHANGES_REQUESTED (BOS-254). The realtime path intentionally does not consult
// the summary body: benign prose cannot be told apart from a change request, so
// promoting on a substantive body alone would false-block. This is the
// review-scoped half of the promotion condition; reviewAuthorHasBlockingThread
// supplies the author-scoped half, in sync with the provider-side promotion in
// github.Provider.promoteBotCommentedReview.
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
