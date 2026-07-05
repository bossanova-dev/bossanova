package accountflow

import (
	"context"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// AccountClient is the narrow subset of client.BossClient the registration
// flows depend on: list existing accounts (duplicate detection), add a new
// account, live-test it, and remove it when a live test is rejected.
type AccountClient interface {
	ListAccounts(ctx context.Context, provider string) ([]*pb.Account, error)
	AddAccount(ctx context.Context, req *pb.AddAccountRequest) (*pb.Account, error)
	TestAccount(ctx context.Context, id string) (*pb.TestAccountResponse, error)
	RemoveAccount(ctx context.Context, id string) error
}

// Compile-time proof that the real client satisfies the narrow view. This
// pins the flows to BOS-160's interface; a name/signature drift fails the
// build here rather than at a call site.
var _ AccountClient = (client.BossClient)(nil)
