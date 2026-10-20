package socketbackend

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (b *Backend) ResolveContext(ctx context.Context, workingDir string) (*pb.ResolveContextResponse, error) {
	resp, err := b.rpc.ResolveContext(ctx, connect.NewRequest(&pb.ResolveContextRequest{WorkingDirectory: workingDir}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// --- Repos ---

func (b *Backend) ValidateRepoPath(ctx context.Context, localPath string) (*pb.ValidateRepoPathResponse, error) {
	resp, err := b.rpc.ValidateRepoPath(ctx, connect.NewRequest(&pb.ValidateRepoPathRequest{LocalPath: localPath}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) RegisterRepo(ctx context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
	resp, err := b.rpc.RegisterRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepo(), nil
}

func (b *Backend) CloneAndRegisterRepo(ctx context.Context, req *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
	resp, err := b.rpc.CloneAndRegisterRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepo(), nil
}

func (b *Backend) ListRepos(ctx context.Context) ([]*pb.Repo, error) {
	resp, err := b.rpc.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepos(), nil
}

func (b *Backend) RemoveRepo(ctx context.Context, id string) error {
	_, err := b.rpc.RemoveRepo(ctx, connect.NewRequest(&pb.RemoveRepoRequest{Id: id}))
	return err
}

func (b *Backend) UpdateRepo(ctx context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
	resp, err := b.rpc.UpdateRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepo(), nil
}

func (b *Backend) ListRepoPRs(ctx context.Context, repoID string) ([]*pb.PRSummary, error) {
	resp, err := b.rpc.ListRepoPRs(ctx, connect.NewRequest(&pb.ListRepoPRsRequest{RepoId: repoID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetPullRequests(), nil
}

func (b *Backend) ReportChatStatus(ctx context.Context, statuses []*pb.ChatStatusReport) error {
	_, err := b.rpc.ReportChatStatus(ctx, connect.NewRequest(&pb.ReportChatStatusRequest{Reports: statuses}))
	return err
}
