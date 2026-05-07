package plugin

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// waitAgentRunPollInterval is how often WaitAgentRun polls the loaded
// AgentRunner plugin's ExitStatus. Picked to balance responsiveness with
// idle CPU cost; the repair plugin's only consumer is single-shot per repair.
const waitAgentRunPollInterval = 500 * time.Millisecond

// HostServiceServer implements the HostService gRPC server on the daemon
// side. Plugins call back to this server via go-plugin's GRPCBroker to
// query VCS data and session state.
type HostServiceServer struct {
	provider       vcs.Provider
	sessionStore   db.SessionStore
	agentChats     db.AgentChatStore
	repoStore      db.RepoStore
	displayTracker *status.DisplayTracker
	chatTracker    *status.Tracker

	// agentClients is the per-name registry of AgentRunnerClient gRPC
	// clients (one per loaded AgentRunner plugin, keyed by the plugin's
	// configured Name — matching session.AgentName). StartAgentRun /
	// WaitAgentRun look up the right client based on the session record's
	// AgentName so non-agent plugins (like bossd-plugin-repair) don't need
	// a direct broker channel into each agent plugin's process. An empty
	// map means no AgentRunner plugins are loaded; per-handler nil guards
	// surface that as FailedPrecondition.
	agentClients map[string]agent.AgentRunnerClient
	// agentLogsDir is the bossd-owned directory where the agent plugin
	// writes per-session NDJSON log files. Forwarded into StartRun.LogPath.
	agentLogsDir string

	// activeRuns maps session_id → activeRun for the currently-active
	// repair run on each session. Guarded by runMu. The repair plugin's
	// in-memory dedup map provides cross-call deduplication; this map is
	// the daemon's last-line defense against two concurrent StartAgentRun
	// calls landing on the same session.
	//
	// agentSessionByID is the reverse index from agent_session_id back to
	// the agent plugin name that started it — needed by WaitAgentRun, which
	// only receives the agent_session_id and must route the ExitStatus poll
	// to the right plugin client. Populated atomically with activeRuns.
	runMu            sync.Mutex
	activeRuns       map[string]activeRun
	agentSessionByID map[string]string
}

// activeRun records the agent plugin name and agent session ID for an
// in-flight StartAgentRun on a given boss session. agentName lets
// WaitAgentRun and isAgentRunning route the follow-up RPCs to the right
// plugin client when multiple agent plugins are loaded.
type activeRun struct {
	agentName      string
	agentSessionID string
}

// NewHostServiceServer creates a HostServiceServer that proxies to the
// given VCS provider. Session-related functionality requires SetSessionDeps
// to be called before use; agent execution (StartAgentRun/WaitAgentRun)
// requires SetAgentClients + SetAgentLogsDir.
func NewHostServiceServer(provider vcs.Provider) *HostServiceServer {
	return &HostServiceServer{
		provider:         provider,
		activeRuns:       make(map[string]activeRun),
		agentSessionByID: make(map[string]string),
		agentClients:     map[string]agent.AgentRunnerClient{},
	}
}

// SetSessionDeps injects the dependencies needed for session-related RPCs
// (ListSessions, GetReviewComments, FireSessionEvent). The chats store is
// needed to surface per-session HasActiveChat in ListSessions.
func (s *HostServiceServer) SetSessionDeps(repos db.RepoStore, sessions db.SessionStore, chats db.AgentChatStore, tracker *status.DisplayTracker, chatTracker *status.Tracker) {
	s.repoStore = repos
	s.sessionStore = sessions
	s.agentChats = chats
	s.displayTracker = tracker
	s.chatTracker = chatTracker
}

// SetAgentClients injects the per-name registry of AgentRunnerClient gRPC
// clients (one per loaded AgentRunner plugin, keyed by plugin Name —
// matching session.AgentName). The repair plugin's StartAgentRun /
// WaitAgentRun host RPCs look up the right client based on the session
// record's AgentName so non-agent plugins don't need a direct broker
// channel into each agent plugin. A nil or empty map disables the agent
// path entirely; per-handler nil guards surface that as FailedPrecondition
// when a plugin actually tries to use it.
//
// Concurrency: called exactly once during daemon startup, before plugins
// begin issuing host RPCs. Not safe for concurrent re-injection.
func (s *HostServiceServer) SetAgentClients(m map[string]agent.AgentRunnerClient) {
	if m == nil {
		s.agentClients = map[string]agent.AgentRunnerClient{}
		return
	}
	s.agentClients = m
}

// SetAgentLogsDir sets the bossd-owned directory where the agent plugin
// writes per-session NDJSON log files. Required alongside SetAgentClients.
func (s *HostServiceServer) SetAgentLogsDir(dir string) {
	s.agentLogsDir = dir
}

// AgentClientNames returns the registered agent plugin names (the keys of
// agentClients). Read-only; used by RepairDoctor to verify a "claude" entry
// is wired without exposing the underlying clients.
func (s *HostServiceServer) AgentClientNames() []string {
	out := make([]string, 0, len(s.agentClients))
	for name := range s.agentClients {
		out = append(out, name)
	}
	return out
}

// AgentLogsDir returns the directory where per-session NDJSON log files
// land. Empty string if SetAgentLogsDir hasn't been called.
func (s *HostServiceServer) AgentLogsDir() string {
	return s.agentLogsDir
}

// Validate reports any dependencies that SetSessionDeps was expected to
// install but that are still nil. Host.Start calls this before launching
// plugins so a missing dep fails the daemon loudly at startup instead of
// surfacing later as an "Unavailable" RPC the first time a plugin tries to
// call back. The per-handler nil guards are kept as defense-in-depth for
// anyone using HostServiceServer directly (e.g. tests).
func (s *HostServiceServer) Validate() error {
	var missing []string
	if s.provider == nil {
		missing = append(missing, "vcs provider")
	}
	if s.sessionStore == nil {
		missing = append(missing, "session store")
	}
	if s.agentChats == nil {
		missing = append(missing, "agent chat store")
	}
	if s.repoStore == nil {
		missing = append(missing, "repo store")
	}
	if s.displayTracker == nil {
		missing = append(missing, "PR tracker")
	}
	if s.chatTracker == nil {
		missing = append(missing, "chat tracker")
	}
	// agentClients / agentLogsDir are intentionally not validated here:
	// they're injected post-Start (main.go calls SetAgentClients after the
	// AgentRunner plugins have been dispensed). Per-handler nil guards in
	// StartAgentRun / WaitAgentRun surface the missing dep at call time.
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("host service missing dependencies: %s", strings.Join(missing, ", "))
}

// hostServiceDesc is a manually-built gRPC service descriptor for
// HostService. We build this manually because the project uses
// connect-go (not protoc-gen-go-grpc) for code generation, so there
// is no generated _ServiceDesc. go-plugin requires raw gRPC registration.
var hostServiceDesc = grpc.ServiceDesc{
	ServiceName: "bossanova.v1.HostService",
	HandlerType: (*hostServiceHandler)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "ListOpenPRs",
			Handler:    hostServiceListOpenPRsHandler,
		},
		{
			MethodName: "GetCheckResults",
			Handler:    hostServiceGetCheckResultsHandler,
		},
		{
			MethodName: "GetPRStatus",
			Handler:    hostServiceGetPRStatusHandler,
		},
		{
			MethodName: "ListClosedPRs",
			Handler:    hostServiceListClosedPRsHandler,
		},
		{
			MethodName: "ListSessions",
			Handler:    hostServiceListSessionsHandler,
		},
		{
			MethodName: "GetReviewComments",
			Handler:    hostServiceGetReviewCommentsHandler,
		},
		{
			MethodName: "FireSessionEvent",
			Handler:    hostServiceFireSessionEventHandler,
		},
		{
			MethodName: "SetRepairStatus",
			Handler:    hostServiceSetRepairStatusHandler,
		},
		{
			MethodName: "StartAgentRun",
			Handler:    hostServiceStartAgentRunHandler,
		},
		{
			MethodName: "WaitAgentRun",
			Handler:    hostServiceWaitAgentRunHandler,
		},
		{
			MethodName: "RecordRepairOutcome",
			Handler:    hostServiceRecordRepairOutcomeHandler,
		},
	},
	Metadata: "bossanova/v1/host_service.proto",
}

// hostServiceHandler is the interface that the gRPC service descriptor
// expects. HostServiceServer implements it.
type hostServiceHandler interface {
	ListOpenPRs(context.Context, *bossanovav1.ListOpenPRsRequest) (*bossanovav1.ListOpenPRsResponse, error)
	GetCheckResults(context.Context, *bossanovav1.GetCheckResultsRequest) (*bossanovav1.GetCheckResultsResponse, error)
	GetPRStatus(context.Context, *bossanovav1.GetPRStatusRequest) (*bossanovav1.GetPRStatusResponse, error)
	ListClosedPRs(context.Context, *bossanovav1.ListClosedPRsRequest) (*bossanovav1.ListClosedPRsResponse, error)
	ListSessions(context.Context, *bossanovav1.HostServiceListSessionsRequest) (*bossanovav1.HostServiceListSessionsResponse, error)
	GetReviewComments(context.Context, *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error)
	FireSessionEvent(context.Context, *bossanovav1.FireSessionEventRequest) (*bossanovav1.FireSessionEventResponse, error)
	SetRepairStatus(context.Context, *bossanovav1.SetRepairStatusRequest) (*bossanovav1.SetRepairStatusResponse, error)
	StartAgentRun(context.Context, *bossanovav1.StartAgentRunHostRequest) (*bossanovav1.StartAgentRunHostResponse, error)
	WaitAgentRun(context.Context, *bossanovav1.WaitAgentRunHostRequest) (*bossanovav1.WaitAgentRunHostResponse, error)
	RecordRepairOutcome(context.Context, *bossanovav1.RecordRepairOutcomeRequest) (*bossanovav1.RecordRepairOutcomeResponse, error)
}

// Register registers the HostService on a gRPC server (used by the
// go-plugin broker).
func (s *HostServiceServer) Register(srv *grpc.Server) {
	srv.RegisterService(&hostServiceDesc, s)
}

func hostServiceListOpenPRsHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.ListOpenPRsRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).ListOpenPRs(ctx, req)
}

func hostServiceGetCheckResultsHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.GetCheckResultsRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).GetCheckResults(ctx, req)
}

func hostServiceGetPRStatusHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.GetPRStatusRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).GetPRStatus(ctx, req)
}

func hostServiceListClosedPRsHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.ListClosedPRsRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).ListClosedPRs(ctx, req)
}

// --- VCS RPC implementations ---

func (s *HostServiceServer) ListOpenPRs(ctx context.Context, req *bossanovav1.ListOpenPRsRequest) (*bossanovav1.ListOpenPRsResponse, error) {
	prs, err := s.provider.ListOpenPRs(ctx, req.GetRepoOriginUrl())
	if err != nil {
		return nil, err
	}

	pbPRs := make([]*bossanovav1.PRSummary, len(prs))
	for i, pr := range prs {
		pbPRs[i] = &bossanovav1.PRSummary{
			Number:     int32(pr.Number),
			Title:      pr.Title,
			HeadBranch: pr.HeadBranch,
			State:      vcsPRStateToProto(pr.State),
			Author:     pr.Author,
		}
	}

	return &bossanovav1.ListOpenPRsResponse{Prs: pbPRs}, nil
}

func (s *HostServiceServer) GetCheckResults(ctx context.Context, req *bossanovav1.GetCheckResultsRequest) (*bossanovav1.GetCheckResultsResponse, error) {
	checks, err := s.provider.GetCheckResults(ctx, req.GetRepoOriginUrl(), int(req.GetPrNumber()))
	if err != nil {
		return nil, err
	}

	pbChecks := make([]*bossanovav1.CheckResult, len(checks))
	for i, c := range checks {
		pbCheck := &bossanovav1.CheckResult{
			Id:     c.ID,
			Name:   c.Name,
			Status: vcsCheckStatusToProto(c.Status),
		}
		if c.Conclusion != nil {
			cc := vcsCheckConclusionToProto(*c.Conclusion)
			pbCheck.Conclusion = &cc
		}
		pbChecks[i] = pbCheck
	}

	return &bossanovav1.GetCheckResultsResponse{Checks: pbChecks}, nil
}

func (s *HostServiceServer) GetPRStatus(ctx context.Context, req *bossanovav1.GetPRStatusRequest) (*bossanovav1.GetPRStatusResponse, error) {
	status, err := s.provider.GetPRStatus(ctx, req.GetRepoOriginUrl(), int(req.GetPrNumber()))
	if err != nil {
		return nil, err
	}

	pbStatus := &bossanovav1.PRStatus{
		State:      vcsPRStateToProto(status.State),
		Title:      status.Title,
		HeadBranch: status.HeadBranch,
		BaseBranch: status.BaseBranch,
	}
	if status.Mergeable != nil {
		pbStatus.Mergeable = status.Mergeable
	}

	return &bossanovav1.GetPRStatusResponse{Status: pbStatus}, nil
}

func (s *HostServiceServer) ListClosedPRs(ctx context.Context, req *bossanovav1.ListClosedPRsRequest) (*bossanovav1.ListClosedPRsResponse, error) {
	prs, err := s.provider.ListClosedPRs(ctx, req.GetRepoOriginUrl())
	if err != nil {
		return nil, err
	}

	pbPRs := make([]*bossanovav1.PRSummary, len(prs))
	for i, pr := range prs {
		pbPRs[i] = &bossanovav1.PRSummary{
			Number:     int32(pr.Number),
			Title:      pr.Title,
			HeadBranch: pr.HeadBranch,
			State:      vcsPRStateToProto(pr.State),
			Author:     pr.Author,
		}
	}

	return &bossanovav1.ListClosedPRsResponse{Prs: pbPRs}, nil
}

func (s *HostServiceServer) ListSessions(ctx context.Context, req *bossanovav1.HostServiceListSessionsRequest) (*bossanovav1.HostServiceListSessionsResponse, error) {
	if s.repoStore == nil || s.sessionStore == nil || s.displayTracker == nil {
		return nil, grpcstatus.Error(codes.Internal, "session dependencies not set")
	}

	// Iterate all repos and collect their active sessions
	repos, err := s.repoStore.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}

	var pbSessions []*bossanovav1.Session
	for _, repo := range repos {
		sessions, err := s.sessionStore.ListActive(ctx, repo.ID)
		if err != nil {
			log.Warn().Err(err).Str("repo_id", repo.ID).Msg("Failed to list sessions for repo")
			continue
		}

		for _, sess := range sessions {
			// Hydrate with DisplayTracker status
			entry := s.displayTracker.Get(sess.ID)
			var displayStatus vcs.DisplayStatus
			var hasFailures bool
			var isRepairing bool
			var headSHA string
			if entry != nil {
				displayStatus = entry.Status
				hasFailures = entry.HasFailures
				isRepairing = entry.IsRepairing
				headSHA = entry.HeadSHA
			}

			// Check heartbeat tracker for active Claude Code chat processes.
			// On DB error, assume active (fail closed) to prevent advancing
			// sessions that may have a live Claude Code process.
			hasActiveChat := false
			var latestChatActivity time.Time
			if s.agentChats != nil && s.chatTracker != nil {
				chats, err := s.agentChats.ListBySession(ctx, sess.ID)
				if err != nil {
					log.Warn().Err(err).Str("session_id", sess.ID).Msg("failed to list chats for active chat check, assuming active")
					hasActiveChat = true
				} else {
					for _, chat := range chats {
						chatEntry := s.chatTracker.Get(chat.AgentSessionID)
						if chatEntry == nil {
							continue
						}
						hasActiveChat = true
						if chatEntry.LastOutputAt.After(latestChatActivity) {
							latestChatActivity = chatEntry.LastOutputAt
						}
					}
				}
			}

			pbSess := &bossanovav1.Session{
				Id:                     sess.ID,
				RepoId:                 sess.RepoID,
				RepoDisplayName:        repo.DisplayName,
				Title:                  sess.Title,
				BranchName:             sess.BranchName,
				State:                  sessionStateToProto(sess.State),
				DisplayStatus:          vcsDisplayStatusToProto(displayStatus),
				DisplayHasFailures:     hasFailures,
				DisplayIsRepairing:     isRepairing,
				HasActiveChat:          hasActiveChat,
				PrDisplayHeadSha:       headSHA,
				LastRepairRunnerError:  sess.LastRepairRunnerError,
				LastRepairExitError:    sess.LastRepairExitError,
				LastRepairAttemptCount: int32(sess.LastRepairAttemptCount),
			}
			if !sess.UpdatedAt.IsZero() {
				pbSess.UpdatedAt = timestamppb.New(sess.UpdatedAt)
			}
			if sess.LastRepairStartedAt != nil {
				pbSess.LastRepairStartedAt = timestamppb.New(*sess.LastRepairStartedAt)
			}
			if !latestChatActivity.IsZero() {
				pbSess.LastChatActivityAt = timestamppb.New(latestChatActivity)
			}
			pbSessions = append(pbSessions, pbSess)
		}
	}

	return &bossanovav1.HostServiceListSessionsResponse{Sessions: pbSessions}, nil
}

func (s *HostServiceServer) GetReviewComments(ctx context.Context, req *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error) {
	comments, err := s.provider.GetReviewComments(ctx, req.GetRepoOriginUrl(), int(req.GetPrNumber()))
	if err != nil {
		return nil, err
	}

	pbComments := make([]*bossanovav1.ReviewComment, len(comments))
	for i, comment := range comments {
		var line *int32
		if comment.Line != nil {
			l := int32(*comment.Line)
			line = &l
		}
		pbComments[i] = &bossanovav1.ReviewComment{
			Author: comment.Author,
			Body:   comment.Body,
			State:  vcsReviewStateToProto(comment.State),
			Path:   comment.Path,
			Line:   line,
		}
	}

	return &bossanovav1.GetReviewCommentsResponse{Comments: pbComments}, nil
}

func (s *HostServiceServer) FireSessionEvent(ctx context.Context, req *bossanovav1.FireSessionEventRequest) (*bossanovav1.FireSessionEventResponse, error) {
	if s.sessionStore == nil {
		return nil, grpcstatus.Error(codes.Internal, "session store not set")
	}

	// Load session
	session, err := s.sessionStore.Get(ctx, req.GetSessionId())
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	// Create state machine with full session context so guards (fixOrBlock,
	// retryOrBlock) correctly evaluate AttemptCount and HasPR.
	hasPR := session.PRNumber != nil
	sm := machine.NewWithContext(session.State, &machine.SessionContext{
		AttemptCount: session.AttemptCount,
		MaxAttempts:  machine.MaxAttempts,
		HasPR:        hasPR,
	})

	// Fire event
	event := protoToSessionEvent(req.GetEvent())
	if err := sm.Fire(event); err != nil {
		return nil, fmt.Errorf("fire event %v on state %v: %w", event, session.State, err)
	}

	// Persist new state and any context mutations (AttemptCount, BlockedReason).
	newState := sm.State()
	stateInt := int(newState)
	attemptCount := sm.Context().AttemptCount
	update := db.UpdateSessionParams{
		State:        &stateInt,
		AttemptCount: &attemptCount,
	}
	if sm.State() == machine.Blocked {
		reason := sm.Context().BlockedReason
		reasonPtr := &reason
		update.BlockedReason = &reasonPtr
	}

	if _, err = s.sessionStore.Update(ctx, session.ID, update); err != nil {
		return nil, fmt.Errorf("update session: %w", err)
	}

	return &bossanovav1.FireSessionEventResponse{NewState: newState.String()}, nil
}

// --- VCS domain type → proto enum converters ---

func vcsPRStateToProto(s vcs.PRState) bossanovav1.PRState {
	switch s {
	case vcs.PRStateOpen:
		return bossanovav1.PRState_PR_STATE_OPEN
	case vcs.PRStateClosed:
		return bossanovav1.PRState_PR_STATE_CLOSED
	case vcs.PRStateMerged:
		return bossanovav1.PRState_PR_STATE_MERGED
	default:
		return bossanovav1.PRState_PR_STATE_UNSPECIFIED
	}
}

func vcsCheckStatusToProto(s vcs.CheckStatus) bossanovav1.CheckStatus {
	switch s {
	case vcs.CheckStatusQueued:
		return bossanovav1.CheckStatus_CHECK_STATUS_QUEUED
	case vcs.CheckStatusInProgress:
		return bossanovav1.CheckStatus_CHECK_STATUS_IN_PROGRESS
	case vcs.CheckStatusCompleted:
		return bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED
	default:
		return bossanovav1.CheckStatus_CHECK_STATUS_UNSPECIFIED
	}
}

func vcsCheckConclusionToProto(c vcs.CheckConclusion) bossanovav1.CheckConclusion {
	switch c {
	case vcs.CheckConclusionSuccess:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS
	case vcs.CheckConclusionFailure:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_FAILURE
	case vcs.CheckConclusionNeutral:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_NEUTRAL
	case vcs.CheckConclusionCancelled:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_CANCELLED
	case vcs.CheckConclusionSkipped:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_SKIPPED
	case vcs.CheckConclusionTimedOut:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_TIMED_OUT
	default:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_UNSPECIFIED
	}
}

func vcsReviewStateToProto(s vcs.ReviewState) bossanovav1.ReviewState {
	switch s {
	case vcs.ReviewStateApproved:
		return bossanovav1.ReviewState_REVIEW_STATE_APPROVED
	case vcs.ReviewStateChangesRequested:
		return bossanovav1.ReviewState_REVIEW_STATE_CHANGES_REQUESTED
	case vcs.ReviewStateCommented:
		return bossanovav1.ReviewState_REVIEW_STATE_COMMENTED
	case vcs.ReviewStateDismissed:
		return bossanovav1.ReviewState_REVIEW_STATE_DISMISSED
	default:
		return bossanovav1.ReviewState_REVIEW_STATE_UNSPECIFIED
	}
}

func vcsDisplayStatusToProto(s vcs.DisplayStatus) bossanovav1.DisplayStatus {
	return bossanovav1.DisplayStatus(s)
}

func sessionStateToProto(s machine.State) bossanovav1.SessionState {
	return bossanovav1.SessionState(s)
}

func protoToSessionEvent(e bossanovav1.SessionEvent) machine.Event {
	return machine.Event(e)
}

// --- gRPC handler adapters ---

func hostServiceListSessionsHandler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(bossanovav1.HostServiceListSessionsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(hostServiceHandler).ListSessions(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/bossanova.v1.HostService/ListSessions",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(hostServiceHandler).ListSessions(ctx, req.(*bossanovav1.HostServiceListSessionsRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func hostServiceGetReviewCommentsHandler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(bossanovav1.GetReviewCommentsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(hostServiceHandler).GetReviewComments(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/bossanova.v1.HostService/GetReviewComments",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(hostServiceHandler).GetReviewComments(ctx, req.(*bossanovav1.GetReviewCommentsRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func (s *HostServiceServer) SetRepairStatus(_ context.Context, req *bossanovav1.SetRepairStatusRequest) (*bossanovav1.SetRepairStatusResponse, error) {
	if s.displayTracker == nil {
		return nil, grpcstatus.Error(codes.Internal, "PR tracker not set")
	}
	s.displayTracker.SetRepairing(req.GetSessionId(), req.GetIsRepairing())
	return &bossanovav1.SetRepairStatusResponse{}, nil
}

func hostServiceSetRepairStatusHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.SetRepairStatusRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).SetRepairStatus(ctx, req)
}

func hostServiceFireSessionEventHandler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(bossanovav1.FireSessionEventRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(hostServiceHandler).FireSessionEvent(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/bossanova.v1.HostService/FireSessionEvent",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(hostServiceHandler).FireSessionEvent(ctx, req.(*bossanovav1.FireSessionEventRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// StartAgentRun resolves the session's worktree and starts an agent run via
// the loaded AgentRunner plugin. Returns the resolved session ID (named
// claude_id in the proto for compatibility with the repair plugin's
// existing variable name).
//
// Enforces one active agent run per session: returns AlreadyExists if a
// previous run on the same session is still IsRunning. The activeRuns map
// is the daemon-side source of truth for that invariant; the repair
// plugin's in-memory dedup map sits in front of this as a courtesy.
func (s *HostServiceServer) StartAgentRun(ctx context.Context, req *bossanovav1.StartAgentRunHostRequest) (*bossanovav1.StartAgentRunHostResponse, error) {
	if len(s.agentClients) == 0 {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "agent client not configured")
	}
	if s.agentLogsDir == "" {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "agent logs dir not configured")
	}
	if s.sessionStore == nil {
		return nil, grpcstatus.Error(codes.Internal, "session store not set")
	}
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "session_id is required")
	}

	sess, err := s.sessionStore.Get(ctx, sessionID)
	if err != nil {
		return nil, grpcstatus.Errorf(codes.NotFound, "session %s: %v", sessionID, err)
	}
	if sess.WorktreePath == "" {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "session %s has no worktree path", sessionID)
	}

	// Route to the agent plugin matching the session's recorded AgentName.
	// Sessions persisted before the multi-agent migration may have an empty
	// AgentName here; the SQLite store's ""→"claude" fallback (Task 1)
	// keeps that case working as long as bossd-plugin-claude is loaded.
	client, ok := s.agentClients[sess.AgentName]
	if !ok || client == nil {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "agent %q not configured", sess.AgentName)
	}

	s.runMu.Lock()
	if existing, ok := s.activeRuns[sessionID]; ok && s.isAgentRunning(existing.agentName, existing.agentSessionID) {
		s.runMu.Unlock()
		return nil, grpcstatus.Errorf(codes.AlreadyExists, "agent run already active for session %s", sessionID)
	}
	// Stale entry (previous run finished) — fall through and replace.
	//
	// Use context.Background, NOT the gRPC handler's ctx: passing ctx would
	// tie the spawned process's lifetime to this RPC call, which returns
	// immediately. The agent plugin has its own Stop / shutdown path.
	startReq := &bossanovav1.StartAgentRunRequest{
		WorkDir: sess.WorktreePath,
		Plan:    req.GetPrompt(),
		LogPath: filepath.Join(s.agentLogsDir, "repair-"+sessionID+".log"),
	}
	startResp, err := client.StartRun(context.Background(), startReq)
	if err != nil {
		s.runMu.Unlock()
		return nil, grpcstatus.Errorf(codes.Internal, "start agent: %v", err)
	}
	resolved := startResp.GetSessionId()
	if prev, ok := s.activeRuns[sessionID]; ok {
		// Drop the reverse-index entry from the previous (now-stale) run so
		// it doesn't leak when a session is repaired multiple times.
		delete(s.agentSessionByID, prev.agentSessionID)
	}
	s.activeRuns[sessionID] = activeRun{agentName: sess.AgentName, agentSessionID: resolved}
	s.agentSessionByID[resolved] = sess.AgentName
	s.runMu.Unlock()

	return &bossanovav1.StartAgentRunHostResponse{AgentSessionId: resolved}, nil
}

// isAgentRunning reports whether the named agent plugin still has an
// active run for the given agent session ID. RPC errors (and a missing
// client) are treated as "not running" so a transient failure or unloaded
// plugin doesn't permanently wedge a session behind a stale activeRuns
// entry.
func (s *HostServiceServer) isAgentRunning(agentName, agentSessionID string) bool {
	client, ok := s.agentClients[agentName]
	if !ok || client == nil {
		return false
	}
	resp, err := client.IsRunning(context.Background(), &bossanovav1.IsAgentRunningRequest{SessionId: agentSessionID})
	if err != nil {
		return false
	}
	return resp.GetRunning()
}

// WaitAgentRun blocks until the agent run identified by agent_session_id
// exits, then returns the agent's exit error string (empty on clean exit).
// Honours the caller's context for cancellation — the daemon's plugin
// shutdown path cancels these contexts so an in-flight repair drains
// promptly.
//
// The agent_session_id is reverse-mapped to the originating agent plugin
// via agentSessionByID (populated by StartAgentRun). When the mapping is
// missing — e.g. a daemon restart between StartAgentRun and WaitAgentRun
// dropped in-memory state — we fall back to "agent client not configured"
// rather than guessing.
func (s *HostServiceServer) WaitAgentRun(ctx context.Context, req *bossanovav1.WaitAgentRunHostRequest) (*bossanovav1.WaitAgentRunHostResponse, error) {
	if len(s.agentClients) == 0 {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "agent client not configured")
	}
	agentSessionID := req.GetAgentSessionId()
	if agentSessionID == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "agent_session_id is required")
	}

	s.runMu.Lock()
	agentName, ok := s.agentSessionByID[agentSessionID]
	s.runMu.Unlock()
	if !ok {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "no agent client recorded for agent session %s", agentSessionID)
	}
	client, ok := s.agentClients[agentName]
	if !ok || client == nil {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "agent %q not configured", agentName)
	}

	ticker := time.NewTicker(waitAgentRunPollInterval)
	defer ticker.Stop()
	for {
		es, err := client.ExitStatus(ctx, &bossanovav1.AgentExitStatusRequest{SessionId: agentSessionID})
		if err != nil {
			return nil, grpcstatus.Errorf(codes.Internal, "exit status: %v", err)
		}
		if es.GetIsComplete() {
			return &bossanovav1.WaitAgentRunHostResponse{ExitError: es.GetExitError()}, nil
		}
		select {
		case <-ctx.Done():
			return nil, grpcstatus.FromContextError(ctx.Err()).Err()
		case <-ticker.C:
		}
	}
}

func hostServiceStartAgentRunHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.StartAgentRunHostRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).StartAgentRun(ctx, req)
}

func hostServiceWaitAgentRunHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.WaitAgentRunHostRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).WaitAgentRun(ctx, req)
}

// RecordRepairOutcome persists the repair-attempt outcome onto the
// session row. Called by the repair plugin's deferred cleanup so every
// attempt — clean exit, agent error, or runner-level failure — leaves
// a structured trace in SQLite that the TUI can surface and that
// outlives the daemon's in-memory cooldowns.
func (s *HostServiceServer) RecordRepairOutcome(ctx context.Context, req *bossanovav1.RecordRepairOutcomeRequest) (*bossanovav1.RecordRepairOutcomeResponse, error) {
	if s.sessionStore == nil {
		return nil, grpcstatus.Error(codes.Internal, "session store not set")
	}
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "session_id is required")
	}
	startedAt := time.Unix(req.GetStartedAtUnix(), 0)
	if req.GetStartedAtUnix() == 0 {
		startedAt = time.Now()
	}
	if err := s.sessionStore.UpdateRepairDiagnostics(ctx, db.UpdateRepairDiagnosticsParams{
		SessionID:   sessionID,
		StartedAt:   startedAt,
		RunnerError: req.GetRunnerError(),
		ExitError:   req.GetExitError(),
	}); err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "update repair diagnostics: %v", err)
	}
	return &bossanovav1.RecordRepairOutcomeResponse{}, nil
}

func hostServiceRecordRepairOutcomeHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.RecordRepairOutcomeRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).RecordRepairOutcome(ctx, req)
}
