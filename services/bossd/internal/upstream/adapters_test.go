package upstream

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// fakeSessionCommandServer captures the last RecordChatRequest and returns a
// stub response. The other three methods return zero-value responses; they are
// unused by these tests but required to satisfy SessionCommandServer.
type fakeSessionCommandServer struct {
	lastRecordChat   *pb.RecordChatRequest
	lastTranscriptID string
	lastSendReq      *pb.SendChatMessageRequest
	lastSwitchReq    *pb.SwitchSessionAccountRequest
	switchResp       *pb.SwitchSessionAccountResponse
}

func (f *fakeSessionCommandServer) MergeSession(_ context.Context, _ *connect.Request[pb.MergeSessionRequest]) (*connect.Response[pb.MergeSessionResponse], error) {
	return connect.NewResponse(&pb.MergeSessionResponse{}), nil
}

func (f *fakeSessionCommandServer) SwitchSessionAccount(_ context.Context, req *connect.Request[pb.SwitchSessionAccountRequest]) (*connect.Response[pb.SwitchSessionAccountResponse], error) {
	f.lastSwitchReq = req.Msg
	resp := f.switchResp
	if resp == nil {
		resp = &pb.SwitchSessionAccountResponse{}
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeSessionCommandServer) ArchiveSession(_ context.Context, _ *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error) {
	return connect.NewResponse(&pb.ArchiveSessionResponse{}), nil
}

func (f *fakeSessionCommandServer) RecordChat(_ context.Context, req *connect.Request[pb.RecordChatRequest]) (*connect.Response[pb.RecordChatResponse], error) {
	f.lastRecordChat = req.Msg
	return connect.NewResponse(&pb.RecordChatResponse{
		Chat: &pb.ClaudeChat{Id: "chat1"},
	}), nil
}

func (f *fakeSessionCommandServer) DeleteChat(_ context.Context, _ *connect.Request[pb.DeleteChatRequest]) (*connect.Response[pb.DeleteChatResponse], error) {
	return connect.NewResponse(&pb.DeleteChatResponse{}), nil
}

func (f *fakeSessionCommandServer) ListRepos(_ context.Context, _ *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	return connect.NewResponse(&pb.ListReposResponse{}), nil
}

func (f *fakeSessionCommandServer) ListAgents(_ context.Context, _ *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error) {
	return connect.NewResponse(&pb.ListAgentsResponse{}), nil
}

func (f *fakeSessionCommandServer) ListAccounts(_ context.Context, _ *connect.Request[pb.ListAccountsRequest]) (*connect.Response[pb.ListAccountsResponse], error) {
	return connect.NewResponse(&pb.ListAccountsResponse{}), nil
}

func (f *fakeSessionCommandServer) ListRepoPRs(_ context.Context, _ *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error) {
	return connect.NewResponse(&pb.ListRepoPRsResponse{}), nil
}

func (f *fakeSessionCommandServer) ListTrackerIssues(_ context.Context, _ *connect.Request[pb.ListTrackerIssuesRequest]) (*connect.Response[pb.ListTrackerIssuesResponse], error) {
	return connect.NewResponse(&pb.ListTrackerIssuesResponse{}), nil
}

func (f *fakeSessionCommandServer) GetChatTranscript(_ context.Context, req *connect.Request[pb.GetChatTranscriptRequest]) (*connect.Response[pb.GetChatTranscriptResponse], error) {
	f.lastTranscriptID = req.Msg.GetAgentSessionId()
	return connect.NewResponse(&pb.GetChatTranscriptResponse{
		Messages:           []*pb.ChatMessage{{Text: "x"}},
		FinalAssistantText: "final",
		Exists:             true,
	}), nil
}

func (f *fakeSessionCommandServer) SendChatMessage(_ context.Context, req *connect.Request[pb.SendChatMessageRequest]) (*connect.Response[pb.SendChatMessageResponse], error) {
	f.lastSendReq = req.Msg
	return connect.NewResponse(&pb.SendChatMessageResponse{TmuxSessionName: "tmux-send", Delivered: true}), nil
}

// fakeAutomationToggler records the last SetAutomationEnabled call and returns
// the configured error, driving the pause/resume adapter paths.
type fakeAutomationToggler struct {
	gotEnabled bool
	gotID      string
	err        error
}

func (f *fakeAutomationToggler) SetAutomationEnabled(_ context.Context, id string, enabled bool) error {
	f.gotID = id
	f.gotEnabled = enabled
	return f.err
}

// fakeCmdSessionReader returns a fixed session/error from GetSession,
// exercising the post-action reload branch of the pause/resume adapters.
type fakeCmdSessionReader struct {
	sess *pb.Session
	err  error
}

func (f *fakeCmdSessionReader) GetSession(_ context.Context, _ string) (*pb.Session, error) {
	return f.sess, f.err
}

// fakeChatWaker returns a scripted WakeChatStream result for the WakeChat
// delegation path.
type fakeChatWaker struct {
	outcome pb.WakeChatResult_Outcome
	tmux    string
	reason  string
	code    pb.CommandResult_ErrorCode
	err     error
}

func (f *fakeChatWaker) WakeChatStream(_ context.Context, _ string, _ bool) (pb.WakeChatResult_Outcome, string, string, pb.CommandResult_ErrorCode, error) {
	return f.outcome, f.tmux, f.reason, f.code, f.err
}

// errCommandServer returns err from every SessionCommandServer method so the
// adapter's error-propagation branches can be exercised.
type errCommandServer struct{ err error }

func (e *errCommandServer) MergeSession(context.Context, *connect.Request[pb.MergeSessionRequest]) (*connect.Response[pb.MergeSessionResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) SwitchSessionAccount(context.Context, *connect.Request[pb.SwitchSessionAccountRequest]) (*connect.Response[pb.SwitchSessionAccountResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ArchiveSession(context.Context, *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) RecordChat(context.Context, *connect.Request[pb.RecordChatRequest]) (*connect.Response[pb.RecordChatResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) DeleteChat(context.Context, *connect.Request[pb.DeleteChatRequest]) (*connect.Response[pb.DeleteChatResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ListRepos(context.Context, *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ListAgents(context.Context, *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ListAccounts(context.Context, *connect.Request[pb.ListAccountsRequest]) (*connect.Response[pb.ListAccountsResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ListRepoPRs(context.Context, *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ListTrackerIssues(context.Context, *connect.Request[pb.ListTrackerIssuesRequest]) (*connect.Response[pb.ListTrackerIssuesResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) GetChatTranscript(context.Context, *connect.Request[pb.GetChatTranscriptRequest]) (*connect.Response[pb.GetChatTranscriptResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) SendChatMessage(context.Context, *connect.Request[pb.SendChatMessageRequest]) (*connect.Response[pb.SendChatMessageResponse], error) {
	return nil, e.err
}

// fakeStreamCreateSessioner drives SessionCreatorAdapter.Create. emit is
// called with a scripted sequence of responses, then returnErr is returned.
type fakeStreamCreateSessioner struct {
	responses []*pb.CreateSessionResponse
	returnErr error
	lastReq   *pb.CreateSessionRequest
}

func (f *fakeStreamCreateSessioner) StreamCreateSession(_ context.Context, msg *pb.CreateSessionRequest, emit func(*pb.CreateSessionResponse) error) error {
	f.lastReq = msg
	for _, r := range f.responses {
		if err := emit(r); err != nil {
			return err
		}
	}
	return f.returnErr
}

func drainCreateChunks(t *testing.T, ch <-chan *pb.SessionCreateChunk) []*pb.SessionCreateChunk {
	t.Helper()
	var got []*pb.SessionCreateChunk
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, c)
		case <-deadline:
			t.Fatalf("channel did not close in time; got %d chunks", len(got))
		}
	}
}

func TestSessionCreatorAdapter_Create_TranslatesAndCloses(t *testing.T) {
	fake := &fakeStreamCreateSessioner{
		responses: []*pb.CreateSessionResponse{
			{Event: &pb.CreateSessionResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: "cloning\n"}}},
			{Event: &pb.CreateSessionResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: "setup.sh\n"}}},
			{Event: &pb.CreateSessionResponse_SessionCreated{SessionCreated: &pb.SessionCreated{Session: &pb.Session{Id: "s9"}}}},
		},
	}
	adapter := &SessionCreatorAdapter{Server: fake, Logger: zerolog.Nop()}

	ch, err := adapter.Create(context.Background(), &pb.CreateSessionCommand{
		RepoId:     "r1",
		Title:      "x",
		Plan:       "p",
		BaseBranch: "main",
		QuickChat:  true,
		AgentName:  "claude",
	}, "cmd-1")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	got := drainCreateChunks(t, ch)
	if len(got) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(got))
	}
	if got[0].GetSetupOutput() != "cloning\n" || got[1].GetSetupOutput() != "setup.sh\n" {
		t.Fatalf("setup outputs wrong: %q %q", got[0].GetSetupOutput(), got[1].GetSetupOutput())
	}
	if got[2].GetCreated().GetId() != "s9" {
		t.Fatalf("created id = %q, want s9", got[2].GetCreated().GetId())
	}
	for i, c := range got {
		if c.GetCommandId() != "cmd-1" {
			t.Fatalf("chunk[%d] command_id = %q", i, c.GetCommandId())
		}
	}

	// Request mapping: AgentName is set only when non-empty.
	if fake.lastReq.GetRepoId() != "r1" || fake.lastReq.GetTitle() != "x" || fake.lastReq.GetPlan() != "p" {
		t.Fatalf("request fields not mapped: %+v", fake.lastReq)
	}
	if fake.lastReq.GetBaseBranch() != "main" || !fake.lastReq.GetQuickChat() {
		t.Fatalf("base_branch/quick_chat not mapped: %+v", fake.lastReq)
	}
	if fake.lastReq.AgentName == nil || *fake.lastReq.AgentName != "claude" {
		t.Fatalf("agent_name not mapped: %+v", fake.lastReq.AgentName)
	}
}

func TestSessionCreatorAdapter_Create_EmptyAgentNameStaysNil(t *testing.T) {
	fake := &fakeStreamCreateSessioner{}
	adapter := &SessionCreatorAdapter{Server: fake, Logger: zerolog.Nop()}
	ch, err := adapter.Create(context.Background(), &pb.CreateSessionCommand{RepoId: "r1", Title: "x"}, "cmd-2")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	drainCreateChunks(t, ch)
	if fake.lastReq.AgentName != nil {
		t.Fatalf("AgentName should be nil for empty agent_name, got %v", fake.lastReq.AgentName)
	}
}

func TestSessionCreatorAdapter_Create_TerminalErrorChunk(t *testing.T) {
	fake := &fakeStreamCreateSessioner{
		responses: []*pb.CreateSessionResponse{
			{Event: &pb.CreateSessionResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: "cloning\n"}}},
		},
		returnErr: errors.New("boom"),
	}
	adapter := &SessionCreatorAdapter{Server: fake, Logger: zerolog.Nop()}
	ch, err := adapter.Create(context.Background(), &pb.CreateSessionCommand{RepoId: "r1", Title: "x"}, "cmd-3")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	got := drainCreateChunks(t, ch)
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks (setup + error), got %d", len(got))
	}
	if got[0].GetSetupOutput() != "cloning\n" {
		t.Fatalf("first chunk should be setup output, got %+v", got[0])
	}
	last := got[1]
	if last.GetError().GetMessage() != "boom" {
		t.Fatalf("terminal chunk error = %q, want boom", last.GetError().GetMessage())
	}
	if last.GetCommandId() != "cmd-3" {
		t.Fatalf("terminal chunk command_id = %q", last.GetCommandId())
	}
}

func TestSessionCreatorAdapter_Create_NewFieldsRoundTrip(t *testing.T) {
	// Assert that the new PR/tracker fields on CreateSessionCommand survive
	// the Command→Request mapping in SessionCreatorAdapter.Create.
	pr := int32(42)
	branch := "feat/my-branch"
	trackerID := "FRE-1"
	trackerURL := "https://linear.app/FRE-1"
	issueTitle := "Do the thing"
	source := "linear"
	model := "claude-opus-4-8"
	accountID := "acct-hosted"

	fake := &fakeStreamCreateSessioner{}
	adapter := &SessionCreatorAdapter{Server: fake, Logger: zerolog.Nop()}

	ch, err := adapter.Create(context.Background(), &pb.CreateSessionCommand{
		RepoId:         "r1",
		Title:          "x",
		PrNumber:       &pr,
		BranchName:     &branch,
		TrackerId:      &trackerID,
		TrackerUrl:     &trackerURL,
		TrackerIssue:   &pb.TrackerIssue{Title: issueTitle},
		TrackerSource:  &source,
		AccountId:      &accountID,
		Force:          true,
		ForceBranch:    true,
		Detach:         true,
		Model:          &model,
		TmuxUnattended: true,
	}, "cmd-rt")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	drainCreateChunks(t, ch)

	req := fake.lastReq
	if req == nil {
		t.Fatal("no CreateSessionRequest captured by the fake")
	}
	if req.PrNumber == nil || *req.PrNumber != pr {
		t.Errorf("PrNumber: got %v, want %d", req.PrNumber, pr)
	}
	if req.BranchName == nil || *req.BranchName != branch {
		t.Errorf("BranchName: got %v, want %q", req.BranchName, branch)
	}
	if req.TrackerId == nil || *req.TrackerId != trackerID {
		t.Errorf("TrackerId: got %v, want %q", req.TrackerId, trackerID)
	}
	if req.TrackerUrl == nil || *req.TrackerUrl != trackerURL {
		t.Errorf("TrackerUrl: got %v, want %q", req.TrackerUrl, trackerURL)
	}
	if req.TrackerIssue == nil || req.TrackerIssue.GetTitle() != issueTitle {
		t.Errorf("TrackerIssue: got %v, want title %q", req.TrackerIssue, issueTitle)
	}
	if req.TrackerSource == nil || *req.TrackerSource != source {
		t.Errorf("TrackerSource: got %v, want %q", req.TrackerSource, source)
	}
	if req.AccountId == nil || *req.AccountId != accountID {
		t.Errorf("AccountId: got %v, want %q", req.AccountId, accountID)
	}
	if !req.GetForce() {
		t.Errorf("Force: got false, want true")
	}
	if !req.GetForceBranch() {
		t.Errorf("ForceBranch: got false, want true")
	}
	// Unattended-session fields must survive the reverse-stream Command→Request
	// mapping so a hosted create runs headless/unattended on the requested model
	// rather than starting interactive on the default.
	if !req.GetDetach() {
		t.Errorf("Detach: got false, want true")
	}
	if !req.GetTmuxUnattended() {
		t.Errorf("TmuxUnattended: got false, want true")
	}
	if req.GetModel() != model {
		t.Errorf("Model: got %q, want %q", req.GetModel(), model)
	}
}

func TestSessionCreatorAdapter_Create_PreservesPresentEmptyAccountID(t *testing.T) {
	fake := &fakeStreamCreateSessioner{}
	adapter := &SessionCreatorAdapter{Server: fake, Logger: zerolog.Nop()}
	accountID := ""

	ch, err := adapter.Create(context.Background(), &pb.CreateSessionCommand{
		RepoId:    "r1",
		Title:     "x",
		AccountId: &accountID,
	}, "cmd-account-empty")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	drainCreateChunks(t, ch)

	if fake.lastReq.AccountId == nil {
		t.Fatal("AccountId nil, want present-empty")
	}
	if *fake.lastReq.AccountId != "" {
		t.Fatalf("AccountId = %q, want present-empty", *fake.lastReq.AccountId)
	}
}

func TestCommandHandlerAdapter_RecordChat_AgentNameMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		agentName       string
		wantAgentNilNil bool // true → req.AgentName must be nil
		wantAgentValue  string
	}{
		{
			name:            "empty agentName leaves AgentName nil",
			agentName:       "",
			wantAgentNilNil: true,
		},
		{
			name:           "non-empty agentName sets AgentName pointer",
			agentName:      "claude",
			wantAgentValue: "claude",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeSessionCommandServer{}
			adapter := &CommandHandlerAdapter{Commands: fake}

			chat, err := adapter.RecordChat(context.Background(), "s1", "as1", "title", false, tc.agentName)
			if err != nil {
				t.Fatalf("RecordChat returned unexpected error: %v", err)
			}

			// Verify the returned chat ID is threaded through from the stub response.
			if chat == nil || chat.Id != "chat1" {
				t.Fatalf("expected chat.Id == %q, got %v", "chat1", chat)
			}

			req := fake.lastRecordChat
			if req == nil {
				t.Fatal("no RecordChatRequest was captured by the fake")
			}

			// Verify the non-agent-name fields are threaded through correctly.
			if req.SessionId != "s1" {
				t.Errorf("SessionId: got %q, want %q", req.SessionId, "s1")
			}
			if req.AgentSessionId != "as1" {
				t.Errorf("AgentSessionId: got %q, want %q", req.AgentSessionId, "as1")
			}
			if req.Title != "title" {
				t.Errorf("Title: got %q, want %q", req.Title, "title")
			}
			if req.Resume != false {
				t.Errorf("Resume: got %v, want false", req.Resume)
			}

			// Verify optional AgentName field mapping — check the raw pointer,
			// NOT GetAgentName(), which masks nil vs "".
			if tc.wantAgentNilNil {
				if req.AgentName != nil {
					t.Errorf("AgentName: got %v (%q), want nil", req.AgentName, *req.AgentName)
				}
			} else {
				if req.AgentName == nil {
					t.Errorf("AgentName: got nil, want pointer to %q", tc.wantAgentValue)
				} else if *req.AgentName != tc.wantAgentValue {
					t.Errorf("AgentName: got %q, want %q", *req.AgentName, tc.wantAgentValue)
				}
			}
		})
	}
}

func TestCommandHandlerAdapter_Pause(t *testing.T) {
	t.Parallel()

	t.Run("empty session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Automation: &fakeAutomationToggler{}}
		if _, err := adapter.Pause(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "pause: session_id required") {
			t.Fatalf("Pause(\"\") error = %v, want pause: session_id required", err)
		}
	})

	t.Run("missing automation toggler is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.Pause(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "pause: automation toggler not wired") {
			t.Fatalf("Pause error = %v, want automation toggler not wired", err)
		}
	})

	t.Run("toggler error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Automation: &fakeAutomationToggler{err: errors.New("boom")}}
		if _, err := adapter.Pause(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "pause session: boom") {
			t.Fatalf("Pause error = %v, want pause session: boom", err)
		}
	})

	t.Run("disables automation and returns nil when no reader is wired", func(t *testing.T) {
		t.Parallel()
		tog := &fakeAutomationToggler{}
		adapter := &CommandHandlerAdapter{Automation: tog}
		sess, err := adapter.Pause(context.Background(), "s1")
		if err != nil {
			t.Fatalf("Pause returned error: %v", err)
		}
		if sess != nil {
			t.Fatalf("Pause session = %v, want nil when no reader wired", sess)
		}
		if tog.gotEnabled {
			t.Errorf("SetAutomationEnabled enabled = true, want false for pause")
		}
		if tog.gotID != "s1" {
			t.Errorf("SetAutomationEnabled id = %q, want s1", tog.gotID)
		}
	})

	t.Run("returns the reloaded session when a reader is wired", func(t *testing.T) {
		t.Parallel()
		want := &pb.Session{Id: "s1"}
		adapter := &CommandHandlerAdapter{Automation: &fakeAutomationToggler{}, Sessions: &fakeCmdSessionReader{sess: want}}
		got, err := adapter.Pause(context.Background(), "s1")
		if err != nil {
			t.Fatalf("Pause returned error: %v", err)
		}
		if got != want {
			t.Fatalf("Pause session = %v, want %v", got, want)
		}
	})
}

func TestCommandHandlerAdapter_Resume(t *testing.T) {
	t.Parallel()

	t.Run("empty session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Automation: &fakeAutomationToggler{}}
		if _, err := adapter.Resume(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "resume: session_id required") {
			t.Fatalf("Resume(\"\") error = %v, want resume: session_id required", err)
		}
	})

	t.Run("missing automation toggler is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.Resume(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "resume: automation toggler not wired") {
			t.Fatalf("Resume error = %v, want automation toggler not wired", err)
		}
	})

	t.Run("toggler error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Automation: &fakeAutomationToggler{err: errors.New("boom")}}
		if _, err := adapter.Resume(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "resume session: boom") {
			t.Fatalf("Resume error = %v, want resume session: boom", err)
		}
	})

	t.Run("enables automation and returns nil when no reader is wired", func(t *testing.T) {
		t.Parallel()
		tog := &fakeAutomationToggler{}
		adapter := &CommandHandlerAdapter{Automation: tog}
		sess, err := adapter.Resume(context.Background(), "s1")
		if err != nil {
			t.Fatalf("Resume returned error: %v", err)
		}
		if sess != nil {
			t.Fatalf("Resume session = %v, want nil when no reader wired", sess)
		}
		if !tog.gotEnabled {
			t.Errorf("SetAutomationEnabled enabled = false, want true for resume")
		}
	})

	t.Run("returns the reloaded session when a reader is wired", func(t *testing.T) {
		t.Parallel()
		want := &pb.Session{Id: "s1"}
		adapter := &CommandHandlerAdapter{Automation: &fakeAutomationToggler{}, Sessions: &fakeCmdSessionReader{sess: want}}
		got, err := adapter.Resume(context.Background(), "s1")
		if err != nil {
			t.Fatalf("Resume returned error: %v", err)
		}
		if got != want {
			t.Fatalf("Resume session = %v, want %v", got, want)
		}
	})
}

func TestCommandHandlerAdapter_WakeChat(t *testing.T) {
	t.Parallel()

	t.Run("empty agent session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Waker: &fakeChatWaker{}}
		_, _, _, _, err := adapter.WakeChat(context.Background(), "", false)
		if err == nil || !strings.Contains(err.Error(), "agent_session_id required") {
			t.Fatalf("WakeChat error = %v, want agent_session_id required", err)
		}
	})

	t.Run("missing waker is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		_, _, _, _, err := adapter.WakeChat(context.Background(), "as1", false)
		if err == nil || !strings.Contains(err.Error(), "waker not wired") {
			t.Fatalf("WakeChat error = %v, want waker not wired", err)
		}
	})

	t.Run("delegates to the configured waker", func(t *testing.T) {
		t.Parallel()
		waker := &fakeChatWaker{outcome: pb.WakeChatResult_OUTCOME_RESUMED, tmux: "tmux1", reason: "fell back"}
		adapter := &CommandHandlerAdapter{Waker: waker}
		outcome, tmux, reason, _, err := adapter.WakeChat(context.Background(), "as1", true)
		if err != nil {
			t.Fatalf("WakeChat returned error: %v", err)
		}
		if outcome != pb.WakeChatResult_OUTCOME_RESUMED {
			t.Errorf("outcome = %v, want OUTCOME_RESUMED", outcome)
		}
		if tmux != "tmux1" {
			t.Errorf("tmux = %q, want tmux1", tmux)
		}
		if reason != "fell back" {
			t.Errorf("reason = %q, want %q", reason, "fell back")
		}
	})
}

func TestCommandHandlerAdapter_MergeSession(t *testing.T) {
	t.Parallel()

	t.Run("empty session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if _, err := adapter.MergeSession(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "merge: session_id required") {
			t.Fatalf("MergeSession error = %v, want merge: session_id required", err)
		}
	})

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.MergeSession(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "merge: command server not wired") {
			t.Fatalf("MergeSession error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.MergeSession(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "merge session: boom") {
			t.Fatalf("MergeSession error = %v, want merge session: boom", err)
		}
	})

	t.Run("returns successfully when the command server succeeds", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if _, err := adapter.MergeSession(context.Background(), "s1"); err != nil {
			t.Fatalf("MergeSession returned error: %v", err)
		}
	})
}

func TestCommandHandlerAdapter_SwitchAccount(t *testing.T) {
	t.Parallel()

	t.Run("empty session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if _, _, _, _, err := adapter.SwitchAccount(context.Background(), "", "", "acct", false); err == nil || !strings.Contains(err.Error(), "switch_account: session_id required") {
			t.Fatalf("SwitchAccount error = %v, want session_id required", err)
		}
	})

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, _, _, _, err := adapter.SwitchAccount(context.Background(), "s1", "", "acct", false); err == nil || !strings.Contains(err.Error(), "switch_account: command server not wired") {
			t.Fatalf("SwitchAccount error = %v, want command server not wired", err)
		}
	})

	t.Run("connect error is classified and wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: connect.NewError(connect.CodeFailedPrecondition, errors.New("cooling"))}}
		_, _, _, code, err := adapter.SwitchAccount(context.Background(), "s1", "", "acct", false)
		if err == nil || !strings.Contains(err.Error(), "switch session account:") || !strings.Contains(err.Error(), "cooling") {
			t.Fatalf("SwitchAccount error = %v, want wrapped cooling", err)
		}
		if code != pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION {
			t.Fatalf("error code = %v, want FAILED_PRECONDITION", code)
		}
	})

	t.Run("forwards fields and maps empty agent id to nil", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{switchResp: &pb.SwitchSessionAccountResponse{Resumed: true, TargetLabel: "Account B", NoticeText: "ok"}}
		adapter := &CommandHandlerAdapter{Commands: fake}
		resumed, label, notice, code, err := adapter.SwitchAccount(context.Background(), "s1", "", "acct-b", true)
		if err != nil {
			t.Fatalf("SwitchAccount returned error: %v", err)
		}
		if !resumed || label != "Account B" || notice != "ok" || code != pb.CommandResult_ERROR_CODE_UNSPECIFIED {
			t.Fatalf("result = (%v,%q,%q,%v)", resumed, label, notice, code)
		}
		if fake.lastSwitchReq.GetSessionId() != "s1" || fake.lastSwitchReq.GetAccountId() != "acct-b" || !fake.lastSwitchReq.GetForce() {
			t.Fatalf("forwarded req = %+v", fake.lastSwitchReq)
		}
		if fake.lastSwitchReq.AgentSessionId != nil {
			t.Fatalf("agent_session_id = %v, want nil for empty input", fake.lastSwitchReq.AgentSessionId)
		}
	})

	t.Run("maps non-empty agent id to a pointer", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		if _, _, _, _, err := adapter.SwitchAccount(context.Background(), "s1", "agent-1", "acct-b", false); err != nil {
			t.Fatalf("SwitchAccount returned error: %v", err)
		}
		if fake.lastSwitchReq.GetAgentSessionId() != "agent-1" {
			t.Fatalf("agent_session_id = %q, want agent-1", fake.lastSwitchReq.GetAgentSessionId())
		}
	})
}

func TestCommandHandlerAdapter_ArchiveSession(t *testing.T) {
	t.Parallel()

	t.Run("empty session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if _, err := adapter.ArchiveSession(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "archive: session_id required") {
			t.Fatalf("ArchiveSession error = %v, want archive: session_id required", err)
		}
	})

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.ArchiveSession(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "archive: command server not wired") {
			t.Fatalf("ArchiveSession error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.ArchiveSession(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "archive session: boom") {
			t.Fatalf("ArchiveSession error = %v, want archive session: boom", err)
		}
	})

	t.Run("returns successfully when the command server succeeds", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if _, err := adapter.ArchiveSession(context.Background(), "s1"); err != nil {
			t.Fatalf("ArchiveSession returned error: %v", err)
		}
	})
}

func TestCommandHandlerAdapter_DeleteChat(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if err := adapter.DeleteChat(context.Background(), "s1", "as1"); err == nil || !strings.Contains(err.Error(), "delete_chat: command server not wired") {
			t.Fatalf("DeleteChat error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if err := adapter.DeleteChat(context.Background(), "s1", "as1"); err == nil || !strings.Contains(err.Error(), "delete chat: boom") {
			t.Fatalf("DeleteChat error = %v, want delete chat: boom", err)
		}
	})

	t.Run("succeeds when the command server returns ok", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if err := adapter.DeleteChat(context.Background(), "s1", "as1"); err != nil {
			t.Fatalf("DeleteChat returned error: %v", err)
		}
	})
}

func TestCommandHandlerAdapter_ListRepos(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.ListRepos(context.Background()); err == nil || !strings.Contains(err.Error(), "list_repos: command server not wired") {
			t.Fatalf("ListRepos error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.ListRepos(context.Background()); err == nil || !strings.Contains(err.Error(), "list repos: boom") {
			t.Fatalf("ListRepos error = %v, want list repos: boom", err)
		}
	})

	t.Run("returns the response on success", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		resp, err := adapter.ListRepos(context.Background())
		if err != nil {
			t.Fatalf("ListRepos returned error: %v", err)
		}
		if resp == nil {
			t.Fatal("ListRepos returned nil response on success")
		}
	})
}

func TestCommandHandlerAdapter_ListAgents(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.ListAgents(context.Background()); err == nil || !strings.Contains(err.Error(), "list_agents: command server not wired") {
			t.Fatalf("ListAgents error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.ListAgents(context.Background()); err == nil || !strings.Contains(err.Error(), "list agents: boom") {
			t.Fatalf("ListAgents error = %v, want list agents: boom", err)
		}
	})

	t.Run("returns the response on success", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		resp, err := adapter.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("ListAgents returned error: %v", err)
		}
		if resp == nil {
			t.Fatal("ListAgents returned nil response on success")
		}
	})
}

func TestCommandHandlerAdapter_ListAccounts(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.ListAccounts(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "list_accounts: command server not wired") {
			t.Fatalf("ListAccounts error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.ListAccounts(context.Background(), "claude"); err == nil || !strings.Contains(err.Error(), "list accounts: boom") {
			t.Fatalf("ListAccounts error = %v, want list accounts: boom", err)
		}
	})

	t.Run("returns the response on success", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		resp, err := adapter.ListAccounts(context.Background(), "")
		if err != nil {
			t.Fatalf("ListAccounts returned error: %v", err)
		}
		if resp == nil {
			t.Fatal("ListAccounts returned nil response on success")
		}
	})
}

func TestCommandHandlerAdapter_ListRepoPRs(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.ListRepoPRs(context.Background(), "repo1"); err == nil || !strings.Contains(err.Error(), "list_repo_prs: command server not wired") {
			t.Fatalf("ListRepoPRs error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.ListRepoPRs(context.Background(), "repo1"); err == nil || !strings.Contains(err.Error(), "list repo prs: boom") {
			t.Fatalf("ListRepoPRs error = %v, want list repo prs: boom", err)
		}
	})

	t.Run("returns the response on success", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		resp, err := adapter.ListRepoPRs(context.Background(), "repo1")
		if err != nil {
			t.Fatalf("ListRepoPRs returned error: %v", err)
		}
		if resp == nil {
			t.Fatal("ListRepoPRs returned nil response on success")
		}
	})
}

func TestCommandHandlerAdapter_ListTrackerIssues(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.ListTrackerIssues(context.Background(), "repo1", "query", nil); err == nil || !strings.Contains(err.Error(), "list_tracker_issues: command server not wired") {
			t.Fatalf("ListTrackerIssues error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.ListTrackerIssues(context.Background(), "repo1", "query", nil); err == nil || !strings.Contains(err.Error(), "list tracker issues: boom") {
			t.Fatalf("ListTrackerIssues error = %v, want list tracker issues: boom", err)
		}
	})

	t.Run("returns the response on success", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		resp, err := adapter.ListTrackerIssues(context.Background(), "repo1", "query", nil)
		if err != nil {
			t.Fatalf("ListTrackerIssues returned error: %v", err)
		}
		if resp == nil {
			t.Fatal("ListTrackerIssues returned nil response on success")
		}
	})
}

func TestCommandHandlerAdapter_GetChatTranscript(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.GetChatTranscript(context.Background(), "s1", "a1", 0); err == nil || !strings.Contains(err.Error(), "get_chat_transcript: command server not wired") {
			t.Fatalf("GetChatTranscript error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.GetChatTranscript(context.Background(), "s1", "a1", 0); err == nil || !strings.Contains(err.Error(), "get chat transcript: boom") {
			t.Fatalf("GetChatTranscript error = %v, want get chat transcript: boom", err)
		}
	})

	t.Run("delegates to the command server and unwraps the response", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		resp, err := adapter.GetChatTranscript(context.Background(), "s1", "agent-9", 3)
		if err != nil {
			t.Fatalf("GetChatTranscript returned error: %v", err)
		}
		if resp == nil || !resp.GetExists() || resp.GetFinalAssistantText() != "final" {
			t.Fatalf("unexpected response: %+v", resp)
		}
		if fake.lastTranscriptID != "agent-9" {
			t.Fatalf("agent_session_id not forwarded: %q", fake.lastTranscriptID)
		}
	})
}

func TestCommandHandlerAdapter_SendChatMessage(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.SendChatMessage(context.Background(), "a1", "m", false, false); err == nil || !strings.Contains(err.Error(), "send_chat_message: command server not wired") {
			t.Fatalf("SendChatMessage error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.SendChatMessage(context.Background(), "a1", "m", false, false); err == nil || !strings.Contains(err.Error(), "send chat message: boom") {
			t.Fatalf("SendChatMessage error = %v, want send chat message: boom", err)
		}
	})

	t.Run("delegates to the command server and forwards all fields", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		resp, err := adapter.SendChatMessage(context.Background(), "agent-9", "hello", true, true)
		if err != nil {
			t.Fatalf("SendChatMessage returned error: %v", err)
		}
		if resp == nil || !resp.GetDelivered() || resp.GetTmuxSessionName() != "tmux-send" {
			t.Fatalf("unexpected response: %+v", resp)
		}
		got := fake.lastSendReq
		if got.GetAgentSessionId() != "agent-9" || got.GetMessage() != "hello" || !got.GetWakeIfAsleep() || !got.GetSubmit() {
			t.Fatalf("send fields not forwarded: %+v", got)
		}
	})
}
