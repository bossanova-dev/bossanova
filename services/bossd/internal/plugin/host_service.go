package plugin

import (
	"context"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"google.golang.org/grpc"
)

// HostServiceServer implements the HostService gRPC server on the daemon
// side. Plugins call back to this server via go-plugin's GRPCBroker to
// query VCS data (open PRs, check results, PR status).
type HostServiceServer struct {
	provider vcs.Provider
}

// NewHostServiceServer creates a HostServiceServer that proxies to the
// given VCS provider.
func NewHostServiceServer(provider vcs.Provider) *HostServiceServer {
	return &HostServiceServer{provider: provider}
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
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "bossanova/v1/host_service.proto",
}

// hostServiceHandler is the interface that the gRPC service descriptor
// expects. HostServiceServer implements it.
type hostServiceHandler interface {
	ListOpenPRs(context.Context, *bossanovav1.ListOpenPRsRequest) (*bossanovav1.ListOpenPRsResponse, error)
	GetCheckResults(context.Context, *bossanovav1.GetCheckResultsRequest) (*bossanovav1.GetCheckResultsResponse, error)
	GetPRStatus(context.Context, *bossanovav1.GetPRStatusRequest) (*bossanovav1.GetPRStatusResponse, error)
	ListClosedPRs(context.Context, *bossanovav1.ListClosedPRsRequest) (*bossanovav1.ListClosedPRsResponse, error)
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
