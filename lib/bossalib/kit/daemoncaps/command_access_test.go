package daemoncaps

import (
	"slices"
	"strings"
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

func TestContentClassificationExhaustive(t *testing.T) {
	for _, item := range []struct {
		oneof   protoreflect.OneofDescriptor
		access  map[protoreflect.FieldNumber]CommandAccess
		content map[protoreflect.FieldNumber]bool
	}{{commandOneof, commandAccess, commandContent}, {terminalOneof, terminalAccess, terminalContent}} {
		fields := item.oneof.Fields()
		for i := 0; i < fields.Len(); i++ {
			field := fields.Get(i)
			if item.access[field.Number()] == AccessObserve {
				if _, ok := item.content[field.Number()]; !ok {
					t.Errorf("observe arm %s has no content classification", field.FullName())
				}
			}
		}
	}
}

func TestCloudControlAllows(t *testing.T) {
	content := map[protoreflect.Name]bool{"attach": true, "get_chat_transcript": true, "list_notes": true, "list_tracker_issues": true}
	for _, name := range []protoreflect.Name{"send_chat_message", "get_chat_transcript", "attach", "list_notes", "list_tracker_issues", "list_repos", "get_session_statuses", "list_chats", "command_cancel", ""} {
		cmd := &pb.OrchestratorCommand{}
		if name != "" {
			field := commandOneof.Fields().ByName(name)
			cmd.ProtoReflect().Set(field, cmd.ProtoReflect().NewField(field))
		}
		for _, mode := range []pb.CloudControl{pb.CloudControl_CLOUD_CONTROL_UNSPECIFIED, pb.CloudControl_CLOUD_CONTROL_FULL, pb.CloudControl_CLOUD_CONTROL_READ_ONLY, pb.CloudControl_CLOUD_CONTROL_PRIVATE} {
			want := mode == pb.CloudControl_CLOUD_CONTROL_FULL || mode == pb.CloudControl_CLOUD_CONTROL_UNSPECIFIED || (name != "send_chat_message" && name != "" && (mode != pb.CloudControl_CLOUD_CONTROL_PRIVATE || !content[name]))
			if got := CloudControlAllows(mode, cmd); got != want {
				t.Errorf("mode %s arm %s allowed=%v want=%v", mode, name, got, want)
			}
		}
		if name != "" && name != "send_chat_message" && name != "command_cancel" && OrchestratorCommandCarriesContent(cmd) != content[name] {
			t.Errorf("arm %s has incorrect content classification", name)
		}
	}
	if !OrchestratorCommandCarriesContent(nil) || !OrchestratorCommandCarriesContent(&pb.OrchestratorCommand{}) {
		t.Fatal("unset commands must fail closed")
	}
}

func TestCloudControlAllowsTerminal(t *testing.T) {
	for _, name := range []protoreflect.Name{"attach", "ready", "ping", "close", "input", "resize", "upload_start", "upload_chunk", "upload_finish", "upload_cancel", ""} {
		msg := &pb.TerminalClientMessage{}
		if name != "" {
			field := terminalOneof.Fields().ByName(name)
			msg.ProtoReflect().Set(field, msg.ProtoReflect().NewField(field))
		}
		for _, mode := range []pb.CloudControl{pb.CloudControl_CLOUD_CONTROL_UNSPECIFIED, pb.CloudControl_CLOUD_CONTROL_FULL, pb.CloudControl_CLOUD_CONTROL_READ_ONLY, pb.CloudControl_CLOUD_CONTROL_PRIVATE} {
			observe := name == "attach" || name == "ready" || name == "ping" || name == "close"
			want := mode == pb.CloudControl_CLOUD_CONTROL_FULL || mode == pb.CloudControl_CLOUD_CONTROL_UNSPECIFIED || (observe && (name != "attach" || mode != pb.CloudControl_CLOUD_CONTROL_PRIVATE))
			if got := CloudControlAllowsTerminal(mode, msg); got != want {
				t.Errorf("mode %s arm %s allowed=%v want=%v", mode, name, got, want)
			}
		}
		if name == "attach" || name == "ready" || name == "ping" || name == "close" {
			if got := TerminalMessageCarriesContent(msg); got != (name == "attach") {
				t.Errorf("terminal %s content=%v", name, got)
			}
		}
	}
	if !TerminalMessageCarriesContent(nil) || !TerminalMessageCarriesContent(&pb.TerminalClientMessage{}) {
		t.Fatal("unset messages must fail closed")
	}
}

func TestPrivateRemedySharesContentWithoutControl(t *testing.T) {
	for _, number := range StreamCommands() {
		fieldNumber := protoreflect.FieldNumber(number)
		if commandAccess[fieldNumber] != AccessObserve || !commandContent[fieldNumber] {
			continue
		}
		cmd := &pb.OrchestratorCommand{}
		m := cmd.ProtoReflect()
		field := m.Descriptor().Fields().ByNumber(fieldNumber)
		m.Set(field, m.NewField(field))
		t.Run(string(field.Name()), func(t *testing.T) {
			if CloudControlAllows(pb.CloudControl_CLOUD_CONTROL_PRIVATE, cmd) {
				t.Fatal("private mode allows content")
			}
			if !CloudControlAllows(privateRemedyMode, cmd) {
				t.Fatal("recommended mode refuses content")
			}
		})
	}
	t.Run("terminal attach", func(t *testing.T) {
		msg := &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Attach{}}
		if CloudControlAllowsTerminal(pb.CloudControl_CLOUD_CONTROL_PRIVATE, msg) {
			t.Fatal("private mode allows terminal content")
		}
		if !CloudControlAllowsTerminal(privateRemedyMode, msg) {
			t.Fatal("recommended mode refuses terminal content")
		}
	})
	t.Run("control", func(t *testing.T) {
		cmd := &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_SendChatMessage{}}
		if CloudControlAllows(privateRemedyMode, cmd) {
			t.Fatal("recommended mode allows remote control")
		}
	})
}

func TestCloudControlPrivateTextOffersRemedy(t *testing.T) {
	text := CloudControlPrivateText("h")
	if !strings.Contains(text, "`"+privateRemedySetting+"`") {
		t.Fatalf("refusal does not offer recommended setting: %q", text)
	}
	if strings.Contains(text, "`full`") {
		t.Fatalf("refusal offers full control: %q", text)
	}
}
