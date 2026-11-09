package upstream

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestRepoCloudControlHasNoStreamCommand(t *testing.T) {
	// Check the wire command vocabulary: a local RPC cannot be dispatched when
	// no corresponding command exists, including through unknown oneof fields.
	command := (&pb.OrchestratorCommand{}).ProtoReflect().Descriptor()
	fields := command.Oneofs().ByName("cmd").Fields()
	for i := 0; i < fields.Len(); i++ {
		if fields.Get(i).Message().FullName() == "bossanova.v1.ListRepoCloudControlsRequest" || fields.Get(i).Name() == "list_repo_cloud_controls" {
			t.Fatal("local grants exposed on cloud stream")
		}
	}
}
