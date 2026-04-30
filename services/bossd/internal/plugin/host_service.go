package plugin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// waitClaudeRunPollInterval is how often WaitClaudeRun polls the runner's
// IsRunning state. Picked to balance responsiveness with idle CPU cost; the
// repair plugin's only consumer of WaitClaudeRun is single-shot per repair.
const waitClaudeRunPollInterval = 500 * time.Millisecond

// HostServiceServer implements the HostService gRPC server on the daemon
// side. Plugins call back to this server via go-plugin's GRPCBroker to
// query VCS data and session state.
type HostServiceServer struct {
	provider       vcs.Provider
	sessionStore   db.SessionStore
	claudeChats    db.ClaudeChatStore
	repoStore      db.RepoStore
	displayTracker *status.DisplayTracker
	chatTracker    *status.Tracker
	claudeRunner   claude.ClaudeRunner

	// activeRuns maps session_id → claude_id for the currently-active
	// repair run on each session. Guarded by runMu. The repair plugin's
	// in-memory dedup map provides cross-call deduplication; this map is
	// the daemon's last-line defense against two concurrent StartClaudeRun
	// calls landing on the same session.
	runMu      sync.Mutex
	activeRuns map[string]string
}

// NewHostServiceServer creates a HostServiceServer that proxies to the
// given VCS provider. Session-related functionality requires SetSessionDeps
// to be called before use; repair execution (StartClaudeRun/WaitClaudeRun)
// requires SetClaudeRunner.
func NewHostServiceServer(provider vcs.Provider) *HostServiceServer {
	return &HostServiceServer{
		provider:   provider,
		activeRuns: make(map[string]string),
	}
}

// SetSessionDeps injects the dependencies needed for session-related RPCs
// (ListSessions, GetReviewComments, FireSessionEvent). The chats store is
// needed to surface per-session HasActiveChat in ListSessions.
func (s *HostServiceServer) SetSessionDeps(repos db.RepoStore, sessions db.SessionStore, chats db.ClaudeChatStore, tracker *status.DisplayTracker, chatTracker *status.Tracker) {
	s.repoStore = repos
	s.sessionStore = sessions
	s.claudeChats = chats
	s.displayTracker = tracker
	s.chatTracker = chatTracker
}

// SetClaudeRunner injects the Claude runner used by StartClaudeRun /
// WaitClaudeRun. The repair plugin needs this to drive an actual Claude
// process when a PR fails. Plugins that only need read-only RPCs can run
// without it, but Validate() flags the missing dep when there's an active
// plugin so misconfiguration fails loudly at startup.
func (s *HostServiceServer) SetClaudeRunner(runner claude.ClaudeRunner) {
	s.claudeRunner = runner
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
	if s.claudeChats == nil {
		missing = append(missing, "claude chat store")
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
	if s.claudeRunner == nil {
		missing = append(missing, "claude runner")
	}
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
			MethodName: "StartClaudeRun",
			Handler:    hostServiceStartClaudeRunHandler,
		},
		{
			MethodName: "WaitClaudeRun",
			Handler:    hostServiceWaitClaudeRunHandler,
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
	StartClaudeRun(context.Context, *bossanovav1.StartClaudeRunRequest) (*bossanovav1.StartClaudeRunResponse, error)
	WaitClaudeRun(context.Context, *bossanovav1.WaitClaudeRunRequest) (*bossanovav1.WaitClaudeRunResponse, error)
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
			if s.claudeChats != nil && s.chatTracker != nil {
				chats, err := s.claudeChats.ListBySession(ctx, sess.ID)
				if err != nil {
					log.Warn().Err(err).Str("session_id", sess.ID).Msg("failed to list chats for active chat check, assuming active")
					hasActiveChat = true
				} else {
					for _, chat := range chats {
						if chatEntry := s.chatTracker.Get(chat.ClaudeID); chatEntry != nil {
							hasActiveChat = true
							break
						}
					}
				}
			}

			pbSess := &bossanovav1.Session{
				Id:                 sess.ID,
				RepoId:             sess.RepoID,
				RepoDisplayName:    repo.DisplayName,
				Title:              sess.Title,
				BranchName:         sess.BranchName,
				State:              sessionStateToProto(sess.State),
				DisplayStatus:      vcsDisplayStatusToProto(displayStatus),
				DisplayHasFailures: hasFailures,
				DisplayIsRepairing: isRepairing,
				HasActiveChat:      hasActiveChat,
				PrDisplayHeadSha:   headSHA,
			}
			if !sess.UpdatedAt.IsZero() {
				pbSess.UpdatedAt = timestamppb.New(sess.UpdatedAt)
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

// StartClaudeRun resolves the session's worktree and starts a Claude process
// running the given prompt. Returns the generated claude_id.
//
// Enforces one active Claude run per session: returns AlreadyExists if a
// previous run on the same session is still IsRunning. The activeRuns map
// is the daemon-side source of truth for that invariant; the repair
// plugin's in-memory dedup map sits in front of this as a courtesy.
func (s *HostServiceServer) StartClaudeRun(ctx context.Context, req *bossanovav1.StartClaudeRunRequest) (*bossanovav1.StartClaudeRunResponse, error) {
	if s.claudeRunner == nil {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "claude runner not configured")
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

	s.runMu.Lock()
	if existing, ok := s.activeRuns[sessionID]; ok && s.claudeRunner.IsRunning(existing) {
		s.runMu.Unlock()
		return nil, grpcstatus.Errorf(codes.AlreadyExists, "claude run already active for session %s", sessionID)
	}
	// Stale entry (previous run finished) — fall through and replace.
	//
	// Use context.Background, NOT the gRPC handler's ctx: passing ctx would
	// tie the spawned Claude process's lifetime to this RPC call, which
	// returns immediately. The runner has its own Stop()/Stop-via-cancel
	// path for shutdown.
	claudeID, err := s.claudeRunner.Start(context.Background(), sess.WorktreePath, req.GetPrompt(), nil, "")
	if err != nil {
		s.runMu.Unlock()
		return nil, grpcstatus.Errorf(codes.Internal, "start claude: %v", err)
	}
	s.activeRuns[sessionID] = claudeID
	s.runMu.Unlock()

	return &bossanovav1.StartClaudeRunResponse{ClaudeId: claudeID}, nil
}

// WaitClaudeRun blocks until the Claude process identified by claude_id exits,
// then returns the runner's exit error string (empty on clean exit). Honours
// the caller's context for cancellation — the daemon's plugin shutdown path
// cancels these contexts so an in-flight repair drains promptly.
func (s *HostServiceServer) WaitClaudeRun(ctx context.Context, req *bossanovav1.WaitClaudeRunRequest) (*bossanovav1.WaitClaudeRunResponse, error) {
	if s.claudeRunner == nil {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "claude runner not configured")
	}
	claudeID := req.GetClaudeId()
	if claudeID == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "claude_id is required")
	}

	ticker := time.NewTicker(waitClaudeRunPollInterval)
	defer ticker.Stop()
	for s.claudeRunner.IsRunning(claudeID) {
		select {
		case <-ctx.Done():
			return nil, grpcstatus.FromContextError(ctx.Err()).Err()
		case <-ticker.C:
		}
	}
	var msg string
	if exitErr := s.claudeRunner.ExitError(claudeID); exitErr != nil {
		msg = exitErr.Error()
	}
	return &bossanovav1.WaitClaudeRunResponse{ExitError: msg}, nil
}

func hostServiceStartClaudeRunHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.StartClaudeRunRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).StartClaudeRun(ctx, req)
}

func hostServiceWaitClaudeRunHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.WaitClaudeRunRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).WaitClaudeRun(ctx, req)
}
