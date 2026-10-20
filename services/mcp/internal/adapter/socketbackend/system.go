package socketbackend

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Diagnostics ---

func (b *Backend) ListCheckSnapshots(ctx context.Context, sessionID string, limit int32) (*pb.ListCheckSnapshotsResponse, error) {
	resp, err := b.rpc.ListCheckSnapshots(ctx, connect.NewRequest(&pb.ListCheckSnapshotsRequest{
		SessionId: sessionID,
		Limit:     limit,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) RepairDoctor(ctx context.Context) (*pb.RepairDoctorResponse, error) {
	resp, err := b.rpc.RepairDoctor(ctx, connect.NewRequest(&pb.RepairDoctorRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) StartRepairWorkflow(ctx context.Context) (*pb.StartRepairWorkflowResponse, error) {
	resp, err := b.rpc.StartRepairWorkflow(ctx, connect.NewRequest(&pb.StartRepairWorkflowRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) ListAgents(ctx context.Context) ([]*pb.AgentInfo, error) {
	resp, err := b.rpc.ListAgents(ctx, connect.NewRequest(&pb.ListAgentsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetAgents(), nil
}

func (b *Backend) ListPlugins(ctx context.Context) ([]*pb.InstalledPlugin, error) {
	resp, err := b.rpc.ListPlugins(ctx, connect.NewRequest(&pb.ListPluginsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetPlugins(), nil
}

// --- Global Settings ---

func (b *Backend) GetSettings(ctx context.Context) (*pb.GlobalSettings, error) {
	resp, err := b.rpc.GetSettings(ctx, connect.NewRequest(&pb.GetSettingsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSettings(), nil
}

func (b *Backend) UpdateSettings(ctx context.Context, req *pb.UpdateSettingsRequest) (*pb.GlobalSettings, error) {
	resp, err := b.rpc.UpdateSettings(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSettings(), nil
}
