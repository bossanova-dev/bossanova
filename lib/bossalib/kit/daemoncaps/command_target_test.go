package daemoncaps

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCommandTargetsComplete(t *testing.T) {
	fields := commandOneof.Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		cmd := &pb.OrchestratorCommand{}
		cmd.ProtoReflect().Set(field, cmd.ProtoReflect().NewField(field))
		if _, ok := OrchestratorCommandTargets(cmd); !ok {
			t.Errorf("%s has no target declaration", field.Name())
		}
	}
}
