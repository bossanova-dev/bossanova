package upstream

import (
	"context"
	"errors"
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
}

func (f *fakeReviewCommentProvider) GetReviewComments(_ context.Context, repoOriginURL string, prNumber int) ([]vcs.ReviewComment, error) {
	f.calls = append(f.calls, reviewCommentCall{repoOriginURL: repoOriginURL, prNumber: prNumber})
	return f.comments, f.err
}

func (f *fakeReviewCommentProvider) GetReviewInlineComments(_ context.Context, repoOriginURL string, prNumber int, reviewID int64) ([]vcs.ReviewComment, error) {
	f.inlineCalls = append(f.inlineCalls, reviewCommentCall{repoOriginURL: repoOriginURL, prNumber: prNumber, reviewID: reviewID})
	return f.inlineComments, f.inlineErr
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
	if review.State != vcs.ReviewStateChangesRequested {
		t.Fatalf("ReviewSubmitted.State = %v, want ChangesRequested", review.State)
	}
	if len(review.Comments) != 1 || review.Comments[0].State != vcs.ReviewStateChangesRequested {
		t.Fatalf("review comments = %+v, want fetched changes-requested comment", review.Comments)
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
// NOT promoted — the realtime path has no review-thread context, so it must not
// promote on a substantive body alone.
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
