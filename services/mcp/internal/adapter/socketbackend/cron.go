package socketbackend

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Cron ---

func (b *Backend) CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	resp, err := b.rpc.CreateCronJob(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJob(), nil
}

func (b *Backend) ListCronJobs(ctx context.Context) ([]*pb.CronJob, error) {
	resp, err := b.rpc.ListCronJobs(ctx, connect.NewRequest(&pb.ListCronJobsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJobs(), nil
}

// GetCronJob is not on the boss client interface, but the DaemonService RPC
// exists, so the adapter calls it directly.
func (b *Backend) GetCronJob(ctx context.Context, id string) (*pb.CronJob, error) {
	resp, err := b.rpc.GetCronJob(ctx, connect.NewRequest(&pb.GetCronJobRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJob(), nil
}

func (b *Backend) UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	resp, err := b.rpc.UpdateCronJob(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJob(), nil
}

func (b *Backend) DeleteCronJob(ctx context.Context, id string) error {
	_, err := b.rpc.DeleteCronJob(ctx, connect.NewRequest(&pb.DeleteCronJobRequest{Id: id}))
	return err
}

func (b *Backend) RunCronJobNow(ctx context.Context, id string) (*pb.RunCronJobNowResponse, error) {
	resp, err := b.rpc.RunCronJobNow(ctx, connect.NewRequest(&pb.RunCronJobNowRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}
