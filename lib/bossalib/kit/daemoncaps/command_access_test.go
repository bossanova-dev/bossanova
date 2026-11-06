package daemoncaps

import (
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestCommandAccessExhaustive(t *testing.T) {
	for _, number := range StreamCommands() {
		cmd := &pb.OrchestratorCommand{}
		m := cmd.ProtoReflect()
		field := m.Descriptor().Fields().ByNumber(protoreflect.FieldNumber(number))
		m.Set(field, m.NewField(field))
		if _, ok := OrchestratorCommandAccess(cmd); !ok {
			t.Errorf("unclassified command %s", field.Name())
		}
	}
	msg := &pb.TerminalClientMessage{}
	fields := msg.ProtoReflect().Descriptor().Oneofs().ByName("msg").Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		m := (&pb.TerminalClientMessage{}).ProtoReflect()
		m.Set(field, m.NewField(field))
		if _, ok := TerminalMessageAccess(m.Interface().(*pb.TerminalClientMessage)); !ok {
			t.Errorf("unclassified terminal message %s", field.Name())
		}
	}
}

func TestOrchestratorCommandAccess(t *testing.T) {
	tests := []struct {
		name string
		cmd  *pb.OrchestratorCommand
		want CommandAccess
		ok   bool
	}{
		{"message", &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_SendChatMessage{}}, AccessControl, true},
		{"session", &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_CreateSession{}}, AccessControl, true},
		{"repo", &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_UpdateRepo{}}, AccessControl, true},
		{"cron", &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_CreateCronJob{}}, AccessControl, true},
		{"trigger", &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_LaunchTriggerSession{}}, AccessControl, true},
		{"broadcast", &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_Broadcast{}}, AccessControl, true},
		{"webhook", &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_Webhook{}}, AccessHousekeeping, true},
		{"transcript", &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_GetChatTranscript{}}, AccessObserve, true},
		{"repos", &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_ListRepos{}}, AccessObserve, true},
		{"credential", &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_AgentCredentialAvailable{}}, AccessHousekeeping, true},
		{"unset", &pb.OrchestratorCommand{}, AccessControl, false},
		{"nil", nil, AccessControl, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := OrchestratorCommandAccess(tt.cmd)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("access = (%v, %v), want (%v, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestTerminalMessageAccess(t *testing.T) {
	tests := []struct {
		name string
		msg  *pb.TerminalClientMessage
		want CommandAccess
		ok   bool
	}{
		{"attach", &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Attach{}}, AccessObserve, true},
		{"ready", &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Ready{}}, AccessObserve, true},
		{"ping", &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Ping{}}, AccessObserve, true},
		{"close", &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Close{}}, AccessObserve, true},
		{"input", &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Input{}}, AccessControl, true},
		{"resize", &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Resize{}}, AccessControl, true},
		{"upload start", &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_UploadStart{}}, AccessControl, true},
		{"upload chunk", &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_UploadChunk{}}, AccessControl, true},
		{"upload finish", &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_UploadFinish{}}, AccessControl, true},
		{"upload cancel", &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_UploadCancel{}}, AccessControl, true},
		{"unset", &pb.TerminalClientMessage{}, AccessControl, false},
		{"nil", nil, AccessControl, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := TerminalMessageAccess(tt.msg)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("access = (%v, %v), want (%v, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestObserveCommandFields(t *testing.T) {
	got := ObserveCommandFields()
	if !slices.IsSorted(got) {
		t.Fatalf("fields are not sorted: %v", got)
	}
	for _, number := range StreamCommands() {
		cmd := &pb.OrchestratorCommand{}
		m := cmd.ProtoReflect()
		field := m.Descriptor().Fields().ByNumber(protoreflect.FieldNumber(number))
		m.Set(field, m.NewField(field))
		access, ok := OrchestratorCommandAccess(cmd)
		if !ok {
			t.Fatalf("unclassified %s", field.Name())
		}
		if want := access != AccessControl; slices.Contains(got, number) != want {
			t.Errorf("field %s membership = %v, want %v", field.Name(), !want, want)
		}
	}
}

func TestCommandAccessString(t *testing.T) {
	for _, tt := range []struct {
		access CommandAccess
		want   string
	}{
		{AccessObserve, "observe"}, {AccessControl, "control"}, {AccessHousekeeping, "housekeeping"}, {CommandAccess(-1), "unknown"},
	} {
		if got := tt.access.String(); got != tt.want {
			t.Errorf("String(%d) = %q, want %q", tt.access, got, tt.want)
		}
	}
}
