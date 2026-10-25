package upstream

import (
	"context"
	"slices"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/daemoncaps"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// newerBossoCommand is a command whose oneof field this build does not know,
// as a newer bosso sends it: the arm decodes into unknown fields.
func newerBossoCommand(t *testing.T, cmdID string) *pb.OrchestratorCommand {
	t.Helper()
	raw := protowire.AppendTag(nil, 1, protowire.BytesType)
	raw = protowire.AppendString(raw, cmdID)
	raw = protowire.AppendTag(raw, 9999, protowire.BytesType)
	raw = protowire.AppendBytes(raw, nil)
	cmd := &pb.OrchestratorCommand{}
	if err := proto.Unmarshal(raw, cmd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cmd.GetCmd() != nil {
		t.Fatalf("field 9999 decoded into a known arm: %T", cmd.GetCmd())
	}
	return cmd
}

func TestDispatchCommand_UnknownCommandAnswersUnimplemented(t *testing.T) {
	client := NewStreamClient(StreamClientConfig{Hostname: "mac-mini", Logger: zerolog.Nop()})
	ev := client.dispatchCommand(context.Background(), newerBossoCommand(t, "c-u"), make(chan *pb.DaemonEvent, 4))
	res := ev.GetResult()
	if res == nil {
		t.Fatalf("unknown command got no CommandResult: %+v", ev)
	}
	if res.GetCommandId() != "c-u" || res.GetOk() || res.GetErrorCode() != pb.CommandResult_ERROR_CODE_UNIMPLEMENTED {
		t.Fatalf("result = %+v, want command c-u failed with ERROR_CODE_UNIMPLEMENTED", res)
	}
	if want := daemoncaps.UnsupportedCommandText("mac-mini"); res.GetError() != want {
		t.Fatalf("error = %q, want %q", res.GetError(), want)
	}
}

// TestDispatchCommand_HasACaseForEveryAdvertisedCommand holds the snapshot's
// supported_command_fields to the dispatcher: every field it advertises must
// reach a case, never the unsupported answer.
func TestDispatchCommand_HasACaseForEveryAdvertisedCommand(t *testing.T) {
	oneof := (&pb.OrchestratorCommand{}).ProtoReflect().Descriptor().Oneofs().ByName("cmd")
	for _, field := range daemoncaps.StreamCommands() {
		fd := oneof.Fields().ByNumber(protoreflect.FieldNumber(field))
		t.Run(string(fd.Name()), func(t *testing.T) {
			cmd := &pb.OrchestratorCommand{CommandId: "c-" + string(fd.Name())}
			m := cmd.ProtoReflect()
			m.Set(fd, protoreflect.ValueOfMessage(m.NewField(fd).Message()))
			if !reachesACase(newDispatcherClient(nil, nil, nil), cmd) {
				t.Fatalf("field %d (%s) is advertised but hits the dispatcher's default branch", field, fd.Name())
			}
		})
	}
}

func TestBuildSnapshot_AdvertisesSupportedCommands(t *testing.T) {
	snap, err := newDispatcherClient(nil, nil, nil).buildSnapshot(context.Background())
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	if got, want := snap.GetSupportedCommandFields(), daemoncaps.StreamCommands(); !slices.Equal(got, want) {
		t.Fatalf("supported_command_fields = %v, want %v", got, want)
	}
}

// reachesACase dispatches cmd against a client with no collaborators. Only the
// default branch answers ERROR_CODE_UNIMPLEMENTED, so a panic, a block or any
// other answer means a case took the command.
func reachesACase(client *StreamClient, cmd *pb.OrchestratorCommand) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	answered := make(chan *pb.DaemonEvent, 1)
	go func() {
		defer func() {
			if recover() != nil {
				answered <- nil
			}
		}()
		answered <- client.dispatchCommand(ctx, cmd, make(chan *pb.DaemonEvent, 64))
	}()
	select {
	case ev := <-answered:
		return ev.GetResult().GetErrorCode() != pb.CommandResult_ERROR_CODE_UNIMPLEMENTED
	case <-ctx.Done():
		return true
	}
}
