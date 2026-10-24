package cron

import (
	"context"

	"github.com/recurser/boss/internal/domain/rpc"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type stubClient struct {
	repos           []*pb.Repo
	reposErr        error
	agents          []rpc.AgentInfo
	agentsErr       error
	createCronCalls int
	createdCronReq  *pb.CreateCronJobRequest
	updatedCronReq  *pb.UpdateCronJobRequest
}

func (s *stubClient) ListRepos(context.Context) ([]*pb.Repo, error) {
	return s.repos, s.reposErr
}
func (s *stubClient) ListAgents(context.Context) ([]rpc.AgentInfo, error) {
	return s.agents, s.agentsErr
}
func (s *stubClient) CreateCronJob(_ context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	s.createCronCalls++
	s.createdCronReq = req
	return &pb.CronJob{Id: "cron-1"}, nil
}
func (s *stubClient) UpdateCronJob(_ context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	s.updatedCronReq = req
	return &pb.CronJob{Id: req.Id}, nil
}
func (*stubClient) ListCronJobs(context.Context, string) ([]*pb.CronJob, error) { return nil, nil }
func (*stubClient) DeleteCronJob(context.Context, string) error                 { return nil }
func (*stubClient) RunCronJobNow(context.Context, string) (*pb.RunCronJobNowResponse, error) {
	return nil, nil
}

var _ Client = (*stubClient)(nil)
