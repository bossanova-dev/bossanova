package reviewqueue

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
)

type queuePendingPollClient struct {
	queueScreenClient
	reads            atomic.Int32
	entered, release chan struct{}
}

func (c *queuePendingPollClient) ListReviewQueueRepos(context.Context) ([]*pb.Repo, error) {
	if c.reads.Add(1) == 1 {
		close(c.entered)
		<-c.release
	}
	return nil, nil
}

func TestQueueInitialLoadPreventsAnotherPoll(t *testing.T) {
	for _, source := range []string{"initial", "action"} {
		t.Run(source, func(t *testing.T) {
			c := &queuePendingPollClient{entered: make(chan struct{}), release: make(chan struct{})}
			m := NewModel(c, context.Background(), func(string) error { return nil })
			initial := m.load()
			if source == "action" {
				m = queueUpdate(m, LoadedMsg{})
				m, initial = m.Update(openResultMsg{})
			}
			if initial == nil {
				t.Fatal("refresh returned no command")
			}
			done := safego.Go(zerolog.Nop(), func() { initial() })
			defer func() { close(c.release); <-done }()
			<-c.entered
			_, cmd := m.Update(tickMsg{generation: m.generation})
			var run func(tea.Cmd)
			run = func(cmd tea.Cmd) {
				if cmd == nil {
					return
				}
				if batch, ok := cmd().(tea.BatchMsg); ok {
					for _, child := range batch {
						run(child)
					}
				}
			}
			run(cmd)
			if calls := c.reads.Load(); calls != 1 {
				t.Fatalf("pending %s load overlapped with %d repository reads", source, calls)
			}
		})
	}
}

type queueDeadlineClient struct{ queueScreenClient }

func (*queueDeadlineClient) ListReviewQueueRepos(ctx context.Context) ([]*pb.Repo, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestQueueLoadTimeoutReleasesRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewModel(&queueDeadlineClient{}, context.Background(), func(string) error { return nil })
		m = queueUpdate(m, m.load()())
		if m.fetching || m.loading || len(m.repoErrs) != 1 || !strings.Contains(m.repoErrs[0], "deadline exceeded") {
			t.Fatalf("timed-out load did not settle: fetching=%v errors=%v", m.fetching, m.repoErrs)
		}
		_, cmd := m.refresh()
		if cmd == nil {
			t.Fatal("timeout prevented next refresh")
		}
	})
}

func TestQueueReopenRejectsOldLoadsAndTimers(t *testing.T) {
	old := queueModel(&queueScreenClient{}, queueRow("old", pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, 1, 1))
	current := queueModel(&queueScreenClient{}, queueRow("new", pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, 1, 1))
	current.fetching = true
	next, cmd := current.Update(old.load()())
	if cmd != nil || !next.fetching || next.rows[0].GetSession().GetId() != "new" {
		t.Fatal("old load replaced current queue")
	}
	next, cmd = next.Update(tickMsg{generation: old.generation})
	if cmd != nil {
		t.Fatal("old queue timer rearmed on new queue")
	}
	_, cmd = next.Update(tickMsg{generation: next.generation})
	if cmd == nil {
		t.Fatal("current timer did not rearm")
	}
}

type queuePreflightDeadlineClient struct{ queueScreenClient }

func (c *queuePreflightDeadlineClient) GetSession(ctx context.Context, _ string, _ rpc.SessionReadOptions) (*pb.Session, error) {
	c.sessionReads++
	if c.sessionReads == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return c.session, nil
}
func TestQueueMergePreflightTimeoutAllowsRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		row := queueRow("merge", pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, 1, 10)
		row.Session.Attention.Actions = []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE}
		c := &queuePreflightDeadlineClient{queueScreenClient: queueScreenClient{session: row.Session}}
		m := queueModel(c, row)
		m, cmd := queueKey(m, 'm')
		m = queueRun(t, m, cmd)
		if m.mergePending != nil || !strings.Contains(m.status, "deadline exceeded") {
			t.Fatal("timeout did not release preflight")
		}
		m, cmd = queueKey(m, 'm')
		m = queueRun(t, m, cmd)
		if !m.action.Confirm.Active || c.sessionReads != 2 {
			t.Fatal("merge could not retry after timeout")
		}
	})
}

type queueSlowRepoClient struct{ queueScreenClient }

func (c *queueSlowRepoClient) ListReviewQueue(ctx context.Context, r *pb.Repo, _ pb.ReviewQueueSort) (*pb.ListReviewQueueResponse, error) {
	if r.GetId() == "slow" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.responses[r.GetId()], nil
}
func TestQueueSlowRepoDoesNotStarveHealthyRepo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		row := queueRow("healthy", pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, 1, 10)
		c := &queueSlowRepoClient{queueScreenClient: queueScreenClient{
			repos:     []*pb.Repo{{Id: "slow", OriginUrl: "https://github.com/test/slow"}, {Id: "healthy", OriginUrl: "https://github.com/test/healthy"}},
			responses: map[string]*pb.ListReviewQueueResponse{"healthy": {Rows: []*pb.ReviewQueueRow{row}}},
		}}
		m := NewModel(c, context.Background(), nil)
		for range 2 {
			m = queueUpdate(m, m.load()())
			if m.fetching || len(m.rows) != 1 || m.rows[0].GetSession().GetId() != "healthy" || len(m.repoErrs) != 1 {
				t.Fatalf("slow repo starved healthy repo: rows=%v errors=%v", m.rows, m.repoErrs)
			}
		}
	})
}
