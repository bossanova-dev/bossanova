package server

import (
	"context"
	"errors"
	"path/filepath"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// GetHostedProofLimits returns the page-shape limits bosso last returned for
// the workspace's repository, so the caller can fit a hosted page before its
// first upload. No limits is a normal answer: the daemon has none, or none
// fresh, and the caller falls back to fitting on refusal.
func (s *Server) GetHostedProofLimits(ctx context.Context, req *connect.Request[pb.GetHostedProofLimitsRequest]) (*connect.Response[pb.GetHostedProofLimitsResponse], error) {
	if req.Msg.WorkspacePath == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workspace_path is required"))
	}
	path, err := filepath.Abs(req.Msg.WorkspacePath)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid workspace path"))
	}
	repo, _, err := s.workspaceOwner(ctx, path)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("path is outside registered workspaces"))
	}
	resp := &pb.GetHostedProofLimitsResponse{}
	if reader, ok := s.evidenceBucketOpener.(hostedProofLimitsReader); ok {
		if limits, fresh := reader.HostedProofLimits(repo.OriginURL); fresh {
			resp.Limits = hostedProofLimitsProto(limits)
		}
	}
	return connect.NewResponse(resp), nil
}

func hostedProofLimitsProto(limits HostedProofLimits) *pb.HostedProofPageLimits {
	if limits == (HostedProofLimits{}) {
		return nil
	}
	return &pb.HostedProofPageLimits{MaxFileCount: clampInt32(int(limits.FileCount)), MaxFileBytes: limits.FileBytes, MaxPageBytes: limits.PageBytes}
}
