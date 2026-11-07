package daemonrpc

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

type reviewQueueDaemonRPC struct {
	bossanovav1connect.DaemonServiceClient
	queueReq  *pb.ListReviewQueueRequest
	healthReq *pb.GetFactoryHealthRequest
	queue     *pb.ListReviewQueueResponse
	health    *pb.GetFactoryHealthResponse
	err       error
}

func (f *reviewQueueDaemonRPC) ListReviewQueue(_ context.Context, req *connect.Request[pb.ListReviewQueueRequest]) (*connect.Response[pb.ListReviewQueueResponse], error) {
	f.queueReq = req.Msg
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(f.queue), nil
}

func (f *reviewQueueDaemonRPC) GetFactoryHealth(_ context.Context, req *connect.Request[pb.GetFactoryHealthRequest]) (*connect.Response[pb.GetFactoryHealthResponse], error) {
	f.healthReq = req.Msg
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(f.health), nil
}

func TestLocalReviewQueueAndFactoryHealth(t *testing.T) {
	f := &reviewQueueDaemonRPC{
		queue:  &pb.ListReviewQueueResponse{Rows: []*pb.ReviewQueueRow{{Session: &pb.Session{Id: "s1"}}}, LevelCounts: []*pb.ReviewQueueLevelCount{{ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, SessionCount: 3}}},
		health: &pb.GetFactoryHealthResponse{Problems: []*pb.FactoryHealthProblem{{Code: "ci", Text: "CI failed"}}},
	}
	c := &LocalClient{rpc: f}
	queue, err := c.ListReviewQueue(t.Context(), &pb.Repo{Id: "repo-1"}, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_SMALLEST)
	if err != nil || queue != f.queue {
		t.Fatalf("queue=%v err=%v", queue, err)
	}
	if f.queueReq.GetRepoId() != "repo-1" || f.queueReq.GetSort() != pb.ReviewQueueSort_REVIEW_QUEUE_SORT_SMALLEST {
		t.Fatalf("queue request=%v", f.queueReq)
	}
	health, err := c.GetFactoryHealth(t.Context(), "repo-1")
	if err != nil || health != f.health {
		t.Fatalf("health=%v err=%v", health, err)
	}
	if f.healthReq.GetRepoId() != "repo-1" || f.healthReq.GetWorkspacePath() != "" {
		t.Fatalf("health request=%v", f.healthReq)
	}
	f.err = connect.NewError(connect.CodeUnavailable, errors.New("daemon unavailable"))
	if got, err := c.ListReviewQueue(t.Context(), &pb.Repo{Id: "repo-1"}, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_OLDEST); got != nil || connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("queue=%v err=%v", got, err)
	}
	if got, err := c.GetFactoryHealth(t.Context(), "repo-1"); got != nil || connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("health=%v err=%v", got, err)
	}
}

func (f *reviewQueueDaemonRPC) ListRepos(_ context.Context, _ *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&pb.ListReposResponse{Repos: []*pb.Repo{{Id: "repo-1", OriginUrl: "origin"}}}), nil
}

func TestLocalReviewQueueRepos(t *testing.T) {
	f := &reviewQueueDaemonRPC{}
	c := &LocalClient{rpc: f}
	repos, err := c.ListReviewQueueRepos(t.Context())
	if err != nil || len(repos) != 1 || repos[0].GetId() != "repo-1" {
		t.Fatalf("repos=%v err=%v", repos, err)
	}
	f.err = connect.NewError(connect.CodeUnavailable, errors.New("offline"))
	if repos, err := c.ListReviewQueueRepos(t.Context()); repos != nil || connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("repos=%v err=%v", repos, err)
	}
}
