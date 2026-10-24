package tui

import (
	"context"
	"fmt"

	"github.com/recurser/boss/internal/domain/rpc"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// stubClient supplies shared router fixtures. Remaining client methods belong
// to routerClientFixture until the chat-picker screen moves.
type stubClient struct {
	routerClientFixture
	repos             []*pb.Repo
	reposErr          error
	created           *pb.Session
	createErr         error
	createCronCalls   int
	createReq         *pb.CreateSessionRequest // captures the last CreateSession request
	createCalls       int
	prs               []*pb.PRSummary
	prsErr            error
	trackerIssues     []*pb.TrackerIssue
	trackerIssuesErr  error
	trackerLastSource string // captures the source arg of the last ListTrackerIssues call
	agents            []rpc.AgentInfo
	agentsErr         error
	accounts          []*pb.AgentAccount // returned by ListAgentAccounts if the wizard asks for accounts
	accountsErr       error
	accountsProvider  string // captures the provider arg of the last ListAgentAccounts call
	createdCronReq    *pb.CreateCronJobRequest
	updatedCronReq    *pb.UpdateCronJobRequest
	authChanges       []string
	notifyAuthChange  func(context.Context, string) (*pb.NotifyAuthChangeResponse, error)
}

func (s *stubClient) ListRepos(context.Context) ([]*pb.Repo, error) {
	return s.repos, s.reposErr
}

func (s *stubClient) CreateSession(_ context.Context, req *pb.CreateSessionRequest) (rpc.CreateSessionStream, error) {
	s.createCalls++
	s.createReq = req
	if s.createErr != nil {
		return nil, s.createErr
	}
	return &stubCreateStream{session: s.created}, nil
}

func (s *stubClient) ListRepoPRs(context.Context, string) ([]*pb.PRSummary, error) {
	return s.prs, s.prsErr
}

func (s *stubClient) ListTrackerIssues(_ context.Context, _, _, source string) ([]*pb.TrackerIssue, error) {
	s.trackerLastSource = source
	return s.trackerIssues, s.trackerIssuesErr
}

// stubCreateStream implements rpc.CreateSessionStream for testing.
// It yields a single SessionCreated message with the provided session.
type stubCreateStream struct {
	session  *pb.Session
	received bool
}

func (s *stubCreateStream) Receive() bool {
	if s.received {
		return false
	}
	s.received = true
	return true
}

func (s *stubCreateStream) Msg() *pb.CreateSessionResponse {
	return &pb.CreateSessionResponse{
		Event: &pb.CreateSessionResponse_SessionCreated{
			SessionCreated: &pb.SessionCreated{
				Session: s.session,
			},
		},
	}
}

func (s *stubCreateStream) Err() error {
	return nil
}

func (s *stubCreateStream) Close() error {
	return nil
}

func (s *stubClient) NotifyAuthChange(ctx context.Context, action string) (*pb.NotifyAuthChangeResponse, error) {
	if s.notifyAuthChange != nil {
		return s.notifyAuthChange(ctx, action)
	}
	s.authChanges = append(s.authChanges, action)
	return nil, nil
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

func (s *stubClient) ListAgentAccounts(_ context.Context, provider string, _ bool) ([]*pb.AgentAccount, error) {
	s.accountsProvider = provider
	return s.accounts, s.accountsErr
}

func (s *stubClient) ListAgents(context.Context) ([]rpc.AgentInfo, error) {
	return s.agents, s.agentsErr
}

func (s *stubClient) WatchSessionSetup(context.Context, string) (rpc.WatchSessionSetupStream, error) {
	return nil, fmt.Errorf("no setup fixture")
}
