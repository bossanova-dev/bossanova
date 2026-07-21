package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	dbpkg "github.com/recurser/bossd/internal/db"
)

// TestGetSession_HydratesShouldArchiveSessionsAfterMerge verifies GetSession carries
// the repo's archive-after-merge flag onto the session proto: true when the
// repo defaults on, and false once the repo is toggled off.
func TestGetSession_HydratesShouldArchiveSessionsAfterMerge(t *testing.T) {
	sqlDB := setupServerTestDB(t)
	repos := dbpkg.NewRepoStore(sqlDB)
	sessions := dbpkg.NewSessionStore(sqlDB)
	s := New(Config{Repos: repos, Sessions: sessions})
	ctx := context.Background()

	// Create a repo (defaults archive-after-merge ON) and a session in it.
	repo, err := repos.Create(ctx, dbpkg.CreateRepoParams{
		DisplayName:       "r",
		LocalPath:         "/tmp/r",
		OriginURL:         "https://github.com/acme/r.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if !repo.ShouldArchiveSessionsAfterMerge {
		t.Fatalf("precondition: new repo ShouldArchiveSessionsAfterMerge = false, want default true")
	}

	sess, err := sessions.Create(ctx, dbpkg.CreateSessionParams{
		RepoID:     repo.ID,
		Title:      "session",
		BranchName: "feature",
		BaseBranch: "main",
		AgentName:  "codex",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	resp, err := s.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sess.ID}))
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !resp.Msg.Session.GetRepoShouldArchiveSessionsAfterMerge() {
		t.Error("expected RepoShouldArchiveSessionsAfterMerge true (repo default on)")
	}

	off := false
	if _, err := repos.Update(ctx, repo.ID, dbpkg.UpdateRepoParams{ShouldArchiveSessionsAfterMerge: &off}); err != nil {
		t.Fatalf("update repo: %v", err)
	}

	resp2, err := s.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sess.ID}))
	if err != nil {
		t.Fatalf("GetSession (after toggle): %v", err)
	}
	if resp2.Msg.Session.GetRepoShouldArchiveSessionsAfterMerge() {
		t.Error("expected RepoShouldArchiveSessionsAfterMerge false after toggling off")
	}
}
