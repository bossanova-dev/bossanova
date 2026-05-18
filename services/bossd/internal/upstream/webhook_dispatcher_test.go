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
}

type fakeReviewCommentProvider struct {
	calls    []reviewCommentCall
	comments []vcs.ReviewComment
	err      error
}

func (f *fakeReviewCommentProvider) GetReviewComments(_ context.Context, repoOriginURL string, prNumber int) ([]vcs.ReviewComment, error) {
	f.calls = append(f.calls, reviewCommentCall{repoOriginURL: repoOriginURL, prNumber: prNumber})
	return f.comments, f.err
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
		comments: []vcs.ReviewComment{{Body: "inline fix", State: vcs.ReviewStateChangesRequested}},
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
	if len(reviews.calls) != 1 {
		t.Fatalf("GetReviewComments call count = %d, want 1", len(reviews.calls))
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

func TestDispatch_ReviewSubmittedMergesSummaryAndInlineComments(t *testing.T) {
	payload, err := os.ReadFile("testdata/pull_request_review_changes_requested.json")
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}

	refresher := &fakePRRefresher{}
	emitter := &fakeEmitter{}
	reviews := &fakeReviewCommentProvider{
		comments: []vcs.ReviewComment{{Body: "inline fix", State: vcs.ReviewStateChangesRequested}},
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
	if len(reviews.calls) != 1 {
		t.Fatalf("GetReviewComments call count = %d, want 1", len(reviews.calls))
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
	reviews := &fakeReviewCommentProvider{err: errors.New("review comments unavailable")}
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
