package server

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/status"
)

// blockerReply is one answer of the fake blockers verb; the last one repeats.
type blockerReply struct {
	result domain.VerifyBlockers
	err    error
}

// blockerVerbs is a verify-verbs double whose blockers verb counts its calls
// and, while gate is open, holds each call until gate closes.
type blockerVerbs struct {
	verifyVerbsFake
	mu      sync.Mutex
	gate    chan struct{}
	replies []blockerReply
	heads   []string
}

func (v *blockerVerbs) Blockers(ctx context.Context, _ domain.VerifyTarget, head string) (domain.VerifyBlockers, error) {
	v.mu.Lock()
	v.heads = append(v.heads, head)
	reply := v.replies[min(len(v.heads), len(v.replies))-1]
	gate := v.gate
	v.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return domain.VerifyBlockers{}, ctx.Err()
		}
	}
	return reply.result, reply.err
}

func (v *blockerVerbs) calls() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.heads...)
}

type blockerSessionStore struct {
	*listSessionsSessionStoreFake
	row *models.Session
}

func (f blockerSessionStore) Get(context.Context, string) (*models.Session, error) { return f.row, nil }

type blockerHarness struct {
	srv     *Server
	row     *models.Session
	tracker *status.DisplayTracker
	verbs   *blockerVerbs
	dones   chan (<-chan struct{})
	now     time.Time
}

func newBlockerHarness(t *testing.T, replies ...blockerReply) *blockerHarness {
	t.Helper()
	pr := 7
	row := &models.Session{ID: "s1", RepoID: "r1", State: machine.AwaitingChecks, PRNumber: &pr, WorktreePath: t.TempDir(), LastRepairAttemptCount: 1, UpdatedAt: time.Now()}
	tracker := status.NewDisplayTracker()
	tracker.Set(row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusFailing, HasFailures: true, HeadSHA: headPin})
	s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tracker, nil)
	s.sessions = blockerSessionStore{listSessionsSessionStoreFake: s.sessions.(*listSessionsSessionStoreFake), row: row}
	wireVerifyTestEnvironment(s)
	h := &blockerHarness{srv: s, row: row, tracker: tracker, verbs: &blockerVerbs{replies: replies}, dones: make(chan (<-chan struct{}), 64), now: time.Unix(1_000_000, 0)}
	s.newVerifyVerbs = func([]string) verifyVerbs { return h.verbs }
	s.attentionCache.blockers.refreshed = func(done <-chan struct{}) { h.dones <- done }
	s.attentionCache.blockers.now = func() time.Time { return h.now }
	return h
}

func (h *blockerHarness) list(t *testing.T) *pb.SessionAttention {
	t.Helper()
	resp, err := h.srv.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return onlySession(t, resp.Msg.Sessions).Attention
}

// awaitRefresh waits for the next background read to finish and fails when
// none was started.
func (h *blockerHarness) awaitRefresh(t *testing.T) {
	t.Helper()
	select {
	case done := <-h.dones:
		<-done
	default:
		t.Fatal("no background read was started")
	}
}

func (h *blockerHarness) noRefresh(t *testing.T) {
	t.Helper()
	if n := len(h.dones); n != 0 {
		t.Fatalf("%d unexpected background reads started", n)
	}
}

func written(blockers ...domain.VerifyBlocker) blockerReply {
	return blockerReply{result: domain.VerifyBlockers{Outcome: "written", Head: headPin, Blockers: blockers}}
}

var oneP2 = domain.VerifyBlocker{ID: "review:123456abcdef", Kind: "review-finding", Severity: "P2", File: "services/a.go", Line: 3, Summary: "s", Text: "t"}

func wantOneP2(t *testing.T, got *pb.SessionAttention) {
	t.Helper()
	tallies := got.GetBlockerTallies()
	if len(tallies) != 1 || tallies[0].GetSeverity() != "P2" || tallies[0].GetKind() != "review-finding" || tallies[0].GetBlockerCount() != 1 || !reflect.DeepEqual(got.GetBlockerFiles(), []string{"services/a.go"}) {
		t.Fatalf("blockers = %v %v, want one P2 review-finding in services/a.go", tallies, got.GetBlockerFiles())
	}
}

func wantNoBlockers(t *testing.T, got *pb.SessionAttention) {
	t.Helper()
	if len(got.GetBlockerTallies()) != 0 || len(got.GetBlockerFiles()) != 0 {
		t.Fatalf("blockers = %v %v, want none", got.GetBlockerTallies(), got.GetBlockerFiles())
	}
}

func TestAttentionBlockers_Sessions_AC233301_AC233302_ServeCurrentHeadAfterOneBackgroundRead(t *testing.T) {
	h := newBlockerHarness(t, written(oneP2), blockerReply{result: domain.VerifyBlockers{Outcome: "absent"}})
	h.verbs.gate = make(chan struct{})

	// The list returns while the read it started is still held at the gate.
	wantNoBlockers(t, h.list(t))
	if len(h.dones) != 1 {
		t.Fatalf("background reads started = %d, want 1", len(h.dones))
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			p := &pb.Session{Id: h.row.ID}
			h.srv.hydrateAttention(context.Background(), p, attentionDeps{row: h.row, display: h.tracker.Get(h.row.ID)})
			if len(p.Attention.GetBlockerTallies()) != 0 {
				t.Errorf("concurrent hydrate served %v before the read landed", p.Attention.GetBlockerTallies())
			}
		})
	}
	wg.Wait()
	if len(h.dones) != 1 {
		t.Fatalf("concurrent hydrates started %d background reads, want 1", len(h.dones))
	}
	close(h.verbs.gate)
	h.awaitRefresh(t)
	if got := h.verbs.calls(); !reflect.DeepEqual(got, []string{headPin}) {
		t.Fatalf("blockers verb heads = %v, want one read of the displayed head", got)
	}

	wantOneP2(t, h.list(t))
	wantOneP2(t, h.list(t))
	h.noRefresh(t)

	moved := strings.Repeat("b", 40)
	h.tracker.Set(h.row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusFailing, HasFailures: true, HeadSHA: moved})
	wantNoBlockers(t, h.list(t))
	h.awaitRefresh(t)
	if got := h.verbs.calls(); !reflect.DeepEqual(got, []string{headPin, moved}) {
		t.Fatalf("blockers verb heads = %v, want the moved head read once", got)
	}
	wantNoBlockers(t, h.list(t))
	h.noRefresh(t)
}

func TestAttentionBlockers_Sessions_AC233301_RetryErrorsAndUnreadableAfterBackoff(t *testing.T) {
	h := newBlockerHarness(t,
		blockerReply{err: errors.New("bridge down")},
		blockerReply{result: domain.VerifyBlockers{Outcome: "unreadable"}},
		written(oneP2))
	for range 2 {
		wantNoBlockers(t, h.list(t))
		h.awaitRefresh(t)
		wantNoBlockers(t, h.list(t))
		h.noRefresh(t)
		h.now = h.now.Add(blockerRetryBackoff + time.Second)
	}
	wantNoBlockers(t, h.list(t))
	h.awaitRefresh(t)
	wantOneP2(t, h.list(t))
	h.noRefresh(t)
	if got := len(h.verbs.calls()); got != 3 {
		t.Fatalf("blockers verb calls = %d, want the error and the unreadable read each retried once", got)
	}
}

// Absent means verify has not posted for the head yet; once it parks the same
// head, the list serves the blockers after the absent entry expires.
func TestAttentionBlockers_Sessions_AC233301_AbsentExpiresForTheSameHead(t *testing.T) {
	h := newBlockerHarness(t, blockerReply{result: domain.VerifyBlockers{Outcome: "absent"}}, written(oneP2))
	wantNoBlockers(t, h.list(t))
	h.awaitRefresh(t)

	h.now = h.now.Add(blockerAbsentTTL - time.Second)
	wantNoBlockers(t, h.list(t))
	h.noRefresh(t)

	h.now = h.now.Add(2 * time.Second)
	wantNoBlockers(t, h.list(t))
	h.awaitRefresh(t)
	for range 3 {
		h.now = h.now.Add(10 * blockerAbsentTTL)
		wantOneP2(t, h.list(t))
	}
	h.noRefresh(t)
	if got := h.verbs.calls(); !reflect.DeepEqual(got, []string{headPin, headPin}) {
		t.Fatalf("blockers verb heads = %v, want the same head read twice", got)
	}
}

func TestAttentionBlockers_NonCandidateNeverReads(t *testing.T) {
	h := newBlockerHarness(t, written(oneP2))
	h.row.LastRepairAttemptCount = 0
	wantNoBlockers(t, h.list(t))
	h.noRefresh(t)

	h.row.LastRepairAttemptCount = 1
	h.tracker.Set(h.row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusMerged, HeadSHA: headPin})
	wantNoBlockers(t, h.list(t))
	h.noRefresh(t)
	if got := h.verbs.calls(); len(got) != 0 {
		t.Fatalf("blockers verb heads = %v, want no read", got)
	}
}

func TestAttentionBlockers_Sessions_AC233302_AcceptListWritesThrough(t *testing.T) {
	h := newBlockerHarness(t, written(oneP2))
	resp, err := h.srv.ListSessionPRBlockers(t.Context(), connect.NewRequest(&pb.ListSessionPRBlockersRequest{Id: h.row.ID}))
	if err != nil || len(resp.Msg.GetBlockers()) != 1 {
		t.Fatalf("ListSessionPRBlockers = %v, %v", resp, err)
	}
	wantOneP2(t, h.list(t))
	h.noRefresh(t)
	if got := len(h.verbs.calls()); got != 1 {
		t.Fatalf("blockers verb calls = %d, want only the accept flow's read", got)
	}
}

// A read that started before the entry's read, such as a background read that
// lands after the accept flow's newer one, never replaces the newer answer.
func TestAttentionBlockers_Sessions_AC233302_OlderReadNeverReplacesNewer(t *testing.T) {
	h := newBlockerHarness(t, written(oneP2))
	resp, err := h.srv.ListSessionPRBlockers(t.Context(), connect.NewRequest(&pb.ListSessionPRBlockersRequest{Id: h.row.ID}))
	if err != nil || len(resp.Msg.GetBlockers()) != 1 {
		t.Fatalf("ListSessionPRBlockers = %v, %v", resp, err)
	}
	h.srv.recordBlockers(h.row.ID, headPin, h.now.Add(-time.Second), domain.VerifyBlockers{Outcome: "absent"})
	wantOneP2(t, h.list(t))

	h.srv.recordBlockers(h.row.ID, headPin, h.now.Add(time.Second), domain.VerifyBlockers{Outcome: "absent"})
	wantNoBlockers(t, h.list(t))
	h.noRefresh(t)
}
