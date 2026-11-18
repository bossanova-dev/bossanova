package upstream

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestCommandHandlerAdapterUpdateRepoServeScript(t *testing.T) {
	for _, script := range []*string{nil, proto.String("pnpm dev"), proto.String("")} {
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		if _, err := adapter.UpdateRepo(context.Background(), &pb.UpdateRepoCommand{RepoId: "repo", ServeScript: script}); err != nil {
			t.Fatal(err)
		}
		got := fake.lastUpdateRepo.ServeScript
		if (got == nil) != (script == nil) || (got != nil && *got != *script) {
			t.Fatalf("daemon ServeScript = %v, want %v", got, script)
		}
	}
}
