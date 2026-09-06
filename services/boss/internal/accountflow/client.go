package accountflow

import (
	"context"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// AccountClient is the narrow subset of client.BossClient the registration
// flows depend on: list existing accounts (duplicate detection), add a new
// account, verify it, remove it when verification is rejected, and replace the
// credential of one that already exists.
//
// RefreshAccount is what makes reauthentication a recovery rather than a second
// registration (BOS-1142). The distinction is load-bearing: AddAccount would
// leave the failed account in place with its bad credential, its label, and its
// bindings, while an operator who thought they had fixed it now has two rows and
// no idea which one their sessions use.
type AccountClient interface {
	ListAccounts(ctx context.Context, provider string, refresh bool) ([]*pb.Account, error)
	AddAccount(ctx context.Context, req *pb.AddAccountRequest) (*pb.Account, error)
	RefreshAccount(ctx context.Context, req *pb.RefreshAccountRequest) (*pb.RefreshAccountResponse, error)
	TestAccount(ctx context.Context, id string) (*pb.TestAccountResponse, error)
	RemoveAccount(ctx context.Context, id string) error
}

// Compile-time proof that the real client satisfies the narrow view. This
// pins the flows to BOS-160's interface; a name/signature drift fails the
// build here rather than at a call site.
var _ AccountClient = (client.BossClient)(nil)
