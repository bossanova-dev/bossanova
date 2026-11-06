package upstream

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/daemoncaps"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type fixedCloudControl config.CloudControlMode

func (p fixedCloudControl) Mode() config.CloudControlMode { return config.CloudControlMode(p) }

func TestCloudControl_DefaultRefusesEveryControlArm_Sessions_AC210201(t *testing.T) {
	client := NewStreamClient(StreamClientConfig{Logger: zerolog.Nop()})
	fields := (&pb.OrchestratorCommand{}).ProtoReflect().Descriptor().Oneofs().ByName("cmd").Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		t.Run(string(field.Name()), func(t *testing.T) {
			cmd := &pb.OrchestratorCommand{CommandId: "refusal"}
			cmd.ProtoReflect().Set(field, protoreflect.ValueOfMessage(cmd.ProtoReflect().NewField(field).Message()))
			access, ok := daemoncaps.OrchestratorCommandAccess(cmd)
			if !ok || access != daemoncaps.AccessControl {
				return
			}
			result := client.dispatchCommand(context.Background(), cmd, nil).GetResult()
			if result.GetOk() || result.GetErrorCode() != pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED {
				t.Fatalf("result = %v", result)
			}
		})
	}
	result := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{}, nil).GetResult()
	if result.GetErrorCode() != pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED {
		t.Fatalf("unclassified result = %v", result)
	}
}

func TestCloudControl_ModeChangesAndObserve_Sessions_AC210202_AC210204(t *testing.T) {
	for _, mode := range []config.CloudControlMode{config.CloudControlReadOnly, config.CloudControlFull} {
		t.Run(string(mode), func(t *testing.T) {
			handler := &fakeCommandHandler{session: &pb.Session{Id: "s"}}
			client := newDispatcherClient(handler, nil, nil)
			client.cloudControl = fixedCloudControl(mode)
			out := make(chan *pb.DaemonEvent, 4)
			result := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_Stop{Stop: &pb.StopSessionCommand{SessionId: "s"}}}, out).GetResult()
			if (handler.stopCalls.Load() == 1) != (mode == config.CloudControlFull) {
				t.Fatalf("calls=%d result=%v", handler.stopCalls.Load(), result)
			}
			client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_ListAccounts{ListAccounts: &pb.ListAgentAccountsCommand{ShouldRefresh: true}}}, out)
			if result := recvEvent(t, out).GetResult(); !result.GetOk() {
				t.Fatalf("observe: %v", result)
			}
			if handler.listAccountsRefresh != (mode == config.CloudControlFull) {
				t.Fatalf("refresh = %v", handler.listAccountsRefresh)
			}
			snap, err := client.buildSnapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := pb.CloudControl_CLOUD_CONTROL_READ_ONLY
			if mode == config.CloudControlFull {
				want = pb.CloudControl_CLOUD_CONTROL_FULL
			}
			if snap.GetCloudControl() != want {
				t.Fatalf("snapshot mode=%v", snap.GetCloudControl())
			}
		})
	}
}

func TestCloudControl_TerminalRevocationAndUploads_Sessions_AC210203(t *testing.T) {
	h := newTestHarness(t)
	a := newFakeAttach("a")
	h.client.attaches["a"] = &activeAttach{id: "a", attach: a}
	out := make(chan *pb.TerminalServerMessage, 8)
	frames := []*pb.TerminalClientMessage{
		{Msg: &pb.TerminalClientMessage_Input{Input: &pb.TerminalInputCommand{AttachId: "a", Data: []byte("secret")}}},
		{Msg: &pb.TerminalClientMessage_Resize{Resize: &pb.TerminalResizeCommand{AttachId: "a", Cols: 200, Rows: 100}}},
	}
	h.client.cloudControl = nil
	for _, frame := range frames {
		h.client.handleClientMessage(context.Background(), frame, out, nil)
	}
	if len(a.inputs) != 0 || len(a.resizes) != 0 || len(out) != 0 {
		t.Fatal("read-only terminal wrote input, resized or emitted a frame")
	}
	uploads := []*pb.TerminalClientMessage{
		{Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{AttachId: "a", UploadId: "u", Filename: "secret"}}},
		{Msg: &pb.TerminalClientMessage_UploadChunk{UploadChunk: &pb.TerminalUploadChunk{AttachId: "a", UploadId: "u", Data: []byte("secret")}}},
		{Msg: &pb.TerminalClientMessage_UploadFinish{UploadFinish: &pb.TerminalUploadFinish{AttachId: "a", UploadId: "u"}}},
	}
	for _, frame := range uploads {
		h.client.handleClientMessage(context.Background(), frame, out, nil)
		result := (<-out).GetUploadResult()
		if result.GetIsOk() || result.GetUploadId() != "u" || result.GetErrorMessage() != "upload: cloud control is read-only on this machine" {
			t.Fatalf("upload result = %v", result)
		}
	}
	h.client.cloudControl = fixedCloudControl(config.CloudControlFull)
	for _, frame := range frames {
		h.client.handleClientMessage(context.Background(), frame, out, nil)
	}
	if len(a.inputs) != 1 || len(a.resizes) != 1 {
		t.Fatal("full terminal did not forward input and resize")
	}
	h.client.cloudControl = fixedCloudControl(config.CloudControlReadOnly)
	for _, frame := range frames {
		h.client.handleClientMessage(context.Background(), frame, out, nil)
	}
	if len(a.inputs) != 1 || len(a.resizes) != 1 {
		t.Fatal("local revocation did not stop existing terminal")
	}
}

func TestCloudControl_LocalPolicyReloadsAndFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)
	policy := NewCloudControlPolicy(zerolog.Nop())
	if policy.Mode() != config.CloudControlReadOnly {
		t.Fatal("missing settings allowed control")
	}
	for _, raw := range []string{"full", "read-only", "unexpected", "full"} {
		if err := config.SaveTo(path, config.Settings{CloudControl: raw}); err != nil {
			t.Fatal(err)
		}
		if got := policy.Mode(); got != config.ResolveCloudControl(raw) {
			t.Fatalf("after local edit mode=%v", got)
		}
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if policy.Mode() != config.CloudControlReadOnly {
		t.Fatal("unreadable settings allowed control")
	}
}

func TestCloudControl_UnreadableSettingsWarningIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	policy := NewCloudControlPolicy(zerolog.New(&logs))
	for range 100 {
		if policy.Mode() != config.CloudControlReadOnly {
			t.Fatal("unreadable settings allowed control")
		}
	}
	if got := bytes.Count(logs.Bytes(), []byte("\n")); got != 1 {
		t.Fatalf("repeated failures emitted %d log entries", got)
	}
	if err := config.SaveTo(path, config.Settings{CloudControl: "full"}); err != nil {
		t.Fatal(err)
	}
	if policy.Mode() != config.CloudControlFull {
		t.Fatal("warning suppression prevented a subsequent settings reload")
	}
}

func TestCloudControl_RevocationCleansActiveUpload(t *testing.T) {
	frames := map[string]*pb.TerminalClientMessage{
		"start":  {Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{AttachId: "a", UploadId: "u"}}},
		"chunk":  {Msg: &pb.TerminalClientMessage_UploadChunk{UploadChunk: &pb.TerminalUploadChunk{AttachId: "a", UploadId: "u", Data: []byte("x")}}},
		"finish": {Msg: &pb.TerminalClientMessage_UploadFinish{UploadFinish: &pb.TerminalUploadFinish{AttachId: "a", UploadId: "u"}}},
		"cancel": {Msg: &pb.TerminalClientMessage_UploadCancel{UploadCancel: &pb.TerminalUploadCancel{AttachId: "a", UploadId: "u"}}},
	}
	for name, frame := range frames {
		t.Run(name, func(t *testing.T) {
			manager, dir := newRealManager(t, &recordingChatSender{})
			t.Cleanup(manager.CancelAll)
			h := newUploadHarness(t, manager)
			h.client.attaches["a"] = &activeAttach{id: "a", chatID: "claude-1", attach: newFakeAttach("a")}
			out := make(chan *pb.TerminalServerMessage, 4)
			ctx := context.Background()
			h.client.handleClientMessage(ctx, &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_UploadStart{UploadStart: &pb.TerminalUploadStart{
				AttachId: "a", UploadId: "u", Filename: "upload.txt", SizeBytes: 1,
			}}}, out, nil)
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || len(out) != 0 {
				t.Fatalf("upload did not start: entries=%v err=%v responses=%d", entries, err, len(out))
			}
			h.client.cloudControl = nil
			h.client.handleClientMessage(ctx, frame, out, nil)
			entries, err = os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("revoked upload retained files: entries=%v err=%v", entries, err)
			}
			if _, err := manager.Chunk("a", "u", 0, []byte("x")); err == nil {
				t.Fatal("revoked upload retained its manager entry")
			}
			if name == "cancel" {
				if len(out) != 0 {
					t.Fatal("cancel emitted a terminal response")
				}
			} else if result := (<-out).GetUploadResult(); result == nil || result.GetIsOk() || result.GetCanRetry() {
				t.Fatalf("revocation did not return a permanent refusal: %v", result)
			}
		})
	}
}

func TestCloudControl_TerminalAttachObservesWithoutWaking(t *testing.T) {
	h := newTestHarness(t)
	h.client.cloudControl = nil
	out := make(chan *pb.TerminalServerMessage, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.client.handleAttach(ctx, &pb.TerminalAttachCommand{AttachId: "viewer", ChatId: "claude-1", Cols: 200, Rows: 100}, out)
	cfg := h.factory.lastConfig()
	if !cfg.ReadOnly || cfg.Cols != 0 || cfg.Rows != 0 {
		t.Fatalf("read-only attach config=%+v", cfg)
	}
	h.client.closeAllAttaches()
	out = make(chan *pb.TerminalServerMessage, 8)
	h.chats.rows["claude-1"] = chatRow{}
	h.client.handleAttach(ctx, &pb.TerminalAttachCommand{AttachId: "sleeping", ChatId: "claude-1"}, out)
	if got := (<-out).GetExited(); got.GetReason() != "cloud control is read-only; cannot wake chat" {
		t.Fatalf("sleeping attach=%v", got)
	}
}

func TestCloudControl_ReadOnlyObserveAndHousekeeping(t *testing.T) {
	handler := &fakeCommandHandler{transcript: &pb.GetChatTranscriptResponse{}}
	client := newDispatcherClient(handler, nil, nil)
	client.cloudControl = nil
	out := make(chan *pb.DaemonEvent, 4)
	commands := []*pb.OrchestratorCommand{
		{Cmd: &pb.OrchestratorCommand_ListRepos{ListRepos: &pb.ListReposCommand{}}},
		{Cmd: &pb.OrchestratorCommand_GetChatTranscript{GetChatTranscript: &pb.GetChatTranscriptCommand{SessionId: "s"}}},
	}
	for _, cmd := range commands {
		event := client.dispatchCommand(context.Background(), cmd, out)
		if event == nil {
			event = recvEvent(t, out)
		}
		if !event.GetResult().GetOk() {
			t.Fatalf("allowed command returned %v", event)
		}
	}
	if event := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{Cmd: &pb.OrchestratorCommand_CommandCancel{CommandCancel: &pb.CommandCancel{}}}, out); event != nil {
		t.Fatalf("housekeeping returned %v", event)
	}
	if handler.listReposCalls.Load() != 1 || handler.transcriptSession != "s" {
		t.Fatal("observation did not reach handler")
	}
}
