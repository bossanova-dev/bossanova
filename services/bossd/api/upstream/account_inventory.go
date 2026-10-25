package upstream

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// AgentAccountSnapshotReader returns metadata only, never credentials.
type AgentAccountSnapshotReader interface {
	SnapshotAgentAccounts(context.Context) ([]*pb.AgentAccount, error)
}

type AgentAccountSnapshotReaderFunc func(context.Context) ([]*pb.AgentAccount, error)

func (f AgentAccountSnapshotReaderFunc) SnapshotAgentAccounts(ctx context.Context) ([]*pb.AgentAccount, error) {
	return f(ctx)
}

// AgentAccountsEvent invalidates the inventory; forwarding re-reads the full set.
type AgentAccountsEvent struct{}
