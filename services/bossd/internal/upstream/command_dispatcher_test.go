package upstream

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

type fakeCommandHandler struct {
	stopCalls        atomic.Int32
	pauseCalls       atomic.Int32
	resumeCalls      atomic.Int32
	wakeCalls        atomic.Int32
	mergeCalls       atomic.Int32
	archiveCalls     atomic.Int32
	recordChatCalls  atomic.Int32
	deleteChatCalls  atomic.Int32
	deleteChatScope  string // last sessionID passed to DeleteChat
	listReposCalls   atomic.Int32
	listAgentsCalls  atomic.Int32
	returnErr        error
	session          *pb.Session
	mergeSession     *pb.Session
	archiveSession   *pb.Session
	recordChatResult *pb.ClaudeChat
	listReposResult  *pb.ListReposResponse
	listAgentsResult *pb.ListAgentsResponse
	// ListRepoPRs / ListTrackerIssues knobs.
	repoPRs    *pb.ListRepoPRsResponse
	issues     *pb.ListTrackerIssuesResponse
	lastRepoID string
	lastQuery  string
	lastSource *string
	// issuesBlock, when non-nil, makes ListTrackerIssues block until the
	// channel is closed — used to prove a slow tracker search doesn't wedge
	// the single-threaded command reader.
	issuesBlock chan struct{}
	// WakeChat-specific knobs.
	wakeOutcome   pb.WakeChatResult_Outcome
	wakeTmuxName  string
	wakeReason    string
	wakeErrorCode pb.CommandResult_ErrorCode
	wakeErr       error
	// GetChatTranscript / SendChatMessage knobs.
	transcript        *pb.GetChatTranscriptResponse
	transcriptSession string // last sessionID passed to GetChatTranscript
	sendResult        *pb.SendChatMessageResponse
	sendAgentID       string // last agentSessionID passed to SendChatMessage
	sendMessage       string // last message passed to SendChatMessage
}

func (f *fakeCommandHandler) Stop(_ context.Context, _ string) (*pb.Session, error) {
	f.stopCalls.Add(1)
	return f.session, f.returnErr
}
func (f *fakeCommandHandler) Pause(_ context.Context, _ string) (*pb.Session, error) {
	f.pauseCalls.Add(1)
	return f.session, f.returnErr
}
func (f *fakeCommandHandler) Resume(_ context.Context, _ string) (*pb.Session, error) {
	f.resumeCalls.Add(1)
	return f.session, f.returnErr
}
func (f *fakeCommandHandler) WakeChat(_ context.Context, _ string, _ bool) (pb.WakeChatResult_Outcome, string, string, pb.CommandResult_ErrorCode, error) {
	f.wakeCalls.Add(1)
	return f.wakeOutcome, f.wakeTmuxName, f.wakeReason, f.wakeErrorCode, f.wakeErr
}
func (f *fakeCommandHandler) MergeSession(_ context.Context, _ string) (*pb.Session, error) {
	f.mergeCalls.Add(1)
	return f.mergeSession, f.returnErr
}
func (f *fakeCommandHandler) ArchiveSession(_ context.Context, _ string) (*pb.Session, error) {
	f.archiveCalls.Add(1)
	return f.archiveSession, f.returnErr
}
func (f *fakeCommandHandler) RecordChat(_ context.Context, _, _, _ string, _ bool, _ string) (*pb.ClaudeChat, error) {
	f.recordChatCalls.Add(1)
	return f.recordChatResult, f.returnErr
}
func (f *fakeCommandHandler) DeleteChat(_ context.Context, sessionID, _ string) error {
	f.deleteChatCalls.Add(1)
	f.deleteChatScope = sessionID
	return f.returnErr
}
func (f *fakeCommandHandler) ListRepos(_ context.Context) (*pb.ListReposResponse, error) {
	f.listReposCalls.Add(1)
	return f.listReposResult, f.returnErr
}
func (f *fakeCommandHandler) ListAgents(_ context.Context) (*pb.ListAgentsResponse, error) {
	f.listAgentsCalls.Add(1)
	return f.listAgentsResult, f.returnErr
}
func (f *fakeCommandHandler) ListRepoPRs(_ context.Context, repoID string) (*pb.ListRepoPRsResponse, error) {
	f.lastRepoID = repoID
	return f.repoPRs, f.returnErr
}
func (f *fakeCommandHandler) ListTrackerIssues(_ context.Context, repoID, query string, source *string) (*pb.ListTrackerIssuesResponse, error) {
	f.lastRepoID = repoID
	f.lastQuery = query
	f.lastSource = source
	if f.issuesBlock != nil {
		<-f.issuesBlock
	}
	return f.issues, f.returnErr
}

func (f *fakeCommandHandler) GetChatTranscript(_ context.Context, sessionID, _ string, _ int32) (*pb.GetChatTranscriptResponse, error) {
	f.transcriptSession = sessionID
	return f.transcript, f.returnErr
}
func (f *fakeCommandHandler) SendChatMessage(_ context.Context, agentSessionID, message string, _ bool) (*pb.SendChatMessageResponse, error) {
	f.sendAgentID = agentSessionID
	f.sendMessage = message
	return f.sendResult, f.returnErr
}

func strPtr(s string) *string { return &s }

// recvEvent reads one DaemonEvent from out, failing if none arrives promptly.
func recvEvent(t *testing.T, out <-chan *pb.DaemonEvent) *pb.DaemonEvent {
	t.Helper()
	select {
	case ev := <-out:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for daemon event on outbound")
		return nil
	}
}

// TestHandleCommand_SlowListDoesNotBlockReader is the regression test for the
// new-session wizard hang: a slow tracker search (Linear/Sentry/GitHub network
// call) must not wedge the single-threaded command reader and starve every
// other command. A fast Stop dispatched right after a blocking ListTrackerIssues
// must complete without waiting for the search to finish.
func TestHandleCommand_SlowListDoesNotBlockReader(t *testing.T) {
	release := make(chan struct{})
	fake := &fakeCommandHandler{
		session:     &pb.Session{Id: "s1"},
		issues:      &pb.ListTrackerIssuesResponse{},
		issuesBlock: release,
	}
	client := newDispatcherClient(fake, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	ctx := context.Background()

	// Slow tracker search first — handleCommand must return immediately even
	// though the handler is still blocked inside ListTrackerIssues.
	client.handleCommand(ctx, &pb.OrchestratorCommand{
		CommandId: "slow",
		Cmd: &pb.OrchestratorCommand_ListTrackerIssues{ListTrackerIssues: &pb.ListTrackerIssuesCommand{
			RepoId: "r1",
		}},
	}, out)

	// Fast lifecycle command right behind it. With a synchronous reader this
	// would never run; with async list dispatch it completes at once.
	client.handleCommand(ctx, &pb.OrchestratorCommand{
		CommandId: "fast",
		Cmd:       &pb.OrchestratorCommand_Stop{Stop: &pb.StopSessionCommand{SessionId: "s1"}},
	}, out)

	ev := recvEvent(t, out)
	if got := ev.GetResult().GetCommandId(); got != "fast" {
		t.Fatalf("expected fast command result first, got %q (slow search wedged the reader)", got)
	}

	// Let the slow search finish so its goroutine doesn't leak.
	close(release)
	ev = recvEvent(t, out)
	if got := ev.GetResult().GetCommandId(); got != "slow" {
		t.Fatalf("expected slow command result after release, got %q", got)
	}
}

func TestDispatch_ListRepoPRs(t *testing.T) {
	fake := &fakeCommandHandler{repoPRs: &pb.ListRepoPRsResponse{PullRequests: []*pb.PRSummary{{Number: 7, Title: "x"}}}}
	client := newDispatcherClient(fake, nil, nil)

	// List handlers dispatch asynchronously (network-bound; must not block the
	// reader), so the result lands on outbound rather than the return value.
	out := make(chan *pb.DaemonEvent, 1)
	if ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "c1",
		Cmd:       &pb.OrchestratorCommand_ListRepoPrs{ListRepoPrs: &pb.ListRepoPRsCommand{RepoId: "r1"}},
	}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async list command, got %+v", ev)
	}

	ev := recvEvent(t, out)
	res := ev.GetResult()
	if res == nil || !res.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
	}
	if res.GetCommandId() != "c1" {
		t.Fatalf("command_id = %q, want c1", res.GetCommandId())
	}
	prs := res.GetListRepoPrs().GetPullRequests()
	if len(prs) == 0 || prs[0].GetNumber() != 7 {
		t.Fatalf("expected PR number 7, got %+v", prs)
	}
	if fake.lastRepoID != "r1" {
		t.Fatalf("lastRepoID = %q, want r1", fake.lastRepoID)
	}
}

func TestDispatch_ListTrackerIssues(t *testing.T) {
	fake := &fakeCommandHandler{issues: &pb.ListTrackerIssuesResponse{Issues: []*pb.TrackerIssue{{ExternalId: "A-1"}}}}
	client := newDispatcherClient(fake, nil, nil)

	out := make(chan *pb.DaemonEvent, 1)
	if ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "c2",
		Cmd: &pb.OrchestratorCommand_ListTrackerIssues{ListTrackerIssues: &pb.ListTrackerIssuesCommand{
			RepoId: "r1", Query: "log", Source: strPtr("linear"),
		}},
	}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async list command, got %+v", ev)
	}

	ev := recvEvent(t, out)
	res := ev.GetResult()
	if res == nil || !res.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
	}
	if res.GetCommandId() != "c2" {
		t.Fatalf("command_id = %q, want c2", res.GetCommandId())
	}
	issues := res.GetListTrackerIssues().GetIssues()
	if len(issues) == 0 || issues[0].GetExternalId() != "A-1" {
		t.Fatalf("expected issue A-1, got %+v", issues)
	}
	if fake.lastQuery != "log" {
		t.Fatalf("lastQuery = %q, want log", fake.lastQuery)
	}
	if fake.lastSource == nil {
		t.Fatalf("lastSource = nil, want non-nil")
	}
	if *fake.lastSource != "linear" {
		t.Fatalf("lastSource = %q, want linear", *fake.lastSource)
	}
}

func TestDispatch_GetChatTranscript(t *testing.T) {
	fake := &fakeCommandHandler{transcript: &pb.GetChatTranscriptResponse{
		Messages:           []*pb.ChatMessage{{Text: "hi"}},
		FinalAssistantText: "done",
		Exists:             true,
	}}
	client := newDispatcherClient(fake, nil, nil)

	out := make(chan *pb.DaemonEvent, 1)
	if ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "ct1",
		Cmd: &pb.OrchestratorCommand_GetChatTranscript{GetChatTranscript: &pb.GetChatTranscriptCommand{
			SessionId: "s1", AgentSessionId: "agent-1", MaxMessages: 5,
		}},
	}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async command, got %+v", ev)
	}

	ev := recvEvent(t, out)
	res := ev.GetResult()
	if res == nil || !res.GetOk() || res.GetCommandId() != "ct1" {
		t.Fatalf("expected ok result for ct1, got %+v", ev)
	}
	tr := res.GetGetChatTranscript()
	if tr == nil || !tr.GetExists() || tr.GetFinalAssistantText() != "done" || len(tr.GetMessages()) != 1 {
		t.Fatalf("unexpected transcript payload: %+v", tr)
	}
	if fake.transcriptSession != "s1" {
		t.Fatalf("session scope not forwarded: %q", fake.transcriptSession)
	}
}

func TestDispatch_GetChatTranscript_TypedError(t *testing.T) {
	fake := &fakeCommandHandler{returnErr: connect.NewError(connect.CodeNotFound, fmt.Errorf("no such chat"))}
	client := newDispatcherClient(fake, nil, nil)

	out := make(chan *pb.DaemonEvent, 1)
	client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "ct2",
		Cmd:       &pb.OrchestratorCommand_GetChatTranscript{GetChatTranscript: &pb.GetChatTranscriptCommand{AgentSessionId: "a"}},
	}, out)

	ev := recvEvent(t, out)
	res := ev.GetResult()
	if res == nil || res.GetOk() {
		t.Fatalf("expected failed result, got %+v", ev)
	}
	if res.GetErrorCode() != pb.CommandResult_ERROR_CODE_NOT_FOUND {
		t.Fatalf("error code = %v, want NOT_FOUND", res.GetErrorCode())
	}
}

func TestDispatch_GetChatTranscript_HandlerNotWired(t *testing.T) {
	client := newDispatcherClient(nil, nil, nil)
	out := make(chan *pb.DaemonEvent, 1)
	ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "ct3",
		Cmd:       &pb.OrchestratorCommand_GetChatTranscript{GetChatTranscript: &pb.GetChatTranscriptCommand{}},
	}, out)
	// Nil handler is reported synchronously (before the async goroutine spawns).
	if ev == nil || ev.GetResult().GetOk() {
		t.Fatalf("expected synchronous wired-error result, got %+v", ev)
	}
}

func TestDispatch_SendChatMessage(t *testing.T) {
	fake := &fakeCommandHandler{sendResult: &pb.SendChatMessageResponse{TmuxSessionName: "boss-x", Delivered: true}}
	client := newDispatcherClient(fake, nil, nil)

	out := make(chan *pb.DaemonEvent, 1)
	if ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "sm1",
		Cmd: &pb.OrchestratorCommand_SendChatMessage{SendChatMessage: &pb.SendChatMessageCommand{
			AgentSessionId: "agent-1", Message: "hello", WakeIfAsleep: true,
		}},
	}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async command, got %+v", ev)
	}

	ev := recvEvent(t, out)
	res := ev.GetResult()
	if res == nil || !res.GetOk() || res.GetCommandId() != "sm1" {
		t.Fatalf("expected ok result for sm1, got %+v", ev)
	}
	sm := res.GetSendChatMessage()
	if sm == nil || !sm.GetDelivered() || sm.GetTmuxSessionName() != "boss-x" {
		t.Fatalf("unexpected send payload: %+v", sm)
	}
	if fake.sendAgentID != "agent-1" || fake.sendMessage != "hello" {
		t.Fatalf("send fields not forwarded: agent=%q msg=%q", fake.sendAgentID, fake.sendMessage)
	}
}

func TestDispatch_SendChatMessage_TypedError(t *testing.T) {
	fake := &fakeCommandHandler{returnErr: connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("chat asleep"))}
	client := newDispatcherClient(fake, nil, nil)

	out := make(chan *pb.DaemonEvent, 1)
	client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "sm2",
		Cmd:       &pb.OrchestratorCommand_SendChatMessage{SendChatMessage: &pb.SendChatMessageCommand{AgentSessionId: "a", Message: "m"}},
	}, out)

	ev := recvEvent(t, out)
	res := ev.GetResult()
	if res == nil || res.GetOk() {
		t.Fatalf("expected failed result, got %+v", ev)
	}
	if res.GetErrorCode() != pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("error code = %v, want FAILED_PRECONDITION", res.GetErrorCode())
	}
}

func TestDispatch_SendChatMessage_HandlerNotWired(t *testing.T) {
	client := newDispatcherClient(nil, nil, nil)
	out := make(chan *pb.DaemonEvent, 1)
	ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "sm3",
		Cmd:       &pb.OrchestratorCommand_SendChatMessage{SendChatMessage: &pb.SendChatMessageCommand{}},
	}, out)
	if ev == nil || ev.GetResult().GetOk() {
		t.Fatalf("expected synchronous wired-error result, got %+v", ev)
	}
}

type fakeWebhookDispatcher struct {
	calls atomic.Int32
	err   error
}

func (f *fakeWebhookDispatcher) Dispatch(_ context.Context, _ *pb.WebhookEvent) error {
	f.calls.Add(1)
	return f.err
}

type fakeAttacher struct {
	calls     atomic.Int32
	chunks    []*pb.SessionAttachChunk
	attachErr error
}

func (f *fakeAttacher) Attach(_ context.Context, sessionID, commandID string) (<-chan *pb.SessionAttachChunk, error) {
	f.calls.Add(1)
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	ch := make(chan *pb.SessionAttachChunk, len(f.chunks)+1)
	for _, c := range f.chunks {
		ch <- c
	}
	close(ch)
	_ = sessionID
	_ = commandID
	return ch, nil
}

type fakeCreator struct {
	calls     atomic.Int32
	chunks    []*pb.SessionCreateChunk
	createErr error
	lastCmd   *pb.CreateSessionCommand
	lastCmdID string
}

func (f *fakeCreator) Create(_ context.Context, cmd *pb.CreateSessionCommand, commandID string) (<-chan *pb.SessionCreateChunk, error) {
	f.calls.Add(1)
	f.lastCmd = cmd
	f.lastCmdID = commandID
	if f.createErr != nil {
		return nil, f.createErr
	}
	ch := make(chan *pb.SessionCreateChunk, len(f.chunks)+1)
	for _, c := range f.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

// newDispatcherClient wires a StreamClient with just the command-side
// collaborators. Other fields stay nil; the dispatcher functions under
// test never touch them.
func newDispatcherClient(
	handler SessionCommandHandler,
	webhooks WebhookCommandDispatcher,
	attacher SessionAttacher,
) *StreamClient {
	return NewStreamClient(StreamClientConfig{
		CommandHandler: handler,
		Webhooks:       webhooks,
		Attacher:       attacher,
		Logger:         zerolog.Nop(),
	})
}

// newDispatcherClientWithCreator wires a StreamClient with a SessionCreator
// for the CreateSession streaming tests.
func newDispatcherClientWithCreator(creator SessionCreator) *StreamClient {
	return NewStreamClient(StreamClientConfig{
		Creator: creator,
		Logger:  zerolog.Nop(),
	})
}

func TestDispatchCommand_Stop_CallsHandler(t *testing.T) {
	sess := &pb.Session{Id: "s1"}
	handler := &fakeCommandHandler{session: sess}
	client := newDispatcherClient(handler, nil, nil)

	out := make(chan *pb.DaemonEvent, 4)
	cmd := &pb.OrchestratorCommand{
		CommandId: "c-1",
		Cmd:       &pb.OrchestratorCommand_Stop{Stop: &pb.StopSessionCommand{SessionId: "s1"}},
	}
	ev := client.dispatchCommand(context.Background(), cmd, out)

	if handler.stopCalls.Load() != 1 {
		t.Fatalf("stop calls = %d, want 1", handler.stopCalls.Load())
	}
	if r := ev.GetResult(); r == nil || !r.GetOk() || r.GetCommandId() != "c-1" {
		t.Fatalf("unexpected result: %+v", ev)
	}
}

func TestDispatchCommand_Pause_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{session: &pb.Session{Id: "s1"}}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-2",
			Cmd:       &pb.OrchestratorCommand_Pause{Pause: &pb.PauseSessionCommand{SessionId: "s1"}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.pauseCalls.Load() != 1 {
		t.Fatalf("pause calls = %d", handler.pauseCalls.Load())
	}
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok result: %+v", ev)
	}
}

func TestDispatchCommand_Resume_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{session: &pb.Session{Id: "s1"}}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-3",
			Cmd:       &pb.OrchestratorCommand_Resume{Resume: &pb.ResumeSessionCommand{SessionId: "s1"}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.resumeCalls.Load() != 1 {
		t.Fatalf("resume calls = %d", handler.resumeCalls.Load())
	}
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok result: %+v", ev)
	}
}

func TestDispatchCommand_WakeChat_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{
		wakeOutcome:  pb.WakeChatResult_OUTCOME_RESUMED,
		wakeTmuxName: "boss-aaa-bbb",
		wakeReason:   "transcript_missing",
	}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-w1",
			Cmd: &pb.OrchestratorCommand_WakeChat{
				WakeChat: &pb.WakeChatCommand{AgentSessionId: "agent-1"},
			},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.wakeCalls.Load() != 1 {
		t.Fatalf("wake calls = %d, want 1", handler.wakeCalls.Load())
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() {
		t.Fatalf("expected ok result: %+v", ev)
	}
	wake := r.GetWakeChat()
	if wake == nil {
		t.Fatalf("expected WakeChatResult payload, got %+v", r)
	}
	if wake.GetOutcome() != pb.WakeChatResult_OUTCOME_RESUMED {
		t.Fatalf("outcome = %v, want RESUMED", wake.GetOutcome())
	}
	if wake.GetTmuxSessionName() != "boss-aaa-bbb" {
		t.Fatalf("tmux name = %q", wake.GetTmuxSessionName())
	}
	if wake.GetReason() != "transcript_missing" {
		t.Fatalf("reason = %q, want transcript_missing", wake.GetReason())
	}
}

func TestDispatchCommand_WakeChat_NotFoundSetsErrorCode(t *testing.T) {
	handler := &fakeCommandHandler{
		wakeErrorCode: pb.CommandResult_ERROR_CODE_NOT_FOUND,
		wakeErr:       errors.New("agent-missing"),
	}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-w2",
			Cmd: &pb.OrchestratorCommand_WakeChat{
				WakeChat: &pb.WakeChatCommand{AgentSessionId: "missing"},
			},
		}, make(chan *pb.DaemonEvent, 4))
	r := ev.GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected error result, got %+v", ev)
	}
	if r.GetErrorCode() != pb.CommandResult_ERROR_CODE_NOT_FOUND {
		t.Fatalf("error_code = %v, want NOT_FOUND", r.GetErrorCode())
	}
	if r.GetError() != "agent-missing" {
		t.Fatalf("error message = %q, want plain (no prefix)", r.GetError())
	}
}

func TestDispatchCommand_WakeChat_FailedPreconditionSetsErrorCode(t *testing.T) {
	handler := &fakeCommandHandler{
		wakeErrorCode: pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION,
		wakeErr:       errors.New("worktree gone"),
	}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-w3",
			Cmd: &pb.OrchestratorCommand_WakeChat{
				WakeChat: &pb.WakeChatCommand{AgentSessionId: "agent-1"},
			},
		}, make(chan *pb.DaemonEvent, 4))
	r := ev.GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected error result, got %+v", ev)
	}
	if r.GetErrorCode() != pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("error_code = %v, want FAILED_PRECONDITION", r.GetErrorCode())
	}
}

func TestDispatchCommand_Transfer_NotYetImplemented(t *testing.T) {
	// T4.6 lands the coordinated transfer protocol on the bosso side.
	// Daemon-side session-lifecycle participation is a follow-up; when
	// no TransferHandler is wired, the dispatcher ACKs a structured
	// error so bosso's command waiter resolves promptly.
	client := newDispatcherClient(nil, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-4",
			Cmd:       &pb.OrchestratorCommand_Transfer{Transfer: &pb.TransferSessionCommand{SessionId: "s1"}},
		}, make(chan *pb.DaemonEvent, 4))
	if r := ev.GetResult(); r == nil || r.GetOk() || r.GetError() == "" {
		t.Fatalf("expected error result for transfer, got %+v", ev)
	}
}

// --- Coordinated transfer protocol (decision #14, T4.6) ---

// fakeTransferHandler records which protocol hook bosso invoked. Tests
// configure the per-call return values to simulate the source-role
// (nil TransferConfirmed) and target-role (non-nil TransferConfirmed)
// outcomes.
type fakeTransferHandler struct {
	transferCalls  atomic.Int32
	confirmedCalls atomic.Int32
	cancelCalls    atomic.Int32
	transferResult *pb.TransferConfirmed
	transferErr    error
	confirmedErr   error
	cancelErr      error
}

func (f *fakeTransferHandler) Transfer(_ context.Context, _ *pb.TransferSessionCommand) (*pb.TransferConfirmed, error) {
	f.transferCalls.Add(1)
	return f.transferResult, f.transferErr
}
func (f *fakeTransferHandler) Confirmed(_ context.Context, _ *pb.TransferConfirmed) error {
	f.confirmedCalls.Add(1)
	return f.confirmedErr
}
func (f *fakeTransferHandler) Cancel(_ context.Context, _ *pb.TransferCancel) error {
	f.cancelCalls.Add(1)
	return f.cancelErr
}

// newDispatcherClientWithTransfer is a constructor shim the transfer
// tests use. Keeps the original three-arg newDispatcherClient intact for
// the non-transfer cases so they stay diff-minimal.
func newDispatcherClientWithTransfer(transfer TransferHandler) *StreamClient {
	return NewStreamClient(StreamClientConfig{
		TransferHandler: transfer,
		Logger:          zerolog.Nop(),
	})
}

func TestDispatchCommand_Transfer_SourceRole_ReturnsOkNoPayload(t *testing.T) {
	// Source role: handler returns (nil, nil). Dispatcher ACKs Ok:true
	// with no TransferConfirmed — bosso reads this as "source accepted,
	// has emitted the SessionDelta{UPDATED, transferring_to=target}".
	handler := &fakeTransferHandler{}
	client := newDispatcherClientWithTransfer(handler)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-tx1",
			Cmd:       &pb.OrchestratorCommand_Transfer{Transfer: &pb.TransferSessionCommand{SessionId: "s1"}},
		}, make(chan *pb.DaemonEvent, 4))
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-tx1" {
		t.Fatalf("expected ok handshake, got %+v", ev)
	}
	if r.GetTransferConfirmed() != nil {
		t.Errorf("source-role result must not carry TransferConfirmed, got %+v", r.GetTransferConfirmed())
	}
	if handler.transferCalls.Load() != 1 {
		t.Errorf("transfer calls = %d, want 1", handler.transferCalls.Load())
	}
}

func TestDispatchCommand_Transfer_TargetRole_EmbedsConfirmed(t *testing.T) {
	// Target role: handler returns a non-nil TransferConfirmed. The
	// dispatcher MUST embed it in CommandResult.Payload so bosso can
	// proceed to step 4 (forward TransferConfirmed to source).
	handler := &fakeTransferHandler{
		transferResult: &pb.TransferConfirmed{SessionId: "s1", TargetDaemonId: "d-b"},
	}
	client := newDispatcherClientWithTransfer(handler)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-tx2",
			Cmd:       &pb.OrchestratorCommand_Transfer{Transfer: &pb.TransferSessionCommand{SessionId: "s1"}},
		}, make(chan *pb.DaemonEvent, 4))
	r := ev.GetResult()
	if r == nil || !r.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
	}
	tc := r.GetTransferConfirmed()
	if tc == nil || tc.GetSessionId() != "s1" || tc.GetTargetDaemonId() != "d-b" {
		t.Fatalf("target-role result missing TransferConfirmed payload: %+v", r.GetPayload())
	}
}

func TestDispatchCommand_TransferConfirmed_AcksOk(t *testing.T) {
	// Step 4 on source: emit DELETED session delta, ACK Ok:true.
	handler := &fakeTransferHandler{}
	client := newDispatcherClientWithTransfer(handler)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-tx3",
			Cmd: &pb.OrchestratorCommand_TransferConfirmed{
				TransferConfirmed: &pb.TransferConfirmed{SessionId: "s1", TargetDaemonId: "d-b"},
			},
		}, make(chan *pb.DaemonEvent, 4))
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
	}
	if handler.confirmedCalls.Load() != 1 {
		t.Errorf("confirmed calls = %d, want 1", handler.confirmedCalls.Load())
	}
}

func TestDispatchCommand_TransferConfirmed_NoHandler_AcksOk(t *testing.T) {
	// No handler wired: TransferConfirmed is idempotent-no-op semantics.
	// Still ACK Ok so bosso's waiter doesn't trip.
	client := newDispatcherClient(nil, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-tx4",
			Cmd: &pb.OrchestratorCommand_TransferConfirmed{
				TransferConfirmed: &pb.TransferConfirmed{SessionId: "s1"},
			},
		}, make(chan *pb.DaemonEvent, 4))
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok no-op result, got %+v", ev)
	}
}

func TestDispatchCommand_TransferCancel_AcksOk(t *testing.T) {
	handler := &fakeTransferHandler{}
	client := newDispatcherClientWithTransfer(handler)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-tx5",
			Cmd: &pb.OrchestratorCommand_TransferCancel{
				TransferCancel: &pb.TransferCancel{SessionId: "s1", Reason: "target create failed"},
			},
		}, make(chan *pb.DaemonEvent, 4))
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
	}
	if handler.cancelCalls.Load() != 1 {
		t.Errorf("cancel calls = %d, want 1", handler.cancelCalls.Load())
	}
}

func TestDispatchCommand_TransferCancel_NoHandler_AcksOk(t *testing.T) {
	// Like TransferConfirmed — no handler means idempotent no-op, still ACK.
	client := newDispatcherClient(nil, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-tx6",
			Cmd: &pb.OrchestratorCommand_TransferCancel{
				TransferCancel: &pb.TransferCancel{SessionId: "s1"},
			},
		}, make(chan *pb.DaemonEvent, 4))
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok no-op result, got %+v", ev)
	}
}

func TestDispatchCommand_Webhook_EmitsAck(t *testing.T) {
	dispatcher := &fakeWebhookDispatcher{}
	client := newDispatcherClient(nil, dispatcher, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-5",
			Cmd:       &pb.OrchestratorCommand_Webhook{Webhook: &pb.WebhookEvent{Provider: "github"}},
		}, make(chan *pb.DaemonEvent, 4))
	if dispatcher.calls.Load() != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", dispatcher.calls.Load())
	}
	ack := ev.GetAck()
	if ack == nil || !ack.GetOk() || ack.GetCommandId() != "c-5" {
		t.Fatalf("expected webhook ack ok=true, got %+v", ev)
	}
}

func TestDispatchCommand_Webhook_FailureAckWithError(t *testing.T) {
	dispatcher := &fakeWebhookDispatcher{err: errors.New("route not found")}
	client := newDispatcherClient(nil, dispatcher, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-5b",
			Cmd:       &pb.OrchestratorCommand_Webhook{Webhook: &pb.WebhookEvent{}},
		}, make(chan *pb.DaemonEvent, 4))
	ack := ev.GetAck()
	if ack == nil || ack.GetOk() || ack.GetError() == "" {
		t.Fatalf("expected webhook ack ok=false with error, got %+v", ev)
	}
}

func TestDispatchCommand_UnknownOneof_LogsAndSkips(t *testing.T) {
	// Nil oneof is the only portable "unknown" we can construct here —
	// a zero-initialized OrchestratorCommand has no Cmd set and so
	// exercises the default branch of dispatchCommand.
	client := newDispatcherClient(nil, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{CommandId: "c-u"},
		make(chan *pb.DaemonEvent, 4))
	if ev != nil {
		t.Fatalf("expected nil DaemonEvent for unknown command, got %+v", ev)
	}
}

func TestDispatchCommand_AttachSession_StreamsChunksUntilClose(t *testing.T) {
	// The attacher fires two chunks and then closes its channel. The
	// dispatcher returns an immediate ok CommandResult (handshake)
	// and a background goroutine pumps the chunks onto outbound.
	chunks := []*pb.SessionAttachChunk{
		{SessionId: "s1", CommandId: "c-att", Event: &pb.SessionAttachChunk_OutputLine{OutputLine: &pb.OutputLine{Text: "hello"}}},
		{SessionId: "s1", CommandId: "c-att", Event: &pb.SessionAttachChunk_SessionEnded{SessionEnded: &pb.SessionEnded{}}},
	}
	attacher := &fakeAttacher{chunks: chunks}
	client := newDispatcherClient(nil, nil, attacher)

	out := make(chan *pb.DaemonEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ev := client.dispatchCommand(ctx,
		&pb.OrchestratorCommand{
			CommandId: "c-att",
			Cmd:       &pb.OrchestratorCommand_Attach{Attach: &pb.AttachSessionCommand{SessionId: "s1"}},
		}, out)

	// Handshake result must be ok, no session payload.
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-att" {
		t.Fatalf("expected ok handshake result, got %+v", ev)
	}

	// Collect chunks arriving asynchronously.
	got := 0
	deadline := time.After(500 * time.Millisecond)
	for got < len(chunks) {
		select {
		case ev := <-out:
			if c := ev.GetAttachChunk(); c != nil {
				got++
			}
		case <-deadline:
			t.Fatalf("expected %d chunks, got %d", len(chunks), got)
		}
	}
	if attacher.calls.Load() != 1 {
		t.Fatalf("attacher calls = %d, want 1", attacher.calls.Load())
	}
}

func createSetupChunk(cmdID, text string) *pb.SessionCreateChunk {
	return &pb.SessionCreateChunk{
		CommandId: cmdID,
		Body:      &pb.SessionCreateChunk_SetupOutput{SetupOutput: text},
	}
}

func TestDispatchCommand_CreateSession_StreamsThenCreated(t *testing.T) {
	const cmdID = "c-create"
	chunks := []*pb.SessionCreateChunk{
		createSetupChunk(cmdID, "cloning\n"),
		createSetupChunk(cmdID, "setup.sh\n"),
		{CommandId: cmdID, Body: &pb.SessionCreateChunk_Created{Created: &pb.Session{Id: "s9"}}},
	}
	creator := &fakeCreator{chunks: chunks}
	client := newDispatcherClientWithCreator(creator)

	out := make(chan *pb.DaemonEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ev := client.dispatchCommand(ctx,
		&pb.OrchestratorCommand{
			CommandId: cmdID,
			Cmd: &pb.OrchestratorCommand_CreateSession{CreateSession: &pb.CreateSessionCommand{
				RepoId: "r1",
				Title:  "x",
			}},
		}, out)

	// Immediate ok handshake ack.
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != cmdID {
		t.Fatalf("expected ok handshake result, got %+v", ev)
	}
	if creator.lastCmdID != cmdID {
		t.Fatalf("creator command id = %q, want %q", creator.lastCmdID, cmdID)
	}

	// Drain the streamed chunks in order.
	got := make([]*pb.SessionCreateChunk, 0, len(chunks))
	deadline := time.After(500 * time.Millisecond)
	for len(got) < len(chunks) {
		select {
		case ev := <-out:
			if c := ev.GetCreateChunk(); c != nil {
				got = append(got, c)
			}
		case <-deadline:
			t.Fatalf("expected %d chunks, got %d", len(chunks), len(got))
		}
	}

	if got[0].GetSetupOutput() != "cloning\n" {
		t.Fatalf("chunk[0] setup_output = %q", got[0].GetSetupOutput())
	}
	if got[1].GetSetupOutput() != "setup.sh\n" {
		t.Fatalf("chunk[1] setup_output = %q", got[1].GetSetupOutput())
	}
	if got[len(got)-1].GetCreated().GetId() != "s9" {
		t.Fatalf("last chunk created id = %q, want s9", got[len(got)-1].GetCreated().GetId())
	}
	for i, c := range got {
		if c.GetCommandId() != cmdID {
			t.Fatalf("chunk[%d] command_id = %q, want %q", i, c.GetCommandId(), cmdID)
		}
	}
}

func TestDispatchCommand_CreateSession_ErrorChunk(t *testing.T) {
	const cmdID = "c-create-err"
	chunks := []*pb.SessionCreateChunk{
		createSetupChunk(cmdID, "cloning\n"),
		{CommandId: cmdID, Body: &pb.SessionCreateChunk_Error{Error: &pb.CreateError{Message: "boom"}}},
	}
	creator := &fakeCreator{chunks: chunks}
	client := newDispatcherClientWithCreator(creator)

	out := make(chan *pb.DaemonEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ev := client.dispatchCommand(ctx,
		&pb.OrchestratorCommand{
			CommandId: cmdID,
			Cmd:       &pb.OrchestratorCommand_CreateSession{CreateSession: &pb.CreateSessionCommand{RepoId: "r1", Title: "x"}},
		}, out)
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok handshake result, got %+v", ev)
	}

	got := make([]*pb.SessionCreateChunk, 0, len(chunks))
	deadline := time.After(500 * time.Millisecond)
	for len(got) < len(chunks) {
		select {
		case ev := <-out:
			if c := ev.GetCreateChunk(); c != nil {
				got = append(got, c)
			}
		case <-deadline:
			t.Fatalf("expected %d chunks, got %d", len(chunks), len(got))
		}
	}

	last := got[len(got)-1]
	if last.GetError().GetMessage() != "boom" {
		t.Fatalf("terminal chunk error = %q, want boom", last.GetError().GetMessage())
	}
	if last.GetCreated() != nil {
		t.Fatalf("expected no created on error path, got %+v", last.GetCreated())
	}
}

func TestDispatchCommand_CreateSession_CreatorNotWired(t *testing.T) {
	client := newDispatcherClientWithCreator(nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-nw",
			Cmd:       &pb.OrchestratorCommand_CreateSession{CreateSession: &pb.CreateSessionCommand{RepoId: "r1", Title: "x"}},
		}, make(chan *pb.DaemonEvent, 4))
	r := ev.GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected error result with Ok=false, got %+v", ev)
	}
	if r.GetCommandId() != "c-nw" {
		t.Fatalf("command id = %q, want c-nw", r.GetCommandId())
	}
}

func TestDispatchCommand_Merge_CallsHandler(t *testing.T) {
	sess := &pb.Session{Id: "s-merge"}
	handler := &fakeCommandHandler{mergeSession: sess}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-m1",
			Cmd:       &pb.OrchestratorCommand_Merge{Merge: &pb.MergeSessionCommand{SessionId: "s-merge"}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.mergeCalls.Load() != 1 {
		t.Fatalf("merge calls = %d, want 1", handler.mergeCalls.Load())
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-m1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	if r.GetSession().GetId() != "s-merge" {
		t.Fatalf("expected session id s-merge, got %q", r.GetSession().GetId())
	}
}

func TestDispatchCommand_Merge_MapsConnectCodeToErrorCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want pb.CommandResult_ErrorCode
	}{
		{"failed_precondition", connect.NewError(connect.CodeFailedPrecondition, errors.New("PR is not passing")), pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION},
		{"not_found", connect.NewError(connect.CodeNotFound, errors.New("session not found")), pb.CommandResult_ERROR_CODE_NOT_FOUND},
		{"plain_error", errors.New("boom"), pb.CommandResult_ERROR_CODE_UNSPECIFIED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Adapters wrap the connect error with %w; emulate that so the
			// dispatcher's connect.CodeOf still recovers the code.
			handler := &fakeCommandHandler{returnErr: fmt.Errorf("merge session: %w", tc.err)}
			client := newDispatcherClient(handler, nil, nil)
			ev := client.dispatchCommand(context.Background(),
				&pb.OrchestratorCommand{
					CommandId: "c-merr",
					Cmd:       &pb.OrchestratorCommand_Merge{Merge: &pb.MergeSessionCommand{SessionId: "s1"}},
				}, make(chan *pb.DaemonEvent, 4))
			r := ev.GetResult()
			if r == nil || r.GetOk() {
				t.Fatalf("expected failed result, got %+v", ev)
			}
			if r.GetErrorCode() != tc.want {
				t.Fatalf("error_code = %v, want %v", r.GetErrorCode(), tc.want)
			}
		})
	}
}

func TestDispatchCommand_Archive_CallsHandler(t *testing.T) {
	sess := &pb.Session{Id: "s-arch"}
	handler := &fakeCommandHandler{archiveSession: sess}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-a1",
			Cmd:       &pb.OrchestratorCommand_Archive{Archive: &pb.ArchiveSessionCommand{SessionId: "s-arch"}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.archiveCalls.Load() != 1 {
		t.Fatalf("archive calls = %d, want 1", handler.archiveCalls.Load())
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-a1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	if r.GetSession().GetId() != "s-arch" {
		t.Fatalf("expected session id s-arch, got %q", r.GetSession().GetId())
	}
}

func TestDispatchCommand_RecordChat_CallsHandler(t *testing.T) {
	chat := &pb.ClaudeChat{Id: "chat-1"}
	handler := &fakeCommandHandler{recordChatResult: chat}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-rc1",
			Cmd: &pb.OrchestratorCommand_RecordChat{RecordChat: &pb.RecordChatCommand{
				SessionId:      "s1",
				AgentSessionId: "agent-1",
				Title:          "my chat",
				Resume:         true,
				AgentName:      "claude",
			}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.recordChatCalls.Load() != 1 {
		t.Fatalf("record_chat calls = %d, want 1", handler.recordChatCalls.Load())
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-rc1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	if r.GetRecordChat().GetId() != "chat-1" {
		t.Fatalf("expected record_chat.id chat-1, got %q", r.GetRecordChat().GetId())
	}
}

func TestDispatchCommand_DeleteChat_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-dc1",
			Cmd: &pb.OrchestratorCommand_DeleteChat{DeleteChat: &pb.DeleteChatCommand{
				AgentSessionId: "agent-1",
				SessionId:      "s1",
			}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.deleteChatCalls.Load() != 1 {
		t.Fatalf("delete_chat calls = %d, want 1", handler.deleteChatCalls.Load())
	}
	// The session_id must reach the handler so the daemon can enforce that the
	// chat belongs to the authorized session (scoping, not advisory).
	if handler.deleteChatScope != "s1" {
		t.Fatalf("delete_chat session scope = %q, want s1", handler.deleteChatScope)
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-dc1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	if r.GetSession() != nil {
		t.Fatalf("expected no session payload for delete_chat, got %+v", r.GetSession())
	}
	if r.GetRecordChat() != nil {
		t.Fatalf("expected no record_chat payload for delete_chat, got %+v", r.GetRecordChat())
	}
}

func TestDispatchCommand_ListRepos_CallsHandler(t *testing.T) {
	repos := &pb.ListReposResponse{Repos: []*pb.Repo{{Id: "r1", OriginUrl: "git@github.com:acme/app.git"}}}
	handler := &fakeCommandHandler{listReposResult: repos}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-lr1",
			Cmd:       &pb.OrchestratorCommand_ListRepos{ListRepos: &pb.ListReposCommand{}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async list command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	if handler.listReposCalls.Load() != 1 {
		t.Fatalf("list_repos calls = %d, want 1", handler.listReposCalls.Load())
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-lr1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	got := r.GetListRepos().GetRepos()
	if len(got) != 1 || got[0].GetId() != "r1" {
		t.Fatalf("expected list_repos payload with r1, got %+v", got)
	}
}

func TestDispatchCommand_ListRepos_HandlerError_ReturnsCommandErr(t *testing.T) {
	handler := &fakeCommandHandler{returnErr: errors.New("list repos boom")}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-lr-err",
			Cmd:       &pb.OrchestratorCommand_ListRepos{ListRepos: &pb.ListReposCommand{}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async list command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	r := ev.GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected error result, got %+v", ev)
	}
	if r.GetError() != "list repos boom" {
		t.Fatalf("expected error %q, got %q", "list repos boom", r.GetError())
	}
}

func TestDispatchCommand_ListAgents_CallsHandler(t *testing.T) {
	agents := &pb.ListAgentsResponse{Agents: []*pb.AgentInfo{{Name: "claude", Version: "1.2.3"}}}
	handler := &fakeCommandHandler{listAgentsResult: agents}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-la1",
			Cmd:       &pb.OrchestratorCommand_ListAgents{ListAgents: &pb.ListAgentsCommand{}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async list command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	if handler.listAgentsCalls.Load() != 1 {
		t.Fatalf("list_agents calls = %d, want 1", handler.listAgentsCalls.Load())
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-la1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	got := r.GetListAgents().GetAgents()
	if len(got) != 1 || got[0].GetName() != "claude" {
		t.Fatalf("expected list_agents payload with claude, got %+v", got)
	}
}

func TestDispatchCommand_ListAgents_HandlerError_ReturnsCommandErr(t *testing.T) {
	handler := &fakeCommandHandler{returnErr: errors.New("list agents boom")}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-la-err",
			Cmd:       &pb.OrchestratorCommand_ListAgents{ListAgents: &pb.ListAgentsCommand{}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async list command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	r := ev.GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected error result, got %+v", ev)
	}
	if r.GetError() != "list agents boom" {
		t.Fatalf("expected error %q, got %q", "list agents boom", r.GetError())
	}
}

func TestDispatchCommand_Merge_HandlerError_ReturnsCommandErr(t *testing.T) {
	handler := &fakeCommandHandler{returnErr: errors.New("merge failed: conflict")}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-m-err",
			Cmd:       &pb.OrchestratorCommand_Merge{Merge: &pb.MergeSessionCommand{SessionId: "s1"}},
		}, make(chan *pb.DaemonEvent, 4))
	r := ev.GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected error result, got %+v", ev)
	}
	if r.GetError() != "merge failed: conflict" {
		t.Fatalf("expected error message %q, got %q", "merge failed: conflict", r.GetError())
	}
}
