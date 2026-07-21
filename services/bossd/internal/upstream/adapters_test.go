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
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeSessionCommandServer captures the last RecordChatRequest and returns a
// stub response. The other three methods return zero-value responses; they are
// unused by these tests but required to satisfy SessionCommandServer.
type fakeSessionCommandServer struct {
	lastRecordChat       *pb.RecordChatRequest
	lastTranscriptID     string
	lastSendReq          *pb.SendChatMessageRequest
	lastSwitchReq        *pb.SwitchSessionAccountRequest
	switchResp           *pb.SwitchSessionAccountResponse
	lastCreateCron       *pb.CreateCronJobRequest
	lastUpdateCron       *pb.UpdateCronJobRequest
	lastDeleteCronID     string
	lastRunCronID        string
	lastAddAccount       *pb.AddAccountRequest
	lastRefreshAcct      *pb.RefreshAccountRequest
	lastUpdateAcct       *pb.UpdateAccountRequest
	lastRemoveAcctID     string
	lastTestAcctID       string
	lastCloseID          string
	lastResurrectID      string
	lastRemoveSessionID  string
	lastEmptyTrashReq    *pb.EmptyTrashRequest
	lastRetryID          string
	lastUpdateReq        *pb.UpdateSessionRequest
	lastLinkReq          *pb.LinkSessionPRRequest
	lastUpdateChatTitle  *pb.UpdateChatTitleRequest
	lastReportChatStatus *pb.ReportChatStatusRequest
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

func (f *fakeSessionCommandServer) CloseSession(_ context.Context, req *connect.Request[pb.CloseSessionRequest]) (*connect.Response[pb.CloseSessionResponse], error) {
	f.lastCloseID = req.Msg.GetId()
	return connect.NewResponse(&pb.CloseSessionResponse{Session: &pb.Session{Id: req.Msg.GetId()}}), nil
}

func (f *fakeSessionCommandServer) ResurrectSession(_ context.Context, req *connect.Request[pb.ResurrectSessionRequest]) (*connect.Response[pb.ResurrectSessionResponse], error) {
	f.lastResurrectID = req.Msg.GetId()
	return connect.NewResponse(&pb.ResurrectSessionResponse{Session: &pb.Session{Id: req.Msg.GetId()}}), nil
}

func (f *fakeSessionCommandServer) RemoveSession(_ context.Context, req *connect.Request[pb.RemoveSessionRequest]) (*connect.Response[pb.RemoveSessionResponse], error) {
	f.lastRemoveSessionID = req.Msg.GetId()
	return connect.NewResponse(&pb.RemoveSessionResponse{}), nil
}

func (f *fakeSessionCommandServer) EmptyTrash(_ context.Context, req *connect.Request[pb.EmptyTrashRequest]) (*connect.Response[pb.EmptyTrashResponse], error) {
	f.lastEmptyTrashReq = req.Msg
	return connect.NewResponse(&pb.EmptyTrashResponse{DeletedCount: 3}), nil
}

func (f *fakeSessionCommandServer) RetrySession(_ context.Context, req *connect.Request[pb.RetrySessionRequest]) (*connect.Response[pb.RetrySessionResponse], error) {
	f.lastRetryID = req.Msg.GetId()
	return connect.NewResponse(&pb.RetrySessionResponse{Session: &pb.Session{Id: req.Msg.GetId()}}), nil
}

func (f *fakeSessionCommandServer) UpdateSession(_ context.Context, req *connect.Request[pb.UpdateSessionRequest]) (*connect.Response[pb.UpdateSessionResponse], error) {
	f.lastUpdateReq = req.Msg
	return connect.NewResponse(&pb.UpdateSessionResponse{Session: &pb.Session{Id: req.Msg.GetId()}}), nil
}

func (f *fakeSessionCommandServer) LinkSessionPR(_ context.Context, req *connect.Request[pb.LinkSessionPRRequest]) (*connect.Response[pb.LinkSessionPRResponse], error) {
	f.lastLinkReq = req.Msg
	return connect.NewResponse(&pb.LinkSessionPRResponse{Session: &pb.Session{Id: req.Msg.GetId()}}), nil
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

func (f *fakeSessionCommandServer) UpdateChatTitle(_ context.Context, req *connect.Request[pb.UpdateChatTitleRequest]) (*connect.Response[pb.UpdateChatTitleResponse], error) {
	f.lastUpdateChatTitle = req.Msg
	return connect.NewResponse(&pb.UpdateChatTitleResponse{}), nil
}

func (f *fakeSessionCommandServer) ReportChatStatus(_ context.Context, req *connect.Request[pb.ReportChatStatusRequest]) (*connect.Response[pb.ReportChatStatusResponse], error) {
	f.lastReportChatStatus = req.Msg
	return connect.NewResponse(&pb.ReportChatStatusResponse{}), nil
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

func (f *fakeSessionCommandServer) GetRepoSettings(_ context.Context, _ *connect.Request[pb.GetRepoSettingsRequest]) (*connect.Response[pb.GetRepoSettingsResponse], error) {
	return connect.NewResponse(&pb.GetRepoSettingsResponse{}), nil
}

func (f *fakeSessionCommandServer) UpdateRepo(_ context.Context, _ *connect.Request[pb.UpdateRepoRequest]) (*connect.Response[pb.UpdateRepoResponse], error) {
	return connect.NewResponse(&pb.UpdateRepoResponse{}), nil
}

func (f *fakeSessionCommandServer) RemoveRepo(_ context.Context, _ *connect.Request[pb.RemoveRepoRequest]) (*connect.Response[pb.RemoveRepoResponse], error) {
	return connect.NewResponse(&pb.RemoveRepoResponse{}), nil
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

func (f *fakeSessionCommandServer) CreateCronJob(_ context.Context, req *connect.Request[pb.CreateCronJobRequest]) (*connect.Response[pb.CreateCronJobResponse], error) {
	f.lastCreateCron = req.Msg
	return connect.NewResponse(&pb.CreateCronJobResponse{CronJob: &pb.CronJob{Id: "cj-new", Name: req.Msg.GetName()}}), nil
}

func (f *fakeSessionCommandServer) ListCronJobs(_ context.Context, _ *connect.Request[pb.ListCronJobsRequest]) (*connect.Response[pb.ListCronJobsResponse], error) {
	return connect.NewResponse(&pb.ListCronJobsResponse{CronJobs: []*pb.CronJob{{Id: "cj1"}}}), nil
}

func (f *fakeSessionCommandServer) UpdateCronJob(_ context.Context, req *connect.Request[pb.UpdateCronJobRequest]) (*connect.Response[pb.UpdateCronJobResponse], error) {
	f.lastUpdateCron = req.Msg
	return connect.NewResponse(&pb.UpdateCronJobResponse{CronJob: &pb.CronJob{Id: req.Msg.GetId()}}), nil
}

func (f *fakeSessionCommandServer) DeleteCronJob(_ context.Context, req *connect.Request[pb.DeleteCronJobRequest]) (*connect.Response[pb.DeleteCronJobResponse], error) {
	f.lastDeleteCronID = req.Msg.GetId()
	return connect.NewResponse(&pb.DeleteCronJobResponse{}), nil
}

func (f *fakeSessionCommandServer) RunCronJobNow(_ context.Context, req *connect.Request[pb.RunCronJobNowRequest]) (*connect.Response[pb.RunCronJobNowResponse], error) {
	f.lastRunCronID = req.Msg.GetId()
	return connect.NewResponse(&pb.RunCronJobNowResponse{SkippedReason: "skip"}), nil
}

func (f *fakeSessionCommandServer) AddAccount(_ context.Context, req *connect.Request[pb.AddAccountRequest]) (*connect.Response[pb.AddAccountResponse], error) {
	f.lastAddAccount = req.Msg
	return connect.NewResponse(&pb.AddAccountResponse{Account: &pb.Account{Id: "acc-new", Provider: req.Msg.GetProvider(), Label: req.Msg.GetLabel()}}), nil
}

func (f *fakeSessionCommandServer) RefreshAccount(_ context.Context, req *connect.Request[pb.RefreshAccountRequest]) (*connect.Response[pb.RefreshAccountResponse], error) {
	f.lastRefreshAcct = req.Msg
	return connect.NewResponse(&pb.RefreshAccountResponse{Account: &pb.Account{Id: req.Msg.GetId()}, Detail: "credential refreshed"}), nil
}

func (f *fakeSessionCommandServer) UpdateAccount(_ context.Context, req *connect.Request[pb.UpdateAccountRequest]) (*connect.Response[pb.UpdateAccountResponse], error) {
	f.lastUpdateAcct = req.Msg
	return connect.NewResponse(&pb.UpdateAccountResponse{Account: &pb.Account{Id: req.Msg.GetId()}}), nil
}

func (f *fakeSessionCommandServer) RemoveAccount(_ context.Context, req *connect.Request[pb.RemoveAccountRequest]) (*connect.Response[pb.RemoveAccountResponse], error) {
	f.lastRemoveAcctID = req.Msg.GetId()
	return connect.NewResponse(&pb.RemoveAccountResponse{}), nil
}

func (f *fakeSessionCommandServer) TestAccount(_ context.Context, req *connect.Request[pb.TestAccountRequest]) (*connect.Response[pb.TestAccountResponse], error) {
	f.lastTestAcctID = req.Msg.GetId()
	return connect.NewResponse(&pb.TestAccountResponse{Account: &pb.Account{Id: req.Msg.GetId()}, LiveSmokeRan: true, Detail: "credential test passed"}), nil
}

func (f *fakeSessionCommandServer) ListChats(_ context.Context, _ *connect.Request[pb.ListChatsRequest]) (*connect.Response[pb.ListChatsResponse], error) {
	return connect.NewResponse(&pb.ListChatsResponse{}), nil
}

func (f *fakeSessionCommandServer) GetSessionStatuses(_ context.Context, _ *connect.Request[pb.GetSessionStatusesRequest]) (*connect.Response[pb.GetSessionStatusesResponse], error) {
	return connect.NewResponse(&pb.GetSessionStatusesResponse{}), nil
}

func (f *fakeSessionCommandServer) ListCheckSnapshots(_ context.Context, _ *connect.Request[pb.ListCheckSnapshotsRequest]) (*connect.Response[pb.ListCheckSnapshotsResponse], error) {
	return connect.NewResponse(&pb.ListCheckSnapshotsResponse{}), nil
}

func (f *fakeSessionCommandServer) ListPlugins(_ context.Context, _ *connect.Request[pb.ListPluginsRequest]) (*connect.Response[pb.ListPluginsResponse], error) {
	return connect.NewResponse(&pb.ListPluginsResponse{}), nil
}

func (f *fakeSessionCommandServer) GetCronJob(_ context.Context, _ *connect.Request[pb.GetCronJobRequest]) (*connect.Response[pb.GetCronJobResponse], error) {
	return connect.NewResponse(&pb.GetCronJobResponse{}), nil
}

func (f *fakeSessionCommandServer) RepairDoctor(_ context.Context, _ *connect.Request[pb.RepairDoctorRequest]) (*connect.Response[pb.RepairDoctorResponse], error) {
	return connect.NewResponse(&pb.RepairDoctorResponse{}), nil
}

// fakeAutomationToggler records the last SetIsAutomationEnabled call and returns
// the configured error, driving the pause/resume adapter paths.
type fakeAutomationToggler struct {
	gotEnabled bool
	gotID      string
	err        error
}

func (f *fakeAutomationToggler) SetIsAutomationEnabled(_ context.Context, id string, enabled bool) error {
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

func (e *errCommandServer) RetrySession(context.Context, *connect.Request[pb.RetrySessionRequest]) (*connect.Response[pb.RetrySessionResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) UpdateSession(context.Context, *connect.Request[pb.UpdateSessionRequest]) (*connect.Response[pb.UpdateSessionResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) LinkSessionPR(context.Context, *connect.Request[pb.LinkSessionPRRequest]) (*connect.Response[pb.LinkSessionPRResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) RecordChat(context.Context, *connect.Request[pb.RecordChatRequest]) (*connect.Response[pb.RecordChatResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) DeleteChat(context.Context, *connect.Request[pb.DeleteChatRequest]) (*connect.Response[pb.DeleteChatResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) UpdateChatTitle(context.Context, *connect.Request[pb.UpdateChatTitleRequest]) (*connect.Response[pb.UpdateChatTitleResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ReportChatStatus(context.Context, *connect.Request[pb.ReportChatStatusRequest]) (*connect.Response[pb.ReportChatStatusResponse], error) {
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

func (e *errCommandServer) GetRepoSettings(context.Context, *connect.Request[pb.GetRepoSettingsRequest]) (*connect.Response[pb.GetRepoSettingsResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) UpdateRepo(context.Context, *connect.Request[pb.UpdateRepoRequest]) (*connect.Response[pb.UpdateRepoResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) RemoveRepo(context.Context, *connect.Request[pb.RemoveRepoRequest]) (*connect.Response[pb.RemoveRepoResponse], error) {
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

func (e *errCommandServer) CreateCronJob(context.Context, *connect.Request[pb.CreateCronJobRequest]) (*connect.Response[pb.CreateCronJobResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ListCronJobs(context.Context, *connect.Request[pb.ListCronJobsRequest]) (*connect.Response[pb.ListCronJobsResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) UpdateCronJob(context.Context, *connect.Request[pb.UpdateCronJobRequest]) (*connect.Response[pb.UpdateCronJobResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) DeleteCronJob(context.Context, *connect.Request[pb.DeleteCronJobRequest]) (*connect.Response[pb.DeleteCronJobResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) RunCronJobNow(context.Context, *connect.Request[pb.RunCronJobNowRequest]) (*connect.Response[pb.RunCronJobNowResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) AddAccount(context.Context, *connect.Request[pb.AddAccountRequest]) (*connect.Response[pb.AddAccountResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) RefreshAccount(context.Context, *connect.Request[pb.RefreshAccountRequest]) (*connect.Response[pb.RefreshAccountResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) UpdateAccount(context.Context, *connect.Request[pb.UpdateAccountRequest]) (*connect.Response[pb.UpdateAccountResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) RemoveAccount(context.Context, *connect.Request[pb.RemoveAccountRequest]) (*connect.Response[pb.RemoveAccountResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) TestAccount(context.Context, *connect.Request[pb.TestAccountRequest]) (*connect.Response[pb.TestAccountResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ListChats(context.Context, *connect.Request[pb.ListChatsRequest]) (*connect.Response[pb.ListChatsResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) GetSessionStatuses(context.Context, *connect.Request[pb.GetSessionStatusesRequest]) (*connect.Response[pb.GetSessionStatusesResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ListCheckSnapshots(context.Context, *connect.Request[pb.ListCheckSnapshotsRequest]) (*connect.Response[pb.ListCheckSnapshotsResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ListPlugins(context.Context, *connect.Request[pb.ListPluginsRequest]) (*connect.Response[pb.ListPluginsResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) GetCronJob(context.Context, *connect.Request[pb.GetCronJobRequest]) (*connect.Response[pb.GetCronJobResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) RepairDoctor(context.Context, *connect.Request[pb.RepairDoctorRequest]) (*connect.Response[pb.RepairDoctorResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) CloseSession(context.Context, *connect.Request[pb.CloseSessionRequest]) (*connect.Response[pb.CloseSessionResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ResurrectSession(context.Context, *connect.Request[pb.ResurrectSessionRequest]) (*connect.Response[pb.ResurrectSessionResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) RemoveSession(context.Context, *connect.Request[pb.RemoveSessionRequest]) (*connect.Response[pb.RemoveSessionResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) EmptyTrash(context.Context, *connect.Request[pb.EmptyTrashRequest]) (*connect.Response[pb.EmptyTrashResponse], error) {
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
		RepoId:      "r1",
		Title:       "x",
		Plan:        "p",
		BaseBranch:  "main",
		IsQuickChat: true,
		AgentName:   "claude",
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
	if fake.lastReq.GetBaseBranch() != "main" || !fake.lastReq.GetIsQuickChat() {
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
		RepoId:           "r1",
		Title:            "x",
		PrNumber:         &pr,
		BranchName:       &branch,
		TrackerId:        &trackerID,
		TrackerUrl:       &trackerURL,
		TrackerIssue:     &pb.TrackerIssue{Title: issueTitle},
		TrackerSource:    &source,
		AccountId:        &accountID,
		Force:            true,
		ForceBranch:      true,
		Detach:           true,
		Model:            &model,
		IsTmuxUnattended: true,
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
	if !req.GetIsTmuxUnattended() {
		t.Errorf("IsTmuxUnattended: got false, want true")
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
			t.Errorf("SetIsAutomationEnabled enabled = true, want false for pause")
		}
		if tog.gotID != "s1" {
			t.Errorf("SetIsAutomationEnabled id = %q, want s1", tog.gotID)
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
			t.Errorf("SetIsAutomationEnabled enabled = false, want true for resume")
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

func TestCommandHandlerAdapter_RetrySession(t *testing.T) {
	t.Parallel()

	t.Run("empty session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if _, err := adapter.RetrySession(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "retry: session_id required") {
			t.Fatalf("RetrySession error = %v, want retry: session_id required", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.RetrySession(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "retry session: boom") {
			t.Fatalf("RetrySession error = %v, want retry session: boom", err)
		}
	})

	t.Run("forwards id and returns session", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		sess, err := adapter.RetrySession(context.Background(), "s1")
		if err != nil {
			t.Fatalf("RetrySession returned error: %v", err)
		}
		if fake.lastRetryID != "s1" || sess.GetId() != "s1" {
			t.Fatalf("retry id = %q, session = %+v", fake.lastRetryID, sess)
		}
	})
}

func TestCommandHandlerAdapter_UpdateSession(t *testing.T) {
	t.Parallel()

	t.Run("empty session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if _, err := adapter.UpdateSession(context.Background(), &pb.UpdateSessionCommand{}); err == nil || !strings.Contains(err.Error(), "update_session: session_id required") {
			t.Fatalf("UpdateSession error = %v, want session_id required", err)
		}
	})

	t.Run("forwards optional title/tracker pointers unchanged", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		title := "New"
		trackerID := "BOS-9"
		if _, err := adapter.UpdateSession(context.Background(), &pb.UpdateSessionCommand{
			SessionId: "s1", Title: &title, TrackerId: &trackerID,
		}); err != nil {
			t.Fatalf("UpdateSession returned error: %v", err)
		}
		if fake.lastUpdateReq.GetId() != "s1" || fake.lastUpdateReq.GetTitle() != title || fake.lastUpdateReq.GetTrackerId() != trackerID {
			t.Fatalf("forwarded req = %+v", fake.lastUpdateReq)
		}
		if fake.lastUpdateReq.TrackerUrl != nil {
			t.Fatalf("tracker_url should stay nil, got %v", fake.lastUpdateReq.TrackerUrl)
		}
	})
}

func TestCommandHandlerAdapter_LinkSessionPR(t *testing.T) {
	t.Parallel()

	t.Run("empty session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if _, err := adapter.LinkSessionPR(context.Background(), "", "1"); err == nil || !strings.Contains(err.Error(), "link_session_pr: session_id required") {
			t.Fatalf("LinkSessionPR error = %v, want session_id required", err)
		}
	})

	t.Run("forwards id and pr", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		if _, err := adapter.LinkSessionPR(context.Background(), "s1", "42"); err != nil {
			t.Fatalf("LinkSessionPR returned error: %v", err)
		}
		if fake.lastLinkReq.GetId() != "s1" || fake.lastLinkReq.GetPr() != "42" {
			t.Fatalf("forwarded req = %+v", fake.lastLinkReq)
		}
	})
}

func TestCommandHandlerAdapter_UpdateChatTitle(t *testing.T) {
	t.Parallel()

	t.Run("empty agent id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if err := adapter.UpdateChatTitle(context.Background(), "", "t"); err == nil || !strings.Contains(err.Error(), "update_chat_title: agent_session_id required") {
			t.Fatalf("UpdateChatTitle error = %v, want agent_session_id required", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if err := adapter.UpdateChatTitle(context.Background(), "agent-1", "t"); err == nil || !strings.Contains(err.Error(), "update chat title: boom") {
			t.Fatalf("UpdateChatTitle error = %v, want update chat title: boom", err)
		}
	})

	t.Run("forwards agent id and title", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		if err := adapter.UpdateChatTitle(context.Background(), "agent-1", "Renamed"); err != nil {
			t.Fatalf("UpdateChatTitle returned error: %v", err)
		}
		if fake.lastUpdateChatTitle.GetAgentSessionId() != "agent-1" || fake.lastUpdateChatTitle.GetTitle() != "Renamed" {
			t.Fatalf("forwarded req = %+v", fake.lastUpdateChatTitle)
		}
	})
}

func TestCommandHandlerAdapter_ReportChatStatus(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if err := adapter.ReportChatStatus(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "report_chat_status: command server not wired") {
			t.Fatalf("ReportChatStatus error = %v, want command server not wired", err)
		}
	})

	t.Run("forwards the reports slice", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		reports := []*pb.ChatStatusReport{{AgentSessionId: "agent-1"}, {AgentSessionId: "agent-2"}}
		if err := adapter.ReportChatStatus(context.Background(), reports); err != nil {
			t.Fatalf("ReportChatStatus returned error: %v", err)
		}
		if len(fake.lastReportChatStatus.GetReports()) != 2 {
			t.Fatalf("forwarded reports = %d, want 2", len(fake.lastReportChatStatus.GetReports()))
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

func TestCommandHandlerAdapter_CloseSession(t *testing.T) {
	t.Parallel()

	t.Run("empty session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if _, err := adapter.CloseSession(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "close: session_id required") {
			t.Fatalf("CloseSession error = %v, want close: session_id required", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.CloseSession(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "close session: boom") {
			t.Fatalf("CloseSession error = %v, want close session: boom", err)
		}
	})

	t.Run("returns the session on success", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		sess, err := adapter.CloseSession(context.Background(), "s1")
		if err != nil {
			t.Fatalf("CloseSession returned error: %v", err)
		}
		if fake.lastCloseID != "s1" || sess.GetId() != "s1" {
			t.Fatalf("CloseSession id plumbing = %q / %q, want s1", fake.lastCloseID, sess.GetId())
		}
	})
}

func TestCommandHandlerAdapter_ResurrectSession(t *testing.T) {
	t.Parallel()

	t.Run("empty session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if _, err := adapter.ResurrectSession(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "resurrect: session_id required") {
			t.Fatalf("ResurrectSession error = %v, want resurrect: session_id required", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.ResurrectSession(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "resurrect session: boom") {
			t.Fatalf("ResurrectSession error = %v, want resurrect session: boom", err)
		}
	})

	t.Run("returns the session on success", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		sess, err := adapter.ResurrectSession(context.Background(), "s1")
		if err != nil {
			t.Fatalf("ResurrectSession returned error: %v", err)
		}
		if fake.lastResurrectID != "s1" || sess.GetId() != "s1" {
			t.Fatalf("ResurrectSession id plumbing = %q / %q, want s1", fake.lastResurrectID, sess.GetId())
		}
	})
}

func TestCommandHandlerAdapter_RemoveSession(t *testing.T) {
	t.Parallel()

	t.Run("empty session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if err := adapter.RemoveSession(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "remove: session_id required") {
			t.Fatalf("RemoveSession error = %v, want remove: session_id required", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if err := adapter.RemoveSession(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "remove session: boom") {
			t.Fatalf("RemoveSession error = %v, want remove session: boom", err)
		}
	})

	t.Run("succeeds when the command server returns ok", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		if err := adapter.RemoveSession(context.Background(), "s1"); err != nil {
			t.Fatalf("RemoveSession returned error: %v", err)
		}
		if fake.lastRemoveSessionID != "s1" {
			t.Fatalf("RemoveSession id = %q, want s1", fake.lastRemoveSessionID)
		}
	})
}

func TestCommandHandlerAdapter_EmptyTrash(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.EmptyTrash(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "empty_trash: command server not wired") {
			t.Fatalf("EmptyTrash error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.EmptyTrash(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "empty trash: boom") {
			t.Fatalf("EmptyTrash error = %v, want empty trash: boom", err)
		}
	})

	t.Run("threads older_than and returns the deleted count", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		older := timestamppb.Now()
		count, err := adapter.EmptyTrash(context.Background(), older)
		if err != nil {
			t.Fatalf("EmptyTrash returned error: %v", err)
		}
		if count != 3 {
			t.Fatalf("EmptyTrash count = %d, want 3", count)
		}
		if fake.lastEmptyTrashReq == nil || fake.lastEmptyTrashReq.GetOlderThan() == nil ||
			!fake.lastEmptyTrashReq.GetOlderThan().AsTime().Equal(older.AsTime()) {
			t.Fatalf("EmptyTrash older_than not threaded through: %+v", fake.lastEmptyTrashReq)
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

func TestCommandHandlerAdapter_AddAccount(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.AddAccount(context.Background(), &pb.AddAccountCommand{}); err == nil || !strings.Contains(err.Error(), "add_account: command server not wired") {
			t.Fatalf("AddAccount error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.AddAccount(context.Background(), &pb.AddAccountCommand{Provider: "claude"}); err == nil || !strings.Contains(err.Error(), "add account: boom") {
			t.Fatalf("AddAccount error = %v, want add account: boom", err)
		}
	})

	t.Run("forwards fields verbatim and unwraps the response", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		resp, err := adapter.AddAccount(context.Background(), &pb.AddAccountCommand{
			Provider: "codex", Label: "work", Priority: 3, Credential: []byte("cred-bytes"),
		})
		if err != nil {
			t.Fatalf("AddAccount returned error: %v", err)
		}
		if resp == nil || resp.GetAccount().GetId() != "acc-new" {
			t.Fatalf("unexpected response: %+v", resp)
		}
		if fake.lastAddAccount.GetProvider() != "codex" || fake.lastAddAccount.GetLabel() != "work" ||
			fake.lastAddAccount.GetPriority() != 3 || string(fake.lastAddAccount.GetCredential()) != "cred-bytes" {
			t.Fatalf("fields not forwarded verbatim: %+v", fake.lastAddAccount)
		}
	})
}

func TestCommandHandlerAdapter_RefreshAccount(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.RefreshAccount(context.Background(), &pb.RefreshAccountCommand{}); err == nil || !strings.Contains(err.Error(), "refresh_account: command server not wired") {
			t.Fatalf("RefreshAccount error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.RefreshAccount(context.Background(), &pb.RefreshAccountCommand{Id: "a1"}); err == nil || !strings.Contains(err.Error(), "refresh account: boom") {
			t.Fatalf("RefreshAccount error = %v, want refresh account: boom", err)
		}
	})

	t.Run("forwards fields verbatim and unwraps the response", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		resp, err := adapter.RefreshAccount(context.Background(), &pb.RefreshAccountCommand{
			Id: "a1", Credential: []byte("new-cred"), TestAfterSave: true,
		})
		if err != nil {
			t.Fatalf("RefreshAccount returned error: %v", err)
		}
		if resp == nil || resp.GetAccount().GetId() != "a1" {
			t.Fatalf("unexpected response: %+v", resp)
		}
		if fake.lastRefreshAcct.GetId() != "a1" || string(fake.lastRefreshAcct.GetCredential()) != "new-cred" ||
			!fake.lastRefreshAcct.GetTestAfterSave() {
			t.Fatalf("fields not forwarded verbatim: %+v", fake.lastRefreshAcct)
		}
	})
}

func TestCommandHandlerAdapter_UpdateAccount(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.UpdateAccount(context.Background(), &pb.UpdateAccountCommand{}); err == nil || !strings.Contains(err.Error(), "update_account: command server not wired") {
			t.Fatalf("UpdateAccount error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.UpdateAccount(context.Background(), &pb.UpdateAccountCommand{Id: "a1"}); err == nil || !strings.Contains(err.Error(), "update account: boom") {
			t.Fatalf("UpdateAccount error = %v, want update account: boom", err)
		}
	})

	t.Run("forwards optional pointers 1:1 and unwraps the response", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		label := "renamed"
		priority := int32(7)
		status := "disabled"
		resp, err := adapter.UpdateAccount(context.Background(), &pb.UpdateAccountCommand{
			Id: "a1", Label: &label, Priority: &priority, Status: &status, AllowedModels: []string{"m1"},
		})
		if err != nil {
			t.Fatalf("UpdateAccount returned error: %v", err)
		}
		if resp == nil || resp.GetAccount().GetId() != "a1" {
			t.Fatalf("unexpected response: %+v", resp)
		}
		got := fake.lastUpdateAcct
		if got.GetId() != "a1" || got.Label == nil || *got.Label != "renamed" ||
			got.Priority == nil || *got.Priority != 7 || got.Status == nil || *got.Status != "disabled" ||
			len(got.GetAllowedModels()) != 1 {
			t.Fatalf("optional pointers not forwarded 1:1: %+v", got)
		}
	})

	t.Run("unset optional pointers stay nil", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		if _, err := adapter.UpdateAccount(context.Background(), &pb.UpdateAccountCommand{Id: "a1"}); err != nil {
			t.Fatalf("UpdateAccount returned error: %v", err)
		}
		got := fake.lastUpdateAcct
		if got.Label != nil || got.Priority != nil || got.Status != nil || len(got.GetAllowedModels()) != 0 {
			t.Fatalf("expected unset optionals to stay nil, got: %+v", got)
		}
	})
}

func TestCommandHandlerAdapter_RemoveAccount(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if err := adapter.RemoveAccount(context.Background(), "a1"); err == nil || !strings.Contains(err.Error(), "remove_account: command server not wired") {
			t.Fatalf("RemoveAccount error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if err := adapter.RemoveAccount(context.Background(), "a1"); err == nil || !strings.Contains(err.Error(), "remove account: boom") {
			t.Fatalf("RemoveAccount error = %v, want remove account: boom", err)
		}
	})

	t.Run("forwards the id and returns nil on success", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		if err := adapter.RemoveAccount(context.Background(), "a1"); err != nil {
			t.Fatalf("RemoveAccount returned error: %v", err)
		}
		if fake.lastRemoveAcctID != "a1" {
			t.Fatalf("id not forwarded: %q", fake.lastRemoveAcctID)
		}
	})
}

func TestCommandHandlerAdapter_TestAccount(t *testing.T) {
	t.Parallel()

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.TestAccount(context.Background(), &pb.TestAccountCommand{}); err == nil || !strings.Contains(err.Error(), "test_account: command server not wired") {
			t.Fatalf("TestAccount error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.TestAccount(context.Background(), &pb.TestAccountCommand{Id: "a1"}); err == nil || !strings.Contains(err.Error(), "test account: boom") {
			t.Fatalf("TestAccount error = %v, want test account: boom", err)
		}
	})

	t.Run("forwards the id and unwraps the response", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		resp, err := adapter.TestAccount(context.Background(), &pb.TestAccountCommand{Id: "a1"})
		if err != nil {
			t.Fatalf("TestAccount returned error: %v", err)
		}
		if resp == nil || resp.GetAccount().GetId() != "a1" || !resp.GetLiveSmokeRan() {
			t.Fatalf("unexpected response: %+v", resp)
		}
		if fake.lastTestAcctID != "a1" {
			t.Fatalf("id not forwarded: %q", fake.lastTestAcctID)
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

func TestCommandHandlerAdapter_CronJobs(t *testing.T) {
	t.Parallel()

	t.Run("create forwards every field including the run_setup_command pointer", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		runSetup := false
		resp, err := adapter.CreateCronJob(context.Background(), &pb.CreateCronJobCommand{
			RepoId:                "repo-1",
			Name:                  "nightly",
			Prompt:                "do the thing",
			Schedule:              "0 3 * * *",
			Timezone:              "UTC",
			IsEnabled:             true,
			AgentName:             "claude",
			Model:                 "opus",
			GateCommand:           "true",
			ShouldRunSetupCommand: &runSetup,
		})
		if err != nil {
			t.Fatalf("CreateCronJob returned error: %v", err)
		}
		if resp.GetCronJob().GetName() != "nightly" {
			t.Fatalf("response job name = %q, want nightly", resp.GetCronJob().GetName())
		}
		got := fake.lastCreateCron
		if got == nil {
			t.Fatal("no CreateCronJobRequest captured by the fake")
		}
		if got.GetRepoId() != "repo-1" || got.GetName() != "nightly" || got.GetPrompt() != "do the thing" ||
			got.GetSchedule() != "0 3 * * *" || got.GetTimezone() != "UTC" || !got.GetIsEnabled() ||
			got.GetAgentName() != "claude" || got.GetModel() != "opus" || got.GetGateCommand() != "true" {
			t.Fatalf("create fields not forwarded: %+v", got)
		}
		// The optional tri-state pointer must reach the daemon by reference, not
		// be flattened by a Get accessor.
		if got.ShouldRunSetupCommand == nil || got.GetShouldRunSetupCommand() != false {
			t.Fatalf("run_setup_command pointer not forwarded: %v", got.ShouldRunSetupCommand)
		}
	})

	t.Run("update forwards set pointers and leaves unset fields nil", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		name := "renamed"
		enabled := false
		resp, err := adapter.UpdateCronJob(context.Background(), &pb.UpdateCronJobCommand{
			Id:        "cj-1",
			Name:      &name,
			IsEnabled: &enabled,
		})
		if err != nil {
			t.Fatalf("UpdateCronJob returned error: %v", err)
		}
		if resp.GetCronJob().GetId() != "cj-1" {
			t.Fatalf("response job id = %q, want cj-1", resp.GetCronJob().GetId())
		}
		got := fake.lastUpdateCron
		if got == nil {
			t.Fatal("no UpdateCronJobRequest captured by the fake")
		}
		if got.GetId() != "cj-1" {
			t.Fatalf("id = %q, want cj-1", got.GetId())
		}
		if got.Name == nil || got.GetName() != "renamed" {
			t.Fatalf("name not forwarded: %v", got.Name)
		}
		if got.IsEnabled == nil || got.GetIsEnabled() != false {
			t.Fatalf("enabled not forwarded: %v", got.IsEnabled)
		}
		// A partial update must leave every untouched field nil so the daemon
		// preserves its stored value.
		if got.Prompt != nil || got.Schedule != nil || got.Timezone != nil ||
			got.AgentName != nil || got.Model != nil || got.GateCommand != nil || got.ShouldRunSetupCommand != nil {
			t.Fatalf("unset fields leaked non-nil: %+v", got)
		}
	})

	t.Run("delete forwards the id", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		if err := adapter.DeleteCronJob(context.Background(), "cj-9"); err != nil {
			t.Fatalf("DeleteCronJob returned error: %v", err)
		}
		if fake.lastDeleteCronID != "cj-9" {
			t.Fatalf("delete id = %q, want cj-9", fake.lastDeleteCronID)
		}
	})

	t.Run("run now forwards the id and returns the response", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		resp, err := adapter.RunCronJobNow(context.Background(), "cj-run")
		if err != nil {
			t.Fatalf("RunCronJobNow returned error: %v", err)
		}
		if resp.GetSkippedReason() != "skip" {
			t.Fatalf("skipped_reason = %q, want skip", resp.GetSkippedReason())
		}
		if fake.lastRunCronID != "cj-run" {
			t.Fatalf("run id = %q, want cj-run", fake.lastRunCronID)
		}
	})

	t.Run("list returns the response", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		resp, err := adapter.ListCronJobs(context.Background())
		if err != nil {
			t.Fatalf("ListCronJobs returned error: %v", err)
		}
		if len(resp.GetCronJobs()) != 1 || resp.GetCronJobs()[0].GetId() != "cj1" {
			t.Fatalf("unexpected list response: %+v", resp)
		}
	})

	t.Run("nil command server is rejected for every verb", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.ListCronJobs(context.Background()); err == nil || !strings.Contains(err.Error(), "list_cron_jobs: command server not wired") {
			t.Fatalf("ListCronJobs error = %v, want command server not wired", err)
		}
		if _, err := adapter.CreateCronJob(context.Background(), &pb.CreateCronJobCommand{}); err == nil || !strings.Contains(err.Error(), "create_cron_job: command server not wired") {
			t.Fatalf("CreateCronJob error = %v, want command server not wired", err)
		}
		if _, err := adapter.UpdateCronJob(context.Background(), &pb.UpdateCronJobCommand{}); err == nil || !strings.Contains(err.Error(), "update_cron_job: command server not wired") {
			t.Fatalf("UpdateCronJob error = %v, want command server not wired", err)
		}
		if err := adapter.DeleteCronJob(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "delete_cron_job: command server not wired") {
			t.Fatalf("DeleteCronJob error = %v, want command server not wired", err)
		}
		if _, err := adapter.RunCronJobNow(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "run_cron_job_now: command server not wired") {
			t.Fatalf("RunCronJobNow error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.UpdateCronJob(context.Background(), &pb.UpdateCronJobCommand{Id: "x"}); err == nil || !strings.Contains(err.Error(), "update cron job: boom") {
			t.Fatalf("UpdateCronJob error = %v, want update cron job: boom", err)
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
