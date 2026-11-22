package upstream

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCommandHandlerAdapter_UpdateRepoForwardsPresentRepoFlags(t *testing.T) {
	on := true
	fake := &fakeSessionCommandServer{}
	adapter := &CommandHandlerAdapter{Commands: fake}
	if _, err := adapter.UpdateRepo(context.Background(), &pb.UpdateRepoCommand{
		RepoId:                    "repo-1",
		ShouldKeepBranchesCurrent: &on,
		CanAutoDeleteBranches:     &on,
		CanAutoRotate:             &on,
	}); err != nil {
		t.Fatalf("UpdateRepo: %v", err)
	}
	got := fake.lastUpdateRepo
	if !got.GetShouldKeepBranchesCurrent() || !got.GetCanAutoDeleteBranches() || !got.GetCanAutoRotate() {
		t.Fatalf("forwarded repo flags = %v/%v/%v, want all three on", got.ShouldKeepBranchesCurrent, got.CanAutoDeleteBranches, got.CanAutoRotate)
	}
}

func TestCommandHandlerAdapter_UpdateRepoLeavesAbsentFlagsUnset(t *testing.T) {
	fake := &fakeSessionCommandServer{}
	adapter := &CommandHandlerAdapter{Commands: fake}
	if _, err := adapter.UpdateRepo(context.Background(), &pb.UpdateRepoCommand{RepoId: "repo-1"}); err != nil {
		t.Fatalf("UpdateRepo: %v", err)
	}
	got := fake.lastUpdateRepo
	if got.ShouldKeepBranchesCurrent != nil || got.CanAutoDeleteBranches != nil || got.CanAutoRotate != nil {
		t.Fatalf("absent repo flags forwarded as %v/%v/%v, want unset", got.ShouldKeepBranchesCurrent, got.CanAutoDeleteBranches, got.CanAutoRotate)
	}
}
