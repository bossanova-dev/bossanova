package session

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/status"
)

type recordingEvidenceObserver struct {
	mu       sync.Mutex
	obs      []evidence.Observation
	retained []map[string]struct{}
}

func (r *recordingEvidenceObserver) Retain(live map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retained = append(r.retained, live)
}

func (r *recordingEvidenceObserver) retains() []map[string]struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]struct{}(nil), r.retained...)
}

func (r *recordingEvidenceObserver) Observe(_ context.Context, obs evidence.Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.obs = append(r.obs, obs)
}

func (r *recordingEvidenceObserver) all() []evidence.Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]evidence.Observation(nil), r.obs...)
}

func newEvidencePoller(t *testing.T, pr *vcs.PRStatus, checks []vcs.CheckResult) (*DisplayPoller, *recordingEvidenceObserver) {
	t.Helper()
	poller, obs, _ := newEvidencePollerWithStore(t, pr, checks)
	return poller, obs
}

func newEvidencePollerWithStore(t *testing.T, pr *vcs.PRStatus, checks []vcs.CheckResult) (*DisplayPoller, *recordingEvidenceObserver, *mockSessionStore) {
	t.Helper()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	prNum := 2979
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", PRNumber: &prNum}
	vp.nextPRStatus = pr
	vp.nextCheckResults = checks
	poller := NewDisplayPoller(sessions, repos, vp, status.NewDisplayTracker(), time.Minute, zerolog.Nop())
	obs := &recordingEvidenceObserver{}
	poller.SetEvidenceObserver(obs)
	return poller, obs, sessions
}

func TestDisplayPoller_ReportsAParkedHeadToTheEvidenceObserver(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	poller, obs := newEvidencePoller(t,
		&vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1"},
		[]vcs.CheckResult{
			{Name: "build", Status: vcs.CheckStatusCompleted, Conclusion: &success},
			{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusQueued, Description: "needs human: human-review:ui"},
		})

	if err := poller.RefreshPR(context.Background(), "owner/repo", 2979); err != nil {
		t.Fatalf("RefreshPR: %v", err)
	}

	got := obs.all()
	if len(got) != 1 {
		t.Fatalf("observations = %d, want 1", len(got))
	}
	want := evidence.Observation{SessionID: "sess-1", RepoPath: "owner/repo", PRNumber: 2979, HeadSHA: "head-1", IsPROpen: true, IsVerifyActive: true}
	if got[0] != want {
		t.Fatalf("observation = %+v, want %+v", got[0], want)
	}
}

func TestDisplayPoller_ReportsAMergedPRAsClosedToTheEvidenceObserver(t *testing.T) {
	poller, obs := newEvidencePoller(t, &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-1"}, nil)

	if err := poller.RefreshPR(context.Background(), "owner/repo", 2979); err != nil {
		t.Fatalf("RefreshPR: %v", err)
	}

	got := obs.all()
	if len(got) != 1 || got[0].IsPROpen || got[0].IsVerifyActive {
		t.Fatalf("observations = %+v, want one closed, inactive observation", got)
	}
}

func TestDisplayPoller_ADraftPRIsNotVerifyActive(t *testing.T) {
	poller, obs := newEvidencePoller(t, &vcs.PRStatus{State: vcs.PRStateOpen, Draft: true, HeadSHA: "head-1"}, nil)

	if err := poller.RefreshPR(context.Background(), "owner/repo", 2979); err != nil {
		t.Fatalf("RefreshPR: %v", err)
	}

	got := obs.all()
	if len(got) != 1 || !got[0].IsPROpen || got[0].IsVerifyActive {
		t.Fatalf("observations = %+v, want one open, inactive observation", got)
	}
}

// TestDisplayPoller_ReportsAConcludedVerifyToTheEvidenceObserver pins the
// signal the evidence reader needs when verify starts and finishes between two
// polls: a completed boss/verify on the head is reported as concluded, never
// as active.
func TestDisplayPoller_ReportsAConcludedVerifyToTheEvidenceObserver(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	poller, obs := newEvidencePoller(t,
		&vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1"},
		[]vcs.CheckResult{
			{Name: "build", Status: vcs.CheckStatusCompleted, Conclusion: &success},
			{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusCompleted, Conclusion: &success},
		})

	if err := poller.RefreshPR(context.Background(), "owner/repo", 2979); err != nil {
		t.Fatalf("RefreshPR: %v", err)
	}

	got := obs.all()
	if len(got) != 1 || !got[0].IsVerifyConcluded || got[0].IsVerifyActive {
		t.Fatalf("observations = %+v, want one concluded, inactive observation", got)
	}
}

// TestDisplayPoller_APendingVerifyIsNotConcluded pins that a head verify still
// holds, or one with no boss/verify at all, is not reported as concluded.
func TestDisplayPoller_APendingVerifyIsNotConcluded(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	for name, checks := range map[string][]vcs.CheckResult{
		"pending": {{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusQueued, Description: "verifying"}},
		"absent":  {{Name: "build", Status: vcs.CheckStatusCompleted, Conclusion: &success}},
	} {
		t.Run(name, func(t *testing.T) {
			poller, obs := newEvidencePoller(t, &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1"}, checks)
			if err := poller.RefreshPR(context.Background(), "owner/repo", 2979); err != nil {
				t.Fatalf("RefreshPR: %v", err)
			}
			got := obs.all()
			if len(got) != 1 || got[0].IsVerifyConcluded {
				t.Fatalf("observations = %+v, want one observation that is not concluded", got)
			}
		})
	}
}

// TestDisplayPoller_RetainsEvidenceForEveryListedSessionEachCycle pins the
// call pattern the evidence reader relies on: a merged PR is polled once and
// then skipped, but every cycle still reports its ended head, and reports it,
// with every other non-archived session, as live.
func TestDisplayPoller_RetainsEvidenceForEveryListedSessionEachCycle(t *testing.T) {
	poller, obs, sessions := newEvidencePollerWithStore(t, &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-1"}, nil)
	sessions.sessions["sess-2"] = &models.Session{ID: "sess-2", RepoID: "repo-1"}

	poller.poll(context.Background())
	poller.poll(context.Background())

	// The first cycle polls the merged PR; the second skips the poll but
	// reports the ended head, which the reader reads at most once.
	got := obs.all()
	if len(got) != 2 {
		t.Fatalf("observations = %d, want 2 (one poll, then the ended head)", len(got))
	}
	ended := evidence.Observation{SessionID: "sess-1", RepoPath: "owner/repo", PRNumber: 2979, HeadSHA: "head-1"}
	if got[1] != ended {
		t.Fatalf("second observation = %+v, want %+v", got[1], ended)
	}
	retains := obs.retains()
	if len(retains) != 2 {
		t.Fatalf("Retain calls = %d, want one per cycle", len(retains))
	}
	for i, set := range retains {
		_, merged := set["sess-1"]
		_, noPR := set["sess-2"]
		if !merged || !noPR || len(set) != 2 {
			t.Fatalf("cycle %d retained %v, want sess-1 and sess-2", i, set)
		}
	}
}

// failingListStore fails ListActive, as a transient store error would.
type failingListStore struct{ *mockSessionStore }

func (failingListStore) ListActive(context.Context, string) ([]*models.Session, error) {
	return nil, errors.New("database is locked")
}

// TestDisplayPoller_SkipsRetainWhenAListingFails pins that a cycle which could
// not list every repo's sessions prunes nothing: the sessions it missed are
// not gone, and a merged session's evidence is not read again.
func TestDisplayPoller_SkipsRetainWhenAListingFails(t *testing.T) {
	poller, obs, sessions := newEvidencePollerWithStore(t, &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-1"}, nil)
	poller.sessions = failingListStore{sessions}

	poller.poll(context.Background())

	if got := obs.retains(); len(got) != 0 {
		t.Fatalf("Retain calls = %v, want none after a failed listing", got)
	}
}

// countingCommentFinder counts evidence comment reads and finds no comment,
// or fails every read while err is set.
type countingCommentFinder struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *countingCommentFinder) FindPRCommentsWithMarker(context.Context, string, int, string) ([]vcs.PRComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return nil, f.err
}

func (f *countingCommentFinder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *countingCommentFinder) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// parkedChecks is green CI with verify parked for a human.
func parkedChecks() []vcs.CheckResult {
	success := vcs.CheckConclusionSuccess
	return []vcs.CheckResult{
		{Name: "build", Status: vcs.CheckStatusCompleted, Conclusion: &success},
		{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusQueued, Description: "needs human: human-review:ui"},
	}
}

// newEndedPRPoller is a poller over one open, parked PR on head-1 with a
// snapshot store and a real evidence reader, after one poll has recorded the
// parked head and read its evidence.
func newEndedPRPoller(t *testing.T) (*DisplayPoller, *mockCheckSnapshotStore, *countingCommentFinder) {
	t.Helper()
	poller, _, _ := newEvidencePollerWithStore(t, &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1"}, parkedChecks())
	snapshots := newMockCheckSnapshotStore()
	poller.SetSnapshotStore(snapshots)
	finder := &countingCommentFinder{}
	poller.SetEvidenceObserver(evidence.NewReader(finder, zerolog.Nop()))

	poller.poll(context.Background())
	if got := len(snapshots.all()); got != 1 {
		t.Fatalf("snapshots after the parked poll = %d, want 1", got)
	}
	if got := finder.count(); got != 1 {
		t.Fatalf("reads after the parked poll = %d, want 1", got)
	}
	return poller, snapshots, finder
}

// TestDisplayPoller_SettlesAWebhookEndedPRIntoTheSnapshotHistory models the
// orderings where the display entry turns terminal before any poll sees the
// PR end: the PR-merged/closed webhook (Dispatcher.persistTerminalPRState)
// sets it with no head, so the session's next scheduled poll reads the PR's
// final head — here also one pushed and merged after the last poll — and
// RefreshSessionPR sets it with the PR's final head, so poll() never polls the
// session again. Either way the poller records the end as one terminal
// snapshot on the head the reader is told about, dated by the entry, reads
// that head's evidence once, and writes nothing more on later cycles.
func TestDisplayPoller_SettlesAWebhookEndedPRIntoTheSnapshotHistory(t *testing.T) {
	for name, tc := range map[string]struct {
		terminal vcs.DisplayInfo
		final    vcs.PRStatus
		wantHead string
	}{
		"webhook dispatcher, no head":                 {terminal: vcs.DisplayInfo{Status: vcs.DisplayStatusMerged}, final: vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-1"}, wantHead: "head-1"},
		"webhook dispatcher, head pushed then merged": {terminal: vcs.DisplayInfo{Status: vcs.DisplayStatusMerged}, final: vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-2"}, wantHead: "head-2"},
		"RefreshSessionPR, head pushed, closed":       {terminal: vcs.DisplayInfo{Status: vcs.DisplayStatusClosed, HeadSHA: "head-2"}, final: vcs.PRStatus{State: vcs.PRStateClosed, HeadSHA: "head-2"}, wantHead: "head-2"},
	} {
		t.Run(name, func(t *testing.T) {
			poller, snapshots, finder := newEndedPRPoller(t)
			final := tc.final
			poller.provider.(*mockVCSProvider).nextPRStatus = &final
			poller.tracker.Set("sess-1", tc.terminal)
			entry := poller.tracker.Get("sess-1")

			for range 3 {
				elapsePollInterval(poller)
				poller.poll(context.Background())
			}

			snaps := snapshots.all()
			if len(snaps) != 2 {
				t.Fatalf("snapshots = %d, want 2 (the parked poll, then one terminal snapshot)", len(snaps))
			}
			end := snaps[1]
			if end.HeadSHA != tc.wantHead || end.ComputedStatus != int(tc.terminal.Status) || !end.PolledAt.Equal(entry.UpdatedAt) || end.RawJSON != "null" {
				t.Fatalf("terminal snapshot = %+v, want status %d on %s at %s", end, tc.terminal.Status, tc.wantHead, entry.UpdatedAt)
			}
			if got := finder.count(); got != 2 {
				t.Fatalf("reads = %d, want 2 (the ended head is read once)", got)
			}
		})
	}
}

// TestDisplayPoller_ReadsAWebhookMergeOnTheHeadItMergedOn pins the merge
// webhook's settlement against a head pushed and merged between polls: the
// dispatcher's entry carries no head, so the newest snapshot's head is stale,
// and the session's next scheduled poll reads the PR's final head once and
// reports that head — never the stale one — to the evidence reader on every
// later cycle, with no further provider read.
func TestDisplayPoller_ReadsAWebhookMergeOnTheHeadItMergedOn(t *testing.T) {
	poller, obs, _ := newEvidencePollerWithStore(t, &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1"}, parkedChecks())
	snapshots := newMockCheckSnapshotStore()
	poller.SetSnapshotStore(snapshots)
	vp := poller.provider.(*mockVCSProvider)
	poller.poll(context.Background())

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-2"}
	poller.tracker.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusMerged})
	for range 3 {
		elapsePollInterval(poller)
		poller.poll(context.Background())
	}

	ended := obs.all()[1:]
	if len(ended) != 3 {
		t.Fatalf("ended observations = %+v, want one per cycle", ended)
	}
	for _, o := range ended {
		if o.HeadSHA != "head-2" || o.IsPROpen {
			t.Fatalf("ended observation = %+v, want the merged head-2", o)
		}
	}
	if got := len(vp.getPRStatusPRNumbers); got != 2 {
		t.Fatalf("PR status reads = %d, want 2 (the parked poll, then one for the final head)", got)
	}
	if snaps := snapshots.all(); len(snaps) != 2 || snaps[1].HeadSHA != "head-2" {
		t.Fatalf("snapshots = %+v, want the merge recorded on head-2", snaps)
	}
}

// elapsePollInterval forgets every session's last poll, as though the poll
// interval had passed since.
func elapsePollInterval(p *DisplayPoller) {
	p.lastPollMu.Lock()
	defer p.lastPollMu.Unlock()
	p.lastPoll = nil
}

// warnLines counts the warn-level lines a JSON zerolog logger wrote.
func warnLines(logs *bytes.Buffer) int {
	return strings.Count(logs.String(), `"level":"warn"`)
}

// TestDisplayPoller_AReopenBehindAWebhookEndResumesOpenPolling pins a PR
// closed by webhook and reopened before the next poll: the dispatcher's entry
// is closed with no head, and bossd has no reopen handler. The session's next
// poll reads the provider once at the poll interval, finds the PR open, and
// replaces the stale terminal entry with the open status, so the session is
// polled as an open PR again — not re-read and warned about every cycle.
func TestDisplayPoller_AReopenBehindAWebhookEndResumesOpenPolling(t *testing.T) {
	poller, _, _ := newEvidencePollerWithStore(t, &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1"}, parkedChecks())
	snapshots := newMockCheckSnapshotStore()
	poller.SetSnapshotStore(snapshots)
	var logs bytes.Buffer
	poller.logger = zerolog.New(&logs)
	vp := poller.provider.(*mockVCSProvider)
	ctx := context.Background()
	poller.poll(ctx)

	poller.tracker.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusClosed})
	logs.Reset()
	elapsePollInterval(poller)
	for range 3 {
		poller.poll(ctx)
	}

	if got := len(vp.getPRStatusPRNumbers); got != 2 {
		t.Fatalf("PR status reads = %d, want 2 (the parked poll, then one at the next interval)", got)
	}
	if e := poller.tracker.Get("sess-1"); e == nil || isTerminalDisplayStatus(e.Status) || e.HeadSHA != "head-1" {
		t.Fatalf("display entry = %+v, want the reopened PR open on head-1", e)
	}
	if got := warnLines(&logs); got != 0 {
		t.Fatalf("warnings = %d, want none:\n%s", got, logs.String())
	}
	elapsePollInterval(poller)
	poller.poll(ctx)
	if got := len(vp.getPRStatusPRNumbers); got != 3 {
		t.Fatalf("PR status reads = %d, want 3 (open polling resumed)", got)
	}
	for _, s := range snapshots.all() {
		if isTerminalDisplayStatus(vcs.DisplayStatus(s.ComputedStatus)) {
			t.Fatalf("snapshot = %+v, want no end recorded for a reopened PR", s)
		}
	}
}

// TestDisplayPoller_RetriesAFailedWebhookEndReadAtThePollInterval pins that a
// webhook end whose PR read fails records nothing, tells the reader nothing
// and is retried only at the poll interval, then settles on the head the
// provider reports once it answers.
func TestDisplayPoller_RetriesAFailedWebhookEndReadAtThePollInterval(t *testing.T) {
	poller, obs, _ := newEvidencePollerWithStore(t, &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1"}, parkedChecks())
	snapshots := newMockCheckSnapshotStore()
	poller.SetSnapshotStore(snapshots)
	vp := poller.provider.(*mockVCSProvider)
	ctx := context.Background()
	poller.poll(ctx)

	poller.tracker.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusMerged})
	vp.prStatusErr = errors.New("gh: HTTP 502")
	elapsePollInterval(poller)
	for range 3 {
		poller.poll(ctx)
	}
	if got := len(vp.getPRStatusPRNumbers); got != 2 {
		t.Fatalf("PR status reads = %d, want 2 (a failed read waits out the poll interval)", got)
	}
	if got := len(snapshots.all()); got != 1 {
		t.Fatalf("snapshots after a failed read = %d, want 1 (the parked poll)", got)
	}
	if got := len(obs.all()); got != 1 {
		t.Fatalf("observations after a failed read = %d, want 1 (the parked poll)", got)
	}

	vp.prStatusErr = nil
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-2"}
	elapsePollInterval(poller)
	poller.poll(ctx)
	if snaps := snapshots.all(); len(snaps) != 2 || snaps[1].HeadSHA != "head-2" || snaps[1].ComputedStatus != int(vcs.DisplayStatusMerged) {
		t.Fatalf("snapshots = %+v, want the merge recorded on head-2", snaps)
	}
	if all := obs.all(); all[len(all)-1].HeadSHA != "head-2" || all[len(all)-1].IsPROpen {
		t.Fatalf("last observation = %+v, want the merged head-2", all[len(all)-1])
	}
}

// TestDisplayPoller_DatesTheEndAfterTheSnapshotItSupersedes pins that the
// terminal snapshot is never dated before the newest snapshot: a poll can
// persist an open snapshot after the webhook set the entry terminal, and a
// terminal snapshot dated earlier would not be the newest, so the timeline
// would stay open and every cycle would write another.
func TestDisplayPoller_DatesTheEndAfterTheSnapshotItSupersedes(t *testing.T) {
	poller, snapshots, _ := newEndedPRPoller(t)
	poller.provider.(*mockVCSProvider).nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-1"}
	poller.tracker.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusMerged})
	late := poller.tracker.Get("sess-1").UpdatedAt.Add(time.Hour)
	if err := snapshots.Insert(context.Background(), db.CheckSnapshot{SessionID: "sess-1", HeadSHA: "head-1", PolledAt: late, ComputedStatus: int(vcs.DisplayStatusNeedsHuman)}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	elapsePollInterval(poller)
	poller.poll(context.Background())

	snaps := snapshots.all()
	if end := snaps[len(snaps)-1]; end.ComputedStatus != int(vcs.DisplayStatusMerged) || end.PolledAt.Before(late) {
		t.Fatalf("terminal snapshot = %+v, want merged, dated no earlier than %s", end, late)
	}
}

// TestDisplayPoller_SettlesAnEndBeforeTheFirstPoll pins the empty-history
// ordering: the entry turns terminal before any poll of the session succeeded,
// so there is no snapshot to take a head from. The end is still recorded as
// one terminal snapshot, on the entry's head (RefreshSessionPR) or the PR's
// final head the session's first poll reads (the webhook), so the timeline
// ends on it.
func TestDisplayPoller_SettlesAnEndBeforeTheFirstPoll(t *testing.T) {
	for name, terminal := range map[string]vcs.DisplayInfo{
		"webhook dispatcher, no head": {Status: vcs.DisplayStatusMerged},
		"RefreshSessionPR":            {Status: vcs.DisplayStatusMerged, HeadSHA: "head-1"},
	} {
		t.Run(name, func(t *testing.T) {
			poller, obs, _ := newEvidencePollerWithStore(t, &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-1"}, nil)
			snapshots := newMockCheckSnapshotStore()
			poller.SetSnapshotStore(snapshots)
			poller.tracker.Set("sess-1", terminal)
			entry := poller.tracker.Get("sess-1")

			poller.poll(context.Background())
			poller.poll(context.Background())

			snaps := snapshots.all()
			if len(snaps) != 1 || snaps[0].HeadSHA != "head-1" || snaps[0].ComputedStatus != int(vcs.DisplayStatusMerged) || !snaps[0].PolledAt.Equal(entry.UpdatedAt) {
				t.Fatalf("snapshots = %+v, want one merged snapshot on head-1 at %s", snaps, entry.UpdatedAt)
			}
			for _, o := range obs.all() {
				if o.HeadSHA != "head-1" {
					t.Fatalf("observation = %+v, want head-1", o)
				}
			}
		})
	}
}

// TestDisplayPoller_LeavesAPollRecordedEndAlone pins that a PR whose end a
// poll recorded gets no second terminal snapshot and no second read.
func TestDisplayPoller_LeavesAPollRecordedEndAlone(t *testing.T) {
	poller, _, _ := newEvidencePollerWithStore(t, &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-1"}, nil)
	snapshots := newMockCheckSnapshotStore()
	poller.SetSnapshotStore(snapshots)
	finder := &countingCommentFinder{}
	poller.SetEvidenceObserver(evidence.NewReader(finder, zerolog.Nop()))

	for range 3 {
		poller.poll(context.Background())
	}

	if got := snapshots.all(); len(got) != 1 {
		t.Fatalf("snapshots = %d, want 1 (the poll that saw the merge)", len(got))
	}
	if got := finder.count(); got != 1 {
		t.Fatalf("reads = %d, want 1", got)
	}
}

// TestDisplayPoller_RetriesAFailedEndedReadEachInterval pins that the ended
// head the poller reports every cycle is what retries a failed read of it:
// not within the reader's refresh interval, then once per interval until a
// read succeeds, and never after.
func TestDisplayPoller_RetriesAFailedEndedReadEachInterval(t *testing.T) {
	poller, _, _ := newEvidencePollerWithStore(t, &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-1"}, nil)
	poller.SetSnapshotStore(newMockCheckSnapshotStore())
	finder := &countingCommentFinder{err: errors.New("gh: HTTP 502")}
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	poller.SetEvidenceObserver(evidence.NewReader(finder, zerolog.Nop(),
		evidence.WithClock(func() time.Time { return now }),
		evidence.WithRefreshInterval(time.Minute)))
	ctx := context.Background()
	wantReads := func(want int, why string) {
		t.Helper()
		if got := finder.count(); got != want {
			t.Fatalf("reads = %d, want %d (%s)", got, want, why)
		}
	}

	poller.poll(ctx)
	wantReads(1, "the poll that saw the merge reads the ended head")
	now = now.Add(30 * time.Second)
	poller.poll(ctx)
	wantReads(1, "a failed read waits out the interval")
	now = now.Add(30 * time.Second)
	poller.poll(ctx)
	wantReads(2, "the failed ended read is retried after the interval")

	finder.fail(nil)
	now = now.Add(time.Minute)
	poller.poll(ctx)
	wantReads(3, "retried again while it keeps failing")
	now = now.Add(time.Minute)
	poller.poll(ctx)
	poller.poll(ctx)
	wantReads(3, "a successful ended read is not repeated")
}

// TestDisplayPoller_RefreshPRSettlesAWebhookEndAtOnce pins the webhook
// ordering where the dispatcher sets the PR-merged entry, with no head, before
// the webhook's own RefreshPR runs, in a repo whose webhooks are healthy so
// the scheduled poll is stretched to webhookSafetyNetInterval: the refresh
// reads the provider once and records the end on the head it read — the
// timeline and the evidence reader do not wait for the stretched poll — and
// later cycles settle with no read.
func TestDisplayPoller_RefreshPRSettlesAWebhookEndAtOnce(t *testing.T) {
	poller, obs, _ := newEvidencePollerWithStore(t, &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1"}, parkedChecks())
	snapshots := newMockCheckSnapshotStore()
	poller.SetSnapshotStore(snapshots)
	poller.SetWebhookHealth(stubWebhookHealth{"owner/repo": true})
	vp := poller.provider.(*mockVCSProvider)
	ctx := context.Background()
	poller.poll(ctx)

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-2"}
	poller.tracker.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusMerged})
	entry := poller.tracker.Get("sess-1")
	poller.poll(ctx)
	if got := len(vp.getPRStatusPRNumbers); got != 1 {
		t.Fatalf("PR status reads before the refresh = %d, want 1 (the stretched scheduled poll is not due)", got)
	}

	if err := poller.RefreshPR(ctx, "owner/repo", 2979); err != nil {
		t.Fatalf("RefreshPR: %v", err)
	}
	if got := len(vp.getPRStatusPRNumbers); got != 2 {
		t.Fatalf("PR status reads = %d, want 2 (the refresh reads the ended PR)", got)
	}
	snaps := snapshots.all()
	if len(snaps) != 2 || snaps[1].HeadSHA != "head-2" || snaps[1].ComputedStatus != int(vcs.DisplayStatusMerged) || !snaps[1].PolledAt.Equal(entry.UpdatedAt) {
		t.Fatalf("snapshots = %+v, want the merge recorded on head-2 at %s", snaps, entry.UpdatedAt)
	}
	if all := obs.all(); len(all) != 2 || all[1].HeadSHA != "head-2" || all[1].IsPROpen {
		t.Fatalf("observations = %+v, want the parked head then the merged head-2", all)
	}

	for range 2 {
		poller.poll(ctx)
		if err := poller.RefreshPR(ctx, "owner/repo", 2979); err != nil {
			t.Fatalf("RefreshPR: %v", err)
		}
	}
	if got := len(vp.getPRStatusPRNumbers); got != 2 {
		t.Fatalf("PR status reads after settling = %d, want 2", got)
	}
	if got := len(snapshots.all()); got != 2 {
		t.Fatalf("snapshots after settling = %d, want 2", got)
	}
}

// TestDisplayPoller_RefreshPRSkipsAnEndCarryingItsHead pins that RefreshPR
// does not read the provider for a terminal entry that already carries the
// head the PR ended on (RefreshSessionPR's); poll() settles that end.
func TestDisplayPoller_RefreshPRSkipsAnEndCarryingItsHead(t *testing.T) {
	poller, obs, _ := newEvidencePollerWithStore(t, &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-1"}, nil)
	poller.SetSnapshotStore(newMockCheckSnapshotStore())
	vp := poller.provider.(*mockVCSProvider)
	poller.tracker.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusMerged, HeadSHA: "head-1"})

	if err := poller.RefreshPR(context.Background(), "owner/repo", 2979); err != nil {
		t.Fatalf("RefreshPR: %v", err)
	}
	if got := len(vp.getPRStatusPRNumbers); got != 0 {
		t.Fatalf("PR status reads = %d, want 0", got)
	}
	if got := len(obs.all()); got != 0 {
		t.Fatalf("observations = %d, want 0", got)
	}
}

// TestDisplayPoller_LeavesAHeadlessEndForTheNextPoll pins that a provider
// answer reporting the PR ended with no head records no terminal snapshot and
// tells the reader nothing: the session's entry stays head-less, so the next
// poll reads the PR again and settles on the head it then reports.
func TestDisplayPoller_LeavesAHeadlessEndForTheNextPoll(t *testing.T) {
	poller, obs, _ := newEvidencePollerWithStore(t, &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "head-1"}, parkedChecks())
	snapshots := newMockCheckSnapshotStore()
	poller.SetSnapshotStore(snapshots)
	vp := poller.provider.(*mockVCSProvider)
	ctx := context.Background()
	poller.poll(ctx)

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged}
	elapsePollInterval(poller)
	poller.poll(ctx)
	if got := len(snapshots.all()); got != 1 {
		t.Fatalf("snapshots after a head-less end = %d, want 1 (the parked poll)", got)
	}
	if got := len(obs.all()); got != 1 {
		t.Fatalf("observations after a head-less end = %d, want 1 (the parked poll)", got)
	}

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "head-2"}
	elapsePollInterval(poller)
	poller.poll(ctx)
	if snaps := snapshots.all(); len(snaps) != 2 || snaps[1].HeadSHA != "head-2" || snaps[1].ComputedStatus != int(vcs.DisplayStatusMerged) {
		t.Fatalf("snapshots = %+v, want the merge recorded on head-2", snaps)
	}
	if all := obs.all(); all[len(all)-1].HeadSHA != "head-2" || all[len(all)-1].IsPROpen {
		t.Fatalf("last observation = %+v, want the merged head-2", all[len(all)-1])
	}
}
