package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

// newDescribeTestServer wires a Server with the wakeHook seam so the handler
// resolves argv/resume through injected fakes instead of live plugins.
func newDescribeTestServer(chat *models.AgentChat, sess *models.Session, transcripts transcriptOracle, argv argvBuilder) *Server {
	return &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			transcripts: transcripts,
			argv:        argv,
		},
	}
}

func TestDescribeChatLaunch_ReturnsFreshCommandAndWorktree(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", WorktreePath: "/work/tree"}
	srv := newDescribeTestServer(chat, sess, &fakeTranscriptOracle{exists: false}, claudeArgvBuilder())

	resp, err := srv.DescribeChatLaunch(context.Background(), connect.NewRequest(&pb.DescribeChatLaunchRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("DescribeChatLaunch: %v", err)
	}
	// No transcript → fresh argv (--session-id), echoing the agent session id.
	wantArgv := []string{"claude", "--session-id", "agent-1"}
	got := resp.Msg.GetArgv()
	if len(got) != len(wantArgv) {
		t.Fatalf("argv = %#v, want %#v", got, wantArgv)
	}
	for i := range wantArgv {
		if got[i] != wantArgv[i] {
			t.Fatalf("argv = %#v, want %#v", got, wantArgv)
		}
	}
	if resp.Msg.GetWorktreePath() != "/work/tree" {
		t.Fatalf("worktree = %q, want %q", resp.Msg.GetWorktreePath(), "/work/tree")
	}
	if resp.Msg.GetAgentName() != "claude" {
		t.Fatalf("agent_name = %q, want %q", resp.Msg.GetAgentName(), "claude")
	}
	// Host is best-effort from os.Hostname; just require it is populated in CI.
	if resp.Msg.GetHost() == "" {
		t.Fatalf("expected a non-empty host")
	}
}

func TestDescribeChatLaunch_UsesResumeArgvWhenTranscriptExists(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-2", AgentName: "claude"}
	sess := &models.Session{ID: "s1", WorktreePath: "/work/tree"}
	srv := newDescribeTestServer(chat, sess, &fakeTranscriptOracle{exists: true}, claudeArgvBuilder())

	resp, err := srv.DescribeChatLaunch(context.Background(), connect.NewRequest(&pb.DescribeChatLaunchRequest{
		AgentSessionId: "agent-2",
	}))
	if err != nil {
		t.Fatalf("DescribeChatLaunch: %v", err)
	}
	if len(resp.Msg.GetArgv()) == 0 || resp.Msg.GetArgv()[1] != "--resume" {
		t.Fatalf("argv = %#v, want a --resume command", resp.Msg.GetArgv())
	}
}

func TestDescribeChatLaunch_ReturnsCapturedOutputWhenPresent(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-3", AgentName: "claude"}
	sess := &models.Session{ID: "s1", WorktreePath: "/work/tree"}
	srv := newDescribeTestServer(chat, sess, &fakeTranscriptOracle{exists: false}, claudeArgvBuilder())

	tracker := status.NewTracker()
	tracker.SetCapturedOutput("agent-3", "Error: Session ID agent-3 is already in use")
	srv.chatStatus = tracker

	resp, err := srv.DescribeChatLaunch(context.Background(), connect.NewRequest(&pb.DescribeChatLaunchRequest{
		AgentSessionId: "agent-3",
	}))
	if err != nil {
		t.Fatalf("DescribeChatLaunch: %v", err)
	}
	if got := resp.Msg.GetCapturedOutput(); got != "Error: Session ID agent-3 is already in use" {
		t.Fatalf("captured_output = %q, want the stored tail", got)
	}
}

func TestDescribeChatLaunch_CapturedOutputEmptyWhenNoneStored(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-4", AgentName: "claude"}
	sess := &models.Session{ID: "s1", WorktreePath: "/work/tree"}
	srv := newDescribeTestServer(chat, sess, &fakeTranscriptOracle{exists: false}, claudeArgvBuilder())
	srv.chatStatus = status.NewTracker()

	resp, err := srv.DescribeChatLaunch(context.Background(), connect.NewRequest(&pb.DescribeChatLaunchRequest{
		AgentSessionId: "agent-4",
	}))
	if err != nil {
		t.Fatalf("DescribeChatLaunch: %v", err)
	}
	if got := resp.Msg.GetCapturedOutput(); got != "" {
		t.Fatalf("captured_output = %q, want empty when none stored", got)
	}
}

func TestDescribeChatLaunch_EmptyAgentSessionIDIsInvalidArgument(t *testing.T) {
	srv := newDescribeTestServer(nil, nil, &fakeTranscriptOracle{}, claudeArgvBuilder())
	_, err := srv.DescribeChatLaunch(context.Background(), connect.NewRequest(&pb.DescribeChatLaunchRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("err code = %v, want InvalidArgument (%v)", connect.CodeOf(err), err)
	}
}

func TestDescribeChatLaunch_UnknownChatIsNotFound(t *testing.T) {
	srv := newDescribeTestServer(nil, nil, &fakeTranscriptOracle{}, claudeArgvBuilder())
	_, err := srv.DescribeChatLaunch(context.Background(), connect.NewRequest(&pb.DescribeChatLaunchRequest{
		AgentSessionId: "missing",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("err code = %v, want NotFound (%v)", connect.CodeOf(err), err)
	}
}

// DescribeChatLaunch is a read-only preview of the launch command, not a spawn.
// It deliberately requests an empty append_system_prompt, so bossd builds no
// instruction classes on this path and therefore has no drop to report —
// whatever the runner declares. Pin that: if this preview ever starts asking
// for the suffix, it must also start reporting on it.
func TestDescribeChatLaunch_RequestsNoInstructionsSoNothingIsReported(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", SessionID: "s1", AgentSessionID: "agent-1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", WorktreePath: "/work/tree"}
	builder := claudeArgvBuilder()
	// A runner that carries nothing into argv — the loudest declaration there is.
	builder.support = pb.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE
	srv := newDescribeTestServer(chat, sess, &fakeTranscriptOracle{exists: false}, builder)

	if _, err := srv.DescribeChatLaunch(context.Background(), connect.NewRequest(&pb.DescribeChatLaunchRequest{
		AgentSessionId: "agent-1",
	})); err != nil {
		t.Fatalf("DescribeChatLaunch: %v", err)
	}

	calls := builder.calls
	if len(calls) != 1 {
		t.Fatalf("expected exactly one argv build, got %d", len(calls))
	}
	if calls[0].appendSystemPrompt != "" {
		t.Fatalf("preview must request no instruction suffix, got %q", calls[0].appendSystemPrompt)
	}
}
