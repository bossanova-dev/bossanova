package cloudrpc

import (
	"context"
	"fmt"
	"strings"

	"github.com/recurser/boss/internal/adapter/rpcerror"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ListReviewQueueRepos lists origins across the caller's organizations. Partial
// reads return both the repositories available and an error naming the gaps.
func (c *RemoteClient) ListReviewQueueRepos(ctx context.Context) ([]*pb.Repo, error) {
	resp, err := c.rpc.ProxyListReposAggregated(ctx, connect.NewRequest(&pb.ProxyListReposAggregatedRequest{}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	repos := make([]*pb.Repo, 0, len(resp.Msg.GetRepos()))
	for _, repo := range resp.Msg.GetRepos() {
		repos = append(repos, &pb.Repo{OriginUrl: repo.GetOriginUrl(), DisplayName: repo.GetDisplayName(), DefaultBaseBranch: repo.GetDefaultBaseBranch()})
	}
	var failures []string
	for _, failure := range resp.Msg.GetFailedOrganizations() {
		failures = append(failures, failure.GetOrganizationName()+": "+failure.GetReason())
	}
	return repos, reviewQueueReadError(failures)
}

// ListReviewQueue proxies the queue by origin because repository IDs differ
// between daemons. Partial reads keep their rows and report the missing scope.
func (c *RemoteClient) ListReviewQueue(ctx context.Context, repo *pb.Repo, sort pb.ReviewQueueSort) (*pb.ListReviewQueueResponse, error) {
	resp, err := c.rpc.ProxyListReviewQueue(ctx, connect.NewRequest(&pb.ProxyListReviewQueueRequest{RepoOriginUrl: repo.GetOriginUrl(), Sort: sort}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	result := &pb.ListReviewQueueResponse{Rows: resp.Msg.GetRows(), LevelCounts: resp.Msg.GetLevelCounts()}
	var failures []string
	for _, failure := range resp.Msg.GetFailedOrganizations() {
		failures = append(failures, failure.GetOrganizationName()+": "+failure.GetReason())
	}
	return result, reviewQueueReadError(failures)
}

func reviewQueueReadError(failures []string) error {
	if len(failures) == 0 {
		return nil
	}
	return rpcerror.WrapError(connect.NewError(connect.CodeUnavailable, fmt.Errorf("could not load organizations: %s", strings.Join(failures, "; "))))
}

// GetFactoryHealth is available only on the local daemon.
func (c *RemoteClient) GetFactoryHealth(_ context.Context, _ string) (*pb.GetFactoryHealthResponse, error) {
	return nil, rpcerror.WrapError(errLocalOnly("GetFactoryHealth"))
}
