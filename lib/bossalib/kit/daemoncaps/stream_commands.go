package daemoncaps

import (
	"fmt"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// legacyCommandCeiling is the highest OrchestratorCommand.cmd field number a
// daemon that advertises no supported commands is assumed to dispatch. Every
// daemon built before DaemonSnapshot.supported_command_fields stops at
// run_trigger_gate (65); fields 68-70 arrived after it, and such a daemon drops
// them without a reply.
const legacyCommandCeiling = 65

var commandOneof = (&pb.OrchestratorCommand{}).ProtoReflect().Descriptor().Oneofs().ByName("cmd")

// StreamCommands lists every OrchestratorCommand.cmd field number this build
// knows. A daemon advertises it as DaemonSnapshot.supported_command_fields; its
// dispatcher has a case for each, which its tests hold it to.
func StreamCommands() []int32 {
	fields := commandOneof.Fields()
	out := make([]int32, fields.Len())
	for i := range out {
		out[i] = int32(fields.Get(i).Number())
	}
	return out
}

// StreamCommandSet is the set of stream commands one daemon dispatches. The
// zero value is a daemon that advertised none.
type StreamCommandSet struct {
	fields map[protoreflect.FieldNumber]struct{}
}

// NewStreamCommandSet builds the set a daemon advertised. An empty list is a
// daemon that predates the advertisement.
func NewStreamCommandSet(advertised []int32) StreamCommandSet {
	if len(advertised) == 0 {
		return StreamCommandSet{}
	}
	fields := make(map[protoreflect.FieldNumber]struct{}, len(advertised))
	for _, f := range advertised {
		fields[protoreflect.FieldNumber(f)] = struct{}{}
	}
	return StreamCommandSet{fields: fields}
}

// Drops reports whether the daemon would drop cmd without a reply: cmd sets a
// command the daemon does not list, or, for a daemon that lists none, one newer
// than legacyCommandCeiling. A command that sets no known arm is not judged.
func (s StreamCommandSet) Drops(cmd *pb.OrchestratorCommand) bool {
	fd := cmd.ProtoReflect().WhichOneof(commandOneof)
	if fd == nil {
		return false
	}
	return !s.SupportsField(int32(fd.Number()))
}

// SupportsField checks one advertised command field without a wire message.
func (s StreamCommandSet) SupportsField(field int32) bool {
	if s.fields == nil {
		return field > 0 && field <= legacyCommandCeiling
	}
	_, ok := s.fields[protoreflect.FieldNumber(field)]
	return ok
}

// UnsupportedCommandText is what an operator reads when a daemon is too old for
// the command they sent. hostname may be empty.
func UnsupportedCommandText(hostname string) string {
	if hostname == "" {
		return "this machine's boss is too old to do this from here"
	}
	return fmt.Sprintf("boss on %s is too old to do this from here", hostname)
}
