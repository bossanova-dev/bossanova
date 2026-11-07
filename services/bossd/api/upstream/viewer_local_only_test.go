package upstream

import (
	"context"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestViewerApprovalsAreLocalOnly(t *testing.T) {
	// Inspect the runtime wire contract, not source: neither the reverse stream
	// nor settings can carry an approval operation or approval state.
	for _, message := range []protoreflect.MessageDescriptor{(&pb.OrchestratorCommand{}).ProtoReflect().Descriptor(), (&pb.UpdateSettingsRequest{}).ProtoReflect().Descriptor(), (&pb.GlobalSettings{}).ProtoReflect().Descriptor()} {
		fields := message.Fields()
		for i := 0; i < fields.Len(); i++ {
			if strings.Contains(string(fields.Get(i).Name()), "viewer") {
				t.Fatalf("%s exposes viewer approval arm %s", message.FullName(), fields.Get(i).Name())
			}
		}
	}
	// A newer cloud cannot tunnel an approval payload through an unknown arm.
	cmd := &pb.OrchestratorCommand{CommandId: "unrecognized-viewer-command"}
	data := protowire.AppendTag(nil, 9999, protowire.BytesType)
	data = protowire.AppendBytes(data, []byte(`{"viewer_device_id":"viewer","fingerprint":"anything"}`))
	cmd.ProtoReflect().SetUnknown(data)
	result := newDispatcherClient(nil, nil, nil).dispatchCommand(context.Background(), cmd, nil).GetResult()
	if result.GetOk() || result.GetErrorCode() != pb.CommandResult_ERROR_CODE_UNIMPLEMENTED {
		t.Fatalf("unknown command accepted: %v", result)
	}
}
