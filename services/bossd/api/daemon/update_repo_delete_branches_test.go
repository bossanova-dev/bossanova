package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	dbpkg "github.com/recurser/bossd/internal/db"
)

// TestUpdateRepo_CanAutoDeleteBranches verifies the optional
// can_auto_delete_branches flag round-trips through the UpdateRepo RPC in both
// directions: a new repo defaults OFF (BOS-424), setting it true both persists
// and is reflected in the returned proto, and setting it false again round-trips
// back.
func TestUpdateRepo_CanAutoDeleteBranches(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "")

	// Sanity: the freshly created repo defaults to auto-delete-branches OFF
	// (BOS-424 — the Create INSERT literal is 0).
	if got, _ := store.Get(context.Background(), id); got.CanAutoDeleteBranches {
		t.Fatalf("new repo CanAutoDeleteBranches = true, want default false (BOS-424)")
	}

	// Toggle ON via the RPC — flips it away from the new default.
	on := true
	resp, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:                    id,
		CanAutoDeleteBranches: &on,
	}))
	if err != nil {
		t.Fatalf("UpdateRepo(on): %v", err)
	}
	if !resp.Msg.Repo.CanAutoDeleteBranches {
		t.Error("response proto CanAutoDeleteBranches = false, want true")
	}
	if got, _ := store.Get(context.Background(), id); !got.CanAutoDeleteBranches {
		t.Error("stored CanAutoDeleteBranches = false, want true")
	}

	// Toggle back OFF via the RPC.
	off := false
	resp, err = s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:                    id,
		CanAutoDeleteBranches: &off,
	}))
	if err != nil {
		t.Fatalf("UpdateRepo(off): %v", err)
	}
	if resp.Msg.Repo.CanAutoDeleteBranches {
		t.Error("response proto CanAutoDeleteBranches = true, want false")
	}
	if got, _ := store.Get(context.Background(), id); got.CanAutoDeleteBranches {
		t.Error("stored CanAutoDeleteBranches = true, want false")
	}
}
