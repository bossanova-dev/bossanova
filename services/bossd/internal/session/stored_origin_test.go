package session

import (
	"context"
	"testing"

	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"
)

// A session start keeps a stored origin even when the checkout's remote has
// moved on: the daemon-start refresh and `boss repo update --refresh-origin`
// own following a renamed remote, so the session path makes no git call.
func TestResolveOriginURL_StoredOriginKeptWhenRemoteDiffers(t *testing.T) {
	ctx := context.Background()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{originURL: "https://github.com/new-owner/repo.git"}
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo", OriginURL: "https://github.com/old-owner/repo.git"}
	lc := newTestLifecycle(newMockSessionStore(), repos, nil, nil, wt, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())

	if err := lc.resolveOriginURL(ctx, repos.repos["repo-1"]); err != nil {
		t.Fatalf("resolveOriginURL: %v", err)
	}
	if got := repos.repos["repo-1"].OriginURL; got != "https://github.com/old-owner/repo.git" {
		t.Errorf("repo.OriginURL = %q, want the stored origin kept", got)
	}
}

func TestOriginURLIfConfigured_StoredOriginKeptWhenRemoteDiffers(t *testing.T) {
	wt := &mockWorktreeManager{originURL: "https://github.com/new-owner/repo.git"}
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo", OriginURL: "https://github.com/old-owner/repo.git"}
	lc := newTestLifecycle(newMockSessionStore(), repos, nil, nil, wt, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())

	got, err := lc.originURLIfConfigured(context.Background(), repos.repos["repo-1"])
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://github.com/old-owner/repo.git" || repos.repos["repo-1"].OriginURL != got {
		t.Fatalf("origin = %q, stored = %q; want the stored origin kept", got, repos.repos["repo-1"].OriginURL)
	}
}
