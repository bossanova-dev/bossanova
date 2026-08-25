package upstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeCommandHandler struct {
	stopCalls               atomic.Int32
	pauseCalls              atomic.Int32
	resumeCalls             atomic.Int32
	wakeCalls               atomic.Int32
	mergeCalls              atomic.Int32
	archiveCalls            atomic.Int32
	retryCalls              atomic.Int32
	updateSessionCalls      atomic.Int32
	linkPRCalls             atomic.Int32
	recordChatCalls         atomic.Int32
	deleteChatCalls         atomic.Int32
	deleteChatScope         string // last sessionID passed to DeleteChat
	updateChatTitleCalls    atomic.Int32
	reportChatStatusCalls   atomic.Int32
	listReposCalls          atomic.Int32
	listAgentsCalls         atomic.Int32
	listAccountsCalls       atomic.Int32
	returnErr               error
	session                 *pb.Session
	mergeSession            *pb.Session
	archiveSession          *pb.Session
	retrySession            *pb.Session
	retrySessionID          string // last sessionID passed to RetrySession
	updateSession           *pb.Session
	updateSessionCmd        *pb.UpdateSessionCommand // last command passed to UpdateSession
	linkSession             *pb.Session
	linkSessionID           string                 // last sessionID passed to LinkSessionPR
	linkPR                  string                 // last pr passed to LinkSessionPR
	updateChatTitleAgentID  string                 // last agentSessionID passed to UpdateChatTitle
	updateChatTitleValue    string                 // last title passed to UpdateChatTitle
	reportChatStatusReports []*pb.ChatStatusReport // last reports passed to ReportChatStatus
	recordChatResult        *pb.ClaudeChat
	listReposResult         *pb.ListReposResponse
	listAgentsResult        *pb.ListAgentsResponse
	listAccountsResult      *pb.ListAccountsResponse
	listAccountsProvider    string // last provider passed to ListAccounts
	listAccountsRefresh     bool   // last refresh passed to ListAccounts
	listAccountsBlock       bool
	repoSettings            *pb.GetRepoSettingsResponse
	updatedRepo             *pb.UpdateRepoResponse
	removedRepoID           string
	updatedRepoRequest      *pb.UpdateRepoCommand
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
	// SwitchAccount-specific knobs.
	switchCalls       atomic.Int32
	switchResumed     bool
	switchTargetLabel string
	switchNoticeText  string
	switchErrorCode   pb.CommandResult_ErrorCode
	switchErr         error
	switchSessionID   string // last sessionID passed to SwitchAccount
	switchAgentID     string // last agentSessionID passed to SwitchAccount
	switchAccountID   string // last accountID passed to SwitchAccount
	switchForce       bool   // last force flag passed to SwitchAccount
	// switchBlock, when non-nil, makes SwitchAccount block until the channel
	// is closed. Used to prove a slow switch no longer wedges the command
	// reader (BOS-897).
	switchBlock chan struct{}
	// GetChatTranscript / SendChatMessage knobs.
	transcript        *pb.GetChatTranscriptResponse
	transcriptSession string // last sessionID passed to GetChatTranscript
	sendResult        *pb.SendChatMessageResponse
	sendAgentID       string // last agentSessionID passed to SendChatMessage
	sendMessage       string // last message passed to SendChatMessage
	sendSubmit        bool   // last submit flag passed to SendChatMessage
	// Cron knobs.
	listCronResult   *pb.ListCronJobsResponse
	createCronResult *pb.CreateCronJobResponse
	createCronCmd    *pb.CreateCronJobCommand // last command passed to CreateCronJob
	updateCronResult *pb.UpdateCronJobResponse
	updateCronCmd    *pb.UpdateCronJobCommand // last command passed to UpdateCronJob
	runCronResult    *pb.RunCronJobNowResponse
	deleteCronID     string // last id passed to DeleteCronJob
	runCronID        string // last id passed to RunCronJobNow
	// GitHub-callback knobs.
	createGithubCallbackResult *pb.CreateGithubCallbackResponse
	createGithubCallbackCmd    *pb.CreateGithubCallbackCommand // last command passed to CreateGithubCallback
	listGithubCallbacksResult  *pb.ListGithubCallbacksResponse
	listGithubCallbacksCmd     *pb.ListGithubCallbacksCommand // last command passed to ListGithubCallbacks
	deleteGithubCallbackID     string                         // last id passed to DeleteGithubCallback
	// Notes knobs (BOS-552).
	createNoteResult *pb.CreateNoteResponse
	createNoteCmd    *pb.CreateNoteCommand // last command passed to CreateNote
	getNoteResult    *pb.GetNoteResponse
	getNoteCmd       *pb.GetNoteCommand // last command passed to GetNote
	listNotesResult  *pb.ListNotesResponse
	listNotesCmd     *pb.ListNotesCommand // last command passed to ListNotes
	updateNoteResult *pb.UpdateNoteResponse
	updateNoteCmd    *pb.UpdateNoteCommand // last command passed to UpdateNote
	deleteNoteID     string                // last id passed to DeleteNote
	// Account-management knobs.
	addAccountResult     *pb.AddAccountResponse
	addAccountCmd        *pb.AddAccountCommand // last command passed to AddAccount
	refreshAccountResult *pb.RefreshAccountResponse
	refreshAccountCmd    *pb.RefreshAccountCommand // last command passed to RefreshAccount
	updateAccountResult  *pb.UpdateAccountResponse
	updateAccountCmd     *pb.UpdateAccountCommand // last command passed to UpdateAccount
	testAccountResult    *pb.TestAccountResponse
	testAccountID        string // last id passed to TestAccount
	removeAccountID      string // last id passed to RemoveAccount
	// Read-tier parity knobs (BOS-401).
	listChatsResult          *pb.ListChatsResponse
	listChatsSession         string // last sessionID passed to ListChats
	chatStatusesResult       *pb.GetChatStatusesResponse
	chatStatusesSession      string // last sessionID passed to GetChatStatuses
	sessionStatusesResult    *pb.GetSessionStatusesResponse
	sessionStatusesIDs       []string // last sessionIDs passed to GetSessionStatuses
	listCheckSnapshotsResult *pb.ListCheckSnapshotsResponse
	checkSnapshotsSession    string // last sessionID passed to ListCheckSnapshots
	checkSnapshotsLimit      int32  // last limit passed to ListCheckSnapshots
	listPluginsResult        *pb.ListPluginsResponse
	getCronJobResult         *pb.GetCronJobResponse
	getCronJobID             string // last id passed to GetCronJob
	repairDoctorResult       *pb.RepairDoctorResponse

	// Cross-daemon broadcast ingress knob (BOS-558). The dispatcher forwards
	// the command verbatim; every decision about it lives behind the handler.
	broadcastCmd *pb.BroadcastCommand // last command passed to DeliverBroadcast

	closeSession        *pb.Session
	closeSessionID      string // last sessionID passed to CloseSession
	resurrectSession    *pb.Session
	resurrectSessionID  string // last sessionID passed to ResurrectSession
	removeSessionID     string // last sessionID passed to RemoveSession
	emptyTrashOlderThan *timestamppb.Timestamp
	emptyTrashCount     int32
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
func (f *fakeCommandHandler) SwitchAccount(ctx context.Context, sessionID, agentSessionID, accountID string, force bool) (bool, string, string, pb.CommandResult_ErrorCode, error) {
	if f.switchBlock != nil {
		select {
		case <-f.switchBlock:
		case <-ctx.Done():
			return false, "", "", pb.CommandResult_ERROR_CODE_CANCELED, ctx.Err()
		}
	}
	f.switchCalls.Add(1)
	f.switchSessionID = sessionID
	f.switchAgentID = agentSessionID
	f.switchAccountID = accountID
	f.switchForce = force
	return f.switchResumed, f.switchTargetLabel, f.switchNoticeText, f.switchErrorCode, f.switchErr
}
func (f *fakeCommandHandler) MergeSession(_ context.Context, _ string) (*pb.Session, error) {
	f.mergeCalls.Add(1)
	return f.mergeSession, f.returnErr
}
func (f *fakeCommandHandler) ArchiveSession(_ context.Context, _ string) (*pb.Session, error) {
	f.archiveCalls.Add(1)
	return f.archiveSession, f.returnErr
}
func (f *fakeCommandHandler) RetrySession(_ context.Context, sessionID string) (*pb.Session, error) {
	f.retryCalls.Add(1)
	f.retrySessionID = sessionID
	return f.retrySession, f.returnErr
}
func (f *fakeCommandHandler) UpdateSession(_ context.Context, req *pb.UpdateSessionCommand) (*pb.Session, error) {
	f.updateSessionCalls.Add(1)
	f.updateSessionCmd = req
	return f.updateSession, f.returnErr
}
func (f *fakeCommandHandler) LinkSessionPR(_ context.Context, sessionID, pr string) (*pb.Session, error) {
	f.linkPRCalls.Add(1)
	f.linkSessionID = sessionID
	f.linkPR = pr
	return f.linkSession, f.returnErr
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
func (f *fakeCommandHandler) UpdateChatTitle(_ context.Context, agentSessionID, title string) error {
	f.updateChatTitleCalls.Add(1)
	f.updateChatTitleAgentID = agentSessionID
	f.updateChatTitleValue = title
	return f.returnErr
}
func (f *fakeCommandHandler) ReportChatStatus(_ context.Context, reports []*pb.ChatStatusReport) error {
	f.reportChatStatusCalls.Add(1)
	f.reportChatStatusReports = reports
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
func (f *fakeCommandHandler) ListAccounts(ctx context.Context, provider string, refresh bool) (*pb.ListAccountsResponse, error) {
	f.listAccountsCalls.Add(1)
	f.listAccountsProvider = provider
	f.listAccountsRefresh = refresh
	if f.listAccountsBlock {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.listAccountsResult, f.returnErr
}
func (f *fakeCommandHandler) GetRepo(_ context.Context, _ string) (*pb.GetRepoSettingsResponse, error) {
	return f.repoSettings, f.returnErr
}
func (f *fakeCommandHandler) UpdateRepo(_ context.Context, req *pb.UpdateRepoCommand) (*pb.UpdateRepoResponse, error) {
	f.updatedRepoRequest = req
	return f.updatedRepo, f.returnErr
}
func (f *fakeCommandHandler) RemoveRepo(_ context.Context, repoID string) error {
	f.removedRepoID = repoID
	return f.returnErr
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
func (f *fakeCommandHandler) SendChatMessage(_ context.Context, agentSessionID, message string, _, submit bool) (*pb.SendChatMessageResponse, error) {
	f.sendAgentID = agentSessionID
	f.sendMessage = message
	f.sendSubmit = submit
	return f.sendResult, f.returnErr
}

func (f *fakeCommandHandler) ListCronJobs(_ context.Context) (*pb.ListCronJobsResponse, error) {
	return f.listCronResult, f.returnErr
}
func (f *fakeCommandHandler) CreateCronJob(_ context.Context, cmd *pb.CreateCronJobCommand) (*pb.CreateCronJobResponse, error) {
	f.createCronCmd = cmd
	return f.createCronResult, f.returnErr
}
func (f *fakeCommandHandler) UpdateCronJob(_ context.Context, cmd *pb.UpdateCronJobCommand) (*pb.UpdateCronJobResponse, error) {
	f.updateCronCmd = cmd
	return f.updateCronResult, f.returnErr
}
func (f *fakeCommandHandler) DeleteCronJob(_ context.Context, id string) error {
	f.deleteCronID = id
	return f.returnErr
}
func (f *fakeCommandHandler) RunCronJobNow(_ context.Context, id string) (*pb.RunCronJobNowResponse, error) {
	f.runCronID = id
	return f.runCronResult, f.returnErr
}
func (f *fakeCommandHandler) CreateGithubCallback(_ context.Context, cmd *pb.CreateGithubCallbackCommand) (*pb.CreateGithubCallbackResponse, error) {
	f.createGithubCallbackCmd = cmd
	return f.createGithubCallbackResult, f.returnErr
}
func (f *fakeCommandHandler) ListGithubCallbacks(_ context.Context, cmd *pb.ListGithubCallbacksCommand) (*pb.ListGithubCallbacksResponse, error) {
	f.listGithubCallbacksCmd = cmd
	return f.listGithubCallbacksResult, f.returnErr
}
func (f *fakeCommandHandler) DeleteGithubCallback(_ context.Context, id string) error {
	f.deleteGithubCallbackID = id
	return f.returnErr
}
func (f *fakeCommandHandler) CreateNote(_ context.Context, cmd *pb.CreateNoteCommand) (*pb.CreateNoteResponse, error) {
	f.createNoteCmd = cmd
	return f.createNoteResult, f.returnErr
}
func (f *fakeCommandHandler) GetNote(_ context.Context, cmd *pb.GetNoteCommand) (*pb.GetNoteResponse, error) {
	f.getNoteCmd = cmd
	return f.getNoteResult, f.returnErr
}
func (f *fakeCommandHandler) ListNotes(_ context.Context, cmd *pb.ListNotesCommand) (*pb.ListNotesResponse, error) {
	f.listNotesCmd = cmd
	return f.listNotesResult, f.returnErr
}
func (f *fakeCommandHandler) UpdateNote(_ context.Context, cmd *pb.UpdateNoteCommand) (*pb.UpdateNoteResponse, error) {
	f.updateNoteCmd = cmd
	return f.updateNoteResult, f.returnErr
}
func (f *fakeCommandHandler) DeleteNote(_ context.Context, id string) error {
	f.deleteNoteID = id
	return f.returnErr
}
func (f *fakeCommandHandler) DeliverBroadcast(_ context.Context, cmd *pb.BroadcastCommand) error {
	f.broadcastCmd = cmd
	return f.returnErr
}
func (f *fakeCommandHandler) AddAccount(_ context.Context, cmd *pb.AddAccountCommand) (*pb.AddAccountResponse, error) {
	f.addAccountCmd = cmd
	return f.addAccountResult, f.returnErr
}
func (f *fakeCommandHandler) RefreshAccount(_ context.Context, cmd *pb.RefreshAccountCommand) (*pb.RefreshAccountResponse, error) {
	f.refreshAccountCmd = cmd
	return f.refreshAccountResult, f.returnErr
}
func (f *fakeCommandHandler) UpdateAccount(_ context.Context, cmd *pb.UpdateAccountCommand) (*pb.UpdateAccountResponse, error) {
	f.updateAccountCmd = cmd
	return f.updateAccountResult, f.returnErr
}
func (f *fakeCommandHandler) RemoveAccount(_ context.Context, id string) error {
	f.removeAccountID = id
	return f.returnErr
}
func (f *fakeCommandHandler) TestAccount(_ context.Context, cmd *pb.TestAccountCommand) (*pb.TestAccountResponse, error) {
	f.testAccountID = cmd.GetId()
	return f.testAccountResult, f.returnErr
}
func (f *fakeCommandHandler) ListChats(_ context.Context, sessionID string) (*pb.ListChatsResponse, error) {
	f.listChatsSession = sessionID
	return f.listChatsResult, f.returnErr
}
func (f *fakeCommandHandler) GetChatStatuses(_ context.Context, sessionID string) (*pb.GetChatStatusesResponse, error) {
	f.chatStatusesSession = sessionID
	return f.chatStatusesResult, f.returnErr
}
func (f *fakeCommandHandler) GetSessionStatuses(_ context.Context, sessionIDs []string) (*pb.GetSessionStatusesResponse, error) {
	f.sessionStatusesIDs = sessionIDs
	return f.sessionStatusesResult, f.returnErr
}
func (f *fakeCommandHandler) ListCheckSnapshots(_ context.Context, sessionID string, limit int32) (*pb.ListCheckSnapshotsResponse, error) {
	f.checkSnapshotsSession = sessionID
	f.checkSnapshotsLimit = limit
	return f.listCheckSnapshotsResult, f.returnErr
}
func (f *fakeCommandHandler) ListPlugins(_ context.Context) (*pb.ListPluginsResponse, error) {
	return f.listPluginsResult, f.returnErr
}
func (f *fakeCommandHandler) GetCronJob(_ context.Context, id string) (*pb.GetCronJobResponse, error) {
	f.getCronJobID = id
	return f.getCronJobResult, f.returnErr
}
func (f *fakeCommandHandler) RepairDoctor(_ context.Context) (*pb.RepairDoctorResponse, error) {
	return f.repairDoctorResult, f.returnErr
}
func (f *fakeCommandHandler) CloseSession(_ context.Context, sessionID string) (*pb.Session, error) {
	f.closeSessionID = sessionID
	return f.closeSession, f.returnErr
}
func (f *fakeCommandHandler) ResurrectSession(_ context.Context, sessionID string) (*pb.Session, error) {
	f.resurrectSessionID = sessionID
	return f.resurrectSession, f.returnErr
}
func (f *fakeCommandHandler) RemoveSession(_ context.Context, sessionID string) error {
	f.removeSessionID = sessionID
	return f.returnErr
}
func (f *fakeCommandHandler) EmptyTrash(_ context.Context, olderThan *timestamppb.Timestamp) (int32, error) {
	f.emptyTrashOlderThan = olderThan
	return f.emptyTrashCount, f.returnErr
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

func TestDispatch_ListCronJobs(t *testing.T) {
	fake := &fakeCommandHandler{listCronResult: &pb.ListCronJobsResponse{
		CronJobs: []*pb.CronJob{{Id: "cj1", Name: "nightly"}},
	}}
	client := newDispatcherClient(fake, nil, nil)

	out := make(chan *pb.DaemonEvent, 1)
	if ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "c1",
		Cmd:       &pb.OrchestratorCommand_ListCronJobs{ListCronJobs: &pb.ListCronJobsCommand{}},
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
	jobs := res.GetListCronJobs().GetCronJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 cron job, got %d", len(jobs))
	}
	if jobs[0].GetId() != "cj1" || jobs[0].GetName() != "nightly" {
		t.Fatalf("job = %+v, want id=cj1 name=nightly", jobs[0])
	}
}

func TestDispatch_CreateCronJob(t *testing.T) {
	fake := &fakeCommandHandler{createCronResult: &pb.CreateCronJobResponse{
		CronJob: &pb.CronJob{Id: "cj-new", Name: "nightly", Schedule: "@daily"},
	}}
	client := newDispatcherClient(fake, nil, nil)

	out := make(chan *pb.DaemonEvent, 1)
	if ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "c1",
		Cmd: &pb.OrchestratorCommand_CreateCronJob{CreateCronJob: &pb.CreateCronJobCommand{
			RepoId:   "r1",
			Name:     "nightly",
			Prompt:   "do the thing",
			Schedule: "@daily",
		}},
	}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async command, got %+v", ev)
	}

	ev := recvEvent(t, out)
	res := ev.GetResult()
	if res == nil || !res.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
	}
	if fake.createCronCmd == nil || fake.createCronCmd.GetName() != "nightly" || fake.createCronCmd.GetRepoId() != "r1" {
		t.Fatalf("create command fields did not reach handler: %+v", fake.createCronCmd)
	}
	job := res.GetCreateCronJob().GetCronJob()
	if job.GetId() != "cj-new" || job.GetName() != "nightly" {
		t.Fatalf("job = %+v, want id=cj-new name=nightly", job)
	}
}

func TestDispatch_UpdateCronJob(t *testing.T) {
	fake := &fakeCommandHandler{updateCronResult: &pb.UpdateCronJobResponse{
		CronJob: &pb.CronJob{Id: "cj1", Name: "renamed"},
	}}
	client := newDispatcherClient(fake, nil, nil)

	name := "renamed"
	enabled := false
	out := make(chan *pb.DaemonEvent, 1)
	if ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "c1",
		Cmd: &pb.OrchestratorCommand_UpdateCronJob{UpdateCronJob: &pb.UpdateCronJobCommand{
			Id:        "cj1",
			Name:      &name,
			IsEnabled: &enabled,
		}},
	}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async command, got %+v", ev)
	}

	ev := recvEvent(t, out)
	res := ev.GetResult()
	if res == nil || !res.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
	}
	got := fake.updateCronCmd
	if got == nil {
		t.Fatal("update command did not reach handler")
	}
	if got.GetId() != "cj1" {
		t.Fatalf("id = %q, want cj1", got.GetId())
	}
	if got.Name == nil || got.GetName() != "renamed" {
		t.Fatalf("name not forwarded: %v", got.Name)
	}
	if got.IsEnabled == nil || got.GetIsEnabled() != false {
		t.Fatalf("enabled not forwarded: %v", got.IsEnabled)
	}
	// Unset optional fields must stay nil so the daemon leaves the stored
	// values untouched on a partial update.
	if got.Prompt != nil || got.Schedule != nil || got.Model != nil || got.ShouldRunSetupCommand != nil {
		t.Fatalf("unset fields leaked non-nil: %+v", got)
	}
	if job := res.GetUpdateCronJob().GetCronJob(); job.GetName() != "renamed" {
		t.Fatalf("job name = %q, want renamed", job.GetName())
	}
}

func TestDispatch_DeleteCronJob(t *testing.T) {
	fake := &fakeCommandHandler{}
	client := newDispatcherClient(fake, nil, nil)

	out := make(chan *pb.DaemonEvent, 1)
	if ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "c1",
		Cmd:       &pb.OrchestratorCommand_DeleteCronJob{DeleteCronJob: &pb.DeleteCronJobCommand{Id: "cj1"}},
	}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async command, got %+v", ev)
	}

	ev := recvEvent(t, out)
	res := ev.GetResult()
	if res == nil || !res.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
	}
	if res.GetPayload() != nil {
		t.Fatalf("expected no payload on delete result, got %+v", res.GetPayload())
	}
	if fake.deleteCronID != "cj1" {
		t.Fatalf("deleteCronID = %q, want cj1", fake.deleteCronID)
	}
}

func TestDispatch_RunCronJobNow(t *testing.T) {
	fake := &fakeCommandHandler{runCronResult: &pb.RunCronJobNowResponse{
		SkippedReason: "overlap with running session",
	}}
	client := newDispatcherClient(fake, nil, nil)

	out := make(chan *pb.DaemonEvent, 1)
	if ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "c1",
		Cmd:       &pb.OrchestratorCommand_RunCronJobNow{RunCronJobNow: &pb.RunCronJobNowCommand{Id: "cj1"}},
	}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async command, got %+v", ev)
	}

	ev := recvEvent(t, out)
	res := ev.GetResult()
	if res == nil || !res.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
	}
	if fake.runCronID != "cj1" {
		t.Fatalf("runCronID = %q, want cj1", fake.runCronID)
	}
	if got := res.GetRunCronJobNow().GetSkippedReason(); got != "overlap with running session" {
		t.Fatalf("skipped_reason = %q, want overlap message", got)
	}
}

func TestDispatch_CreateCronJob_ValidationError(t *testing.T) {
	fake := &fakeCommandHandler{returnErr: connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("schedule is required"))}
	client := newDispatcherClient(fake, nil, nil)

	out := make(chan *pb.DaemonEvent, 1)
	if ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "c1",
		Cmd: &pb.OrchestratorCommand_CreateCronJob{CreateCronJob: &pb.CreateCronJobCommand{
			RepoId: "r1", Name: "nightly", Prompt: "x",
		}},
	}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async command, got %+v", ev)
	}

	ev := recvEvent(t, out)
	res := ev.GetResult()
	if res == nil || res.GetOk() {
		t.Fatalf("expected failed result, got %+v", ev)
	}
	if res.GetError() == "" {
		t.Fatalf("expected non-empty error message on validation failure")
	}
}

func TestDispatch_RepoManagement(t *testing.T) {
	fake := &fakeCommandHandler{
		repoSettings: &pb.GetRepoSettingsResponse{Settings: &pb.RepoSettings{Id: "repo-1"}},
		updatedRepo:  &pb.UpdateRepoResponse{Repo: &pb.Repo{Id: "repo-1"}},
	}
	client := newDispatcherClient(fake, nil, nil)

	tests := []struct {
		name      string
		command   *pb.OrchestratorCommand
		assertion func(t *testing.T, result *pb.CommandResult)
	}{
		{
			name: "get",
			command: &pb.OrchestratorCommand{CommandId: "get", Cmd: &pb.OrchestratorCommand_GetRepo{
				GetRepo: &pb.GetRepoCommand{RepoId: "repo-1"},
			}},
			assertion: func(t *testing.T, result *pb.CommandResult) {
				if got := result.GetGetRepo().GetSettings().GetId(); got != "repo-1" {
					t.Fatalf("settings repo_id = %q, want repo-1", got)
				}
			},
		},
		{
			name: "update",
			command: &pb.OrchestratorCommand{CommandId: "update", Cmd: &pb.OrchestratorCommand_UpdateRepo{
				UpdateRepo: &pb.UpdateRepoCommand{RepoId: "repo-1"},
			}},
			assertion: func(t *testing.T, result *pb.CommandResult) {
				if got := result.GetUpdateRepo().GetRepo().GetId(); got != "repo-1" {
					t.Fatalf("updated repo_id = %q, want repo-1", got)
				}
			},
		},
		{
			name: "remove",
			command: &pb.OrchestratorCommand{CommandId: "remove", Cmd: &pb.OrchestratorCommand_RemoveRepo{
				RemoveRepo: &pb.RemoveRepoCommand{RepoId: "repo-1"},
			}},
			assertion: func(t *testing.T, result *pb.CommandResult) {
				if got := fake.removedRepoID; got != "repo-1" {
					t.Fatalf("removed repo_id = %q, want repo-1", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := make(chan *pb.DaemonEvent, 1)
			if event := client.dispatchCommand(context.Background(), tt.command, out); event != nil {
				t.Fatalf("expected async command to return nil, got %+v", event)
			}
			result := recvEvent(t, out).GetResult()
			if result == nil || !result.GetOk() {
				t.Fatalf("result = %+v, want ok", result)
			}
			tt.assertion(t, result)
		})
	}
	if got := fake.updatedRepoRequest.GetRepoId(); got != "repo-1" {
		t.Fatalf("updated repo_id = %q, want repo-1", got)
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
			AgentSessionId: "agent-1", Message: "hello", WakeIfAsleep: true, Submit: true,
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
	if fake.sendAgentID != "agent-1" || fake.sendMessage != "hello" || !fake.sendSubmit {
		t.Fatalf("send fields not forwarded: agent=%q msg=%q submit=%v", fake.sendAgentID, fake.sendMessage, fake.sendSubmit)
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

func TestDispatchCommand_SwitchAccount_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{
		switchResumed:     true,
		switchTargetLabel: "Account B",
		switchNoticeText:  "Switched to Account B",
	}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	// The switch is dispatched asynchronously (BOS-897): it now runs under a
	// real respawn budget, which must not be spent on the command reader. The
	// payload below is byte-for-byte the synchronous form's — only the delivery
	// channel changed.
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-s1",
			Cmd: &pb.OrchestratorCommand_SwitchAccount{
				SwitchAccount: &pb.SwitchAccountCommand{
					SessionId:      "sess-1",
					AgentSessionId: "agent-1",
					AccountId:      "acct-b",
					Force:          true,
				},
			},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async switch command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	if handler.switchCalls.Load() != 1 {
		t.Fatalf("switch calls = %d, want 1", handler.switchCalls.Load())
	}
	if handler.switchSessionID != "sess-1" || handler.switchAgentID != "agent-1" ||
		handler.switchAccountID != "acct-b" || !handler.switchForce {
		t.Fatalf("forwarded args = (%q,%q,%q,%v)", handler.switchSessionID, handler.switchAgentID, handler.switchAccountID, handler.switchForce)
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() {
		t.Fatalf("expected ok result: %+v", ev)
	}
	sw := r.GetSwitchAccount()
	if sw == nil {
		t.Fatalf("expected SwitchAccountResult payload, got %+v", r)
	}
	if !sw.GetResumed() || sw.GetTargetLabel() != "Account B" || sw.GetNoticeText() != "Switched to Account B" {
		t.Fatalf("payload = %+v", sw)
	}
}

func TestDispatchCommand_SwitchAccount_CommandCancelCancelsInFlightSwitch(t *testing.T) {
	handler := &fakeCommandHandler{switchBlock: make(chan struct{})}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if ev := client.dispatchCommand(ctx,
		&pb.OrchestratorCommand{
			CommandId: "c-switch-cancel",
			Cmd: &pb.OrchestratorCommand_SwitchAccount{
				SwitchAccount: &pb.SwitchAccountCommand{SessionId: "sess-1", AccountId: "acct-b"},
			},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async switch command, got %+v", ev)
	}
	waitFor(t, "switch cancel registered", func() bool {
		client.asyncMu.Lock()
		_, ok := client.asyncCancels["c-switch-cancel"]
		client.asyncMu.Unlock()
		return ok
	})
	if ev := client.dispatchCommand(ctx,
		&pb.OrchestratorCommand{
			CommandId: "c-switch-cancel",
			Cmd: &pb.OrchestratorCommand_CommandCancel{
				CommandCancel: &pb.CommandCancel{CommandId: "c-switch-cancel"},
			},
		}, out); ev != nil {
		t.Fatalf("expected nil result for command cancel, got %+v", ev)
	}

	ev := recvEvent(t, out)
	r := ev.GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected failed switch result, got %+v", ev)
	}
	if r.GetErrorCode() != pb.CommandResult_ERROR_CODE_CANCELED {
		t.Fatalf("error_code = %v, want CANCELED", r.GetErrorCode())
	}
	if !strings.Contains(r.GetError(), "context canceled") {
		t.Fatalf("error = %q, want context canceled", r.GetError())
	}
}

func TestDispatchCommand_SwitchAccount_CommandCancelBeforeSwitchCancelsWhenSwitchArrives(t *testing.T) {
	handler := &fakeCommandHandler{switchBlock: make(chan struct{})}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if ev := client.dispatchCommand(ctx,
		&pb.OrchestratorCommand{
			CommandId: "c-switch-early-cancel",
			Cmd: &pb.OrchestratorCommand_CommandCancel{
				CommandCancel: &pb.CommandCancel{CommandId: "c-switch-early-cancel"},
			},
		}, out); ev != nil {
		t.Fatalf("expected nil result for command cancel, got %+v", ev)
	}
	if ev := client.dispatchCommand(ctx,
		&pb.OrchestratorCommand{
			CommandId: "c-switch-early-cancel",
			Cmd: &pb.OrchestratorCommand_SwitchAccount{
				SwitchAccount: &pb.SwitchAccountCommand{SessionId: "sess-1", AccountId: "acct-b"},
			},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async switch command, got %+v", ev)
	}

	ev := recvEvent(t, out)
	r := ev.GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected failed switch result, got %+v", ev)
	}
	if r.GetErrorCode() != pb.CommandResult_ERROR_CODE_CANCELED {
		t.Fatalf("error_code = %v, want CANCELED", r.GetErrorCode())
	}
	if !strings.Contains(r.GetError(), "context canceled") {
		t.Fatalf("error = %q, want context canceled", r.GetError())
	}
}

func TestRunCancelableAsyncCommand_TeardownWaitsForWorker(t *testing.T) {
	client := newDispatcherClient(nil, nil, nil)
	out := make(chan *pb.DaemonEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})

	if ev := client.runCancelableAsyncCommand(ctx, "c-switch-teardown", out, func(context.Context) *pb.DaemonEvent {
		close(entered)
		<-release
		return commandOK("c-switch-teardown", nil)
	}); ev != nil {
		t.Fatalf("expected nil synchronous result for async command, got %+v", ev)
	}
	<-entered
	cancel()

	waitDone := make(chan struct{})
	go func() {
		client.cancelAndWaitAsyncCommands()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("teardown returned before async worker exited")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("teardown did not return after async worker exited")
	}
	client.asyncMu.Lock()
	defer client.asyncMu.Unlock()
	if len(client.asyncCancels) != 0 {
		t.Fatalf("asyncCancels length = %d, want 0", len(client.asyncCancels))
	}
	if len(client.asyncDone) != 0 {
		t.Fatalf("asyncDone length = %d, want 0", len(client.asyncDone))
	}
}

func TestDispatchCommand_SwitchAccount_FailedPreconditionSetsErrorCode(t *testing.T) {
	handler := &fakeCommandHandler{
		switchErrorCode: pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION,
		switchErr:       errors.New("target account is cooling down"),
	}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-s2",
			Cmd: &pb.OrchestratorCommand_SwitchAccount{
				SwitchAccount: &pb.SwitchAccountCommand{SessionId: "sess-1", AccountId: "acct-b"},
			},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async switch command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	r := ev.GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected error result, got %+v", ev)
	}
	if r.GetErrorCode() != pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("error_code = %v, want FAILED_PRECONDITION", r.GetErrorCode())
	}
	if r.GetError() != "target account is cooling down" {
		t.Fatalf("error message = %q", r.GetError())
	}
}

// TestHandleCommand_SlowSwitchDoesNotBlockReader is R7. A switch respawns the
// chat's pane and waits for its composer, and BOS-897 grants it a legitimate
// respawn budget to do it in. handleCommand runs inline on the
// single-threaded Receive loop, which also services the heartbeat, so spending
// that budget there is connection-fatal rather than merely slow. A fast Stop
// dispatched right behind a wedged switch must complete without waiting for it.
func TestHandleCommand_SlowSwitchDoesNotBlockReader(t *testing.T) {
	release := make(chan struct{})
	fake := &fakeCommandHandler{
		session:     &pb.Session{Id: "s1"},
		switchBlock: release,
	}
	client := newDispatcherClient(fake, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	ctx := context.Background()

	client.handleCommand(ctx, &pb.OrchestratorCommand{
		CommandId: "slow-switch",
		Cmd: &pb.OrchestratorCommand_SwitchAccount{SwitchAccount: &pb.SwitchAccountCommand{
			SessionId: "sess-1", AgentSessionId: "agent-1", AccountId: "acct-b",
		}},
	}, out)

	client.handleCommand(ctx, &pb.OrchestratorCommand{
		CommandId: "fast",
		Cmd:       &pb.OrchestratorCommand_Stop{Stop: &pb.StopSessionCommand{SessionId: "s1"}},
	}, out)

	ev := recvEvent(t, out)
	if got := ev.GetResult().GetCommandId(); got != "fast" {
		t.Fatalf("expected fast command result first, got %q (the switch wedged the reader)", got)
	}

	// Let the switch finish so its goroutine doesn't leak, and confirm the
	// result still arrives on outbound afterwards.
	close(release)
	ev = recvEvent(t, out)
	if got := ev.GetResult().GetCommandId(); got != "slow-switch" {
		t.Fatalf("expected the switch result after release, got %q", got)
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
	out := make(chan *pb.DaemonEvent, 4)
	// Merge is dispatched asynchronously (it can queue behind another merge on
	// the same repo), so dispatchCommand returns nil and the result arrives on
	// outbound.
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-m1",
			Cmd:       &pb.OrchestratorCommand_Merge{Merge: &pb.MergeSessionCommand{SessionId: "s-merge"}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async merge command, got %+v", ev)
	}
	ev := recvEvent(t, out)
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
			out := make(chan *pb.DaemonEvent, 4)
			if ev := client.dispatchCommand(context.Background(),
				&pb.OrchestratorCommand{
					CommandId: "c-merr",
					Cmd:       &pb.OrchestratorCommand_Merge{Merge: &pb.MergeSessionCommand{SessionId: "s1"}},
				}, out); ev != nil {
				t.Fatalf("expected nil synchronous result for async merge command, got %+v", ev)
			}
			ev := recvEvent(t, out)
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

func TestDispatchCommand_RetrySession_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{retrySession: &pb.Session{Id: "s-retry"}}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-r1",
			Cmd:       &pb.OrchestratorCommand_RetrySession{RetrySession: &pb.RetrySessionCommand{SessionId: "s-retry"}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.retryCalls.Load() != 1 || handler.retrySessionID != "s-retry" {
		t.Fatalf("retry not called with s-retry: calls=%d id=%q", handler.retryCalls.Load(), handler.retrySessionID)
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetSession().GetId() != "s-retry" {
		t.Fatalf("expected ok result with session s-retry, got %+v", ev)
	}
}

func TestDispatchCommand_UpdateSession_ForwardsFields(t *testing.T) {
	handler := &fakeCommandHandler{updateSession: &pb.Session{Id: "s-upd"}}
	client := newDispatcherClient(handler, nil, nil)
	title := "Renamed"
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-u1",
			Cmd: &pb.OrchestratorCommand_UpdateSession{UpdateSession: &pb.UpdateSessionCommand{
				SessionId: "s-upd", Title: &title,
			}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.updateSessionCalls.Load() != 1 {
		t.Fatalf("update calls = %d, want 1", handler.updateSessionCalls.Load())
	}
	if handler.updateSessionCmd.GetSessionId() != "s-upd" || handler.updateSessionCmd.GetTitle() != title {
		t.Fatalf("update command not forwarded: %+v", handler.updateSessionCmd)
	}
	if r := ev.GetResult(); r == nil || !r.GetOk() || r.GetSession().GetId() != "s-upd" {
		t.Fatalf("expected ok result with session s-upd, got %+v", ev)
	}
}

func TestDispatchCommand_LinkSessionPR_ForwardsFields(t *testing.T) {
	handler := &fakeCommandHandler{linkSession: &pb.Session{Id: "s-link"}}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-l1",
			Cmd:       &pb.OrchestratorCommand_LinkSessionPr{LinkSessionPr: &pb.LinkSessionPRCommand{SessionId: "s-link", Pr: "42"}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.linkPRCalls.Load() != 1 || handler.linkSessionID != "s-link" || handler.linkPR != "42" {
		t.Fatalf("link not forwarded: calls=%d id=%q pr=%q", handler.linkPRCalls.Load(), handler.linkSessionID, handler.linkPR)
	}
	if r := ev.GetResult(); r == nil || !r.GetOk() || r.GetSession().GetId() != "s-link" {
		t.Fatalf("expected ok result with session s-link, got %+v", ev)
	}
}

func TestDispatchCommand_SessionMutations_MapConnectCodeToErrorCode(t *testing.T) {
	handler := &fakeCommandHandler{returnErr: fmt.Errorf("retry session: %w", connect.NewError(connect.CodeNotFound, errors.New("session not found")))}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-rerr",
			Cmd:       &pb.OrchestratorCommand_RetrySession{RetrySession: &pb.RetrySessionCommand{SessionId: "s1"}},
		}, make(chan *pb.DaemonEvent, 4))
	r := ev.GetResult()
	if r == nil || r.GetOk() || r.GetErrorCode() != pb.CommandResult_ERROR_CODE_NOT_FOUND {
		t.Fatalf("expected NotFound error_code, got %+v", ev)
	}
}

func TestDispatchCommand_UpdateChatTitle_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-ct1",
			Cmd:       &pb.OrchestratorCommand_UpdateChatTitle{UpdateChatTitle: &pb.UpdateChatTitleCommand{AgentSessionId: "agent-1", Title: "Renamed"}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.updateChatTitleCalls.Load() != 1 || handler.updateChatTitleAgentID != "agent-1" || handler.updateChatTitleValue != "Renamed" {
		t.Fatalf("update_chat_title not forwarded: calls=%d id=%q title=%q", handler.updateChatTitleCalls.Load(), handler.updateChatTitleAgentID, handler.updateChatTitleValue)
	}
	// Empty result (no payload), Ok=true.
	if r := ev.GetResult(); r == nil || !r.GetOk() || r.GetSession() != nil {
		t.Fatalf("expected ok result with no session payload, got %+v", ev)
	}
}

func TestDispatchCommand_ReportChatStatus_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{}
	client := newDispatcherClient(handler, nil, nil)
	reports := []*pb.ChatStatusReport{{AgentSessionId: "agent-1"}, {AgentSessionId: "agent-2"}}
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-rs1",
			Cmd:       &pb.OrchestratorCommand_ReportChatStatus{ReportChatStatus: &pb.ReportChatStatusCommand{Reports: reports}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.reportChatStatusCalls.Load() != 1 || len(handler.reportChatStatusReports) != 2 {
		t.Fatalf("report_chat_status not forwarded: calls=%d reports=%d", handler.reportChatStatusCalls.Load(), len(handler.reportChatStatusReports))
	}
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
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

func TestDispatchCommand_CloseSession_CallsHandler(t *testing.T) {
	sess := &pb.Session{Id: "s-close"}
	handler := &fakeCommandHandler{closeSession: sess}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-cl1",
			Cmd:       &pb.OrchestratorCommand_CloseSession{CloseSession: &pb.CloseSessionCommand{SessionId: "s-close"}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.closeSessionID != "s-close" {
		t.Fatalf("close session id = %q, want s-close", handler.closeSessionID)
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-cl1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	if r.GetSession().GetId() != "s-close" {
		t.Fatalf("expected session id s-close, got %q", r.GetSession().GetId())
	}
}

func TestDispatchCommand_ResurrectSession_CallsHandler(t *testing.T) {
	sess := &pb.Session{Id: "s-res"}
	handler := &fakeCommandHandler{resurrectSession: sess}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	// Resurrect is dispatched ASYNCHRONOUSLY (BOS-984) — it runs the repo setup
	// script, so it must not hold the command reader — hence a nil synchronous
	// return and the result arriving on the outbound channel, exactly like
	// remove_session and empty_trash.
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-rs1",
			Cmd:       &pb.OrchestratorCommand_ResurrectSession{ResurrectSession: &pb.ResurrectSessionCommand{SessionId: "s-res"}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async resurrect command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	if handler.resurrectSessionID != "s-res" {
		t.Fatalf("resurrect session id = %q, want s-res", handler.resurrectSessionID)
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-rs1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	if r.GetSession().GetId() != "s-res" {
		t.Fatalf("expected session id s-res, got %q", r.GetSession().GetId())
	}
}

func TestDispatchCommand_RemoveSession_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-rms1",
			Cmd:       &pb.OrchestratorCommand_RemoveSession{RemoveSession: &pb.RemoveSessionCommand{SessionId: "s-rm"}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async remove command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	if handler.removeSessionID != "s-rm" {
		t.Fatalf("remove session id = %q, want s-rm", handler.removeSessionID)
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-rms1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	if r.GetSession() != nil {
		t.Fatalf("expected no session payload for remove_session, got %+v", r.GetSession())
	}
}

func TestDispatchCommand_EmptyTrash_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{emptyTrashCount: 7}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	older := timestamppb.Now()
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-et1",
			Cmd:       &pb.OrchestratorCommand_EmptyTrash{EmptyTrash: &pb.EmptyTrashCommand{OlderThan: older}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async empty_trash command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	if handler.emptyTrashOlderThan == nil || !handler.emptyTrashOlderThan.AsTime().Equal(older.AsTime()) {
		t.Fatalf("empty_trash older_than not threaded through: %+v", handler.emptyTrashOlderThan)
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-et1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	if r.GetEmptyTrash().GetDeletedCount() != 7 {
		t.Fatalf("expected deleted_count 7, got %d", r.GetEmptyTrash().GetDeletedCount())
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

func TestDispatchCommand_GetChatStatuses_CallsHandler(t *testing.T) {
	statuses := &pb.GetChatStatusesResponse{Statuses: []*pb.ChatStatusEntry{
		{AgentSessionId: "chat-1", Status: pb.ChatStatus_CHAT_STATUS_IDLE},
		{AgentSessionId: "chat-2", Status: pb.ChatStatus_CHAT_STATUS_WORKING},
	}}
	handler := &fakeCommandHandler{chatStatusesResult: statuses}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-gcs1",
			Cmd: &pb.OrchestratorCommand_GetChatStatuses{
				GetChatStatuses: &pb.GetChatStatusesCommand{SessionId: "sess-1"},
			},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async get_chat_statuses, got %+v", ev)
	}
	ev := recvEvent(t, out)
	// session_id must reach the handler unchanged: it is what scopes the read for authz.
	if handler.chatStatusesSession != "sess-1" {
		t.Fatalf("handler session_id = %q, want %q", handler.chatStatusesSession, "sess-1")
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-gcs1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	// The reply must land in the new get_chat_statuses arm, not the aggregate
	// get_session_statuses one — they are not interchangeable.
	if r.GetGetSessionStatuses() != nil {
		t.Fatalf("expected no get_session_statuses payload, got %+v", r.GetGetSessionStatuses())
	}
	got := r.GetGetChatStatuses().GetStatuses()
	if len(got) != 2 {
		t.Fatalf("expected 2 chat statuses, got %+v", got)
	}
	if got[0].GetAgentSessionId() != "chat-1" || got[0].GetStatus() != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatalf("unexpected first chat status: %+v", got[0])
	}
	if got[1].GetAgentSessionId() != "chat-2" || got[1].GetStatus() != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Fatalf("unexpected second chat status: %+v", got[1])
	}
}

func TestDispatchCommand_GetChatStatuses_HandlerError_ReturnsCommandErr(t *testing.T) {
	handler := &fakeCommandHandler{
		returnErr: connect.NewError(connect.CodeNotFound, errors.New("session not found")),
	}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-gcs-err",
			Cmd: &pb.OrchestratorCommand_GetChatStatuses{
				GetChatStatuses: &pb.GetChatStatusesCommand{SessionId: "sess-missing"},
			},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async get_chat_statuses, got %+v", ev)
	}
	ev := recvEvent(t, out)
	r := ev.GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected error result, got %+v", ev)
	}
	// The daemon-side code must survive so --remote can map it like a local call.
	if r.GetErrorCode() != pb.CommandResult_ERROR_CODE_NOT_FOUND {
		t.Fatalf("error_code = %v, want NOT_FOUND", r.GetErrorCode())
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

func TestDispatchCommand_ListAccounts_CallsHandler(t *testing.T) {
	accounts := &pb.ListAccountsResponse{Accounts: []*pb.Account{{Id: "acc-1", Provider: "claude", Label: "primary"}}}
	handler := &fakeCommandHandler{listAccountsResult: accounts}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-lacc1",
			Cmd:       &pb.OrchestratorCommand_ListAccounts{ListAccounts: &pb.ListAccountsCommand{Provider: "claude"}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async list command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	if handler.listAccountsCalls.Load() != 1 {
		t.Fatalf("list_accounts calls = %d, want 1", handler.listAccountsCalls.Load())
	}
	if handler.listAccountsProvider != "claude" {
		t.Fatalf("list_accounts provider = %q, want claude", handler.listAccountsProvider)
	}
	if handler.listAccountsRefresh {
		t.Fatalf("list_accounts refresh = true, want false for an omitted command field")
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-lacc1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	got := r.GetListAccounts().GetAccounts()
	if len(got) != 1 || got[0].GetId() != "acc-1" {
		t.Fatalf("expected list_accounts payload with acc-1, got %+v", got)
	}
}

// TestDispatchCommand_ListAccounts_ForwardsRefresh proves refresh=true and
// refresh=false both travel from the command through to the handler unchanged.
func TestDispatchCommand_ListAccounts_ForwardsRefresh(t *testing.T) {
	for _, refresh := range []bool{true, false} {
		t.Run(fmt.Sprintf("refresh=%v", refresh), func(t *testing.T) {
			handler := &fakeCommandHandler{listAccountsResult: &pb.ListAccountsResponse{}}
			client := newDispatcherClient(handler, nil, nil)
			out := make(chan *pb.DaemonEvent, 4)
			if ev := client.dispatchCommand(context.Background(),
				&pb.OrchestratorCommand{
					CommandId: "c-lacc-refresh",
					Cmd:       &pb.OrchestratorCommand_ListAccounts{ListAccounts: &pb.ListAccountsCommand{ShouldRefresh: refresh}},
				}, out); ev != nil {
				t.Fatalf("expected nil synchronous result for async list command, got %+v", ev)
			}
			recvEvent(t, out)
			if handler.listAccountsRefresh != refresh {
				t.Fatalf("list_accounts refresh = %v, want %v", handler.listAccountsRefresh, refresh)
			}
		})
	}
}

func TestDispatchCommand_ListAccounts_RefreshDeadlineCancelsHandler(t *testing.T) {
	previousDeadline := listAccountsRefreshDeadline
	listAccountsRefreshDeadline = 20 * time.Millisecond
	t.Cleanup(func() { listAccountsRefreshDeadline = previousDeadline })

	handler := &fakeCommandHandler{listAccountsBlock: true}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 1)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-lacc-deadline",
			Cmd:       &pb.OrchestratorCommand_ListAccounts{ListAccounts: &pb.ListAccountsCommand{ShouldRefresh: true}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async list command, got %+v", ev)
	}

	ev := recvEvent(t, out)
	if got := ev.GetResult().GetError(); !strings.Contains(got, context.DeadlineExceeded.Error()) {
		t.Fatalf("list_accounts error = %q, want deadline exceeded", got)
	}
}

func TestDispatchCommand_ListAccounts_HandlerError_ReturnsCommandErr(t *testing.T) {
	handler := &fakeCommandHandler{returnErr: errors.New("list accounts boom")}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-lacc-err",
			Cmd:       &pb.OrchestratorCommand_ListAccounts{ListAccounts: &pb.ListAccountsCommand{}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async list command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	r := ev.GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected error result, got %+v", ev)
	}
	if r.GetError() != "list accounts boom" {
		t.Fatalf("expected error %q, got %q", "list accounts boom", r.GetError())
	}
}

func TestDispatchCommand_AddAccount_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{addAccountResult: &pb.AddAccountResponse{Account: &pb.Account{Id: "acc-1", Provider: "claude", Label: "primary"}}}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-add1",
			Cmd: &pb.OrchestratorCommand_AddAccount{AddAccount: &pb.AddAccountCommand{
				Provider: "claude", Label: "primary", Priority: 2, Credential: []byte("secret-token"),
			}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	if handler.addAccountCmd == nil || handler.addAccountCmd.GetProvider() != "claude" ||
		handler.addAccountCmd.GetLabel() != "primary" || handler.addAccountCmd.GetPriority() != 2 ||
		string(handler.addAccountCmd.GetCredential()) != "secret-token" {
		t.Fatalf("add_account command not forwarded verbatim: %+v", handler.addAccountCmd)
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-add1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	if r.GetAddAccount().GetAccount().GetId() != "acc-1" {
		t.Fatalf("expected add_account payload with acc-1, got %+v", r.GetAddAccount())
	}
}

func TestDispatchCommand_AddAccount_HandlerError_ClassifiesCode(t *testing.T) {
	handler := &fakeCommandHandler{returnErr: connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("dupe"))}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-add-err",
			Cmd:       &pb.OrchestratorCommand_AddAccount{AddAccount: &pb.AddAccountCommand{Provider: "claude", Label: "x"}},
		}, out)
	r := recvEvent(t, out).GetResult()
	if r == nil || r.GetOk() || !strings.Contains(r.GetError(), "dupe") {
		t.Fatalf("expected failed result mentioning dupe, got %+v", r)
	}
	// CodeAlreadyExists isn't modeled by the reverse-stream protocol, so it
	// collapses to UNSPECIFIED (bosso treats it as Aborted).
	if r.GetErrorCode() != pb.CommandResult_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("error_code = %v, want UNSPECIFIED", r.GetErrorCode())
	}
}

func TestDispatchCommand_RefreshAccount_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{refreshAccountResult: &pb.RefreshAccountResponse{Account: &pb.Account{Id: "acc-1"}, LiveSmokeRan: true, Detail: "ok"}}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-ref1",
			Cmd: &pb.OrchestratorCommand_RefreshAccount{RefreshAccount: &pb.RefreshAccountCommand{
				Id: "acc-1", Credential: []byte("new-token"), TestAfterSave: true,
			}},
		}, out)
	ev := recvEvent(t, out)
	if handler.refreshAccountCmd == nil || handler.refreshAccountCmd.GetId() != "acc-1" ||
		string(handler.refreshAccountCmd.GetCredential()) != "new-token" || !handler.refreshAccountCmd.GetTestAfterSave() {
		t.Fatalf("refresh_account command not forwarded verbatim: %+v", handler.refreshAccountCmd)
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-ref1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	if !r.GetRefreshAccount().GetLiveSmokeRan() || r.GetRefreshAccount().GetAccount().GetId() != "acc-1" {
		t.Fatalf("unexpected refresh_account payload: %+v", r.GetRefreshAccount())
	}
}

func TestDispatchCommand_RefreshAccount_HandlerError_ClassifiesCode(t *testing.T) {
	handler := &fakeCommandHandler{returnErr: connect.NewError(connect.CodeNotFound, fmt.Errorf("no account"))}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-ref-err",
			Cmd:       &pb.OrchestratorCommand_RefreshAccount{RefreshAccount: &pb.RefreshAccountCommand{Id: "gone"}},
		}, out)
	r := recvEvent(t, out).GetResult()
	if r == nil || r.GetOk() || !strings.Contains(r.GetError(), "no account") {
		t.Fatalf("expected failed result mentioning no account, got %+v", r)
	}
	if r.GetErrorCode() != pb.CommandResult_ERROR_CODE_NOT_FOUND {
		t.Fatalf("error_code = %v, want NOT_FOUND", r.GetErrorCode())
	}
}

func TestDispatchCommand_UpdateAccount_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{updateAccountResult: &pb.UpdateAccountResponse{Account: &pb.Account{Id: "acc-1", Label: "renamed"}}}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	newLabel := "renamed"
	newPriority := int32(5)
	newStatus := "disabled"
	client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-upd1",
			Cmd: &pb.OrchestratorCommand_UpdateAccount{UpdateAccount: &pb.UpdateAccountCommand{
				Id: "acc-1", Label: &newLabel, Priority: &newPriority, Status: &newStatus, AllowedModels: []string{"m1", "m2"},
			}},
		}, out)
	ev := recvEvent(t, out)
	cmd := handler.updateAccountCmd
	if cmd == nil || cmd.GetId() != "acc-1" || cmd.Label == nil || *cmd.Label != "renamed" ||
		cmd.Priority == nil || *cmd.Priority != 5 || cmd.Status == nil || *cmd.Status != "disabled" ||
		len(cmd.GetAllowedModels()) != 2 {
		t.Fatalf("update_account optional pointers not forwarded 1:1: %+v", cmd)
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetUpdateAccount().GetAccount().GetLabel() != "renamed" {
		t.Fatalf("unexpected update_account result: %+v", r)
	}
}

func TestDispatchCommand_UpdateAccount_HandlerError_ClassifiesCode(t *testing.T) {
	handler := &fakeCommandHandler{returnErr: connect.NewError(connect.CodeNotFound, fmt.Errorf("no account"))}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-upd-err",
			Cmd:       &pb.OrchestratorCommand_UpdateAccount{UpdateAccount: &pb.UpdateAccountCommand{Id: "gone"}},
		}, out)
	r := recvEvent(t, out).GetResult()
	if r == nil || r.GetOk() || r.GetErrorCode() != pb.CommandResult_ERROR_CODE_NOT_FOUND {
		t.Fatalf("expected NOT_FOUND failed result, got %+v", r)
	}
}

func TestDispatchCommand_RemoveAccount_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-rem1",
			Cmd:       &pb.OrchestratorCommand_RemoveAccount{RemoveAccount: &pb.RemoveAccountCommand{Id: "acc-1"}},
		}, out)
	r := recvEvent(t, out).GetResult()
	if handler.removeAccountID != "acc-1" {
		t.Fatalf("remove_account id = %q, want acc-1", handler.removeAccountID)
	}
	// RemoveAccount succeeds with no payload (mirrors dispatchRemoveRepo).
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-rem1" || r.GetPayload() != nil {
		t.Fatalf("expected ok result with no payload, got %+v", r)
	}
}

func TestDispatchCommand_RemoveAccount_HandlerError_ClassifiesCode(t *testing.T) {
	handler := &fakeCommandHandler{returnErr: connect.NewError(connect.CodeNotFound, fmt.Errorf("no account"))}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-rem-err",
			Cmd:       &pb.OrchestratorCommand_RemoveAccount{RemoveAccount: &pb.RemoveAccountCommand{Id: "gone"}},
		}, out)
	r := recvEvent(t, out).GetResult()
	if r == nil || r.GetOk() || r.GetErrorCode() != pb.CommandResult_ERROR_CODE_NOT_FOUND {
		t.Fatalf("expected NOT_FOUND failed result, got %+v", r)
	}
}

func TestDispatchCommand_TestAccount_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{testAccountResult: &pb.TestAccountResponse{Account: &pb.Account{Id: "acc-1"}, LiveSmokeRan: true, Detail: "credential test passed"}}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-test1",
			Cmd:       &pb.OrchestratorCommand_TestAccount{TestAccount: &pb.TestAccountCommand{Id: "acc-1"}},
		}, out)
	ev := recvEvent(t, out)
	if handler.testAccountID != "acc-1" {
		t.Fatalf("test_account id = %q, want acc-1", handler.testAccountID)
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-test1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	if !r.GetTestAccount().GetLiveSmokeRan() || r.GetTestAccount().GetAccount().GetId() != "acc-1" {
		t.Fatalf("unexpected test_account payload: %+v", r.GetTestAccount())
	}
}

func TestDispatchCommand_TestAccount_HandlerError_ClassifiesCode(t *testing.T) {
	handler := &fakeCommandHandler{returnErr: connect.NewError(connect.CodeNotFound, fmt.Errorf("no account"))}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-test-err",
			Cmd:       &pb.OrchestratorCommand_TestAccount{TestAccount: &pb.TestAccountCommand{Id: "gone"}},
		}, out)
	r := recvEvent(t, out).GetResult()
	if r == nil || r.GetOk() || r.GetErrorCode() != pb.CommandResult_ERROR_CODE_NOT_FOUND {
		t.Fatalf("expected NOT_FOUND failed result, got %+v", r)
	}
}

func TestDispatchCommand_AccountCommands_HandlerNotWired(t *testing.T) {
	client := newDispatcherClient(nil, nil, nil) // no command handler
	cases := []struct {
		name string
		cmd  *pb.OrchestratorCommand
	}{
		{"add", &pb.OrchestratorCommand{CommandId: "n1", Cmd: &pb.OrchestratorCommand_AddAccount{AddAccount: &pb.AddAccountCommand{}}}},
		{"refresh", &pb.OrchestratorCommand{CommandId: "n2", Cmd: &pb.OrchestratorCommand_RefreshAccount{RefreshAccount: &pb.RefreshAccountCommand{}}}},
		{"update", &pb.OrchestratorCommand{CommandId: "n3", Cmd: &pb.OrchestratorCommand_UpdateAccount{UpdateAccount: &pb.UpdateAccountCommand{}}}},
		{"remove", &pb.OrchestratorCommand{CommandId: "n4", Cmd: &pb.OrchestratorCommand_RemoveAccount{RemoveAccount: &pb.RemoveAccountCommand{}}}},
		{"test", &pb.OrchestratorCommand{CommandId: "n5", Cmd: &pb.OrchestratorCommand_TestAccount{TestAccount: &pb.TestAccountCommand{}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := client.dispatchCommand(context.Background(), tc.cmd, make(chan *pb.DaemonEvent, 1))
			r := ev.GetResult()
			if r == nil || r.GetOk() || r.GetError() != "command handler not wired" {
				t.Fatalf("expected synchronous not-wired error, got %+v", ev)
			}
		})
	}
}

func TestDispatchCommand_Merge_HandlerError_ReturnsCommandErr(t *testing.T) {
	handler := &fakeCommandHandler{returnErr: errors.New("merge failed: conflict")}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-m-err",
			Cmd:       &pb.OrchestratorCommand_Merge{Merge: &pb.MergeSessionCommand{SessionId: "s1"}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async merge command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	r := ev.GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected error result, got %+v", ev)
	}
	if r.GetError() != "merge failed: conflict" {
		t.Fatalf("expected error message %q, got %q", "merge failed: conflict", r.GetError())
	}
}

// TestDispatchCommand_Notes_RoundTrip covers the five BOS-552 notes commands:
// each reaches the handler with its command intact and comes back as the
// matching CommandResult payload. DeleteNote is Ok-only with no payload,
// exactly like DeleteGithubCallback.
func TestDispatchCommand_Notes_RoundTrip(t *testing.T) {
	sessionID, chatID := "sess-1", "chat-1"
	body, search := "edited body", "needle"

	cases := []struct {
		name    string
		handler *fakeCommandHandler
		cmd     *pb.OrchestratorCommand
		verify  func(t *testing.T, h *fakeCommandHandler, r *pb.CommandResult)
	}{
		{
			name:    "create",
			handler: &fakeCommandHandler{createNoteResult: &pb.CreateNoteResponse{Note: &pb.Note{Id: "n-1", Body: "hello"}}},
			cmd: &pb.OrchestratorCommand{
				CommandId: "c-note-create",
				Cmd: &pb.OrchestratorCommand_CreateNote{CreateNote: &pb.CreateNoteCommand{
					RepoId:    "repo-1",
					SessionId: &sessionID,
					ChatId:    &chatID,
					Body:      "hello",
					Tags:      []string{"a", "b"},
				}},
			},
			verify: func(t *testing.T, h *fakeCommandHandler, r *pb.CommandResult) {
				t.Helper()
				got := h.createNoteCmd
				if got == nil || got.GetRepoId() != "repo-1" || got.GetBody() != "hello" ||
					got.GetSessionId() != "sess-1" || got.GetChatId() != "chat-1" ||
					len(got.GetTags()) != 2 || got.GetTags()[0] != "a" {
					t.Fatalf("create_note command not forwarded verbatim: %+v", got)
				}
				if r.GetCreateNote().GetNote().GetId() != "n-1" || r.GetCreateNote().GetNote().GetBody() != "hello" {
					t.Fatalf("unexpected create_note payload: %+v", r.GetCreateNote())
				}
			},
		},
		{
			name:    "get",
			handler: &fakeCommandHandler{getNoteResult: &pb.GetNoteResponse{Note: &pb.Note{Id: "n-2", Body: "read me"}}},
			cmd: &pb.OrchestratorCommand{
				CommandId: "c-note-get",
				Cmd:       &pb.OrchestratorCommand_GetNote{GetNote: &pb.GetNoteCommand{Id: "n-2"}},
			},
			verify: func(t *testing.T, h *fakeCommandHandler, r *pb.CommandResult) {
				t.Helper()
				if h.getNoteCmd.GetId() != "n-2" {
					t.Fatalf("get_note id = %q, want n-2", h.getNoteCmd.GetId())
				}
				if r.GetGetNote().GetNote().GetBody() != "read me" {
					t.Fatalf("unexpected get_note payload: %+v", r.GetGetNote())
				}
			},
		},
		{
			name: "list",
			handler: &fakeCommandHandler{listNotesResult: &pb.ListNotesResponse{
				Notes: []*pb.Note{{Id: "n-3"}, {Id: "n-4"}},
			}},
			cmd: &pb.OrchestratorCommand{
				CommandId: "c-note-list",
				Cmd: &pb.OrchestratorCommand_ListNotes{ListNotes: &pb.ListNotesCommand{
					RepoId:    strPtr("repo-1"),
					SessionId: &sessionID,
					ChatId:    &chatID,
					Tags:      []string{"x"},
					Search:    &search,
					Limit:     7,
				}},
			},
			verify: func(t *testing.T, h *fakeCommandHandler, r *pb.CommandResult) {
				t.Helper()
				got := h.listNotesCmd
				// Every filter, including the limit, must reach the handler
				// intact — a dropped limit silently unbounds a fleet list.
				if got == nil || got.GetRepoId() != "repo-1" || got.GetSessionId() != "sess-1" ||
					got.GetChatId() != "chat-1" || got.GetSearch() != "needle" || got.GetLimit() != 7 ||
					len(got.GetTags()) != 1 || got.GetTags()[0] != "x" {
					t.Fatalf("list_notes filters not forwarded verbatim: %+v", got)
				}
				if notes := r.GetListNotes().GetNotes(); len(notes) != 2 || notes[1].GetId() != "n-4" {
					t.Fatalf("unexpected list_notes payload: %+v", notes)
				}
			},
		},
		{
			name:    "update",
			handler: &fakeCommandHandler{updateNoteResult: &pb.UpdateNoteResponse{Note: &pb.Note{Id: "n-5", Body: "edited body"}}},
			cmd: &pb.OrchestratorCommand{
				CommandId: "c-note-update",
				Cmd: &pb.OrchestratorCommand_UpdateNote{UpdateNote: &pb.UpdateNoteCommand{
					Id:   "n-5",
					Body: &body,
					Tags: &pb.NoteTagSet{Tags: []string{"kept"}},
				}},
			},
			verify: func(t *testing.T, h *fakeCommandHandler, r *pb.CommandResult) {
				t.Helper()
				got := h.updateNoteCmd
				if got == nil || got.GetId() != "n-5" || got.Body == nil || got.GetBody() != "edited body" ||
					got.Tags == nil || len(got.GetTags().GetTags()) != 1 {
					t.Fatalf("update_note command not forwarded verbatim: %+v", got)
				}
				if r.GetUpdateNote().GetNote().GetBody() != "edited body" {
					t.Fatalf("unexpected update_note payload: %+v", r.GetUpdateNote())
				}
			},
		},
		{
			name:    "delete",
			handler: &fakeCommandHandler{},
			cmd: &pb.OrchestratorCommand{
				CommandId: "c-note-delete",
				Cmd:       &pb.OrchestratorCommand_DeleteNote{DeleteNote: &pb.DeleteNoteCommand{Id: "n-6"}},
			},
			verify: func(t *testing.T, h *fakeCommandHandler, r *pb.CommandResult) {
				t.Helper()
				if h.deleteNoteID != "n-6" {
					t.Fatalf("delete_note id = %q, want n-6", h.deleteNoteID)
				}
				// Ok-only: no payload, mirroring DeleteGithubCallback.
				if r.GetPayload() != nil {
					t.Fatalf("delete_note must reply Ok-only, got payload %+v", r.GetPayload())
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newDispatcherClient(tc.handler, nil, nil)
			out := make(chan *pb.DaemonEvent, 4)
			// Notes commands are store-bound and dispatched async, so the
			// result lands on outbound rather than the return value.
			if ev := client.dispatchCommand(context.Background(), tc.cmd, out); ev != nil {
				t.Fatalf("expected nil synchronous result for async notes command, got %+v", ev)
			}
			r := recvEvent(t, out).GetResult()
			if r == nil || !r.GetOk() {
				t.Fatalf("expected ok result, got %+v", r)
			}
			if r.GetCommandId() != tc.cmd.GetCommandId() {
				t.Fatalf("command_id = %q, want %q", r.GetCommandId(), tc.cmd.GetCommandId())
			}
			tc.verify(t, tc.handler, r)
		})
	}
}

// TestDispatchCommand_Notes_NotFound_ClassifiesCode pins the error mapping the
// notes dispatchers share with the callback dispatchers: a connect NotFound
// from the handler becomes CommandResult.error_code NOT_FOUND, not the
// UNSPECIFIED catch-all.
func TestDispatchCommand_Notes_NotFound_ClassifiesCode(t *testing.T) {
	cases := []struct {
		name string
		cmd  *pb.OrchestratorCommand
	}{
		{"create", &pb.OrchestratorCommand{CommandId: "e1", Cmd: &pb.OrchestratorCommand_CreateNote{CreateNote: &pb.CreateNoteCommand{RepoId: "r"}}}},
		{"get", &pb.OrchestratorCommand{CommandId: "e2", Cmd: &pb.OrchestratorCommand_GetNote{GetNote: &pb.GetNoteCommand{Id: "nope"}}}},
		{"list", &pb.OrchestratorCommand{CommandId: "e3", Cmd: &pb.OrchestratorCommand_ListNotes{ListNotes: &pb.ListNotesCommand{}}}},
		{"update", &pb.OrchestratorCommand{CommandId: "e4", Cmd: &pb.OrchestratorCommand_UpdateNote{UpdateNote: &pb.UpdateNoteCommand{Id: "nope"}}}},
		{"delete", &pb.OrchestratorCommand{CommandId: "e5", Cmd: &pb.OrchestratorCommand_DeleteNote{DeleteNote: &pb.DeleteNoteCommand{Id: "nope"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := &fakeCommandHandler{returnErr: connect.NewError(connect.CodeNotFound, fmt.Errorf("no such note"))}
			client := newDispatcherClient(handler, nil, nil)
			out := make(chan *pb.DaemonEvent, 4)
			client.dispatchCommand(context.Background(), tc.cmd, out)
			r := recvEvent(t, out).GetResult()
			if r == nil || r.GetOk() || !strings.Contains(r.GetError(), "no such note") {
				t.Fatalf("expected failed result mentioning the absent note, got %+v", r)
			}
			if r.GetErrorCode() != pb.CommandResult_ERROR_CODE_NOT_FOUND {
				t.Fatalf("error_code = %v, want NOT_FOUND", r.GetErrorCode())
			}
		})
	}
}

func TestDispatchCommand_NoteCommands_HandlerNotWired(t *testing.T) {
	client := newDispatcherClient(nil, nil, nil) // no command handler
	cases := []struct {
		name string
		cmd  *pb.OrchestratorCommand
	}{
		{"create", &pb.OrchestratorCommand{CommandId: "w1", Cmd: &pb.OrchestratorCommand_CreateNote{CreateNote: &pb.CreateNoteCommand{}}}},
		{"get", &pb.OrchestratorCommand{CommandId: "w2", Cmd: &pb.OrchestratorCommand_GetNote{GetNote: &pb.GetNoteCommand{}}}},
		{"list", &pb.OrchestratorCommand{CommandId: "w3", Cmd: &pb.OrchestratorCommand_ListNotes{ListNotes: &pb.ListNotesCommand{}}}},
		{"update", &pb.OrchestratorCommand{CommandId: "w4", Cmd: &pb.OrchestratorCommand_UpdateNote{UpdateNote: &pb.UpdateNoteCommand{}}}},
		{"delete", &pb.OrchestratorCommand{CommandId: "w5", Cmd: &pb.OrchestratorCommand_DeleteNote{DeleteNote: &pb.DeleteNoteCommand{}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := client.dispatchCommand(context.Background(), tc.cmd, make(chan *pb.DaemonEvent, 1))
			r := ev.GetResult()
			if r == nil || r.GetOk() || r.GetError() != "command handler not wired" {
				t.Fatalf("expected synchronous not-wired error, got %+v", ev)
			}
		})
	}
}

// TestDispatchCommand_Broadcast_ForwardsToHandler pins the INGRESS half of the
// cross-daemon broadcast path at the dispatcher boundary (BOS-558): a
// BroadcastCommand routed here by bosso reaches the handler with every field
// intact, and comes back as an Ok-only CommandResult with no payload (the
// receiving daemon reports "materialised", not what it resolved — target
// counts stay local, and the body never round-trips).
//
// Forwarded VERBATIM is the contract being pinned: the dispatcher makes no
// decision about an inbound broadcast. The loop guard, the idempotency probe
// and local-only resolution all live behind the handler in internal/broadcast.
func TestDispatchCommand_Broadcast_ForwardsToHandler(t *testing.T) {
	expires := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	handler := &fakeCommandHandler{}
	client := newDispatcherClient(handler, nil, nil)

	out := make(chan *pb.DaemonEvent, 4)
	cmd := &pb.OrchestratorCommand{
		CommandId: "c-bcast-1",
		Cmd: &pb.OrchestratorCommand_Broadcast{Broadcast: &pb.BroadcastCommand{
			BroadcastId: "b-1",
			Selector: &pb.BroadcastSelector{Clauses: []*pb.BroadcastSelectorClause{
				{RepoIds: []string{"repo-1"}},
			}},
			OriginDaemonId: "daemon-far",
			OriginChatId:   "chat-far",
			Message:        "secret body",
			ExpiresAt:      timestamppb.New(expires),
		}},
	}
	// Store-bound and dispatched async, so the result lands on outbound.
	if ev := client.dispatchCommand(context.Background(), cmd, out); ev != nil {
		t.Fatalf("expected nil synchronous result for the async broadcast command, got %+v", ev)
	}
	r := recvEvent(t, out).GetResult()
	if r == nil || !r.GetOk() {
		t.Fatalf("expected ok result, got %+v", r)
	}
	if r.GetCommandId() != "c-bcast-1" {
		t.Fatalf("command_id = %q, want c-bcast-1", r.GetCommandId())
	}
	if r.GetPayload() != nil {
		t.Fatalf("broadcast must reply Ok-only, got payload %+v", r.GetPayload())
	}
	got := handler.broadcastCmd
	if got == nil {
		t.Fatal("handler never saw the broadcast command")
	}
	if got.GetBroadcastId() != "b-1" {
		t.Fatalf("broadcast_id = %q, want b-1", got.GetBroadcastId())
	}
	if got.GetOriginDaemonId() != "daemon-far" || got.GetOriginChatId() != "chat-far" {
		t.Fatalf("origin ids not forwarded verbatim: %+v", got)
	}
	if got.GetMessage() != "secret body" {
		t.Fatalf("message = %q, want it forwarded verbatim", got.GetMessage())
	}
	if !got.GetExpiresAt().AsTime().Equal(expires) {
		t.Fatalf("expires_at = %v, want %v", got.GetExpiresAt().AsTime(), expires)
	}
	sel := got.GetSelector().GetClauses()
	if len(sel) != 1 || len(sel[0].GetRepoIds()) != 1 || sel[0].GetRepoIds()[0] != "repo-1" {
		t.Fatalf("selector not forwarded verbatim: %+v", got.GetSelector())
	}
}

// TestDispatchCommand_Broadcast_HandlerErrorFailsTheCommand pins that a
// materialisation failure comes back as a failed CommandResult carrying the
// handler's own message (so bosso can tell "too many targets" from "the write
// broke"), with the connect code classified rather than flattened.
//
// SECRET BODY: the error text asserted here rides back to bosso on
// CommandResult.error. It must stay derived from the handler's error alone —
// the ingress never puts the broadcast body in one, and this test is the place
// a change that started to would be noticed.
func TestDispatchCommand_Broadcast_HandlerErrorFailsTheCommand(t *testing.T) {
	handler := &fakeCommandHandler{returnErr: connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("selector resolves 99 targets, over the cap of 64"))}
	client := newDispatcherClient(handler, nil, nil)

	out := make(chan *pb.DaemonEvent, 4)
	cmd := &pb.OrchestratorCommand{
		CommandId: "c-bcast-err",
		Cmd: &pb.OrchestratorCommand_Broadcast{Broadcast: &pb.BroadcastCommand{
			BroadcastId: "b-2",
			Message:     "secret body",
		}},
	}
	client.dispatchCommand(context.Background(), cmd, out)
	r := recvEvent(t, out).GetResult()
	if r == nil || r.GetOk() {
		t.Fatalf("expected a failed result, got %+v", r)
	}
	if !strings.Contains(r.GetError(), "over the cap of 64") {
		t.Fatalf("error = %q, want the handler's own message", r.GetError())
	}
	if strings.Contains(r.GetError(), "secret body") {
		t.Fatalf("the broadcast body leaked into CommandResult.error: %q", r.GetError())
	}
	if r.GetErrorCode() != pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("error_code = %v, want FAILED_PRECONDITION", r.GetErrorCode())
	}
}

func TestDispatchCommand_Broadcast_HandlerNotWired(t *testing.T) {
	client := newDispatcherClient(nil, nil, nil) // no command handler
	ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "w-bcast",
		Cmd:       &pb.OrchestratorCommand_Broadcast{Broadcast: &pb.BroadcastCommand{}},
	}, make(chan *pb.DaemonEvent, 1))
	r := ev.GetResult()
	if r == nil || r.GetOk() || r.GetError() != "command handler not wired" {
		t.Fatalf("expected synchronous not-wired error, got %+v", ev)
	}
}

// TestClassifyBroadcastCommandErrorIsScopedToBroadcast pins the API-compatibility
// boundary BOS-558 deliberately drew: the INVALID_ARGUMENT arm belongs to the
// broadcast command ALONE.
//
// The shared classifier is reached by every other inbound command, and most of
// them delegate to a bossd server handler that already returns
// connect.CodeInvalidArgument for ordinary validation failures. Those classify
// as UNSPECIFIED, which bosso renders as CodeAborted. Teaching the shared
// classifier the arm would silently re-render all of them as CodeInvalidArgument
// on the bossanova.v1 surface — a change in the meaning of an existing response,
// which needs an apiversion bump plus a down-convert transform. This test fails
// the moment someone widens it, so that change cannot ride in unversioned.
func TestClassifyBroadcastCommandErrorIsScopedToBroadcast(t *testing.T) {
	t.Parallel()

	invalid := connect.NewError(connect.CodeInvalidArgument, errors.New("malformed"))

	if got := classifyBroadcastCommandError(invalid); got != pb.CommandResult_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("broadcast classifier: want ERROR_CODE_INVALID_ARGUMENT, got %v", got)
	}
	// The api-compat pin: unchanged for every non-broadcast command.
	if got := classifyCommandError(invalid); got != pb.CommandResult_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("shared classifier must stay UNSPECIFIED for CodeInvalidArgument "+
			"(widening it is an unversioned bossanova.v1 behaviour change); got %v", got)
	}
	// The arms both classifiers share must keep agreeing.
	for _, tc := range []struct {
		name string
		err  error
		want pb.CommandResult_ErrorCode
	}{
		{"not found", connect.NewError(connect.CodeNotFound, errors.New("gone")), pb.CommandResult_ERROR_CODE_NOT_FOUND},
		{"failed precondition", connect.NewError(connect.CodeFailedPrecondition, errors.New("bad state")), pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION},
		{"untyped", errors.New("boom"), pb.CommandResult_ERROR_CODE_UNSPECIFIED},
	} {
		if got := classifyBroadcastCommandError(tc.err); got != tc.want {
			t.Errorf("%s: broadcast classifier got %v, want %v", tc.name, got, tc.want)
		}
		if got := classifyCommandError(tc.err); got != tc.want {
			t.Errorf("%s: shared classifier got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestClassifySwitchCommandErrorIsScopedToSwitch pins the API-compatibility
// boundary BOS-947 and BOS-958 deliberately drew, mirroring
// TestClassifyBroadcastCommandErrorIsScopedToBroadcast above: the
// DEADLINE_EXCEEDED and CANCELED arms belong to the switch command ALONE.
//
// A deadline or cancellation reads as verb-independent, which is exactly what
// makes widening the shared classifier tempting. But the shared classifier is
// reached by every other inbound command, and any of those handlers can surface
// connect.CodeDeadlineExceeded or connect.CodeCanceled from a context ending — a
// slow upstream call, a cancelled parent. Those classify as UNSPECIFIED, which
// bosso renders as CodeAborted. Teaching the shared classifier either arm would
// silently re-render all of them on the bossanova.v1 surface, a change in the
// meaning of an existing response that needs its own apiversion bump plus a
// down-convert transform.
//
// V20260820 / SwitchDeadlineCodeChange and V20260821 /
// SwitchCanceledCodeChange version these changes for the switch and for nothing
// else. This test fails the moment someone widens them, so the next such change
// cannot ride in unversioned.
func TestClassifySwitchCommandErrorIsScopedToSwitch(t *testing.T) {
	t.Parallel()

	deadline := connect.NewError(connect.CodeDeadlineExceeded, errors.New("switch respawn budget exhausted"))
	canceled := connect.NewError(connect.CodeCanceled, errors.New("context canceled"))

	if got := classifySwitchCommandError(deadline); got != pb.CommandResult_ERROR_CODE_DEADLINE_EXCEEDED {
		t.Fatalf("switch classifier: want ERROR_CODE_DEADLINE_EXCEEDED, got %v", got)
	}
	if got := classifySwitchCommandError(canceled); got != pb.CommandResult_ERROR_CODE_CANCELED {
		t.Fatalf("switch classifier: want ERROR_CODE_CANCELED, got %v", got)
	}
	// The api-compat pin: unchanged for every non-switch command.
	if got := classifyCommandError(deadline); got != pb.CommandResult_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("shared classifier must stay UNSPECIFIED for CodeDeadlineExceeded "+
			"(widening it is an unversioned bossanova.v1 behaviour change); got %v", got)
	}
	if got := classifyCommandError(canceled); got != pb.CommandResult_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("shared classifier must stay UNSPECIFIED for CodeCanceled "+
			"(widening it is an unversioned bossanova.v1 behaviour change); got %v", got)
	}
	// The broadcast classifier is a sibling scope, not a superset: it must not
	// have acquired the deadline or cancellation arms either.
	if got := classifyBroadcastCommandError(deadline); got != pb.CommandResult_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("broadcast classifier must stay UNSPECIFIED for CodeDeadlineExceeded; got %v", got)
	}
	if got := classifyBroadcastCommandError(canceled); got != pb.CommandResult_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("broadcast classifier must stay UNSPECIFIED for CodeCanceled; got %v", got)
	}
	// Conversely, the switch classifier must not have acquired the broadcast's
	// INVALID_ARGUMENT arm — each scope carries exactly its own.
	invalid := connect.NewError(connect.CodeInvalidArgument, errors.New("malformed"))
	if got := classifySwitchCommandError(invalid); got != pb.CommandResult_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("switch classifier must stay UNSPECIFIED for CodeInvalidArgument; got %v", got)
	}
	// The arms both classifiers share must keep agreeing.
	for _, tc := range []struct {
		name string
		err  error
		want pb.CommandResult_ErrorCode
	}{
		{"not found", connect.NewError(connect.CodeNotFound, errors.New("gone")), pb.CommandResult_ERROR_CODE_NOT_FOUND},
		{"failed precondition", connect.NewError(connect.CodeFailedPrecondition, errors.New("bad state")), pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION},
		{"unmapped code", connect.NewError(connect.CodeResourceExhausted, errors.New("quota")), pb.CommandResult_ERROR_CODE_UNSPECIFIED},
		{"untyped", errors.New("boom"), pb.CommandResult_ERROR_CODE_UNSPECIFIED},
	} {
		if got := classifySwitchCommandError(tc.err); got != tc.want {
			t.Errorf("%s: switch classifier got %v, want %v", tc.name, got, tc.want)
		}
		if got := classifyCommandError(tc.err); got != tc.want {
			t.Errorf("%s: shared classifier got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSwitchCommandResultForTestUsesProductionSwitchFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantCode   pb.CommandResult_ErrorCode
		wantText   string
		forbidText string
	}{
		{
			name:     "deadline",
			err:      connect.NewError(connect.CodeDeadlineExceeded, errors.New("context deadline exceeded")),
			wantCode: pb.CommandResult_ERROR_CODE_DEADLINE_EXCEEDED,
			wantText: "switch session account: deadline_exceeded: context deadline exceeded",
		},
		{
			name:       "non deadline",
			err:        connect.NewError(connect.CodeFailedPrecondition, errors.New("cooling")),
			wantCode:   pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION,
			wantText:   "switch session account: failed_precondition: cooling",
			forbidText: "deadline",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := SwitchCommandResultForTest("cmd-switch", tt.err)
			if got.GetOk() {
				t.Fatal("result ok = true, want failure")
			}
			if got.GetCommandId() != "cmd-switch" {
				t.Fatalf("command id = %q, want cmd-switch", got.GetCommandId())
			}
			if got.GetErrorCode() != tt.wantCode {
				t.Fatalf("error code = %v, want %v", got.GetErrorCode(), tt.wantCode)
			}
			if !strings.Contains(got.GetError(), tt.wantText) {
				t.Fatalf("error = %q, want %q", got.GetError(), tt.wantText)
			}
			if tt.forbidText != "" && strings.Contains(got.GetError(), tt.forbidText) {
				t.Fatalf("error = %q, did not expect %q", got.GetError(), tt.forbidText)
			}
		})
	}
}
