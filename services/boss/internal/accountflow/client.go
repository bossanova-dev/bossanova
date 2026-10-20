package accountflow

import (
	"context"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// AgentAccountClient is the narrow subset of client.BossClient the registration
// flows depend on: list existing accounts (duplicate detection), add a new
// account, verify it, remove it when verification is rejected, and replace the
// credential of one that already exists.
//
// RefreshAgentAccount is what makes reauthentication a recovery rather than a second
// registration (BOS-1142). The distinction is load-bearing: AddAgentAccount would
// leave the failed account in place with its bad credential, its label, and its
// bindings, while an operator who thought they had fixed it now has two rows and
// no idea which one their sessions use.
type AgentAccountClient interface {
	ListAgentAccounts(ctx context.Context, provider string, refresh bool) ([]*pb.AgentAccount, error)
	AddAgentAccount(ctx context.Context, req *pb.AddAgentAccountRequest) (*pb.AgentAccount, error)
	RefreshAgentAccount(ctx context.Context, req *pb.RefreshAgentAccountRequest) (*pb.RefreshAgentAccountResponse, error)
	TestAgentAccount(ctx context.Context, id string) (*pb.TestAgentAccountResponse, error)
	RemoveAgentAccount(ctx context.Context, id string) error
}

// Compile-time proof that the real client satisfies the narrow view. This
// pins the flows to BOS-160's interface; a name/signature drift fails the
// build here rather than at a call site.
var _ AgentAccountClient = (client.BossClient)(nil)
