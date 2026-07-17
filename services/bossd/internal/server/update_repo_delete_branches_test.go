package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	dbpkg "github.com/recurser/bossd/internal/db"
)

// TestUpdateRepo_CanAutoDeleteBranches verifies the optional
// can_auto_delete_branches flag round-trips through the UpdateRepo RPC: a new
// repo defaults ON, and setting it false both persists and is reflected in the
// returned proto.
func TestUpdateRepo_CanAutoDeleteBranches(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "")

	// Sanity: the freshly created repo defaults to auto-delete-branches ON.
	if got, _ := store.Get(context.Background(), id); !got.CanAutoDeleteBranches {
		t.Fatalf("new repo CanAutoDeleteBranches = false, want default true")
	}

	off := false
	resp, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:                    id,
		CanAutoDeleteBranches: &off,
	}))
	if err != nil {
		t.Fatalf("UpdateRepo: %v", err)
	}
	if resp.Msg.Repo.CanAutoDeleteBranches {
		t.Error("response proto CanAutoDeleteBranches = true, want false")
	}
	if got, _ := store.Get(context.Background(), id); got.CanAutoDeleteBranches {
		t.Error("stored CanAutoDeleteBranches = true, want false")
	}
}
