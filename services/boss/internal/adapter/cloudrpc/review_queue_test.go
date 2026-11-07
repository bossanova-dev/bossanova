package cloudrpc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

type reviewQueueCloudRPC struct {
	bossanovav1connect.OrchestratorServiceClient
	request       *pb.ProxyListReviewQueueRequest
	response      *pb.ProxyListReviewQueueResponse
	err           error
	reposReq      *pb.ProxyListReposAggregatedRequest
	reposResponse *pb.ProxyListReposAggregatedResponse
}

func (f *reviewQueueCloudRPC) ProxyListReviewQueue(_ context.Context, req *connect.Request[pb.ProxyListReviewQueueRequest]) (*connect.Response[pb.ProxyListReviewQueueResponse], error) {
	f.request = req.Msg
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(f.response), nil
}

func TestRemoteReviewQueueProxy(t *testing.T) {
	f := &reviewQueueCloudRPC{response: &pb.ProxyListReviewQueueResponse{
		Rows:        []*pb.ReviewQueueRow{{Session: &pb.Session{Id: "s1", PrNumber: proto.Int32(42)}}},
		LevelCounts: []*pb.ReviewQueueLevelCount{{ConfidenceLevel: pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, SessionCount: 3}},
	}}
	c := &RemoteClient{rpc: f}
	got, err := c.ListReviewQueue(t.Context(), &pb.Repo{Id: "local-id", OriginUrl: "https://github.com/org/repo"}, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_LEAST_CONFIDENT)
	want := &pb.ListReviewQueueResponse{Rows: f.response.Rows, LevelCounts: f.response.LevelCounts}
	if err != nil || !proto.Equal(got, want) {
		t.Fatalf("queue=%v err=%v", got, err)
	}
	if f.request.GetRepoOriginUrl() != "https://github.com/org/repo" || f.request.GetSort() != pb.ReviewQueueSort_REVIEW_QUEUE_SORT_LEAST_CONFIDENT || f.request.OrganizationId != nil {
		t.Fatalf("request=%v", f.request)
	}
	f.err = connect.NewError(connect.CodePermissionDenied, errors.New("access denied"))
	if got, err := c.ListReviewQueue(t.Context(), &pb.Repo{OriginUrl: "origin"}, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_OLDEST); got != nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("queue=%v err=%v", got, err)
	}
}

func TestRemoteFactoryHealthIsLocalOnly(t *testing.T) {
	c := &RemoteClient{}
	if got, err := c.GetFactoryHealth(t.Context(), "repo-1"); got != nil || connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("health=%v err=%v", got, err)
	}
}

func (f *reviewQueueCloudRPC) ProxyListReposAggregated(_ context.Context, req *connect.Request[pb.ProxyListReposAggregatedRequest]) (*connect.Response[pb.ProxyListReposAggregatedResponse], error) {
	f.reposReq = req.Msg
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(f.reposResponse), nil
}

func TestRemoteReviewQueueRepos(t *testing.T) {
	f := &reviewQueueCloudRPC{reposResponse: &pb.ProxyListReposAggregatedResponse{Repos: []*pb.AggregatedRepo{{OriginUrl: "https://github.com/org/repo", DisplayName: "repo", DefaultBaseBranch: "main"}}}}
	c := &RemoteClient{rpc: f}
	repos, err := c.ListReviewQueueRepos(t.Context())
	want := &pb.Repo{OriginUrl: "https://github.com/org/repo", DisplayName: "repo", DefaultBaseBranch: "main"}
	if err != nil || len(repos) != 1 || !proto.Equal(repos[0], want) {
		t.Fatalf("repos=%v err=%v", repos, err)
	}
	if f.reposReq.OrganizationId != nil {
		t.Fatalf("scope=%v, want all memberships", f.reposReq)
	}
	f.reposResponse.FailedOrganizations = []*pb.OrganizationRepoReadFailure{{OrganizationName: "team", Reason: "offline"}}
	repos, err = c.ListReviewQueueRepos(t.Context())
	if len(repos) != 1 || connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "team: offline") {
		t.Fatalf("partial repos=%v err=%v", repos, err)
	}
	f.err = connect.NewError(connect.CodePermissionDenied, errors.New("access denied"))
	if repos, err := c.ListReviewQueueRepos(t.Context()); repos != nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("repos=%v err=%v", repos, err)
	}
}

func TestRemoteReviewQueuePreservesPartialRows(t *testing.T) {
	row := &pb.ReviewQueueRow{Session: &pb.Session{Id: "s1"}}
	f := &reviewQueueCloudRPC{response: &pb.ProxyListReviewQueueResponse{Rows: []*pb.ReviewQueueRow{row}, FailedOrganizations: []*pb.OrganizationSessionReadFailure{{OrganizationName: "team", Reason: "offline"}}}}
	got, err := (&RemoteClient{rpc: f}).ListReviewQueue(t.Context(), &pb.Repo{OriginUrl: "origin"}, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_OLDEST)
	if got == nil || len(got.GetRows()) != 1 || got.GetRows()[0] != row || connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "team: offline") {
		t.Fatalf("queue=%v err=%v", got, err)
	}
}
