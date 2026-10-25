package daemoncaps

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestStreamCommandSetDrops(t *testing.T) {
	triggerGate := &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_RunTriggerGate{RunTriggerGate: &pb.RunTriggerGateCommand{}}}
	dismiss := &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_DismissSessionAlert{DismissSessionAlert: &pb.DismissSessionAlertCommand{}}}
	resume := &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_ResumeSessionRun{ResumeSessionRun: &pb.ResumeSessionRunCommand{}}}
	merge := &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_OperatorMergeSession{OperatorMergeSession: &pb.OperatorMergeSessionCommand{}}}
	stop := &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_Stop{Stop: &pb.StopSessionCommand{}}}

	tests := []struct {
		name string
		set  StreamCommandSet
		cmd  *pb.OrchestratorCommand
		want bool
	}{
		{"legacy daemon runs stop", NewStreamCommandSet(nil), stop, false},
		{"legacy daemon runs run_trigger_gate", NewStreamCommandSet(nil), triggerGate, false},
		{"legacy daemon drops dismiss", NewStreamCommandSet(nil), dismiss, true},
		{"legacy daemon drops resume run", NewStreamCommandSet(nil), resume, true},
		{"legacy daemon drops operator merge", NewStreamCommandSet(nil), merge, true},
		{"zero value is a legacy daemon", StreamCommandSet{}, merge, true},
		{"current daemon runs operator merge", NewStreamCommandSet(StreamCommands()), merge, false},
		{"advertised set is exact", NewStreamCommandSet([]int32{2}), triggerGate, true},
		{"no command arm is not judged", NewStreamCommandSet([]int32{2}), &pb.OrchestratorCommand{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.Drops(tt.cmd); got != tt.want {
				t.Fatalf("Drops = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStreamCommandsListsEveryOneofField(t *testing.T) {
	got := StreamCommands()
	for _, want := range []int32{2, 65, 68, 69, 70} {
		if !slices.Contains(got, want) {
			t.Fatalf("StreamCommands() = %v, missing %d", got, want)
		}
	}
	if n := commandOneof.Fields().Len(); len(got) != n {
		t.Fatalf("StreamCommands() has %d fields, the oneof has %d", len(got), n)
	}
}

func TestUnsupportedCommandText(t *testing.T) {
	if got, want := UnsupportedCommandText("mac-mini"), "boss on mac-mini is too old to do this from here"; got != want {
		t.Fatalf("UnsupportedCommandText(mac-mini) = %q, want %q", got, want)
	}
	if got, want := UnsupportedCommandText(""), "this machine's boss is too old to do this from here"; got != want {
		t.Fatalf("UnsupportedCommandText(\"\") = %q, want %q", got, want)
	}
}
