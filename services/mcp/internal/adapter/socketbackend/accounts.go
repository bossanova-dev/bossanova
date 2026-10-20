package socketbackend

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Accounts ---

func (b *Backend) ListAgentAccounts(ctx context.Context, provider string, refresh bool) ([]*pb.AgentAccount, error) {
	req := &pb.ListAgentAccountsRequest{}
	if provider != "" {
		req.Provider = &provider
	}
	if refresh {
		req.Refresh = &refresh
	}
	resp, err := b.rpc.ListAgentAccounts(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetAccounts(), nil
}

func (b *Backend) AddAgentAccount(ctx context.Context, req *pb.AddAgentAccountRequest) (*pb.AgentAccount, error) {
	resp, err := b.rpc.AddAgentAccount(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetAccount(), nil
}

func (b *Backend) UpdateAgentAccount(ctx context.Context, req *pb.UpdateAgentAccountRequest) (*pb.AgentAccount, error) {
	resp, err := b.rpc.UpdateAgentAccount(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetAccount(), nil
}

func (b *Backend) RefreshAgentAccount(ctx context.Context, req *pb.RefreshAgentAccountRequest) (*pb.RefreshAgentAccountResponse, error) {
	resp, err := b.rpc.RefreshAgentAccount(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) RemoveAgentAccount(ctx context.Context, id string) error {
	_, err := b.rpc.RemoveAgentAccount(ctx, connect.NewRequest(&pb.RemoveAgentAccountRequest{Id: id}))
	return err
}

func (b *Backend) TestAgentAccount(ctx context.Context, id string) (*pb.TestAgentAccountResponse, error) {
	resp, err := b.rpc.TestAgentAccount(ctx, connect.NewRequest(&pb.TestAgentAccountRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) SwitchSessionAgentAccount(ctx context.Context, req *pb.SwitchSessionAgentAccountRequest) (*pb.SwitchSessionAgentAccountResponse, error) {
	resp, err := b.rpc.SwitchSessionAgentAccount(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}
