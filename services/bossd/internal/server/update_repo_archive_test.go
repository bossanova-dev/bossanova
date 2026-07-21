package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	dbpkg "github.com/recurser/bossd/internal/db"
)

// TestUpdateRepo_ShouldArchiveSessionsAfterMerge verifies the optional
// archive_sessions_after_merge flag round-trips through the UpdateRepo RPC: a new
// repo defaults ON, and setting it false both persists and is reflected in the
// returned proto.
func TestUpdateRepo_ShouldArchiveSessionsAfterMerge(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "")

	// Sanity: the freshly created repo defaults to archive-after-merge ON.
	if got, _ := store.Get(context.Background(), id); !got.ShouldArchiveSessionsAfterMerge {
		t.Fatalf("new repo ShouldArchiveSessionsAfterMerge = false, want default true")
	}

	off := false
	resp, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:                              id,
		ShouldArchiveSessionsAfterMerge: &off,
	}))
	if err != nil {
		t.Fatalf("UpdateRepo: %v", err)
	}
	if resp.Msg.Repo.ShouldArchiveSessionsAfterMerge {
		t.Error("response proto ShouldArchiveSessionsAfterMerge = true, want false")
	}
	if got, _ := store.Get(context.Background(), id); got.ShouldArchiveSessionsAfterMerge {
		t.Error("stored ShouldArchiveSessionsAfterMerge = true, want false")
	}
}
