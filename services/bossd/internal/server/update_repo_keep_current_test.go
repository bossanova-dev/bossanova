package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	dbpkg "github.com/recurser/bossd/internal/db"
)

// TestUpdateRepo_ShouldKeepBranchesCurrent verifies the optional
// should_keep_branches_current flag round-trips through the UpdateRepo RPC in both
// directions: a new repo defaults OFF (BOS-521 — proactive rebases force-push
// and re-run CI, so the sweep is opt-in), setting it true both persists and is
// reflected in the returned proto, and setting it false again round-trips back.
func TestUpdateRepo_ShouldKeepBranchesCurrent(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "")

	if got, _ := store.Get(context.Background(), id); got.ShouldKeepBranchesCurrent {
		t.Fatalf("new repo ShouldKeepBranchesCurrent = true, want default false (BOS-521)")
	}

	on := true
	resp, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:                        id,
		ShouldKeepBranchesCurrent: &on,
	}))
	if err != nil {
		t.Fatalf("UpdateRepo(on): %v", err)
	}
	if !resp.Msg.Repo.ShouldKeepBranchesCurrent {
		t.Error("response proto ShouldKeepBranchesCurrent = false, want true")
	}
	if got, _ := store.Get(context.Background(), id); !got.ShouldKeepBranchesCurrent {
		t.Error("stored ShouldKeepBranchesCurrent = false, want true")
	}

	off := false
	resp, err = s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:                        id,
		ShouldKeepBranchesCurrent: &off,
	}))
	if err != nil {
		t.Fatalf("UpdateRepo(off): %v", err)
	}
	if resp.Msg.Repo.ShouldKeepBranchesCurrent {
		t.Error("response proto ShouldKeepBranchesCurrent = true, want false")
	}
	if got, _ := store.Get(context.Background(), id); got.ShouldKeepBranchesCurrent {
		t.Error("stored ShouldKeepBranchesCurrent = true, want false")
	}
}
