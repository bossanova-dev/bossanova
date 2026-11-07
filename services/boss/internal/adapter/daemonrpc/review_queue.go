package daemonrpc

import (
	"context"

	"github.com/recurser/boss/internal/adapter/rpcerror"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ListReviewQueueRepos lists the repositories registered on this daemon.
func (c *LocalClient) ListReviewQueueRepos(ctx context.Context) ([]*pb.Repo, error) {
	return c.ListRepos(ctx)
}

// ListReviewQueue reads a repository's queue using its daemon-local identity.
func (c *LocalClient) ListReviewQueue(ctx context.Context, repo *pb.Repo, sort pb.ReviewQueueSort) (*pb.ListReviewQueueResponse, error) {
	resp, err := c.rpc.ListReviewQueue(ctx, connect.NewRequest(&pb.ListReviewQueueRequest{RepoId: repo.GetId(), Sort: sort}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}

// GetFactoryHealth reads the current problems for a registered repository.
func (c *LocalClient) GetFactoryHealth(ctx context.Context, repoID string) (*pb.GetFactoryHealthResponse, error) {
	resp, err := c.rpc.GetFactoryHealth(ctx, connect.NewRequest(&pb.GetFactoryHealthRequest{RepoId: repoID}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
