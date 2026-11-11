package upstream

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/daemoncaps"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type fixedCloudControl config.CloudControlMode

func (p fixedCloudControl) Mode() config.CloudControlMode { return config.CloudControlMode(p) }

func TestCloudControl_ReadOnlyRefusesEveryControlArm_Sessions_AC210201(t *testing.T) {
	client := NewStreamClient(StreamClientConfig{Logger: zerolog.Nop()})
	client.cloudControl = fixedCloudControl(config.CloudControlReadOnly)
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
	for _, mode := range []config.CloudControlMode{config.CloudControlReadOnly, config.CloudControlFull, config.CloudControlPrivate} {
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
			if mode == config.CloudControlPrivate {
				want = pb.CloudControl_CLOUD_CONTROL_PRIVATE
			}
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
	if policy.Mode() != config.CloudControlFull {
		t.Fatal("missing settings did not default to full")
	}
	for _, raw := range []string{"read-only", "", "unexpected", "full", " ", "private"} {
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

func TestCloudControl_ExistingTerminalOutputRespectsPrivate(t *testing.T) {
	for _, initial := range []string{"read-only", "full"} {
		for _, queued := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/queued=%t", initial, queued), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "settings.json")
					t.Setenv("BOSS_SETTINGS_PATH", path)
					setMode := func(mode string) {
						t.Helper()
						if err := config.SaveTo(path, config.Settings{CloudControl: mode}); err != nil {
							t.Fatal(err)
						}
					}
					setMode(initial)
					h := newTestHarness(t)
					h.client.cloudControl = NewCloudControlPolicy(zerolog.Nop())
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					out := make(chan *pb.TerminalServerMessage, 8)
					h.client.handleAttach(ctx, &pb.TerminalAttachCommand{AttachId: "viewer", ChatId: "claude-1"}, out)
					t.Cleanup(h.client.closeAllAttaches)
					attach := h.factory.attaches["viewer"]
					state := h.client.lookupAttach("viewer")
					if attach == nil || state == nil {
						t.Fatal("attachment did not start")
					}
					// The writer case queues multiple frames while sharing is enabled;
					// the pump case encounters new private content with no writer running.
					if queued {
						attach.output <- &pb.TerminalDataChunk{AttachId: "viewer", Data: []byte("queued content")}
						synctest.Wait()
						out <- &pb.TerminalServerMessage{Msg: &pb.TerminalServerMessage_Pong{Pong: &pb.TerminalPong{Seq: 1}}}
						attach.output <- &pb.TerminalDataChunk{AttachId: "viewer", Data: []byte("more queued content")}
						synctest.Wait()
						if len(out) != 3 {
							t.Fatalf("queued frames = %d", len(out))
						}
					} else {
						attach.output <- &pb.TerminalDataChunk{AttachId: "viewer", Data: []byte("shared content")}
						synctest.Wait()
						if len(out) != 1 {
							t.Fatalf("got %d initial frames, want one output frame", len(out))
						}
						if got := (<-out).GetData(); string(got.GetData()) != "shared content" {
							t.Fatalf("initial terminal output = %v", got)
						}
					}
					setMode("private")
					if !queued {
						attach.output <- &pb.TerminalDataChunk{AttachId: "viewer", Data: []byte("private content")}
						synctest.Wait()
						if !attach.closed.Load() {
							t.Fatal("private output left the attach alive")
						}
						// Re-enabling sharing cannot revive the terminal whose bytes were lost.
						setMode(initial)
					}
					stream := newFakeTerminalStream()
					stream.sent = make(chan *pb.TerminalServerMessage)
					writerDone := safego.Go(zerolog.Nop(), func() { h.client.runWriter(ctx, stream, out) })
					defer func() { cancel(); <-writerDone }()
					synctest.Wait()
					if !attach.closed.Load() {
						t.Fatal("queued private output left the attach alive")
					}
					select {
					case <-state.done:
					default:
						t.Fatal("revoked attach pump did not finish")
					}
					if queued {
						// Resume while the writer is paused on housekeeping, with another
						// old chunk still queued behind it. That chunk must stay suppressed.
						setMode(initial)
						if msg := <-stream.sent; msg.GetPong() == nil {
							t.Fatalf("housekeeping = %v", msg)
						}
					}
					exit := (<-stream.sent).GetExited()
					if exit.GetAttachId() != "viewer" || exit.GetReason() != "cloud control is private on this machine" || exit.GetExitCode() != -1 {
						t.Fatalf("revoked terminal exit = %v", exit)
					}
					if h.client.lookupAttach("viewer") != nil {
						t.Fatal("revoked attach remains registered")
					}
					setMode(initial)
					h.client.handleAttach(ctx, &pb.TerminalAttachCommand{AttachId: "reopened", ChatId: "claude-1"}, out)
					fresh := h.factory.attaches["reopened"]
					if fresh == nil {
						t.Fatal("fresh attachment did not start")
					}
					fresh.output <- &pb.TerminalDataChunk{AttachId: "reopened", Data: []byte("fresh screen")}
					synctest.Wait()
					data := (<-stream.sent).GetData()
					if data.GetAttachId() != "reopened" || string(data.GetData()) != "fresh screen" {
						t.Fatalf("reopened output = %v", data)
					}
				})
			})
		}
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
		{Cmd: &pb.OrchestratorCommand_GetChatTranscript{GetChatTranscript: &pb.GetChatTranscriptCommand{SessionId: "s", AgentSessionId: "chat"}}},
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

func TestCloudControl_PrivateRefusesContentBeforeHandlers(t *testing.T) {
	// No handlers are wired: a content refusal must happen before dispatch.
	client := NewStreamClient(StreamClientConfig{Logger: zerolog.Nop(), Hostname: "private-host", CloudControl: fixedCloudControl(config.CloudControlPrivate)})
	for _, name := range []protoreflect.Name{"attach", "get_chat_transcript", "get_note", "list_notes", "list_cron_jobs", "get_cron_job", "list_github_callbacks", "list_tracker_issues", "send_chat_message"} {
		t.Run(string(name), func(t *testing.T) {
			cmd := &pb.OrchestratorCommand{CommandId: "private-refusal"}
			field := cmd.ProtoReflect().Descriptor().Oneofs().ByName("cmd").Fields().ByName(name)
			cmd.ProtoReflect().Set(field, cmd.ProtoReflect().NewField(field))
			result := client.dispatchCommand(context.Background(), cmd, nil).GetResult()
			wantText := daemoncaps.CloudControlPrivateText("private-host")
			if name == "send_chat_message" {
				wantText = client.cloudControlRefusal()
			}
			if result.GetOk() || result.GetErrorCode() != pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED || result.GetError() != wantText {
				t.Fatalf("refusal = %v, want disabled with %q", result, wantText)
			}
		})
	}
}

func TestCloudControl_PrivateAnswersMetadata(t *testing.T) {
	handler := &fakeCommandHandler{sessionStatusesResult: &pb.GetSessionStatusesResponse{}}
	client := newDispatcherClient(handler, nil, nil)
	client.cloudControl = fixedCloudControl(config.CloudControlPrivate)
	out := make(chan *pb.DaemonEvent, 4)
	for _, name := range []protoreflect.Name{"list_repos", "list_chats", "get_session_statuses"} {
		cmd := &pb.OrchestratorCommand{}
		field := cmd.ProtoReflect().Descriptor().Oneofs().ByName("cmd").Fields().ByName(name)
		cmd.ProtoReflect().Set(field, cmd.ProtoReflect().NewField(field))
		if name == "list_chats" {
			cmd.GetListChats().SessionId = "s"
		}
		event := client.dispatchCommand(context.Background(), cmd, out)
		if event == nil {
			event = recvEvent(t, out)
		}
		if !event.GetResult().GetOk() {
			t.Errorf("metadata %s = %v", name, event)
		}
	}
}

func TestCloudControl_PrivateTerminalAttachAfterLookup(t *testing.T) {
	h := newTestHarness(t)
	h.client.cloudControl = fixedCloudControl(config.CloudControlPrivate)
	out := make(chan *pb.TerminalServerMessage, 2)
	h.client.handleClientMessage(context.Background(), &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Attach{Attach: &pb.TerminalAttachCommand{AttachId: "private", ChatId: "claude-1"}}}, out, nil)
	if len(out) != 1 {
		t.Fatalf("got %d frames, want one refusal", len(out))
	}
	exited := (<-out).GetExited()
	if exited.GetAttachId() != "private" || exited.GetReason() != "cloud control is private on this machine" {
		t.Fatalf("attach refusal = %v", exited)
	}
	if h.factory.lastConfig().AttachID != "" {
		t.Fatal("private attach spawned tmux")
	}
}

func (p fixedCloudControl) RepoMode(context.Context, string) (config.CloudControlMode, config.CloudControlSource) {
	return p.Mode(), config.CloudControlSourceMachine
}

func TestCloudControl_RepoPrivateTerminalAttach(t *testing.T) {
	h := newTestHarness(t)
	h.client.cloudControl = targetPolicy{machine: config.CloudControlFull, repos: map[string]config.CloudControlMode{"fixture": config.CloudControlPrivate}}
	out := make(chan *pb.TerminalServerMessage, 8)
	h.client.handleAttach(t.Context(), &pb.TerminalAttachCommand{AttachId: "private", ChatId: "claude-1"}, out)
	defer h.client.closeAllAttaches()
	select {
	case msg := <-out:
		if msg.GetExited().GetReason() != "cloud control is private on this machine" {
			t.Fatalf("private attach = %v", msg)
		}
	default:
		t.Fatal("repo-private attach was accepted under machine full")
	}
}

func TestCloudControl_PrivateRevokesIdleTerminal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _ := newRealManager(t, &recordingChatSender{})
		t.Cleanup(manager.CancelAll)
		h := newUploadHarness(t, manager)
		clock := newFakeClock()
		h.client.clock = clock
		policy := &switchableCloudControl{}
		policy.mode.Store(config.CloudControlFull)
		h.client.cloudControl = &repoSwitchPolicy{repo: policy}
		ctx := t.Context()
		out := make(chan *pb.TerminalServerMessage, 8)
		session := newTerminalStreamSession()
		h.client.handleClientMessage(ctx, &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Attach{
			Attach: &pb.TerminalAttachCommand{AttachId: "viewer", ChatId: "claude-1"},
		}}, out, session)
		defer h.client.closeAllAttaches()
		attach := h.factory.attaches["viewer"]
		if attach == nil {
			t.Fatal("attachment did not start")
		}
		if err := manager.Start("viewer", "upload", "claude-1", "upload.txt", 2); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		clock.Advance(time.Second)
		synctest.Wait()
		if attach.closed.Load() || len(out) != 0 {
			t.Fatal("unchanged policy interrupted an idle terminal")
		}

		policy.mode.Store(config.CloudControlPrivate)
		clock.Advance(time.Second)
		synctest.Wait()
		if !attach.closed.Load() || h.client.lookupOutputAttach("viewer") != nil {
			t.Fatal("idle terminal stayed attached after one recheck interval")
		}
		if len(out) != 1 {
			t.Fatalf("got %d frames, want one synthetic exit", len(out))
		}
		exit := (<-out).GetExited()
		if exit.GetAttachId() != "viewer" || exit.GetExitCode() != -1 || exit.GetReason() != "cloud control is private on this machine" {
			t.Fatalf("idle revocation exit = %v", exit)
		}
		if _, err := manager.Chunk("viewer", "upload", 0, []byte("x")); err == nil {
			t.Fatal("idle revocation retained the active upload")
		}
		h.client.handleClientMessage(ctx, &pb.TerminalClientMessage{Msg: &pb.TerminalClientMessage_Ping{
			Ping: &pb.TerminalPing{Seq: 42},
		}}, out, session)
		if len(out) != 1 {
			t.Fatalf("got %d frames, want one pong", len(out))
		}
		if pong := (<-out).GetPong(); pong.GetSeq() != 42 {
			t.Fatalf("stream pong after revocation = %v", pong)
		}
	})
}

func TestCloudControl_ReadOnlyKeepsViewing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHarness(t)
		clock := newFakeClock()
		h.client.clock = clock
		policy := &switchableCloudControl{}
		policy.mode.Store(config.CloudControlReadOnly)
		h.client.cloudControl = policy
		out := make(chan *pb.TerminalServerMessage, 8)
		h.client.handleAttach(t.Context(), &pb.TerminalAttachCommand{AttachId: "viewer", ChatId: "claude-1"}, out)
		defer h.client.closeAllAttaches()
		attach := h.factory.attaches["viewer"]
		if attach == nil {
			t.Fatal("attachment did not start")
		}
		synctest.Wait()
		for _, mode := range []config.CloudControlMode{config.CloudControlFull, config.CloudControlReadOnly} {
			policy.mode.Store(mode)
			clock.Advance(time.Second)
			synctest.Wait()
			if attach.closed.Load() {
				t.Fatalf("view-only terminal closed in %s mode", mode)
			}
			attach.output <- &pb.TerminalDataChunk{AttachId: "viewer", Data: []byte(mode)}
			synctest.Wait()
			if len(out) != 1 {
				t.Fatalf("got %d frames in %s mode, want one output frame", len(out), mode)
			}
			if got := (<-out).GetData(); string(got.GetData()) != string(mode) {
				t.Fatalf("terminal output in %s mode = %v", mode, got)
			}
		}
	})
}
