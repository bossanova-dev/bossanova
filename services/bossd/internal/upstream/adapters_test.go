package upstream

import (
	"context"
	"errors"
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
	lastRecordChat *pb.RecordChatRequest
}

func (f *fakeSessionCommandServer) MergeSession(_ context.Context, _ *connect.Request[pb.MergeSessionRequest]) (*connect.Response[pb.MergeSessionResponse], error) {
	return connect.NewResponse(&pb.MergeSessionResponse{}), nil
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

func (f *fakeSessionCommandServer) ListRepoPRs(_ context.Context, _ *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error) {
	return connect.NewResponse(&pb.ListRepoPRsResponse{}), nil
}

func (f *fakeSessionCommandServer) ListTrackerIssues(_ context.Context, _ *connect.Request[pb.ListTrackerIssuesRequest]) (*connect.Response[pb.ListTrackerIssuesResponse], error) {
	return connect.NewResponse(&pb.ListTrackerIssuesResponse{}), nil
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

	fake := &fakeStreamCreateSessioner{}
	adapter := &SessionCreatorAdapter{Server: fake, Logger: zerolog.Nop()}

	ch, err := adapter.Create(context.Background(), &pb.CreateSessionCommand{
		RepoId:        "r1",
		Title:         "x",
		PrNumber:      &pr,
		BranchName:    &branch,
		TrackerId:     &trackerID,
		TrackerUrl:    &trackerURL,
		TrackerIssue:  &pb.TrackerIssue{Title: issueTitle},
		TrackerSource: &source,
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
