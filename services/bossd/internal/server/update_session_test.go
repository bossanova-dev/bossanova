package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/db"
)

// updateSessionRepoStoreFake is a minimal db.RepoStore whose Get always fails,
// so UpdateSession's best-effort repo enrichment is skipped. The embedded
// interface makes any other method call a loud nil-panic.
type updateSessionRepoStoreFake struct {
	db.RepoStore
}

func (updateSessionRepoStoreFake) Get(_ context.Context, _ string) (*models.Repo, error) {
	return nil, errors.New("no repo")
}

// TestUpdateSession_EmitsOnSessionUpdated verifies that a successful rename
// publishes the updated session via onSessionUpdated so the cloud/web sees the
// new title — previously UpdateSession updated only the local DB.
func TestUpdateSession_EmitsOnSessionUpdated(t *testing.T) {
	var emitted *pb.Session
	srv := &Server{
		sessions: &lifecycleSessionStoreFake{session: &models.Session{ID: "s1", Title: "new title"}},
		repos:    updateSessionRepoStoreFake{},
		onSessionUpdated: func(_ context.Context, s *pb.Session) {
			emitted = s
		},
	}

	title := "new title"
	resp, err := srv.UpdateSession(context.Background(), connect.NewRequest(&pb.UpdateSessionRequest{
		Id:    "s1",
		Title: &title,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Msg.GetSession().GetId() != "s1" {
		t.Errorf("session id = %q, want s1", resp.Msg.GetSession().GetId())
	}
	if emitted == nil {
		t.Fatal("onSessionUpdated was not invoked")
	}
	if emitted.GetId() != "s1" {
		t.Errorf("emitted session id = %q, want s1", emitted.GetId())
	}
	if emitted.GetTitle() != "new title" {
		t.Errorf("emitted title = %q, want %q", emitted.GetTitle(), "new title")
	}
}

// TestUpdateSession_NilNotifierDoesNotPanic ensures the nil-guard around
// onSessionUpdated holds (the callback is optional).
func TestUpdateSession_NilNotifierDoesNotPanic(t *testing.T) {
	srv := &Server{
		sessions: &lifecycleSessionStoreFake{session: &models.Session{ID: "s1"}},
		repos:    updateSessionRepoStoreFake{},
	}

	title := "x"
	if _, err := srv.UpdateSession(context.Background(), connect.NewRequest(&pb.UpdateSessionRequest{
		Id:    "s1",
		Title: &title,
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
