package upstream

import (
	"context"
	"errors"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
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
