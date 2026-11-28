package cloudrpc

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/recurser/boss/internal/adapter/rpcerror"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// errLocalOnly is returned for operations that only work on a local daemon.
func errLocalOnly(op string) error {
	return rpcerror.WrapError(connect.NewError(connect.CodeUnimplemented, fmt.Errorf("%s is only available on a local daemon", op)))
}

// RefreshRepoOrigin reads the daemon's local checkout, and the orchestrator
// carries no command that repoints a repo.
func (c *RemoteClient) RefreshRepoOrigin(context.Context, string) (*pb.RefreshRepoOriginResponse, error) {
	return nil, errLocalOnly("refreshing a repo origin")
}

// GetGitHubUsage reads one daemon's own usage ledger, so it has no remote form.
func (c *RemoteClient) GetGitHubUsage(context.Context, *pb.GetGitHubUsageRequest) (*pb.GetGitHubUsageResponse, error) {
	return nil, errLocalOnly("GitHub usage")
}

// GetPRState reads one daemon's own GitHub read cache, so it has no remote form.
func (c *RemoteClient) GetPRState(context.Context, *pb.GetPRStateRequest) (*pb.GetPRStateResponse, error) {
	return nil, errLocalOnly("PR state")
}

// WaitPRState is woken by one daemon's own webhook invalidations, so it has no
// remote form.
func (c *RemoteClient) WaitPRState(context.Context, *pb.WaitPRStateRequest) (*pb.WaitPRStateResponse, error) {
	return nil, errLocalOnly("PR wait")
}

// AcceptSessionPRBlocker requires the local daemon's verify toolbox.
func (c *RemoteClient) AcceptSessionPRBlocker(context.Context, *pb.AcceptSessionPRBlockerRequest) (*pb.AcceptSessionPRBlockerResponse, error) {
	return nil, errLocalOnly("AcceptSessionPRBlocker")
}

// ListSessionPRBlockers requires the local daemon's verify toolbox.
func (c *RemoteClient) ListSessionPRBlockers(context.Context, string) (*pb.ListSessionPRBlockersResponse, error) {
	return nil, errLocalOnly("ListSessionPRBlockers")
}

func (c *RemoteClient) FollowUpSessionPRNotes(context.Context, string, []string, bool, bool) (*pb.VerifyFollowUpResult, error) {
	return nil, errLocalOnly("FollowUpSessionPRNotes")
}

// GetGitHubReadCredential exposes a secret on the local socket only.
func (c *RemoteClient) GetGitHubReadCredential(context.Context, *pb.GetGitHubReadCredentialRequest) (*pb.GetGitHubReadCredentialResponse, error) {
	return nil, errLocalOnly("GitHub read credential")
}

// ListSetupChecks is host-local by design: setup checks inspect the daemon's
// machine environment and local checkout, so the remote orchestrator refuses it.
func (c *RemoteClient) ListSetupChecks(_ context.Context, _ *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error) {
	return nil, rpcerror.WrapError(connect.NewError(connect.CodeUnimplemented, errors.New("setup checks are host-local; run boss init check on the host")))
}

func (c *RemoteClient) CheckAgentLogins(context.Context) ([]rpc.AgentLogin, error) {
	return nil, errLocalOnly("CheckAgentLogins")
}
