package bossmcp

import (
	"context"
	"errors"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

var errNotImpl = errors.New("not implemented in fake")

// fakeBackend implements Backend. Each method delegates to an optional func
// field; unset fields return errNotImpl so tests opt into exactly the ops they
// exercise.
type fakeBackend struct {
	ping                 func(ctx context.Context) error
	resolveContext       func(ctx context.Context, workingDir string) (*pb.ResolveContextResponse, error)
	validateRepoPath     func(ctx context.Context, localPath string) (*pb.ValidateRepoPathResponse, error)
	registerRepo         func(ctx context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error)
	cloneAndRegisterRepo func(ctx context.Context, req *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error)
	listRepos            func(ctx context.Context) ([]*pb.Repo, error)
	removeRepo           func(ctx context.Context, id string) error
	updateRepo           func(ctx context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error)
	listRepoPRs          func(ctx context.Context, repoID string) ([]*pb.PRSummary, error)
	listTrackerIssues    func(ctx context.Context, repoID, query, source string) ([]*pb.TrackerIssue, error)
	createSession        func(ctx context.Context, req *pb.CreateSessionRequest) (*CreateSessionResult, error)
	getSession           func(ctx context.Context, id string) (*pb.Session, error)
	listSessions         func(ctx context.Context, req *pb.ListSessionsRequest) ([]*pb.Session, error)
	stopSession          func(ctx context.Context, id string) (*pb.Session, error)
	pauseSession         func(ctx context.Context, id string) (*pb.Session, error)
	resumeSession        func(ctx context.Context, id string) (*pb.Session, error)
	retrySession         func(ctx context.Context, id string) (*pb.Session, error)
	closeSession         func(ctx context.Context, id string) (*pb.Session, error)
	mergeSession         func(ctx context.Context, id string) (*pb.Session, error)
	removeSession        func(ctx context.Context, id string) error
	updateSession        func(ctx context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error)
	linkSessionPR        func(ctx context.Context, id, pr string) (*pb.Session, error)
	archiveSession       func(ctx context.Context, id string) (*pb.Session, error)
	resurrectSession     func(ctx context.Context, id string) (*pb.Session, error)
	emptyTrash           func(ctx context.Context, req *pb.EmptyTrashRequest) (int32, error)
	recordChat           func(ctx context.Context, sessionID, agentSessionID, title, agentName string, resume bool) (*pb.ClaudeChat, error)
	listChats            func(ctx context.Context, sessionID string) ([]*pb.ClaudeChat, error)
	updateChatTitle      func(ctx context.Context, agentSessionID, title string) error
	deleteChat           func(ctx context.Context, agentSessionID string) error
	wakeChat             func(ctx context.Context, sessionID, agentSessionID string, forceFresh bool) (*pb.WakeChatResponse, error)
	reportChatStatus     func(ctx context.Context, statuses []*pb.ChatStatusReport) error
	getChatStatuses      func(ctx context.Context, sessionID string) ([]*pb.ChatStatusEntry, error)
	getSessionStatuses   func(ctx context.Context, sessionIDs []string) ([]*pb.SessionStatusEntry, error)
	getChatTranscript    func(ctx context.Context, req *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error)
	sendChatMessage      func(ctx context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error)
	createCronJob        func(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error)
	listCronJobs         func(ctx context.Context) ([]*pb.CronJob, error)
	getCronJob           func(ctx context.Context, id string) (*pb.CronJob, error)
	updateCronJob        func(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error)
	deleteCronJob        func(ctx context.Context, id string) error
	runCronJobNow        func(ctx context.Context, id string) (*pb.RunCronJobNowResponse, error)
	listAccounts         func(ctx context.Context, provider string, refresh bool) ([]*pb.Account, error)
	addAccount           func(ctx context.Context, req *pb.AddAccountRequest) (*pb.Account, error)
	refreshAccount       func(ctx context.Context, req *pb.RefreshAccountRequest) (*pb.RefreshAccountResponse, error)
	updateAccount        func(ctx context.Context, req *pb.UpdateAccountRequest) (*pb.Account, error)
	removeAccount        func(ctx context.Context, id string) error
	testAccount          func(ctx context.Context, id string) (*pb.TestAccountResponse, error)
	switchSessionAccount func(ctx context.Context, req *pb.SwitchSessionAccountRequest) (*pb.SwitchSessionAccountResponse, error)
	listCheckSnapshots   func(ctx context.Context, sessionID string, limit int32) (*pb.ListCheckSnapshotsResponse, error)
	repairDoctor         func(ctx context.Context) (*pb.RepairDoctorResponse, error)
	startRepairWorkflow  func(ctx context.Context) (*pb.StartRepairWorkflowResponse, error)
	listAgents           func(ctx context.Context) ([]*pb.AgentInfo, error)
	listPlugins          func(ctx context.Context) ([]*pb.InstalledPlugin, error)
	getSettings          func(ctx context.Context) (*pb.GlobalSettings, error)
	updateSettings       func(ctx context.Context, req *pb.UpdateSettingsRequest) (*pb.GlobalSettings, error)
}

func (f *fakeBackend) Ping(ctx context.Context) error {
	if f.ping != nil {
		return f.ping(ctx)
	}
	return errNotImpl
}

func (f *fakeBackend) ResolveContext(ctx context.Context, workingDir string) (*pb.ResolveContextResponse, error) {
	if f.resolveContext != nil {
		return f.resolveContext(ctx, workingDir)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ValidateRepoPath(ctx context.Context, localPath string) (*pb.ValidateRepoPathResponse, error) {
	if f.validateRepoPath != nil {
		return f.validateRepoPath(ctx, localPath)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) RegisterRepo(ctx context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
	if f.registerRepo != nil {
		return f.registerRepo(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) CloneAndRegisterRepo(ctx context.Context, req *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
	if f.cloneAndRegisterRepo != nil {
		return f.cloneAndRegisterRepo(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ListRepos(ctx context.Context) ([]*pb.Repo, error) {
	if f.listRepos != nil {
		return f.listRepos(ctx)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) RemoveRepo(ctx context.Context, id string) error {
	if f.removeRepo != nil {
		return f.removeRepo(ctx, id)
	}
	return errNotImpl
}

func (f *fakeBackend) UpdateRepo(ctx context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
	if f.updateRepo != nil {
		return f.updateRepo(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ListRepoPRs(ctx context.Context, repoID string) ([]*pb.PRSummary, error) {
	if f.listRepoPRs != nil {
		return f.listRepoPRs(ctx, repoID)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ListTrackerIssues(ctx context.Context, repoID, query, source string) ([]*pb.TrackerIssue, error) {
	if f.listTrackerIssues != nil {
		return f.listTrackerIssues(ctx, repoID, query, source)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*CreateSessionResult, error) {
	if f.createSession != nil {
		return f.createSession(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) GetSession(ctx context.Context, id string) (*pb.Session, error) {
	if f.getSession != nil {
		return f.getSession(ctx, id)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) ([]*pb.Session, error) {
	if f.listSessions != nil {
		return f.listSessions(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) StopSession(ctx context.Context, id string) (*pb.Session, error) {
	if f.stopSession != nil {
		return f.stopSession(ctx, id)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) PauseSession(ctx context.Context, id string) (*pb.Session, error) {
	if f.pauseSession != nil {
		return f.pauseSession(ctx, id)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ResumeSession(ctx context.Context, id string) (*pb.Session, error) {
	if f.resumeSession != nil {
		return f.resumeSession(ctx, id)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) RetrySession(ctx context.Context, id string) (*pb.Session, error) {
	if f.retrySession != nil {
		return f.retrySession(ctx, id)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) CloseSession(ctx context.Context, id string) (*pb.Session, error) {
	if f.closeSession != nil {
		return f.closeSession(ctx, id)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) MergeSession(ctx context.Context, id string) (*pb.Session, error) {
	if f.mergeSession != nil {
		return f.mergeSession(ctx, id)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) RemoveSession(ctx context.Context, id string) error {
	if f.removeSession != nil {
		return f.removeSession(ctx, id)
	}
	return errNotImpl
}

func (f *fakeBackend) UpdateSession(ctx context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error) {
	if f.updateSession != nil {
		return f.updateSession(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) LinkSessionPR(ctx context.Context, id, pr string) (*pb.Session, error) {
	if f.linkSessionPR != nil {
		return f.linkSessionPR(ctx, id, pr)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ArchiveSession(ctx context.Context, id string) (*pb.Session, error) {
	if f.archiveSession != nil {
		return f.archiveSession(ctx, id)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ResurrectSession(ctx context.Context, id string) (*pb.Session, error) {
	if f.resurrectSession != nil {
		return f.resurrectSession(ctx, id)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) EmptyTrash(ctx context.Context, req *pb.EmptyTrashRequest) (int32, error) {
	if f.emptyTrash != nil {
		return f.emptyTrash(ctx, req)
	}
	return 0, errNotImpl
}

func (f *fakeBackend) RecordChat(ctx context.Context, sessionID, agentSessionID, title, agentName string, resume bool) (*pb.ClaudeChat, error) {
	if f.recordChat != nil {
		return f.recordChat(ctx, sessionID, agentSessionID, title, agentName, resume)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ListChats(ctx context.Context, sessionID string) ([]*pb.ClaudeChat, error) {
	if f.listChats != nil {
		return f.listChats(ctx, sessionID)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) UpdateChatTitle(ctx context.Context, agentSessionID, title string) error {
	if f.updateChatTitle != nil {
		return f.updateChatTitle(ctx, agentSessionID, title)
	}
	return errNotImpl
}

func (f *fakeBackend) DeleteChat(ctx context.Context, agentSessionID string) error {
	if f.deleteChat != nil {
		return f.deleteChat(ctx, agentSessionID)
	}
	return errNotImpl
}

func (f *fakeBackend) WakeChat(ctx context.Context, sessionID, agentSessionID string, forceFresh bool) (*pb.WakeChatResponse, error) {
	if f.wakeChat != nil {
		return f.wakeChat(ctx, sessionID, agentSessionID, forceFresh)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ReportChatStatus(ctx context.Context, statuses []*pb.ChatStatusReport) error {
	if f.reportChatStatus != nil {
		return f.reportChatStatus(ctx, statuses)
	}
	return errNotImpl
}

func (f *fakeBackend) GetChatStatuses(ctx context.Context, sessionID string) ([]*pb.ChatStatusEntry, error) {
	if f.getChatStatuses != nil {
		return f.getChatStatuses(ctx, sessionID)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) GetSessionStatuses(ctx context.Context, sessionIDs []string) ([]*pb.SessionStatusEntry, error) {
	if f.getSessionStatuses != nil {
		return f.getSessionStatuses(ctx, sessionIDs)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) GetChatTranscript(ctx context.Context, req *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	if f.getChatTranscript != nil {
		return f.getChatTranscript(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) SendChatMessage(ctx context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	if f.sendChatMessage != nil {
		return f.sendChatMessage(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	if f.createCronJob != nil {
		return f.createCronJob(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ListCronJobs(ctx context.Context) ([]*pb.CronJob, error) {
	if f.listCronJobs != nil {
		return f.listCronJobs(ctx)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) GetCronJob(ctx context.Context, id string) (*pb.CronJob, error) {
	if f.getCronJob != nil {
		return f.getCronJob(ctx, id)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	if f.updateCronJob != nil {
		return f.updateCronJob(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) DeleteCronJob(ctx context.Context, id string) error {
	if f.deleteCronJob != nil {
		return f.deleteCronJob(ctx, id)
	}
	return errNotImpl
}

func (f *fakeBackend) RunCronJobNow(ctx context.Context, id string) (*pb.RunCronJobNowResponse, error) {
	if f.runCronJobNow != nil {
		return f.runCronJobNow(ctx, id)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ListAccounts(ctx context.Context, provider string, refresh bool) ([]*pb.Account, error) {
	if f.listAccounts != nil {
		return f.listAccounts(ctx, provider, refresh)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) AddAccount(ctx context.Context, req *pb.AddAccountRequest) (*pb.Account, error) {
	if f.addAccount != nil {
		return f.addAccount(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) UpdateAccount(ctx context.Context, req *pb.UpdateAccountRequest) (*pb.Account, error) {
	if f.updateAccount != nil {
		return f.updateAccount(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) RefreshAccount(ctx context.Context, req *pb.RefreshAccountRequest) (*pb.RefreshAccountResponse, error) {
	if f.refreshAccount != nil {
		return f.refreshAccount(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) RemoveAccount(ctx context.Context, id string) error {
	if f.removeAccount != nil {
		return f.removeAccount(ctx, id)
	}
	return errNotImpl
}

func (f *fakeBackend) TestAccount(ctx context.Context, id string) (*pb.TestAccountResponse, error) {
	if f.testAccount != nil {
		return f.testAccount(ctx, id)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) SwitchSessionAccount(ctx context.Context, req *pb.SwitchSessionAccountRequest) (*pb.SwitchSessionAccountResponse, error) {
	if f.switchSessionAccount != nil {
		return f.switchSessionAccount(ctx, req)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ListCheckSnapshots(ctx context.Context, sessionID string, limit int32) (*pb.ListCheckSnapshotsResponse, error) {
	if f.listCheckSnapshots != nil {
		return f.listCheckSnapshots(ctx, sessionID, limit)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) RepairDoctor(ctx context.Context) (*pb.RepairDoctorResponse, error) {
	if f.repairDoctor != nil {
		return f.repairDoctor(ctx)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) StartRepairWorkflow(ctx context.Context) (*pb.StartRepairWorkflowResponse, error) {
	if f.startRepairWorkflow != nil {
		return f.startRepairWorkflow(ctx)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ListAgents(ctx context.Context) ([]*pb.AgentInfo, error) {
	if f.listAgents != nil {
		return f.listAgents(ctx)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) ListPlugins(ctx context.Context) ([]*pb.InstalledPlugin, error) {
	if f.listPlugins != nil {
		return f.listPlugins(ctx)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) GetSettings(ctx context.Context) (*pb.GlobalSettings, error) {
	if f.getSettings != nil {
		return f.getSettings(ctx)
	}
	return nil, errNotImpl
}

func (f *fakeBackend) UpdateSettings(ctx context.Context, req *pb.UpdateSettingsRequest) (*pb.GlobalSettings, error) {
	if f.updateSettings != nil {
		return f.updateSettings(ctx, req)
	}
	return nil, errNotImpl
}

// compile-time assertion that the fake satisfies Backend.
var _ Backend = (*fakeBackend)(nil)
