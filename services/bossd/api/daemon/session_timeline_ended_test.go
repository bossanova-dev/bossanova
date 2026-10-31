package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
)

const endedNewHead = "29792979297929792979297929792979297929bb"

// endedPRProvider serves one PR whose state, head and checks the test moves
// between poll cycles, plus its evidence comments, counting comment reads.
type endedPRProvider struct {
	vcs.Provider
	mu       sync.Mutex
	pr       vcs.PRStatus
	checks   []vcs.CheckResult
	comments []vcs.PRComment
	reads    int
}

func (p *endedPRProvider) set(pr vcs.PRStatus, checks []vcs.CheckResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pr, p.checks = pr, checks
}

// postEvidence has the verify stage write its evidence comment for head.
func (p *endedPRProvider) postEvidence(head string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	body := "<!-- boss-verify-evidence -->\n" + evidence.Format(evidence.Summary{
		Head: head, VideoURL: "https://evidence.example/v.mp4", VideoDuration: 160 * time.Second, Proven: 5, Total: 5,
	})
	p.comments = append([]vcs.PRComment{{Body: body, AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: at}}, p.comments...)
}

func (p *endedPRProvider) readCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads
}

func (p *endedPRProvider) GetPRStatus(context.Context, string, int) (*vcs.PRStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := p.pr
	return &pr, nil
}

func (p *endedPRProvider) GetCheckResults(context.Context, string, int) ([]vcs.CheckResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]vcs.CheckResult(nil), p.checks...), nil
}

func (p *endedPRProvider) GetReviewComments(context.Context, string, int) ([]vcs.ReviewComment, error) {
	return nil, nil
}

func (p *endedPRProvider) FindPRCommentsWithMarker(context.Context, string, int, string) ([]vcs.PRComment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	return append([]vcs.PRComment(nil), p.comments...), nil
}

// steppedRepos holds each display poll cycle at its repo listing until the
// test releases it, so the test runs whole cycles one at a time through the
// poller's exported Run.
type steppedRepos struct {
	db.RepoStore
	entered chan struct{}
	release chan struct{}
}

func (s steppedRepos) List(ctx context.Context) ([]*models.Repo, error) {
	select {
	case s.entered <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.RepoStore.List(ctx)
}

// endedPRHarness wires the daemon's real pieces for one session's PR: SQLite
// stores, the display tracker, the evidence reader, the display poller, the
// PR-event dispatcher and the timeline handler.
type endedPRHarness struct {
	sessions  db.SessionStore
	snapshots *db.SQLiteCheckSnapshotStore
	tracker   *status.DisplayTracker
	provider  *endedPRProvider
	srv       *Server
	repo      *models.Repo
	sess      *models.Session
	repos     steppedRepos
}

func newEndedPRHarness(t *testing.T) *endedPRHarness {
	t.Helper()
	ctx := context.Background()
	sqlDB := setupServerTestDB(t)
	repoStore := db.NewRepoStore(sqlDB)
	h := &endedPRHarness{
		sessions:  db.NewSessionStore(sqlDB),
		snapshots: db.NewCheckSnapshotStore(sqlDB),
		tracker:   status.NewDisplayTracker(),
		provider:  &endedPRProvider{},
		repos:     steppedRepos{RepoStore: repoStore, entered: make(chan struct{}), release: make(chan struct{})},
	}
	repo, err := repoStore.Create(ctx, db.CreateRepoParams{
		DisplayName: "r", LocalPath: "/tmp/r", OriginURL: "https://github.com/acme/r.git",
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	pr := 2979
	sess, err := h.sessions.Create(ctx, db.CreateSessionParams{
		RepoID: repo.ID, Title: "s", BranchName: "feat/x", BaseBranch: "main", PRNumber: &pr,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	h.repo, h.sess = repo, sess

	reader := evidence.NewReader(h.provider, zerolog.Nop())
	poller := session.NewDisplayPoller(h.sessions, h.repos, h.provider, h.tracker, endedPollInterval, zerolog.Nop())
	poller.SetSnapshotStore(h.snapshots)
	poller.SetEvidenceObserver(reader)
	runCtx, cancel := context.WithCancel(ctx)
	poller.Run(runCtx)
	t.Cleanup(func() {
		cancel()
		<-poller.Done()
	})
	// The first cycle starts at once and waits at its listing.
	<-h.repos.entered

	h.srv = New(Config{
		Repos: repoStore, Sessions: h.sessions, CheckSnapshots: h.snapshots,
		DisplayTracker: h.tracker, Evidence: reader, Provider: h.provider,
	})
	return h
}

// endedPollInterval is the harness poller's poll interval.
const endedPollInterval = time.Millisecond

// cycle runs one whole display poll cycle, after the poll interval has passed
// since the previous one, so the cycle polls every session not yet ended.
func (h *endedPRHarness) cycle() {
	time.Sleep(2 * endedPollInterval)
	h.repos.release <- struct{}{}
	// The next cycle reaches its listing only once this one has finished.
	<-h.repos.entered
}

// parkOn has the PR open on head with verify parked for a human.
func (h *endedPRHarness) parkOn(head string) {
	h.provider.set(vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: head}, greenCI(3, "needs human: human-review:ui"))
}

// mergeOn has the provider serve the PR merged on head.
func (h *endedPRHarness) mergeOn(head string) {
	h.provider.set(vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: head}, nil)
}

// dispatchMerged delivers the PR-merged webhook event through the dispatcher,
// which marks the display entry merged with no head.
func (h *endedPRHarness) dispatchMerged(t *testing.T) {
	t.Helper()
	dispatcher := session.NewDispatcher(h.sessions, h.repos.RepoStore, nil, zerolog.Nop())
	dispatcher.SetDisplayStatusSetter(h.tracker)
	events := make(chan session.Event, 1)
	events <- session.Event{SessionID: h.sess.ID, Event: vcs.PRMerged{PRID: 2979}}
	close(events)
	dispatcher.Run(context.Background(), events)
	if e := h.tracker.Get(h.sess.ID); e == nil || e.Status != vcs.DisplayStatusMerged || e.HeadSHA != "" {
		t.Fatalf("display entry after the webhook = %+v, want merged with no head", e)
	}
}

// refreshSessionPR runs RefreshSessionPR's display refresh against the PR as
// the provider now serves it.
func (h *endedPRHarness) refreshSessionPR(t *testing.T) {
	t.Helper()
	if err := h.srv.refreshSessionPRDisplay(context.Background(), h.repo, h.sess, 2979); err != nil {
		t.Fatalf("refreshSessionPRDisplay: %v", err)
	}
}

func (h *endedPRHarness) timeline(t *testing.T) []*pb.TimelineStep {
	t.Helper()
	resp, err := h.srv.GetSessionTimeline(context.Background(), connect.NewRequest(&pb.GetSessionTimelineRequest{SessionId: h.sess.ID}))
	if err != nil {
		t.Fatalf("GetSessionTimeline: %v", err)
	}
	return resp.Msg.GetSteps()
}

// terminalSnapshots counts the session's merged or closed snapshots.
func (h *endedPRHarness) terminalSnapshots(t *testing.T) int {
	t.Helper()
	snaps, err := h.snapshots.ListBySession(context.Background(), h.sess.ID, 100)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	n := 0
	for _, s := range snaps {
		if st := vcs.DisplayStatus(s.ComputedStatus); st == vcs.DisplayStatusMerged || st == vcs.DisplayStatusClosed {
			n++
		}
	}
	return n
}

func logSteps(t *testing.T, steps []*pb.TimelineStep) {
	t.Helper()
	for _, st := range steps {
		t.Logf("  %s %s | %s | %s", st.GetAt().AsTime().Format(time.RFC3339Nano), st.GetTitle(), st.GetDetail(), st.GetState())
	}
}

func assertNothingCurrent(t *testing.T, steps []*pb.TimelineStep) {
	t.Helper()
	for _, st := range steps {
		if st.GetState() == current {
			t.Fatalf("step %q is CURRENT; an ended PR waits on nothing", st.GetTitle())
		}
	}
}

// TestGetSessionTimeline_AWebhookEndedPRResolvesThroughTheDisplayPoller runs
// the real ordering end to end: polls see the PR parked, then the PR ends by a
// path that records no snapshot and the display poller never polls the
// session as open again — the PR-merged webhook through the dispatcher (no
// head, so the session's next scheduled poll reads the final head), with or
// without a new head pushed and verified between polls, or RefreshSessionPR
// after such a push. The poller's next cycles record the end in the snapshot history on
// the final head and read that head's evidence, so the timeline ends on
// Merged/Closed with nothing CURRENT and keeps the evidence step; later
// cycles neither write a second terminal snapshot nor read again.
func TestGetSessionTimeline_AWebhookEndedPRResolvesThroughTheDisplayPoller(t *testing.T) {
	for name, tc := range map[string]struct {
		end      func(t *testing.T, h *endedPRHarness)
		title    string
		wantHead string
	}{
		"PR-merged webhook": {
			end: func(t *testing.T, h *endedPRHarness) {
				// Verify posts its evidence on the parked head and merges.
				h.provider.postEvidence(timelineHead, at(44))
				h.mergeOn(timelineHead)
				h.dispatchMerged(t)
			},
			title:    "Merged",
			wantHead: timelineHead,
		},
		"PR-merged webhook after a push": {
			end: func(t *testing.T, h *endedPRHarness) {
				// A new head is pushed, verified and merged between polls.
				h.provider.postEvidence(endedNewHead, at(44))
				h.mergeOn(endedNewHead)
				h.dispatchMerged(t)
			},
			title:    "Merged",
			wantHead: endedNewHead,
		},
		"RefreshSessionPR after a push": {
			end: func(t *testing.T, h *endedPRHarness) {
				// A new head is pushed, verified and the PR closed between polls.
				h.provider.postEvidence(endedNewHead, at(44))
				h.provider.set(vcs.PRStatus{State: vcs.PRStateClosed, HeadSHA: endedNewHead}, nil)
				h.refreshSessionPR(t)
			},
			title:    "Closed",
			wantHead: endedNewHead,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newEndedPRHarness(t)
			h.parkOn(timelineHead)
			h.cycle()
			if got := h.provider.readCount(); got != 1 {
				t.Fatalf("reads after the parked poll = %d, want 1", got)
			}

			tc.end(t, h)
			endedAt := h.tracker.Get(h.sess.ID).UpdatedAt.Truncate(time.Millisecond)
			for range 3 {
				h.cycle()
			}

			steps := h.timeline(t)
			logSteps(t, steps)
			assertNothingCurrent(t, steps)
			last := steps[len(steps)-1]
			if last.GetTitle() != tc.title || !last.GetAt().AsTime().Equal(endedAt) {
				t.Fatalf("last step = %q at %s, want %q at %s", last.GetTitle(), last.GetAt().AsTime(), tc.title, endedAt)
			}
			var evidenceStep *pb.TimelineStep
			for _, st := range steps {
				if st.GetTitle() == "Evidence recorded" {
					evidenceStep = st
				}
			}
			if evidenceStep == nil || !evidenceStep.GetAt().AsTime().Equal(at(44)) {
				t.Fatalf("evidence step = %v, want the final head's evidence recorded at %s", evidenceStep, at(44))
			}
			if got := h.provider.readCount(); got != 2 {
				t.Fatalf("reads = %d, want 2 (the final head is read once when the PR ends)", got)
			}
			if got := h.terminalSnapshots(t); got != 1 {
				t.Fatalf("terminal snapshots = %d, want 1", got)
			}
			newest, err := h.snapshots.ListBySession(context.Background(), h.sess.ID, 1)
			if err != nil || len(newest) != 1 || newest[0].HeadSHA != tc.wantHead {
				t.Fatalf("newest snapshot = %+v, %v; want one on %s", newest, err, tc.wantHead)
			}
		})
	}
}

// TestGetSessionTimeline_AReopenedPRIsOpenAgain pins that the terminal
// snapshot the poller records for a webhook-ended PR does not outlive a
// reopen: once the reopened PR's display entry is open again, poll() resumes
// and the newer open snapshot supersedes the terminal one, so the timeline
// waits on the parked PR again — although the session state still says
// Merged.
func TestGetSessionTimeline_AReopenedPRIsOpenAgain(t *testing.T) {
	h := newEndedPRHarness(t)
	h.parkOn(timelineHead)
	h.cycle()
	h.mergeOn(timelineHead)
	h.dispatchMerged(t)
	h.cycle()
	if got := h.terminalSnapshots(t); got != 1 {
		t.Fatalf("terminal snapshots after the merge = %d, want 1", got)
	}
	assertNothingCurrent(t, h.timeline(t))

	h.parkOn(timelineHead)
	h.refreshSessionPR(t)
	h.cycle()

	steps := h.timeline(t)
	logSteps(t, steps)
	last := steps[len(steps)-1]
	if last.GetTitle() != "Parked for you · UI change" || last.GetState() != current {
		t.Fatalf("last step = %q (%s), want the parked PR CURRENT again", last.GetTitle(), last.GetState())
	}
	for _, st := range steps {
		if st.GetTitle() == "Merged" {
			t.Fatal("the reopened PR still shows Merged")
		}
	}
	if got := h.terminalSnapshots(t); got != 1 {
		t.Fatalf("terminal snapshots = %d, want 1 (an open PR records no end)", got)
	}
}

// TestGetSessionTimeline_APREndedBeforeItsFirstPollEndsOnTheEnd pins the
// empty-history ordering: the PR-merged webhook lands before any display poll
// of the session succeeded, so there is no snapshot to take a head from. The
// poller's next cycle records the end on the final head the provider reports,
// so the timeline ends on Merged with nothing CURRENT, and later cycles write
// no second terminal snapshot.
func TestGetSessionTimeline_APREndedBeforeItsFirstPollEndsOnTheEnd(t *testing.T) {
	h := newEndedPRHarness(t)
	h.mergeOn(timelineHead)
	h.dispatchMerged(t)
	endedAt := h.tracker.Get(h.sess.ID).UpdatedAt.Truncate(time.Millisecond)
	for range 3 {
		h.cycle()
	}

	steps := h.timeline(t)
	logSteps(t, steps)
	assertNothingCurrent(t, steps)
	if len(steps) == 0 {
		t.Fatal("timeline is empty, want it to end on Merged")
	}
	if last := steps[len(steps)-1]; last.GetTitle() != "Merged" || !last.GetAt().AsTime().Equal(endedAt) {
		t.Fatalf("last step = %q at %s, want Merged at %s", last.GetTitle(), last.GetAt().AsTime(), endedAt)
	}
	if got := h.terminalSnapshots(t); got != 1 {
		t.Fatalf("terminal snapshots = %d, want 1", got)
	}
}
