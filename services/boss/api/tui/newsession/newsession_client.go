package newsession

import (
	"context"

	"github.com/recurser/boss/internal/domain/rpc"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Client supplies the operations used by the new-session wizard.
type Client interface {
	ListRepos(context.Context) ([]*pb.Repo, error)
	ListAgents(context.Context) ([]rpc.AgentInfo, error)
	ListRepoPRs(context.Context, string) ([]*pb.PRSummary, error)
	ListTrackerIssues(context.Context, string, string, string) ([]*pb.TrackerIssue, error)
	CreateSession(context.Context, *pb.CreateSessionRequest) (rpc.CreateSessionStream, error)
	WatchSessionSetup(context.Context, string) (rpc.WatchSessionSetupStream, error)
}
