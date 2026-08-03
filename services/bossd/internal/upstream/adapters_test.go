package upstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	bcast "github.com/recurser/bossalib/broadcast"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	bcastsvc "github.com/recurser/bossd/internal/broadcast"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeSessionCommandServer captures the last RecordChatRequest and returns a
// stub response. The other three methods return zero-value responses; they are
// unused by these tests but required to satisfy SessionCommandServer.
type fakeSessionCommandServer struct {
	lastRecordChat             *pb.RecordChatRequest
	lastTranscriptID           string
	lastSendReq                *pb.SendChatMessageRequest
	lastSwitchReq              *pb.SwitchSessionAccountRequest
	switchResp                 *pb.SwitchSessionAccountResponse
	lastCreateCron             *pb.CreateCronJobRequest
	lastUpdateCron             *pb.UpdateCronJobRequest
	lastDeleteCronID           string
	lastRunCronID              string
	lastCreateGithubCallback   *pb.CreateGithubCallbackRequest
	lastListGithubCallbacks    *pb.ListGithubCallbacksRequest
	lastDeleteGithubCallbackID string
	lastCreateNote             *pb.CreateNoteRequest
	lastGetNoteID              string
	lastListNotes              *pb.ListNotesRequest
	lastUpdateNote             *pb.UpdateNoteRequest
	lastDeleteNoteID           string
	lastListAccountsReq        *pb.ListAccountsRequest
	lastAddAccount             *pb.AddAccountRequest
	lastRefreshAcct            *pb.RefreshAccountRequest
	lastUpdateAcct             *pb.UpdateAccountRequest
	lastRemoveAcctID           string
	lastTestAcctID             string
	lastCloseID                string
	lastResurrectID            string
	lastRemoveSessionID        string
	lastEmptyTrashReq          *pb.EmptyTrashRequest
	lastRetryID                string
	lastUpdateReq              *pb.UpdateSessionRequest
	lastLinkReq                *pb.LinkSessionPRRequest
	lastUpdateChatTitle        *pb.UpdateChatTitleRequest
	lastReportChatStatus       *pb.ReportChatStatusRequest
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

func (f *fakeSessionCommandServer) ListAccounts(_ context.Context, req *connect.Request[pb.ListAccountsRequest]) (*connect.Response[pb.ListAccountsResponse], error) {
	f.lastListAccountsReq = req.Msg
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

func (f *fakeSessionCommandServer) CreateGithubCallback(_ context.Context, req *connect.Request[pb.CreateGithubCallbackRequest]) (*connect.Response[pb.CreateGithubCallbackResponse], error) {
	f.lastCreateGithubCallback = req.Msg
	return connect.NewResponse(&pb.CreateGithubCallbackResponse{}), nil
}

func (f *fakeSessionCommandServer) ListGithubCallbacks(_ context.Context, req *connect.Request[pb.ListGithubCallbacksRequest]) (*connect.Response[pb.ListGithubCallbacksResponse], error) {
	f.lastListGithubCallbacks = req.Msg
	return connect.NewResponse(&pb.ListGithubCallbacksResponse{}), nil
}

func (f *fakeSessionCommandServer) DeleteGithubCallback(_ context.Context, req *connect.Request[pb.DeleteGithubCallbackRequest]) (*connect.Response[pb.DeleteGithubCallbackResponse], error) {
	f.lastDeleteGithubCallbackID = req.Msg.GetId()
	return connect.NewResponse(&pb.DeleteGithubCallbackResponse{}), nil
}

func (f *fakeSessionCommandServer) CreateNote(_ context.Context, req *connect.Request[pb.CreateNoteRequest]) (*connect.Response[pb.CreateNoteResponse], error) {
	f.lastCreateNote = req.Msg
	return connect.NewResponse(&pb.CreateNoteResponse{Note: &pb.Note{Id: "note-new", Body: req.Msg.GetBody()}}), nil
}

func (f *fakeSessionCommandServer) GetNote(_ context.Context, req *connect.Request[pb.GetNoteRequest]) (*connect.Response[pb.GetNoteResponse], error) {
	f.lastGetNoteID = req.Msg.GetId()
	return connect.NewResponse(&pb.GetNoteResponse{Note: &pb.Note{Id: req.Msg.GetId()}}), nil
}

func (f *fakeSessionCommandServer) ListNotes(_ context.Context, req *connect.Request[pb.ListNotesRequest]) (*connect.Response[pb.ListNotesResponse], error) {
	f.lastListNotes = req.Msg
	return connect.NewResponse(&pb.ListNotesResponse{Notes: []*pb.Note{{Id: "note-1"}}}), nil
}

func (f *fakeSessionCommandServer) UpdateNote(_ context.Context, req *connect.Request[pb.UpdateNoteRequest]) (*connect.Response[pb.UpdateNoteResponse], error) {
	f.lastUpdateNote = req.Msg
	return connect.NewResponse(&pb.UpdateNoteResponse{Note: &pb.Note{Id: req.Msg.GetId()}}), nil
}

func (f *fakeSessionCommandServer) DeleteNote(_ context.Context, req *connect.Request[pb.DeleteNoteRequest]) (*connect.Response[pb.DeleteNoteResponse], error) {
	f.lastDeleteNoteID = req.Msg.GetId()
	return connect.NewResponse(&pb.DeleteNoteResponse{}), nil
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

func (e *errCommandServer) CreateGithubCallback(context.Context, *connect.Request[pb.CreateGithubCallbackRequest]) (*connect.Response[pb.CreateGithubCallbackResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ListGithubCallbacks(context.Context, *connect.Request[pb.ListGithubCallbacksRequest]) (*connect.Response[pb.ListGithubCallbacksResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) DeleteGithubCallback(context.Context, *connect.Request[pb.DeleteGithubCallbackRequest]) (*connect.Response[pb.DeleteGithubCallbackResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) CreateNote(context.Context, *connect.Request[pb.CreateNoteRequest]) (*connect.Response[pb.CreateNoteResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) GetNote(context.Context, *connect.Request[pb.GetNoteRequest]) (*connect.Response[pb.GetNoteResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) ListNotes(context.Context, *connect.Request[pb.ListNotesRequest]) (*connect.Response[pb.ListNotesResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) UpdateNote(context.Context, *connect.Request[pb.UpdateNoteRequest]) (*connect.Response[pb.UpdateNoteResponse], error) {
	return nil, e.err
}

func (e *errCommandServer) DeleteNote(context.Context, *connect.Request[pb.DeleteNoteRequest]) (*connect.Response[pb.DeleteNoteResponse], error) {
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
		DeferPr:          true,
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
	// defer_pr must survive the reverse-stream Command→Request rebuild, else a
	// hosted defer_pr:true create silently re-enables the eager up-front draft PR.
	if !req.GetDeferPr() {
		t.Errorf("DeferPr: got false, want true")
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
		if _, err := adapter.ListAccounts(context.Background(), "", false); err == nil || !strings.Contains(err.Error(), "list_accounts: command server not wired") {
			t.Fatalf("ListAccounts error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.ListAccounts(context.Background(), "claude", false); err == nil || !strings.Contains(err.Error(), "list accounts: boom") {
			t.Fatalf("ListAccounts error = %v, want list accounts: boom", err)
		}
	})

	t.Run("returns the response on success", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		resp, err := adapter.ListAccounts(context.Background(), "", false)
		if err != nil {
			t.Fatalf("ListAccounts returned error: %v", err)
		}
		if resp == nil {
			t.Fatal("ListAccounts returned nil response on success")
		}
	})

	// BOS-655: refresh=false must leave the wire-level Refresh pointer nil (the
	// byte-for-byte pre-change request); refresh=true must set it non-nil true.
	t.Run("refresh false leaves the request field unset", func(t *testing.T) {
		t.Parallel()
		server := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: server}
		if _, err := adapter.ListAccounts(context.Background(), "claude", false); err != nil {
			t.Fatalf("ListAccounts returned error: %v", err)
		}
		if server.lastListAccountsReq.GetRefresh() {
			t.Fatalf("forwarded refresh = true, want false")
		}
		if server.lastListAccountsReq.Refresh != nil {
			t.Fatalf("forwarded Refresh pointer = %v, want nil (unset)", server.lastListAccountsReq.Refresh)
		}
	})

	t.Run("refresh true sets the request field", func(t *testing.T) {
		t.Parallel()
		server := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: server}
		if _, err := adapter.ListAccounts(context.Background(), "claude", true); err != nil {
			t.Fatalf("ListAccounts returned error: %v", err)
		}
		if server.lastListAccountsReq.Refresh == nil || !server.lastListAccountsReq.GetRefresh() {
			t.Fatalf("forwarded Refresh = %v, want non-nil true", server.lastListAccountsReq.Refresh)
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

// TestCommandHandlerAdapter_Notes pins the BOS-552 stream-command → daemon-request
// translation for the five notes verbs: every field (including the optional
// provenance pointers, the tag-set wrapper, and the list limit) must reach the
// daemon unchanged, and an unwired command server must be rejected rather than
// panicking.
func TestCommandHandlerAdapter_Notes(t *testing.T) {
	t.Parallel()

	t.Run("create forwards every field including the optional provenance pointers", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		sessionID, chatID := "sess-1", "chat-1"
		resp, err := adapter.CreateNote(context.Background(), &pb.CreateNoteCommand{
			RepoId:    "repo-1",
			SessionId: &sessionID,
			ChatId:    &chatID,
			Body:      "remember this",
			Tags:      []string{"alpha", "beta"},
		})
		if err != nil {
			t.Fatalf("CreateNote returned error: %v", err)
		}
		if resp.GetNote().GetBody() != "remember this" {
			t.Fatalf("response note body = %q, want remember this", resp.GetNote().GetBody())
		}
		got := fake.lastCreateNote
		if got == nil {
			t.Fatal("no CreateNoteRequest captured by the fake")
		}
		if got.GetRepoId() != "repo-1" || got.GetBody() != "remember this" ||
			len(got.GetTags()) != 2 || got.GetTags()[1] != "beta" {
			t.Fatalf("create fields not forwarded: %+v", got)
		}
		// The optional provenance fields must arrive by reference so their
		// unset state survives the hop.
		if got.SessionId == nil || got.GetSessionId() != "sess-1" || got.ChatId == nil || got.GetChatId() != "chat-1" {
			t.Fatalf("provenance pointers not forwarded: session=%v chat=%v", got.SessionId, got.ChatId)
		}
	})

	t.Run("create leaves unset provenance pointers nil", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		if _, err := adapter.CreateNote(context.Background(), &pb.CreateNoteCommand{RepoId: "repo-1", Body: "x"}); err != nil {
			t.Fatalf("CreateNote returned error: %v", err)
		}
		if fake.lastCreateNote.SessionId != nil || fake.lastCreateNote.ChatId != nil {
			t.Fatalf("unset provenance leaked non-nil: %+v", fake.lastCreateNote)
		}
	})

	t.Run("get forwards the id", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		resp, err := adapter.GetNote(context.Background(), &pb.GetNoteCommand{Id: "note-7"})
		if err != nil {
			t.Fatalf("GetNote returned error: %v", err)
		}
		if resp.GetNote().GetId() != "note-7" {
			t.Fatalf("response note id = %q, want note-7", resp.GetNote().GetId())
		}
		if fake.lastGetNoteID != "note-7" {
			t.Fatalf("get id = %q, want note-7", fake.lastGetNoteID)
		}
	})

	t.Run("list forwards every filter including the limit", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		repoID, sessionID, chatID, search := "repo-1", "sess-1", "chat-1", "needle"
		resp, err := adapter.ListNotes(context.Background(), &pb.ListNotesCommand{
			RepoId:    &repoID,
			SessionId: &sessionID,
			ChatId:    &chatID,
			Tags:      []string{"alpha"},
			Search:    &search,
			Limit:     11,
		})
		if err != nil {
			t.Fatalf("ListNotes returned error: %v", err)
		}
		if len(resp.GetNotes()) != 1 || resp.GetNotes()[0].GetId() != "note-1" {
			t.Fatalf("unexpected list response: %+v", resp)
		}
		got := fake.lastListNotes
		if got == nil {
			t.Fatal("no ListNotesRequest captured by the fake")
		}
		// A dropped limit silently unbounds a fleet-wide list, so it is
		// asserted alongside the provenance/tag/search filters.
		if got.RepoId == nil || got.GetRepoId() != "repo-1" || got.SessionId == nil || got.GetSessionId() != "sess-1" ||
			got.ChatId == nil || got.GetChatId() != "chat-1" || got.Search == nil || got.GetSearch() != "needle" ||
			got.GetLimit() != 11 || len(got.GetTags()) != 1 || got.GetTags()[0] != "alpha" {
			t.Fatalf("list filters not forwarded: %+v", got)
		}
	})

	t.Run("list leaves unset filters nil", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		if _, err := adapter.ListNotes(context.Background(), &pb.ListNotesCommand{}); err != nil {
			t.Fatalf("ListNotes returned error: %v", err)
		}
		got := fake.lastListNotes
		if got.RepoId != nil || got.SessionId != nil || got.ChatId != nil || got.Search != nil || got.GetLimit() != 0 {
			t.Fatalf("unset filters leaked non-nil: %+v", got)
		}
	})

	t.Run("update forwards the body pointer and the tag-set wrapper", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		body := "edited"
		resp, err := adapter.UpdateNote(context.Background(), &pb.UpdateNoteCommand{
			Id:   "note-9",
			Body: &body,
			Tags: &pb.NoteTagSet{Tags: []string{"kept"}},
		})
		if err != nil {
			t.Fatalf("UpdateNote returned error: %v", err)
		}
		if resp.GetNote().GetId() != "note-9" {
			t.Fatalf("response note id = %q, want note-9", resp.GetNote().GetId())
		}
		got := fake.lastUpdateNote
		if got == nil || got.GetId() != "note-9" || got.Body == nil || got.GetBody() != "edited" {
			t.Fatalf("update fields not forwarded: %+v", got)
		}
		if got.Tags == nil || len(got.GetTags().GetTags()) != 1 || got.GetTags().GetTags()[0] != "kept" {
			t.Fatalf("tag set not forwarded: %+v", got.Tags)
		}
	})

	t.Run("update preserves a set-but-empty tag set as a clear", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		if _, err := adapter.UpdateNote(context.Background(), &pb.UpdateNoteCommand{
			Id:   "note-10",
			Tags: &pb.NoteTagSet{},
		}); err != nil {
			t.Fatalf("UpdateNote returned error: %v", err)
		}
		got := fake.lastUpdateNote
		// A set-but-empty wrapper means "clear the tags"; flattening it to nil
		// would silently turn a clear into a no-op.
		if got.Tags == nil {
			t.Fatal("set-but-empty tag set flattened to nil: the clear would be lost")
		}
		if got.Body != nil {
			t.Fatalf("unset body leaked non-nil: %v", got.Body)
		}
	})

	t.Run("delete forwards the id", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		if err := adapter.DeleteNote(context.Background(), "note-11"); err != nil {
			t.Fatalf("DeleteNote returned error: %v", err)
		}
		if fake.lastDeleteNoteID != "note-11" {
			t.Fatalf("delete id = %q, want note-11", fake.lastDeleteNoteID)
		}
	})

	t.Run("nil command server is rejected for every verb", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.CreateNote(context.Background(), &pb.CreateNoteCommand{}); err == nil || !strings.Contains(err.Error(), "create_note: command server not wired") {
			t.Fatalf("CreateNote error = %v, want command server not wired", err)
		}
		if _, err := adapter.GetNote(context.Background(), &pb.GetNoteCommand{}); err == nil || !strings.Contains(err.Error(), "get_note: command server not wired") {
			t.Fatalf("GetNote error = %v, want command server not wired", err)
		}
		if _, err := adapter.ListNotes(context.Background(), &pb.ListNotesCommand{}); err == nil || !strings.Contains(err.Error(), "list_notes: command server not wired") {
			t.Fatalf("ListNotes error = %v, want command server not wired", err)
		}
		if _, err := adapter.UpdateNote(context.Background(), &pb.UpdateNoteCommand{}); err == nil || !strings.Contains(err.Error(), "update_note: command server not wired") {
			t.Fatalf("UpdateNote error = %v, want command server not wired", err)
		}
		if err := adapter.DeleteNote(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "delete_note: command server not wired") {
			t.Fatalf("DeleteNote error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped and keeps its connect code", func(t *testing.T) {
		t.Parallel()
		notFound := connect.NewError(connect.CodeNotFound, errors.New("no such note"))
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: notFound}}
		_, err := adapter.GetNote(context.Background(), &pb.GetNoteCommand{Id: "gone"})
		if err == nil || !strings.Contains(err.Error(), "get note: ") || !strings.Contains(err.Error(), "no such note") {
			t.Fatalf("GetNote error = %v, want it wrapped as get note: … no such note", err)
		}
		// The dispatcher classifies on the connect code, so wrapping must not
		// erase it.
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("connect code = %v, want NotFound", connect.CodeOf(err))
		}
	})
}

// TestCommandHandlerAdapter_CronJobZeroOutputPassThrough pins the BOS-563
// is_zero_output tri-state across the stream-command → daemon-request hop. All
// three states are asserted (nil stays nil, &true arrives true, &false arrives
// false) so a hardcoded value on either side cannot pass.
func TestCommandHandlerAdapter_CronJobZeroOutputPassThrough(t *testing.T) {
	t.Parallel()

	tr, fa := true, false
	cases := []struct {
		name string
		in   *bool
	}{
		{"unset stays nil", nil},
		{"explicit true", &tr},
		{"explicit false", &fa},
	}

	for _, tc := range cases {
		t.Run("create "+tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeSessionCommandServer{}
			adapter := &CommandHandlerAdapter{Commands: fake}
			if _, err := adapter.CreateCronJob(context.Background(), &pb.CreateCronJobCommand{
				RepoId:       "repo-1",
				Name:         "nightly",
				IsZeroOutput: tc.in,
			}); err != nil {
				t.Fatalf("CreateCronJob returned error: %v", err)
			}
			got := fake.lastCreateCron
			if got == nil {
				t.Fatal("no CreateCronJobRequest captured by the fake")
			}
			assertZeroOutputPointer(t, tc.in, got.IsZeroOutput)
		})

		t.Run("update "+tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeSessionCommandServer{}
			adapter := &CommandHandlerAdapter{Commands: fake}
			if _, err := adapter.UpdateCronJob(context.Background(), &pb.UpdateCronJobCommand{
				Id:           "cj-1",
				IsZeroOutput: tc.in,
			}); err != nil {
				t.Fatalf("UpdateCronJob returned error: %v", err)
			}
			got := fake.lastUpdateCron
			if got == nil {
				t.Fatal("no UpdateCronJobRequest captured by the fake")
			}
			assertZeroOutputPointer(t, tc.in, got.IsZeroOutput)
		})
	}
}

// assertZeroOutputPointer compares an is_zero_output pointer against the value
// the caller supplied, treating nil as a distinct state from a pointer to false.
func assertZeroOutputPointer(t *testing.T, want, got *bool) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("is_zero_output = &%v, want nil (unset must stay unset)", *got)
	case want != nil && got == nil:
		t.Fatalf("is_zero_output = nil, want &%v", *want)
	case want != nil && *got != *want:
		t.Fatalf("is_zero_output = %v, want %v", *got, *want)
	}
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

// fakeBroadcastReceiver captures the domain value the adapter translated the
// wire command into, and can script a failure.
type fakeBroadcastReceiver struct {
	last  bcastsvc.InboundBroadcast
	calls int
	err   error
}

func (f *fakeBroadcastReceiver) Receive(_ context.Context, in bcastsvc.InboundBroadcast) error {
	f.calls++
	f.last = in
	return f.err
}

// TestCommandHandlerAdapter_DeliverBroadcast covers the wire->domain seam of
// the cross-daemon broadcast INGRESS (BOS-558). The adapter is a translator and
// nothing more: every decision about an inbound broadcast (loop guard,
// idempotency, local-only resolution) lives behind the receiver.
func TestCommandHandlerAdapter_DeliverBroadcast(t *testing.T) {
	t.Parallel()

	t.Run("translates the command into the domain value", func(t *testing.T) {
		t.Parallel()
		expires := time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)
		recv := &fakeBroadcastReceiver{}
		adapter := &CommandHandlerAdapter{Broadcasts: recv}
		err := adapter.DeliverBroadcast(context.Background(), &pb.BroadcastCommand{
			BroadcastId: "  b-1  ",
			Selector: &pb.BroadcastSelector{Clauses: []*pb.BroadcastSelectorClause{
				{RepoIds: []string{"repo-1"}, DaemonIds: []string{"daemon-here"}},
			}},
			OriginDaemonId: "  daemon-far  ",
			OriginChatId:   "  chat-far  ",
			Message:        "secret body",
			ExpiresAt:      timestamppb.New(expires),
		})
		if err != nil {
			t.Fatalf("DeliverBroadcast returned error: %v", err)
		}
		if recv.calls != 1 {
			t.Fatalf("receiver calls = %d, want 1", recv.calls)
		}
		got := recv.last
		// Ids are trimmed: the loop guard compares origin_daemon_id against the
		// local id, and a stray space would defeat it.
		if got.ID != "b-1" || got.OriginDaemonID != "daemon-far" || got.OriginChatID != "chat-far" {
			t.Fatalf("ids not passed through trimmed: %+v", got)
		}
		if got.Message != "secret body" {
			t.Fatalf("message = %q, want it carried verbatim", got.Message)
		}
		// The ORIGIN's absolute expiry, honoured verbatim: a slow route must not
		// extend a broadcast's lifetime past what the sender asked for.
		if !got.ExpiresAt.Equal(expires) {
			t.Fatalf("expires_at = %v, want %v", got.ExpiresAt, expires)
		}
		if len(got.Selector.Clauses) != 1 ||
			len(got.Selector.Clauses[0].RepoIDs) != 1 || got.Selector.Clauses[0].RepoIDs[0] != "repo-1" ||
			len(got.Selector.Clauses[0].DaemonIDs) != 1 || got.Selector.Clauses[0].DaemonIDs[0] != "daemon-here" {
			t.Fatalf("selector not decoded: %+v", got.Selector)
		}
	})

	t.Run("an absent expiry decodes to the zero time, never the epoch", func(t *testing.T) {
		t.Parallel()
		// The RULE — an absent expiry is a rejection, never a default — lives in
		// the ingress, which owns it for every caller of the exported
		// BroadcastReceiver, not just this transport. What the adapter owes it is
		// a faithful decode: timestamppb's nil AsTime() is the 1970 epoch, which
		// the ingress cannot distinguish from "the caller sent an expiry", and
		// which would make every delivery instantly overdue with no error
		// anywhere. So the adapter normalises absent/zero to the ZERO time.Time
		// the ingress rejects.
		for _, tc := range []struct {
			name string
			ts   *timestamppb.Timestamp
		}{
			{"nil", nil},
			{"epoch zero", &timestamppb.Timestamp{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				recv := &fakeBroadcastReceiver{}
				adapter := &CommandHandlerAdapter{Broadcasts: recv}
				err := adapter.DeliverBroadcast(context.Background(), &pb.BroadcastCommand{
					BroadcastId: "b-2",
					Message:     "secret body",
					ExpiresAt:   tc.ts,
				})
				if err != nil {
					t.Fatalf("DeliverBroadcast: %v", err)
				}
				if !recv.last.ExpiresAt.IsZero() {
					t.Fatalf("expires_at = %v, want the zero time (never the 1970 epoch)", recv.last.ExpiresAt)
				}
			})
		}
	})

	t.Run("a permanent failure is typed so the router can stop retrying", func(t *testing.T) {
		t.Parallel()
		// The stream is at-least-once, so an error that reads as "try again" WILL
		// come back. A malformed command and an over-cap selector are both
		// deterministic in the command itself, so they must classify as
		// InvalidArgument; anything else stays untyped and therefore retryable.
		for _, tc := range []struct {
			name     string
			err      error
			wantCode connect.Code
		}{
			{"invalid inbound", fmt.Errorf("%w: broadcast b-3: message is required", bcastsvc.ErrInvalidInbound), connect.CodeInvalidArgument},
			{"over the fan-out cap", fmt.Errorf("materialise inbound broadcast b-3: %w", bcastsvc.ErrTooManyTargets), connect.CodeInvalidArgument},
			{"a transient write failure stays retryable", errors.New("database is locked"), connect.CodeUnknown},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				adapter := &CommandHandlerAdapter{Broadcasts: &fakeBroadcastReceiver{err: tc.err}}
				err := adapter.DeliverBroadcast(context.Background(), &pb.BroadcastCommand{
					BroadcastId: "b-3",
					Message:     "secret body",
					ExpiresAt:   timestamppb.New(time.Now().Add(time.Hour)),
				})
				if err == nil {
					t.Fatal("expected an error")
				}
				if got := connect.CodeOf(err); got != tc.wantCode {
					t.Fatalf("connect code = %v, want %v (err = %v)", got, tc.wantCode, err)
				}
				if strings.Contains(err.Error(), "secret body") {
					t.Fatalf("the broadcast body leaked into the error: %v", err)
				}
			})
		}
	})

	t.Run("a nil receiver names the missing wiring", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		err := adapter.DeliverBroadcast(context.Background(), &pb.BroadcastCommand{
			BroadcastId: "b-3",
			Message:     "secret body",
			ExpiresAt:   timestamppb.New(time.Now()),
		})
		if err == nil || !strings.Contains(err.Error(), "broadcast: ingress not wired") {
			t.Fatalf("error = %v, want broadcast: ingress not wired", err)
		}
	})

	t.Run("a receiver failure propagates", func(t *testing.T) {
		t.Parallel()
		recv := &fakeBroadcastReceiver{err: errors.New("materialise inbound broadcast b-4: store is down")}
		adapter := &CommandHandlerAdapter{Broadcasts: recv}
		err := adapter.DeliverBroadcast(context.Background(), &pb.BroadcastCommand{
			BroadcastId: "b-4",
			Message:     "secret body",
			ExpiresAt:   timestamppb.New(time.Now()),
		})
		if err == nil || !strings.Contains(err.Error(), "store is down") {
			t.Fatalf("error = %v, want the receiver's own error", err)
		}
	})
}

// TestBroadcastEgressPublisher pins the EGRESS half's production adapter: the
// send path's domain event becomes exactly one pb.BroadcastEgress on the
// StreamBus, and the publish reports success because the bus cannot fail.
func TestBroadcastEgressPublisher(t *testing.T) {
	t.Parallel()

	t.Run("publishes one egress event onto the bus", func(t *testing.T) {
		t.Parallel()
		expires := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
		bus := NewStreamBus(zerolog.Nop())
		defer bus.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		events := bus.Subscribe(ctx)

		pub := NewBroadcastEgressPublisher(bus)
		if err := pub.PublishBroadcastEgress(context.Background(), bcastsvc.EgressEvent{
			BroadcastID: "b-1",
			Selector: bcast.Selector{Clauses: []bcast.Clause{
				{RepoIDs: []string{"repo-1"}},
			}},
			OriginDaemonID: "daemon-here",
			OriginChatID:   "chat-here",
			Message:        "secret body",
			ExpiresAt:      expires,
		}); err != nil {
			t.Fatalf("PublishBroadcastEgress returned error: %v", err)
		}

		var ev StreamEvent
		select {
		case ev = <-events:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for the egress event")
		}
		if ev.EgressBroadcast == nil || ev.EgressBroadcast.Egress == nil {
			t.Fatalf("expected an EgressBroadcast envelope, got %+v", ev)
		}
		eg := ev.EgressBroadcast.Egress
		if eg.GetBroadcastId() != "b-1" || eg.GetOriginDaemonId() != "daemon-here" || eg.GetOriginChatId() != "chat-here" {
			t.Fatalf("ids not carried: %+v", eg)
		}
		if eg.GetMessage() != "secret body" {
			t.Fatalf("message = %q, want it carried verbatim", eg.GetMessage())
		}
		if !eg.GetExpiresAt().AsTime().Equal(expires) {
			t.Fatalf("expires_at = %v, want %v", eg.GetExpiresAt().AsTime(), expires)
		}
		cl := eg.GetSelector().GetClauses()
		if len(cl) != 1 || len(cl[0].GetRepoIds()) != 1 || cl[0].GetRepoIds()[0] != "repo-1" {
			t.Fatalf("selector not encoded: %+v", eg.GetSelector())
		}
		// Exactly one event: a second would be a duplicate delivery fleet-wide.
		select {
		case extra := <-events:
			t.Fatalf("a second event was published: %+v", extra)
		case <-time.After(50 * time.Millisecond):
		}
	})

	t.Run("an unwired bus is an error, not a silent drop", func(t *testing.T) {
		t.Parallel()
		pub := NewBroadcastEgressPublisher(nil)
		err := pub.PublishBroadcastEgress(context.Background(), bcastsvc.EgressEvent{BroadcastID: "b-2", Message: "secret body"})
		if err == nil || !strings.Contains(err.Error(), "stream bus not wired") {
			t.Fatalf("error = %v, want stream bus not wired", err)
		}
		if strings.Contains(err.Error(), "secret body") {
			t.Fatalf("the broadcast body leaked into the error: %v", err)
		}
	})
}
