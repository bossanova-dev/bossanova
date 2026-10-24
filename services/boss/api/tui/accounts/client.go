package accounts

import (
	"context"

	"github.com/recurser/boss/internal/domain/rpc"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type ActionClient interface {
	UpdateAgentAccount(context.Context, *pb.UpdateAgentAccountRequest) (*pb.AgentAccount, error)
	RemoveAgentAccount(context.Context, string) error
}
type EditClient interface {
	ActionClient
	TestAgentAccount(context.Context, string) (*pb.TestAgentAccountResponse, error)
}
type ListClient interface {
	EditClient
	ListAgentAccounts(context.Context, string, bool) ([]*pb.AgentAccount, error)
	ListSessions(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, error)
}
type RegisterClient interface {
	ListAgentAccounts(context.Context, string, bool) ([]*pb.AgentAccount, error)
	AddAgentAccount(context.Context, *pb.AddAgentAccountRequest) (*pb.AgentAccount, error)
	RefreshAgentAccount(context.Context, *pb.RefreshAgentAccountRequest) (*pb.RefreshAgentAccountResponse, error)
	TestAgentAccount(context.Context, string) (*pb.TestAgentAccountResponse, error)
	RemoveAgentAccount(context.Context, string) error
}
