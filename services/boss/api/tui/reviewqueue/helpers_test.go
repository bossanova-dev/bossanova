package reviewqueue

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type queueScreenClient struct {
	Client
	repos                []*pb.Repo
	responses            map[string]*pb.ListReviewQueueResponse
	queueErrs            map[string]error
	health               map[string]*pb.GetFactoryHealthResponse
	healthErrs           map[string]error
	queueCalls           []string
	session              *pb.Session
	sessionReads         int
	mergedHead, mergedID string
	mergedApprove        bool
	mergedCodes          []string
	blockers             *pb.ListSessionPRBlockersResponse
	accepted             []*pb.AcceptSessionPRBlockerRequest
}

func (c *queueScreenClient) ListReviewQueueRepos(context.Context) ([]*pb.Repo, error) {
	return c.repos, nil
}
func (c *queueScreenClient) ListReviewQueue(_ context.Context, r *pb.Repo, _ pb.ReviewQueueSort) (*pb.ListReviewQueueResponse, error) {
	c.queueCalls = append(c.queueCalls, r.GetId())
	return c.responses[r.GetId()], c.queueErrs[r.GetId()]
}
func (c *queueScreenClient) GetFactoryHealth(_ context.Context, id string) (*pb.GetFactoryHealthResponse, error) {
	return c.health[id], c.healthErrs[id]
}
func (c *queueScreenClient) GetSession(context.Context, string, rpc.SessionReadOptions) (*pb.Session, error) {
	c.sessionReads++
	return c.session, nil
}
func (c *queueScreenClient) OperatorMergeSession(_ context.Context, id, head string, approve bool, codes []string) (*pb.OperatorMergeSessionResponse, error) {
	c.mergedID, c.mergedHead, c.mergedApprove, c.mergedCodes = id, head, approve, codes
	return &pb.OperatorMergeSessionResponse{IsMerged: true}, nil
}
func (c *queueScreenClient) ListSessionPRBlockers(context.Context, string) (*pb.ListSessionPRBlockersResponse, error) {
	return c.blockers, nil
}
func (c *queueScreenClient) AcceptSessionPRBlocker(_ context.Context, r *pb.AcceptSessionPRBlockerRequest) (*pb.AcceptSessionPRBlockerResponse, error) {
	c.accepted = append(c.accepted, proto.CloneOf(r))
	return &pb.AcceptSessionPRBlockerResponse{IsAccepted: true, TicketIdentifier: "BOS-9001", TicketUrl: "https://linear.test/BOS-9001"}, nil
}

func queueRow(id string, level pb.ConfidenceLevel, size int32, age int64) *pb.ReviewQueueRow {
	return &pb.ReviewQueueRow{ConfidenceLevel: level, Session: &pb.Session{Id: id, Title: "Change " + id, RepoDisplayName: "repo-" + id, RepoOriginUrl: "https://github.com/test/" + id, PrNumber: proto.Int32(42), PrUrl: proto.String("https://github.com/test/" + id + "/pull/42"), PrDisplayHeadSha: "head-shown", PrChangedFileCount: 1, PrAddedLineCount: size, Attention: &pb.SessionAttention{Since: &timestamppb.Timestamp{Seconds: age}}}}
}
func queueModel(c Client, rows ...*pb.ReviewQueueRow) Model {
	m := NewModel(c, context.Background(), func(string) error { return nil })
	m.SetSize(180, 30)
	return queueUpdate(m, LoadedMsg{Rows: rows, Counts: map[pb.ConfidenceLevel]int32{}})
}
func queueUpdate(m Model, msg tea.Msg) Model {
	if loaded, ok := msg.(LoadedMsg); ok && loaded.generation == nil {
		loaded.generation = m.generation
		msg = loaded
	}
	next, _ := m.Update(msg)
	return next
}
func queueKey(m Model, key rune) (Model, tea.Cmd) {
	next, cmd := m.Update(tea.KeyPressMsg{Code: key})
	return next, cmd
}
func queueRun(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected command")
	}
	return queueUpdate(m, cmd())
}
