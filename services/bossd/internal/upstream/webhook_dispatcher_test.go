package upstream

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"github.com/rs/zerolog"
)

type refreshCall struct {
	repoOriginURL string
	prNumber      int
}

type fakePRRefresher struct {
	calls []refreshCall
	err   error
}

func (f *fakePRRefresher) RefreshPR(_ context.Context, repoOriginURL string, prNumber int) error {
	f.calls = append(f.calls, refreshCall{
		repoOriginURL: repoOriginURL,
		prNumber:      prNumber,
	})
	return f.err
}

type emitCall struct {
	repoOriginURL string
	prNumber      int
	events        []vcs.Event
}

type fakeEmitter struct {
	calls []emitCall
	err   error
}

func (f *fakeEmitter) EmitForPR(_ context.Context, repoOriginURL string, prNumber int, events []vcs.Event) error {
	f.calls = append(f.calls, emitCall{
		repoOriginURL: repoOriginURL,
		prNumber:      prNumber,
		events:        events,
	})
	return f.err
}

type reviewCommentCall struct {
	repoOriginURL string
	prNumber      int
	reviewID      int64
}

type fakeReviewCommentProvider struct {
	calls          []reviewCommentCall
	inlineCalls    []reviewCommentCall
	comments       []vcs.ReviewComment
	inlineComments []vcs.ReviewComment
	err            error
	inlineErr      error
	// blockingAuthors is the freshness fixture: the logins BlockingThreadAuthors
	// reports as still owning a blocking thread on the PR.
	blockingAuthors map[string]bool
	// blockingErr makes the freshness query unverifiable.
	blockingErr error
	// blockingCalls counts BlockingThreadAuthors invocations, so the promotion
	// short-circuit is provable rather than merely commented (BOS-669).
	blockingCalls int
}

func (f *fakeReviewCommentProvider) GetReviewComments(_ context.Context, repoOriginURL string, prNumber int) ([]vcs.ReviewComment, error) {
	f.calls = append(f.calls, reviewCommentCall{repoOriginURL: repoOriginURL, prNumber: prNumber})
	return f.comments, f.err
}

func (f *fakeReviewCommentProvider) GetReviewInlineComments(_ context.Context, repoOriginURL string, prNumber int, reviewID int64) ([]vcs.ReviewComment, error) {
	f.inlineCalls = append(f.inlineCalls, reviewCommentCall{repoOriginURL: repoOriginURL, prNumber: prNumber, reviewID: reviewID})
	return f.inlineComments, f.inlineErr
}

func (f *fakeReviewCommentProvider) BlockingThreadAuthors(_ context.Context, _ string, _ int, botUsers map[string]bool) (map[string]bool, error) {
	f.blockingCalls++
	if f.blockingErr != nil {
		return nil, f.blockingErr
	}
	blocking := make(map[string]bool)
	for login := range botUsers {
		if f.blockingAuthors[login] {
			blocking[login] = true
		}
	}
	return blocking, nil
}

// commentsOnlyReviewProvider implements ReviewCommentProvider and
// ReviewInlineCommentProvider but NOT ReviewThreadFreshnessProvider, so the
// realtime path cannot establish freshness at all.
type commentsOnlyReviewProvider struct {
	inlineComments []vcs.ReviewComment
}

func (f *commentsOnlyReviewProvider) GetReviewComments(_ context.Context, _ string, _ int) ([]vcs.ReviewComment, error) {
	return nil, nil
}

func (f *commentsOnlyReviewProvider) GetReviewInlineComments(_ context.Context, _ string, _ int, _ int64) ([]vcs.ReviewComment, error) {
	return f.inlineComments, nil
}

func TestWebhookDispatcherRoutesPRPullRequestEvent(t *testing.T) {
	refresher := &fakePRRefresher{}
	dispatcher := NewWebhookDispatcher(refresher, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://github.com/owner/repo",
		PullRequest:   42,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(refresher.calls) != 1 {
		t.Fatalf("RefreshPR call count = %d, want 1", len(refresher.calls))
	}
	if got := refresher.calls[0]; got.repoOriginURL != "https://github.com/owner/repo" || got.prNumber != 42 {
		t.Fatalf("RefreshPR call = %+v, want repo URL and PR 42", got)
	}
}

func TestDispatch_PayloadEmitsRealtimeEvent(t *testing.T) {
	payload, err := os.ReadFile("testdata/pull_request_synchronize_conflict.json")
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	dispatcher := NewWebhookDispatcherWithEmitter(refresher, emitter, zerolog.Nop())

	err = dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(emitter.calls) != 1 {
		t.Fatalf("EmitForPR call count = %d, want 1", len(emitter.calls))
	}
	if got := emitter.calls[0]; got.repoOriginURL != "https://github.com/recurser/bossanova" || got.prNumber != 345 {
		t.Fatalf("EmitForPR call = %+v, want repo URL and PR 345", got)
	}
	if len(emitter.calls[0].events) == 0 {
		t.Fatal("EmitForPR events empty, want ConflictDetected")
	}
	if _, ok := emitter.calls[0].events[0].(vcs.ConflictDetected); !ok {
		t.Fatalf("EmitForPR first event = %T, want vcs.ConflictDetected", emitter.calls[0].events[0])
	}
	if len(refresher.calls) != 1 {
		t.Fatalf("RefreshPR call count = %d, want 1", len(refresher.calls))
	}
}

func TestDispatch_PayloadPREmitsRealtimeWithoutEnvelopePR(t *testing.T) {
	payload, err := os.ReadFile("testdata/pull_request_synchronize_conflict.json")
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	dispatcher := NewWebhookDispatcherWithEmitter(refresher, emitter, zerolog.Nop())

	err = dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   0,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(emitter.calls) != 1 {
		t.Fatalf("EmitForPR call count = %d, want 1", len(emitter.calls))
	}
	if got := emitter.calls[0]; got.repoOriginURL != "https://github.com/recurser/bossanova" || got.prNumber != 345 {
		t.Fatalf("EmitForPR call = %+v, want repo URL and PR 345", got)
	}
	if len(refresher.calls) != 1 {
		t.Fatalf("RefreshPR call count = %d, want 1", len(refresher.calls))
	}
	if got := refresher.calls[0]; got.repoOriginURL != "https://github.com/recurser/bossanova" || got.prNumber != 345 {
		t.Fatalf("RefreshPR call = %+v, want repo URL and PR 345", got)
	}
}

func TestDispatch_PayloadPRMismatchSkipsRealtimeAndRefreshesEnvelopePR(t *testing.T) {
	payload, err := os.ReadFile("testdata/pull_request_synchronize_conflict.json")
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	dispatcher := NewWebhookDispatcherWithEmitter(refresher, emitter, zerolog.Nop())

	err = dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   42,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("EmitForPR call count = %d, want 0", len(emitter.calls))
	}
	if len(refresher.calls) != 1 {
		t.Fatalf("RefreshPR call count = %d, want 1", len(refresher.calls))
	}
	if got := refresher.calls[0]; got.repoOriginURL != "https://github.com/recurser/bossanova" || got.prNumber != 42 {
		t.Fatalf("RefreshPR call = %+v, want repo URL and PR 42", got)
	}
}

func TestDispatch_PayloadDoesNotEmitWhenRepoOriginURLMissing(t *testing.T) {
	payload, err := os.ReadFile("testdata/pull_request_synchronize_conflict.json")
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	dispatcher := NewWebhookDispatcherWithEmitter(refresher, emitter, zerolog.Nop())

	err = dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:   "pull_request",
		PullRequest: 345,
		Payload:     payload,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want error")
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("EmitForPR call count = %d, want 0", len(emitter.calls))
	}
	if len(refresher.calls) != 0 {
		t.Fatalf("RefreshPR call count = %d, want 0", len(refresher.calls))
	}
}

func TestDispatch_PayloadDoesNotEmitWhenRefresherMissing(t *testing.T) {
	payload, err := os.ReadFile("testdata/pull_request_synchronize_conflict.json")
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}

	emitter := &fakeEmitter{}
	dispatcher := NewWebhookDispatcherWithEmitter(nil, emitter, zerolog.Nop())

	err = dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want error")
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("EmitForPR call count = %d, want 0", len(emitter.calls))
	}
}

func TestDispatch_PayloadParseFailureStillRefreshes(t *testing.T) {
	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	dispatcher := NewWebhookDispatcherWithEmitter(refresher, emitter, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       []byte("{"),
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("EmitForPR call count = %d, want 0", len(emitter.calls))
	}
	if len(refresher.calls) != 1 {
		t.Fatalf("RefreshPR call count = %d, want 1", len(refresher.calls))
	}
}

func TestDispatch_EmitterFailureStillRefreshes(t *testing.T) {
	payload, err := os.ReadFile("testdata/pull_request_synchronize_conflict.json")
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{err: errors.New("emit failed")}
	dispatcher := NewWebhookDispatcherWithEmitter(refresher, emitter, zerolog.Nop())

	err = dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(emitter.calls) != 1 {
		t.Fatalf("EmitForPR call count = %d, want 1", len(emitter.calls))
	}
	if len(refresher.calls) != 1 {
		t.Fatalf("RefreshPR call count = %d, want 1", len(refresher.calls))
	}
}

func TestDispatch_ReviewSubmittedFetchesCommentsWhenPayloadBodyEmpty(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
		review := body["review"].(map[string]any)
		review["body"] = ""
	})

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &fakeReviewCommentProvider{
		inlineComments: []vcs.ReviewComment{{Body: "inline fix", State: vcs.ReviewStateChangesRequested}},
	}
	dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request_review",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(reviews.inlineCalls) != 1 {
		t.Fatalf("GetReviewInlineComments call count = %d, want 1", len(reviews.inlineCalls))
	}
	if len(emitter.calls) != 1 {
		t.Fatalf("EmitForPR call count = %d, want 1", len(emitter.calls))
	}
	review, ok := emitter.calls[0].events[0].(vcs.ReviewSubmitted)
	if !ok {
		t.Fatalf("event type = %T, want vcs.ReviewSubmitted", emitter.calls[0].events[0])
	}
	if len(review.Comments) != 1 || review.Comments[0].Body != "inline fix" {
		t.Fatalf("review comments = %+v, want fetched inline comments", review.Comments)
	}
}

func TestDispatch_ReviewSubmittedCommentedBotReviewPromotesFromFetchedComments(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
		review := body["review"].(map[string]any)
		review["state"] = "commented"
		review["body"] = ""
		user := review["user"].(map[string]any)
		user["login"] = "chatgpt-codex-connector[bot]"
	})

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &fakeReviewCommentProvider{
		inlineComments: []vcs.ReviewComment{{
			Author: "chatgpt-codex-connector[bot]",
			Body:   "handle the nil case",
			State:  vcs.ReviewStateChangesRequested,
		}},
		blockingAuthors: map[string]bool{"chatgpt-codex-connector[bot]": true},
	}
	var logs strings.Builder
	dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.New(&logs))

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request_review",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(reviews.inlineCalls) != 1 {
		t.Fatalf("GetReviewInlineComments call count = %d, want 1", len(reviews.inlineCalls))
	}
	if reviews.blockingCalls != 1 {
		t.Fatalf("BlockingThreadAuthors call count = %d, want 1 for a promote candidate", reviews.blockingCalls)
	}
	if len(emitter.calls) != 1 {
		t.Fatalf("EmitForPR call count = %d, want 1", len(emitter.calls))
	}
	review, ok := emitter.calls[0].events[0].(vcs.ReviewSubmitted)
	if !ok {
		t.Fatalf("event type = %T, want vcs.ReviewSubmitted", emitter.calls[0].events[0])
	}
	if review.State != vcs.ReviewStateChangesRequested {
		t.Fatalf("ReviewSubmitted.State = %v, want ChangesRequested", review.State)
	}
	if len(review.Comments) != 1 || review.Comments[0].State != vcs.ReviewStateChangesRequested {
		t.Fatalf("review comments = %+v, want fetched changes-requested comment", review.Comments)
	}
	if !strings.Contains(logs.String(), "realtime review promoted to changes-requested") {
		t.Fatalf("expected the promotion log line, got: %s", logs.String())
	}
}

// TestDispatch_ReviewSubmittedCommentedBotReviewNotPromotedWhenAuthorHasNoBlockingThread
// is BOS-669's named scenario: a delayed or redelivered bot COMMENTED review
// whose anchored hunk was rewritten in the interim. Its author owns no blocking
// thread any more, so promoting it would restart a fix loop over feedback that
// is already moot. The review must stay COMMENTED — and the event must still be
// emitted, comments intact, because suppression is not a drop.
func TestDispatch_ReviewSubmittedCommentedBotReviewNotPromotedWhenAuthorHasNoBlockingThread(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
		review := body["review"].(map[string]any)
		review["state"] = "commented"
		review["body"] = ""
		user := review["user"].(map[string]any)
		user["login"] = "chatgpt-codex-connector[bot]"
	})

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &fakeReviewCommentProvider{
		inlineComments: []vcs.ReviewComment{{
			Author: "chatgpt-codex-connector[bot]",
			Body:   "handle the nil case",
			State:  vcs.ReviewStateChangesRequested,
		}},
		// The author owns no blocking thread: every thread it raised is now
		// resolved or outdated.
		blockingAuthors: map[string]bool{},
	}
	var logs strings.Builder
	dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.New(&logs))

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request_review",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if reviews.blockingCalls != 1 {
		t.Fatalf("BlockingThreadAuthors call count = %d, want 1", reviews.blockingCalls)
	}
	if len(emitter.calls) != 1 {
		t.Fatalf("EmitForPR call count = %d, want 1: a suppressed promotion must not drop the event", len(emitter.calls))
	}
	review, ok := emitter.calls[0].events[0].(vcs.ReviewSubmitted)
	if !ok {
		t.Fatalf("event type = %T, want vcs.ReviewSubmitted", emitter.calls[0].events[0])
	}
	if review.State != vcs.ReviewStateCommented {
		t.Fatalf("ReviewSubmitted.State = %v, want Commented (stale feedback must not promote)", review.State)
	}
	if len(review.Comments) != 1 || review.Comments[0].Body != "handle the nil case" {
		t.Fatalf("review comments = %+v, want the fetched comments retained", review.Comments)
	}
	if !strings.Contains(logs.String(), "realtime review promotion suppressed: author has no blocking review thread") {
		t.Fatalf("expected the freshness-suppression log line, got: %s", logs.String())
	}
}

// TestDispatch_ReviewSubmittedCommentedBotReviewNotPromotedWhenThreadsUnverifiable
// pins BOS-669 Q1: an unverifiable freshness query suppresses the promotion
// rather than guessing. The batch is NOT dropped — the existing error branch
// discards every event in the batch, which would take a genuine native
// CHANGES_REQUESTED review down with it.
func TestDispatch_ReviewSubmittedCommentedBotReviewNotPromotedWhenThreadsUnverifiable(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
		review := body["review"].(map[string]any)
		review["state"] = "commented"
		review["body"] = ""
		user := review["user"].(map[string]any)
		user["login"] = "chatgpt-codex-connector[bot]"
	})

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &fakeReviewCommentProvider{
		inlineComments: []vcs.ReviewComment{{
			Author: "chatgpt-codex-connector[bot]",
			Body:   "handle the nil case",
			State:  vcs.ReviewStateChangesRequested,
		}},
		blockingErr: fmt.Errorf("%w: GraphQL thread query failed", vcs.ErrReviewThreadsUnverified),
	}
	var logs strings.Builder
	dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.New(&logs))

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request_review",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(emitter.calls) != 1 {
		t.Fatalf("EmitForPR call count = %d, want 1: an unverifiable query must not drop the batch", len(emitter.calls))
	}
	review, ok := emitter.calls[0].events[0].(vcs.ReviewSubmitted)
	if !ok {
		t.Fatalf("event type = %T, want vcs.ReviewSubmitted", emitter.calls[0].events[0])
	}
	if review.State != vcs.ReviewStateCommented {
		t.Fatalf("ReviewSubmitted.State = %v, want Commented (unverifiable freshness must not promote)", review.State)
	}
	if !strings.Contains(logs.String(), "realtime review promotion suppressed: review-thread freshness unverifiable") {
		t.Fatalf("expected the unverifiable-suppression log line, got: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"level":"warn"`) {
		t.Fatalf("expected WARN level for an unverifiable freshness query, got: %s", logs.String())
	}
}

// TestDispatch_ReviewSubmittedCommentedBotReviewNotPromotedWithoutFreshnessCapability
// covers a provider that implements review-comment fetching but not
// ReviewThreadFreshnessProvider: freshness cannot be established, so the
// promotion is suppressed — no panic, no dropped event.
func TestDispatch_ReviewSubmittedCommentedBotReviewNotPromotedWithoutFreshnessCapability(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
		review := body["review"].(map[string]any)
		review["state"] = "commented"
		review["body"] = ""
		user := review["user"].(map[string]any)
		user["login"] = "chatgpt-codex-connector[bot]"
	})

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &commentsOnlyReviewProvider{
		inlineComments: []vcs.ReviewComment{{
			Author: "chatgpt-codex-connector[bot]",
			Body:   "handle the nil case",
			State:  vcs.ReviewStateChangesRequested,
		}},
	}
	var logs strings.Builder
	dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.New(&logs))

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request_review",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(emitter.calls) != 1 {
		t.Fatalf("EmitForPR call count = %d, want 1", len(emitter.calls))
	}
	review, ok := emitter.calls[0].events[0].(vcs.ReviewSubmitted)
	if !ok {
		t.Fatalf("event type = %T, want vcs.ReviewSubmitted", emitter.calls[0].events[0])
	}
	if review.State != vcs.ReviewStateCommented {
		t.Fatalf("ReviewSubmitted.State = %v, want Commented (no freshness capability must not promote)", review.State)
	}
	if !strings.Contains(logs.String(), "realtime review promotion suppressed: no review-thread freshness capability") {
		t.Fatalf("expected the absent-capability log line, got: %s", logs.String())
	}
}

// TestDispatch_ReviewSubmittedNativeChangesRequestedSkipsFreshnessQuery pins
// that freshness gates the PROMOTION, never a native state. A review GitHub
// itself reports as changes_requested is emitted unchanged and costs no thread
// query — mirroring the poller side (8700e8b6b).
func TestDispatch_ReviewSubmittedNativeChangesRequestedSkipsFreshnessQuery(t *testing.T) {
	payload := loadFixture(t, "pull_request_review_changes_requested.json")

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &fakeReviewCommentProvider{
		inlineComments: []vcs.ReviewComment{{Body: "inline fix", State: vcs.ReviewStateChangesRequested}},
		// Deliberately empty: were the gate to run, it would suppress — and a
		// native CHANGES_REQUESTED must never be suppressible.
		blockingAuthors: map[string]bool{},
	}
	dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request_review",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if reviews.blockingCalls != 0 {
		t.Fatalf("BlockingThreadAuthors call count = %d, want 0 for a native changes-requested review", reviews.blockingCalls)
	}
	review, ok := emitter.calls[0].events[0].(vcs.ReviewSubmitted)
	if !ok {
		t.Fatalf("event type = %T, want vcs.ReviewSubmitted", emitter.calls[0].events[0])
	}
	if review.State != vcs.ReviewStateChangesRequested {
		t.Fatalf("ReviewSubmitted.State = %v, want ChangesRequested unchanged", review.State)
	}
}

// TestDispatch_ReviewSubmittedNonCandidateReviewsSkipFreshnessQuery is the
// ACK-latency mitigation, pinned rather than merely commented: the extra GitHub
// round-trip must be short-circuited away for any review that could not be
// promoted anyway.
func TestDispatch_ReviewSubmittedNonCandidateReviewsSkipFreshnessQuery(t *testing.T) {
	tests := []struct {
		name           string
		author         string
		inlineComments []vcs.ReviewComment
	}{
		{
			name:   "bot commented review with no actionable inline comments",
			author: "chatgpt-codex-connector[bot]",
			inlineComments: []vcs.ReviewComment{{
				Author: "chatgpt-codex-connector[bot]",
				Body:   "nice work",
				State:  vcs.ReviewStateCommented,
			}},
		},
		{
			name:   "human commented review with inline feedback",
			author: "human-reviewer",
			inlineComments: []vcs.ReviewComment{{
				Author: "human-reviewer",
				Body:   "non-blocking question",
				State:  vcs.ReviewStateChangesRequested,
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
				review := body["review"].(map[string]any)
				review["state"] = "commented"
				review["body"] = ""
				user := review["user"].(map[string]any)
				user["login"] = tc.author
			})

			refresher := &fakePRRefresher{}
			emitter := &fakeEmitter{}
			reviews := &fakeReviewCommentProvider{
				inlineComments:  tc.inlineComments,
				blockingAuthors: map[string]bool{tc.author: true},
			}
			dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.Nop())

			err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
				EventType:     "pull_request_review",
				RepoOriginUrl: "https://github.com/recurser/bossanova",
				PullRequest:   345,
				Payload:       payload,
			})
			if err != nil {
				t.Fatalf("Dispatch returned error: %v", err)
			}
			if reviews.blockingCalls != 0 {
				t.Fatalf("BlockingThreadAuthors call count = %d, want 0 for a non-candidate review", reviews.blockingCalls)
			}
			review, ok := emitter.calls[0].events[0].(vcs.ReviewSubmitted)
			if !ok {
				t.Fatalf("event type = %T, want vcs.ReviewSubmitted", emitter.calls[0].events[0])
			}
			if review.State != vcs.ReviewStateCommented {
				t.Fatalf("ReviewSubmitted.State = %v, want Commented", review.State)
			}
		})
	}
}

func TestDispatch_ReviewSubmittedCommentedBotReviewIgnoresStalePRComments(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
		review := body["review"].(map[string]any)
		review["state"] = "commented"
		review["body"] = ""
		user := review["user"].(map[string]any)
		user["login"] = "chatgpt-codex-connector[bot]"
	})

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &fakeReviewCommentProvider{
		comments: []vcs.ReviewComment{{
			Author: "chatgpt-codex-connector[bot]",
			Body:   "old unresolved review",
			State:  vcs.ReviewStateChangesRequested,
		}},
	}
	dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request_review",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(reviews.calls) != 0 {
		t.Fatalf("GetReviewComments call count = %d, want 0", len(reviews.calls))
	}
	if len(reviews.inlineCalls) != 1 {
		t.Fatalf("GetReviewInlineComments call count = %d, want 1", len(reviews.inlineCalls))
	}
	review, ok := emitter.calls[0].events[0].(vcs.ReviewSubmitted)
	if !ok {
		t.Fatalf("event type = %T, want vcs.ReviewSubmitted", emitter.calls[0].events[0])
	}
	if review.State != vcs.ReviewStateCommented {
		t.Fatalf("ReviewSubmitted.State = %v, want Commented", review.State)
	}
}

func TestDispatch_ReviewSubmittedCommentedHumanReviewStaysCommentedWithInlineFeedback(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
		review := body["review"].(map[string]any)
		review["state"] = "commented"
		review["body"] = ""
		user := review["user"].(map[string]any)
		user["login"] = "human-reviewer"
	})

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &fakeReviewCommentProvider{
		inlineComments: []vcs.ReviewComment{{
			Author: "human-reviewer",
			Body:   "non-blocking question",
			State:  vcs.ReviewStateChangesRequested,
		}},
	}
	dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request_review",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(reviews.inlineCalls) != 1 {
		t.Fatalf("GetReviewInlineComments call count = %d, want 1", len(reviews.inlineCalls))
	}
	if len(emitter.calls) != 1 {
		t.Fatalf("EmitForPR call count = %d, want 1", len(emitter.calls))
	}
	review, ok := emitter.calls[0].events[0].(vcs.ReviewSubmitted)
	if !ok {
		t.Fatalf("event type = %T, want vcs.ReviewSubmitted", emitter.calls[0].events[0])
	}
	if review.State != vcs.ReviewStateCommented {
		t.Fatalf("ReviewSubmitted.State = %v, want Commented", review.State)
	}
	if len(review.Comments) != 1 || review.Comments[0].State != vcs.ReviewStateChangesRequested {
		t.Fatalf("review comments = %+v, want fetched inline comment retained", review.Comments)
	}
}

func TestDispatch_ReviewSubmittedCommentedBotReviewStaysCommentedWhenNoActionableComments(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
		review := body["review"].(map[string]any)
		review["state"] = "commented"
		review["body"] = ""
		user := review["user"].(map[string]any)
		user["login"] = "chatgpt-codex-connector[bot]"
	})

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &fakeReviewCommentProvider{
		inlineComments: []vcs.ReviewComment{{
			Author: "chatgpt-codex-connector[bot]",
			Body:   "nice work",
			State:  vcs.ReviewStateCommented,
		}},
	}
	dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request_review",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(emitter.calls) != 1 {
		t.Fatalf("EmitForPR call count = %d, want 1", len(emitter.calls))
	}
	review, ok := emitter.calls[0].events[0].(vcs.ReviewSubmitted)
	if !ok {
		t.Fatalf("event type = %T, want vcs.ReviewSubmitted", emitter.calls[0].events[0])
	}
	if review.State != vcs.ReviewStateCommented {
		t.Fatalf("ReviewSubmitted.State = %v, want Commented (benign comment must not be promoted)", review.State)
	}
}

// TestDispatch_ReviewSubmittedCommentedBotReviewWithBenignBodyStaysCommented
// pins the realtime-path half of BOS-254: a bot COMMENTED review whose summary
// body is benign prose ("LGTM") with no changes-requested inline comments is
// NOT promoted. The body is never consulted: benign prose cannot be told apart
// from a change request, so promoting on a substantive body alone would
// false-block. Note this holds independently of review-thread freshness — the
// review-scoped inline-comment term fails first, so the freshness query is not
// even reached.
func TestDispatch_ReviewSubmittedCommentedBotReviewWithBenignBodyStaysCommented(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
		review := body["review"].(map[string]any)
		review["state"] = "commented"
		review["body"] = "LGTM — no issues found."
		user := review["user"].(map[string]any)
		user["login"] = "chatgpt-codex-connector[bot]"
	})

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &fakeReviewCommentProvider{}
	dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request_review",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(emitter.calls) != 1 {
		t.Fatalf("EmitForPR call count = %d, want 1", len(emitter.calls))
	}
	review, ok := emitter.calls[0].events[0].(vcs.ReviewSubmitted)
	if !ok {
		t.Fatalf("event type = %T, want vcs.ReviewSubmitted", emitter.calls[0].events[0])
	}
	if review.State != vcs.ReviewStateCommented {
		t.Fatalf("ReviewSubmitted.State = %v, want Commented (benign body must not promote)", review.State)
	}
}

func TestDispatch_ReviewSubmittedMergesSummaryAndInlineComments(t *testing.T) {
	payload, err := os.ReadFile("testdata/pull_request_review_changes_requested.json")
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &fakeReviewCommentProvider{
		inlineComments: []vcs.ReviewComment{{Body: "inline fix", State: vcs.ReviewStateChangesRequested}},
	}
	dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.Nop())

	err = dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request_review",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(reviews.inlineCalls) != 1 {
		t.Fatalf("GetReviewInlineComments call count = %d, want 1", len(reviews.inlineCalls))
	}
	review, ok := emitter.calls[0].events[0].(vcs.ReviewSubmitted)
	if !ok {
		t.Fatalf("event type = %T, want vcs.ReviewSubmitted", emitter.calls[0].events[0])
	}
	if len(review.Comments) != 2 {
		t.Fatalf("review comments length = %d, want summary plus inline comment", len(review.Comments))
	}
	if review.Comments[1].Body != "inline fix" {
		t.Fatalf("inline review comment = %q, want inline fix", review.Comments[1].Body)
	}
}

func TestDispatch_ReviewSubmittedSkipsRealtimeWhenCommentFetchFails(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
		review := body["review"].(map[string]any)
		review["body"] = ""
	})

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &fakeReviewCommentProvider{inlineErr: errors.New("review comments unavailable")}
	dispatcher := NewWebhookDispatcherWithEmitterAndReviewComments(refresher, emitter, reviews, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request_review",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   345,
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("EmitForPR call count = %d, want 0", len(emitter.calls))
	}
	if len(refresher.calls) != 1 {
		t.Fatalf("RefreshPR call count = %d, want 1", len(refresher.calls))
	}
}

func TestDispatch_BackwardCompatibleWithoutEmitter(t *testing.T) {
	refresher := &fakePRRefresher{}
	dispatcher := NewWebhookDispatcher(refresher, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://github.com/recurser/bossanova",
		PullRequest:   42,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(refresher.calls) != 1 {
		t.Fatalf("RefreshPR call count = %d, want 1", len(refresher.calls))
	}
}

func TestWebhookDispatcherSkipsEventsWithoutPR(t *testing.T) {
	refresher := &fakePRRefresher{}
	dispatcher := NewWebhookDispatcher(refresher, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "push",
		RepoOriginUrl: "https://github.com/owner/repo",
		PullRequest:   0,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(refresher.calls) != 0 {
		t.Fatalf("RefreshPR call count = %d, want 0", len(refresher.calls))
	}
}

func TestWebhookDispatcherWarnsWhenNoPRNumber(t *testing.T) {
	refresher := &fakePRRefresher{}
	var logs strings.Builder
	dispatcher := NewWebhookDispatcher(refresher, zerolog.New(&logs))

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "owner/repo",
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	if len(refresher.calls) != 0 {
		t.Fatalf("RefreshPR called %d times, want 0 when no PR number", len(refresher.calls))
	}
	if !strings.Contains(logs.String(), "no PR number") {
		t.Fatalf("expected warning about missing PR number, got: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"level":"warn"`) {
		t.Fatalf("expected WARN level for PR-scoped event missing a PR number, got: %s", logs.String())
	}
}

func TestWebhookDispatcherDebugsNonPREventWithoutPRNumber(t *testing.T) {
	refresher := &fakePRRefresher{}
	var logs strings.Builder
	dispatcher := NewWebhookDispatcher(refresher, zerolog.New(&logs))

	// check_suite/check_run fire on non-PR refs (e.g. CI on the default
	// branch) and legitimately resolve to no PR number. Those must not
	// emit WARN-level noise on routine traffic.
	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "check_suite",
		RepoOriginUrl: "https://github.com/owner/repo",
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	if len(refresher.calls) != 0 {
		t.Fatalf("RefreshPR called %d times, want 0 when no PR number", len(refresher.calls))
	}
	if strings.Contains(logs.String(), `"level":"warn"`) {
		t.Fatalf("non-PR event without a PR number should not WARN, got: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"level":"debug"`) {
		t.Fatalf("expected DEBUG level for non-PR event missing a PR number, got: %s", logs.String())
	}
}

func TestWebhookDispatcherSurfacesRefreshError(t *testing.T) {
	refreshErr := errors.New("refresh failed")
	refresher := &fakePRRefresher{err: refreshErr}
	dispatcher := NewWebhookDispatcher(refresher, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://github.com/owner/repo",
		PullRequest:   42,
	})
	if !errors.Is(err, refreshErr) {
		t.Fatalf("Dispatch error = %v, want wrapped refresh error", err)
	}
	if len(refresher.calls) != 1 {
		t.Fatalf("RefreshPR call count = %d, want 1", len(refresher.calls))
	}
}

func TestWebhookDispatcherRejectsNilEvent(t *testing.T) {
	dispatcher := NewWebhookDispatcher(&fakePRRefresher{}, zerolog.Nop())

	if err := dispatcher.Dispatch(context.Background(), nil); err == nil {
		t.Fatal("Dispatch returned nil error, want error")
	}
}

func TestWebhookDispatcherRejectsPREventWithoutRepoOriginURL(t *testing.T) {
	refresher := &fakePRRefresher{}
	dispatcher := NewWebhookDispatcher(refresher, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:   "pull_request",
		PullRequest: 42,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want error")
	}
	if len(refresher.calls) != 0 {
		t.Fatalf("RefreshPR call count = %d, want 0", len(refresher.calls))
	}
}

func TestWebhookDispatcherRejectsPREventWithNilRefresher(t *testing.T) {
	dispatcher := NewWebhookDispatcher(nil, zerolog.Nop())

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://github.com/owner/repo",
		PullRequest:   42,
	})
	if err == nil {
		t.Fatal("Dispatch returned nil error, want error")
	}
	if !strings.Contains(err.Error(), "webhook dispatcher refresher not wired") {
		t.Fatalf("Dispatch error = %v, want refresher not wired error", err)
	}
}

// blockingEvaluator blocks EvaluatePR until release is closed, so a test can
// assert Dispatch (and thus the webhook ACK path) does not wait for it.
type blockingEvaluator struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	calls    []evalCall
}

type evalCall struct {
	owner    string
	name     string
	prNumber int
}

func (e *blockingEvaluator) EvaluatePR(_ context.Context, owner, name string, prNumber int) error {
	e.calls = append(e.calls, evalCall{owner: owner, name: name, prNumber: prNumber})
	close(e.started)
	<-e.release
	close(e.finished)
	return nil
}

// captureEvalDone returns a hook that records each detached evaluation's done
// channel, letting a test await goroutine completion without the production
// path ever blocking on it.
func captureEvalDone(t *testing.T) (func(<-chan struct{}), func()) {
	t.Helper()
	var done <-chan struct{}
	set := make(chan struct{})
	hook := func(d <-chan struct{}) {
		done = d
		close(set)
	}
	wait := func() {
		<-set
		<-done
	}
	return hook, wait
}

func TestDispatch_EvaluatorRunsDetachedOffAckPath(t *testing.T) {
	refresher := &fakePRRefresher{}
	ev := &blockingEvaluator{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	dispatcher := NewWebhookDispatcher(refresher, zerolog.Nop()).WithEvaluator(ev)
	hook, waitEval := captureEvalDone(t)
	dispatcher.evalDoneHook = hook

	// Dispatch must return before the (blocked) evaluator finishes: the
	// evaluation is off the acknowledgement path.
	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://github.com/owner/repo",
		PullRequest:   42,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	// The evaluator has begun but is still blocked — proving Dispatch did not
	// wait for it. RefreshPR (the rest of Dispatch) already ran.
	<-ev.started
	select {
	case <-ev.finished:
		t.Fatal("evaluator finished before release; Dispatch blocked on the ACK path")
	default:
	}
	if len(refresher.calls) != 1 {
		t.Fatalf("RefreshPR call count = %d, want 1", len(refresher.calls))
	}

	close(ev.release)
	waitEval()
	if len(ev.calls) != 1 || ev.calls[0].owner != "owner" || ev.calls[0].name != "repo" || ev.calls[0].prNumber != 42 {
		t.Fatalf("evaluator calls = %+v, want one owner/repo#42 call", ev.calls)
	}
}

func TestDispatch_NoEvaluatorSkipsEvaluation(t *testing.T) {
	refresher := &fakePRRefresher{}
	dispatcher := NewWebhookDispatcher(refresher, zerolog.Nop())
	called := false
	dispatcher.evalDoneHook = func(<-chan struct{}) { called = true }

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://github.com/owner/repo",
		PullRequest:   42,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if called {
		t.Fatal("evalDoneHook fired with no evaluator wired")
	}
}

func TestDispatch_NonGitHubOriginSkipsEvaluation(t *testing.T) {
	refresher := &fakePRRefresher{}
	ev := &blockingEvaluator{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	dispatcher := NewWebhookDispatcher(refresher, zerolog.Nop()).WithEvaluator(ev)
	called := false
	dispatcher.evalDoneHook = func(<-chan struct{}) { called = true }

	err := dispatcher.Dispatch(context.Background(), &pb.WebhookEvent{
		EventType:     "pull_request",
		RepoOriginUrl: "https://gitlab.example.com/owner/repo",
		PullRequest:   42,
	})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if called {
		t.Fatal("evalDoneHook fired for a non-GitHub origin URL")
	}
	if len(ev.calls) != 0 {
		t.Fatalf("evaluator calls = %d, want 0 for non-GitHub origin", len(ev.calls))
	}
}
