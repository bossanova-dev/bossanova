package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// newPauseResumeServer builds a Server backed by a real in-memory session store
// plus a session whose automation starts enabled (the Create default).
func newPauseResumeServer(t *testing.T) (*Server, string) {
	t.Helper()
	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	ctx := context.Background()

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "repo",
		LocalPath:         "/tmp/repo",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt/repo",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	sess, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:     repo.ID,
		Title:      "session",
		BranchName: "feature",
		BaseBranch: "main",
		AgentName:  "codex",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if !sess.IsAutomationEnabled {
		t.Fatalf("precondition: expected automation enabled by default")
	}

	srv := &Server{sessions: sessions, logger: zerolog.Nop()}
	return srv, sess.ID
}

// TestPauseSessionDisablesAutomation pins that PauseSession flips automation off
// and echoes the updated session back to the caller.
func TestPauseSessionDisablesAutomation(t *testing.T) {
	srv, id := newPauseResumeServer(t)
	ctx := context.Background()

	resp, err := srv.PauseSession(ctx, connect.NewRequest(&pb.PauseSessionRequest{Id: id}))
	if err != nil {
		t.Fatalf("PauseSession: %v", err)
	}
	if got := resp.Msg.Session.GetIsAutomationEnabled(); got {
		t.Fatalf("response automation_enabled = %v, want false", got)
	}
	if resp.Msg.Session.GetId() != id {
		t.Fatalf("response session id = %q, want %q", resp.Msg.Session.GetId(), id)
	}

	stored, err := srv.sessions.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.IsAutomationEnabled {
		t.Fatalf("stored automation_enabled = true, want false")
	}
}

// TestResumeSessionEnablesAutomation pins that ResumeSession flips automation
// back on, including after a prior pause.
func TestResumeSessionEnablesAutomation(t *testing.T) {
	srv, id := newPauseResumeServer(t)
	ctx := context.Background()

	if _, err := srv.PauseSession(ctx, connect.NewRequest(&pb.PauseSessionRequest{Id: id})); err != nil {
		t.Fatalf("PauseSession: %v", err)
	}

	resp, err := srv.ResumeSession(ctx, connect.NewRequest(&pb.ResumeSessionRequest{Id: id}))
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if got := resp.Msg.Session.GetIsAutomationEnabled(); !got {
		t.Fatalf("response automation_enabled = %v, want true", got)
	}

	stored, err := srv.sessions.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !stored.IsAutomationEnabled {
		t.Fatalf("stored automation_enabled = false, want true")
	}
}

// TestPauseResumeRejectEmptyID pins the shared argument-validation behavior of
// both handlers.
func TestPauseResumeRejectEmptyID(t *testing.T) {
	srv, _ := newPauseResumeServer(t)
	ctx := context.Background()

	if _, err := srv.PauseSession(ctx, connect.NewRequest(&pb.PauseSessionRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("PauseSession empty id: got %v, want InvalidArgument", err)
	}
	if _, err := srv.ResumeSession(ctx, connect.NewRequest(&pb.ResumeSessionRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("ResumeSession empty id: got %v, want InvalidArgument", err)
	}
}
