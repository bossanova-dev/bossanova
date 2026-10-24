package settings

import (
	"context"

	"github.com/recurser/boss/internal/domain/rpc"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// GeneralClient supplies the agents available to the general settings screen.
type GeneralClient interface {
	ListAgents(context.Context) ([]rpc.AgentInfo, error)
}

// SessionClient supplies the session read and edit operations used by settings.
type SessionClient interface {
	GetSession(context.Context, string, rpc.SessionReadOptions) (*pb.Session, error)
	UpdateSession(context.Context, *pb.UpdateSessionRequest) (*pb.Session, error)
}

// BillingClient opens the customer's billing portal.
type BillingClient interface {
	CreateBillingPortalSession(context.Context, string) (string, error)
}

// MCPTokenManager manages the local MCP HTTP credential.
type MCPTokenManager interface {
	LoadMCPToken() (string, error)
	RotateMCPToken() (MCPTokenRotation, error)
}

// MCPTokenRotation describes the token and whether its server needs a restart.
type MCPTokenRotation struct {
	Token                 string
	Restarted             bool
	ManualRestartRequired bool
}
