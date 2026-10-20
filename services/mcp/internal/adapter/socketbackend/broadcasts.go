package socketbackend

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Broadcasts ---

func (b *Backend) SendBroadcast(ctx context.Context, req *pb.SendBroadcastRequest) (*pb.SendBroadcastResponse, error) {
	resp, err := b.rpc.SendBroadcast(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) ListBroadcasts(ctx context.Context, req *pb.ListBroadcastsRequest) ([]*pb.Broadcast, error) {
	resp, err := b.rpc.ListBroadcasts(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetBroadcasts(), nil
}

func (b *Backend) DeleteBroadcast(ctx context.Context, id string) error {
	_, err := b.rpc.DeleteBroadcast(ctx, connect.NewRequest(&pb.DeleteBroadcastRequest{Id: id}))
	return err
}

func (b *Backend) CreateBroadcastSubscription(ctx context.Context, req *pb.CreateBroadcastSubscriptionRequest) (*pb.BroadcastSubscription, error) {
	resp, err := b.rpc.CreateBroadcastSubscription(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSubscription(), nil
}

func (b *Backend) ListBroadcastSubscriptions(ctx context.Context, req *pb.ListBroadcastSubscriptionsRequest) ([]*pb.BroadcastSubscription, error) {
	resp, err := b.rpc.ListBroadcastSubscriptions(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSubscriptions(), nil
}

func (b *Backend) DeleteBroadcastSubscription(ctx context.Context, id string) error {
	_, err := b.rpc.DeleteBroadcastSubscription(ctx, connect.NewRequest(&pb.DeleteBroadcastSubscriptionRequest{Id: id}))
	return err
}
