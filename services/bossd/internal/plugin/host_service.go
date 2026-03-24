package plugin

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// HostServiceServer implements the HostService gRPC server on the daemon
// side. Plugins call back to this server via go-plugin's GRPCBroker to
// query VCS data, manage workflows, and control Claude attempts.
type HostServiceServer struct {
	provider      vcs.Provider
	workflowStore db.WorkflowStore
	sessionStore  db.SessionStore
	claudeChats   db.ClaudeChatStore
	claude        claude.ClaudeRunner
}

// NewHostServiceServer creates a HostServiceServer that proxies to the
// given VCS provider. Workflow and attempt functionality requires
// SetWorkflowDeps to be called before use.
func NewHostServiceServer(provider vcs.Provider) *HostServiceServer {
	return &HostServiceServer{provider: provider}
}

// SetWorkflowDeps injects the dependencies needed for workflow and attempt
// RPCs. This is called after construction so that the existing plugin
// wiring doesn't need to change until the full wiring is done.
func (s *HostServiceServer) SetWorkflowDeps(store db.WorkflowStore, sessions db.SessionStore, chats db.ClaudeChatStore, runner claude.ClaudeRunner) {
	s.workflowStore = store
	s.sessionStore = sessions
	s.claudeChats = chats
	s.claude = runner
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
			MethodName: "CreateWorkflow",
			Handler:    hostServiceCreateWorkflowHandler,
		},
		{
			MethodName: "UpdateWorkflow",
			Handler:    hostServiceUpdateWorkflowHandler,
		},
		{
			MethodName: "GetWorkflow",
			Handler:    hostServiceGetWorkflowHandler,
		},
		{
			MethodName: "ListWorkflows",
			Handler:    hostServiceListWorkflowsHandler,
		},
		{
			MethodName: "CreateAttempt",
			Handler:    hostServiceCreateAttemptHandler,
		},
		{
			MethodName: "GetAttemptStatus",
			Handler:    hostServiceGetAttemptStatusHandler,
		},
	},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "StreamAttemptOutput",
			Handler:       hostServiceStreamAttemptOutputHandler,
			ServerStreams: true,
			ClientStreams: false,
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
	CreateWorkflow(context.Context, *bossanovav1.CreateWorkflowRequest) (*bossanovav1.CreateWorkflowResponse, error)
	UpdateWorkflow(context.Context, *bossanovav1.UpdateWorkflowRequest) (*bossanovav1.UpdateWorkflowResponse, error)
	GetWorkflow(context.Context, *bossanovav1.GetWorkflowRequest) (*bossanovav1.GetWorkflowResponse, error)
	ListWorkflows(context.Context, *bossanovav1.ListWorkflowsRequest) (*bossanovav1.ListWorkflowsResponse, error)
	CreateAttempt(context.Context, *bossanovav1.CreateAttemptRequest) (*bossanovav1.CreateAttemptResponse, error)
	GetAttemptStatus(context.Context, *bossanovav1.GetAttemptStatusRequest) (*bossanovav1.GetAttemptStatusResponse, error)
	StreamAttemptOutput(*bossanovav1.StreamAttemptOutputRequest, grpc.ServerStream) error
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

// --- Workflow management handler stubs ---

func hostServiceCreateWorkflowHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.CreateWorkflowRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).CreateWorkflow(ctx, req)
}

func hostServiceUpdateWorkflowHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.UpdateWorkflowRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).UpdateWorkflow(ctx, req)
}

func hostServiceGetWorkflowHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.GetWorkflowRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).GetWorkflow(ctx, req)
}

func hostServiceListWorkflowsHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.ListWorkflowsRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).ListWorkflows(ctx, req)
}

func hostServiceCreateAttemptHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.CreateAttemptRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).CreateAttempt(ctx, req)
}

func hostServiceGetAttemptStatusHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.GetAttemptStatusRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).GetAttemptStatus(ctx, req)
}

func hostServiceStreamAttemptOutputHandler(srv any, stream grpc.ServerStream) error {
	req := new(bossanovav1.StreamAttemptOutputRequest)
	if err := stream.RecvMsg(req); err != nil {
		return err
	}
	return srv.(hostServiceHandler).StreamAttemptOutput(req, stream)
}

// --- Workflow RPC implementations ---

func (s *HostServiceServer) CreateWorkflow(ctx context.Context, req *bossanovav1.CreateWorkflowRequest) (*bossanovav1.CreateWorkflowResponse, error) {
	if s.workflowStore == nil {
		return nil, status.Error(codes.Unavailable, "workflow store not configured")
	}

	var startSHA *string
	if v := req.GetStartCommitSha(); v != "" {
		startSHA = &v
	}
	var configJSON *string
	if v := req.GetConfigJson(); v != "" {
		configJSON = &v
	}

	w, err := s.workflowStore.Create(ctx, db.CreateWorkflowParams{
		SessionID:      req.GetSessionId(),
		RepoID:         req.GetRepoId(),
		PlanPath:       req.GetPlanPath(),
		MaxLegs:        int(req.GetMaxLegs()),
		StartCommitSHA: startSHA,
		ConfigJSON:     configJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("create workflow: %w", err)
	}

	return &bossanovav1.CreateWorkflowResponse{
		Workflow: workflowToProto(w),
	}, nil
}

func (s *HostServiceServer) UpdateWorkflow(ctx context.Context, req *bossanovav1.UpdateWorkflowRequest) (*bossanovav1.UpdateWorkflowResponse, error) {
	if s.workflowStore == nil {
		return nil, status.Error(codes.Unavailable, "workflow store not configured")
	}

	params := db.UpdateWorkflowParams{}
	if v := req.GetStatus(); v != "" {
		params.Status = &v
	}
	if v := req.GetCurrentStep(); v != "" {
		params.CurrentStep = &v
	}
	if req.FlightLeg != nil {
		legInt := int(req.GetFlightLeg())
		params.FlightLeg = &legInt
	}
	if req.LastError != nil {
		v := req.GetLastError()
		if v == "" {
			// Explicitly clear the error: outer pointer set, inner nil.
			params.LastError = new(*string)
		} else {
			errStr := &v
			params.LastError = &errStr
		}
	}

	w, err := s.workflowStore.Update(ctx, req.GetId(), params)
	if err != nil {
		return nil, fmt.Errorf("update workflow: %w", err)
	}

	return &bossanovav1.UpdateWorkflowResponse{
		Workflow: workflowToProto(w),
	}, nil
}

func (s *HostServiceServer) GetWorkflow(ctx context.Context, req *bossanovav1.GetWorkflowRequest) (*bossanovav1.GetWorkflowResponse, error) {
	if s.workflowStore == nil {
		return nil, status.Error(codes.Unavailable, "workflow store not configured")
	}

	w, err := s.workflowStore.Get(ctx, req.GetId())
	if err != nil {
		return nil, fmt.Errorf("get workflow: %w", err)
	}

	return &bossanovav1.GetWorkflowResponse{
		Workflow: workflowToProto(w),
	}, nil
}

func (s *HostServiceServer) ListWorkflows(ctx context.Context, req *bossanovav1.ListWorkflowsRequest) (*bossanovav1.ListWorkflowsResponse, error) {
	if s.workflowStore == nil {
		return nil, status.Error(codes.Unavailable, "workflow store not configured")
	}

	var workflows []*bossanovav1.Workflow
	if statusFilter := req.GetStatusFilter(); statusFilter != "" {
		ws, err := s.workflowStore.ListByStatus(ctx, statusFilter)
		if err != nil {
			return nil, fmt.Errorf("list workflows: %w", err)
		}
		for _, w := range ws {
			workflows = append(workflows, workflowToProto(w))
		}
	} else {
		ws, err := s.workflowStore.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list workflows: %w", err)
		}
		for _, w := range ws {
			workflows = append(workflows, workflowToProto(w))
		}
	}

	return &bossanovav1.ListWorkflowsResponse{
		Workflows: workflows,
	}, nil
}

// --- Attempt RPC implementations ---

func (s *HostServiceServer) CreateAttempt(ctx context.Context, req *bossanovav1.CreateAttemptRequest) (*bossanovav1.CreateAttemptResponse, error) {
	if s.claude == nil {
		return nil, status.Error(codes.Unavailable, "claude runner not configured")
	}

	// Resolve the workflow once (used for both workDir resolution and chat
	// registration below).
	workDir := req.GetWorkDir()
	var wf *models.Workflow
	if req.GetWorkflowId() != "" && s.workflowStore != nil {
		wf, _ = s.workflowStore.Get(ctx, req.GetWorkflowId())
	}

	// Resolve working directory from workflow → session when not provided.
	if workDir == "" && wf != nil && s.sessionStore != nil {
		if sess, err := s.sessionStore.Get(ctx, wf.SessionID); err == nil {
			workDir = sess.WorktreePath
		}
	}

	// When this attempt is tied to a workflow, generate a UUID and register
	// a claude_chats record so the chat appears in the session's chat picker.
	// The UUID is also passed to Claude via --session-id, creating a real
	// Claude Code session file for title backfill and potential resume.
	var chatID string
	if wf != nil && s.claudeChats != nil {
		chatID = uuid.New().String()
		if _, err := s.claudeChats.Create(ctx, db.CreateClaudeChatParams{
			SessionID: wf.SessionID,
			ClaudeID:  chatID,
			Title:     fmt.Sprintf("autopilot: %s", req.GetSkillName()),
		}); err != nil {
			log.Warn().Err(err).Str("workflow_id", req.GetWorkflowId()).Msg("chat registration failed")
			chatID = "" // fall back to auto-generated ID
		}
	}

	// Use context.Background() so the Claude process outlives this RPC.
	// The gRPC request context is cancelled when the RPC returns, which
	// would immediately kill the long-running Claude subprocess.
	sessionID, err := s.claude.Start(context.Background(), workDir, req.GetInput(), nil, chatID)
	if err != nil {
		// Clean up orphaned chat record if Claude failed to start.
		if chatID != "" && s.claudeChats != nil {
			_ = s.claudeChats.DeleteByClaudeID(ctx, chatID)
		}
		return nil, fmt.Errorf("start claude: %w", err)
	}

	return &bossanovav1.CreateAttemptResponse{
		AttemptId: sessionID,
	}, nil
}

func (s *HostServiceServer) GetAttemptStatus(_ context.Context, req *bossanovav1.GetAttemptStatusRequest) (*bossanovav1.GetAttemptStatusResponse, error) {
	if s.claude == nil {
		return nil, status.Error(codes.Unavailable, "claude runner not configured")
	}

	attemptID := req.GetAttemptId()
	running := s.claude.IsRunning(attemptID)

	resp := &bossanovav1.GetAttemptStatusResponse{}
	if running {
		resp.Status = bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_RUNNING
	} else if exitErr := s.claude.ExitError(attemptID); exitErr != nil {
		resp.Status = bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_FAILED
		resp.Error = exitErr.Error()
	} else {
		resp.Status = bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_COMPLETED
	}

	history := s.claude.History(attemptID)
	lines := make([]string, len(history))
	for i, line := range history {
		lines[i] = line.Text
	}
	resp.OutputLines = lines

	return resp, nil
}

func (s *HostServiceServer) StreamAttemptOutput(req *bossanovav1.StreamAttemptOutputRequest, stream grpc.ServerStream) error {
	if s.claude == nil {
		return status.Error(codes.Unavailable, "claude runner not configured")
	}

	ch, err := s.claude.Subscribe(stream.Context(), req.GetAttemptId())
	if err != nil {
		return fmt.Errorf("subscribe to attempt: %w", err)
	}

	for line := range ch {
		if err := stream.SendMsg(&bossanovav1.StreamAttemptOutputResponse{
			Line: line.Text,
		}); err != nil {
			return err
		}
	}

	return nil
}

// --- Model converters ---

func workflowToProto(w *models.Workflow) *bossanovav1.Workflow {
	pb := &bossanovav1.Workflow{
		Id:          w.ID,
		SessionId:   w.SessionID,
		RepoId:      w.RepoID,
		PlanPath:    w.PlanPath,
		Status:      string(w.Status),
		CurrentStep: string(w.CurrentStep),
		FlightLeg:   int32(w.FlightLeg),
		MaxLegs:     int32(w.MaxLegs),
		CreatedAt:   w.CreatedAt.Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   w.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}
	if w.LastError != nil {
		pb.LastError = *w.LastError
	}
	if w.StartCommitSHA != nil {
		pb.StartCommitSha = *w.StartCommitSHA
	}
	if w.ConfigJSON != nil {
		pb.ConfigJson = *w.ConfigJSON
	}
	return pb
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
