package server

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/attention"
	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/status"
)

// verifiedChecks are green ordinary checks plus a successful boss/verify, the
// check set of a head the verify stage passed.
func verifiedChecks() []vcs.CheckResult {
	success := vcs.CheckConclusionSuccess
	return append(greenCI(2, ""), vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusCompleted, Conclusion: &success, Description: "verified"})
}

// headSnap is a persisted poll of the session "s" on timelineHead.
func headSnap(t *testing.T, when time.Time, st vcs.DisplayStatus, checks []vcs.CheckResult) db.CheckSnapshot {
	t.Helper()
	s := snap(t, when, st, checks)
	s.SessionID = "s"
	return s
}

// newestFirst orders snaps as ListBySession returns them.
func newestFirst(snaps []db.CheckSnapshot) []db.CheckSnapshot {
	out := slices.Clone(snaps)
	slices.SortStableFunc(out, func(a, b db.CheckSnapshot) int { return b.PolledAt.Compare(a.PolledAt) })
	return out
}

func listOneAttention(t *testing.T, s *Server) *pb.SessionAttention {
	t.Helper()
	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return onlySession(t, resp.Msg.Sessions).GetAttention()
}

// TestAttentionVerifiedNotMerged covers #3186's verdict finding 2: in a repo
// with an enabled verify job, a green, mergeable PR whose head verify passed
// stays in progress while verify can still merge it, and is a MERGE under
// NEEDS YOU once verify has left it unmerged for attention.VerifyMergeGrace,
// dated from the first poll of the newest unbroken run that saw it verified.
func TestAttentionVerifiedNotMerged(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	green := greenCI(2, "")
	for _, tc := range []struct {
		name     string
		factory  bool
		snaps    []db.CheckSnapshot
		kind     pb.AttentionKind
		sentence string
		since    time.Time
	}{
		{
			name:    "verified moments ago",
			factory: true,
			snaps:   []db.CheckSnapshot{headSnap(t, ago(50*time.Minute), vcs.DisplayStatusPassing, green), headSnap(t, ago(2*time.Minute), vcs.DisplayStatusPassing, verifiedChecks())},
			kind:    pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
		},
		{
			name:     "verified and left unmerged",
			factory:  true,
			snaps:    []db.CheckSnapshot{headSnap(t, ago(90*time.Minute), vcs.DisplayStatusPassing, green), headSnap(t, ago(45*time.Minute), vcs.DisplayStatusPassing, verifiedChecks()), headSnap(t, ago(time.Minute), vcs.DisplayStatusPassing, verifiedChecks())},
			kind:     pb.AttentionKind_ATTENTION_KIND_MERGE,
			sentence: attention.VerifiedNotMergedSentence,
			since:    ago(45 * time.Minute),
		},
		{
			name:    "re-verified moments ago after its status was cleared",
			factory: true,
			snaps:   []db.CheckSnapshot{headSnap(t, ago(80*time.Minute), vcs.DisplayStatusPassing, verifiedChecks()), headSnap(t, ago(10*time.Minute), vcs.DisplayStatusPassing, green), headSnap(t, ago(3*time.Minute), vcs.DisplayStatusPassing, verifiedChecks())},
			kind:    pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
		},
		{
			name:     "no verify job",
			snaps:    []db.CheckSnapshot{headSnap(t, ago(2*time.Minute), vcs.DisplayStatusPassing, verifiedChecks())},
			kind:     pb.AttentionKind_ATTENTION_KIND_MERGE,
			sentence: "Ready to merge",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 1
			mergeable := true
			entered := ago(7 * time.Hour)
			row := &models.Session{ID: "s", RepoID: "r", PRNumber: &n, State: machine.ReadyForReview, StateEnteredAt: &entered, UpdatedAt: entered}
			tr := status.NewDisplayTracker()
			tr.Set("s", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead, Mergeable: &mergeable})
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tr, nil)
			if tc.factory {
				s.cronJobs = &attentionCronStore{jobs: []*domain.CronJob{{Prompt: "/boss-verify", IsEnabled: true}}}
			}
			s.checkSnapshots = &fakeCheckSnapshotStore{snaps: newestFirst(tc.snaps)}
			got := listOneAttention(t, s)
			if got.GetKind() != tc.kind {
				t.Fatalf("attention = %v, want %v", got, tc.kind)
			}
			if tc.kind != pb.AttentionKind_ATTENTION_KIND_MERGE {
				return
			}
			if got.GetSentence() != tc.sentence || got.GetIsUnverified() {
				t.Fatalf("attention = %v, want a verified MERGE reading %q", got, tc.sentence)
			}
			if !tc.since.IsZero() && !got.GetSince().AsTime().Equal(tc.since) {
				t.Fatalf("since = %v, want the verification at %v", got.GetSince().AsTime(), tc.since)
			}
			if again := listOneAttention(t, s); again.GetKind() != got.GetKind() || !again.GetSince().AsTime().Equal(got.GetSince().AsTime()) {
				t.Fatalf("a second read answered %v, first %v", again, got)
			}
			if !tc.factory {
				return
			}
			steps := buildTimeline(timelineFacts{HasPR: true, HeadSHA: timelineHead, Snapshots: tc.snaps, IsFactoryRepo: true, Now: now})
			i := slices.IndexFunc(steps, func(st *pb.TimelineStep) bool {
				return st.GetState() == pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT
			})
			if i < 0 || steps[i].GetTitle() != "Verified" || !steps[i].GetAt().AsTime().Equal(got.GetSince().AsTime()) {
				t.Fatalf("timeline = %v, want its current step \"Verified\" at the header's %v", steps, got.GetSince().AsTime())
			}
		})
	}
}

// TestAttentionSentBackHeadIsNotVerifiedNotMerged covers #3210's verdict
// finding 7: verify's merge refuses a head whose newest operator record is a
// legacy send-back, so neither the session row nor the timeline offers it as
// verified but not merged; it stays in progress, as verify sees it.
func TestAttentionSentBackHeadIsNotVerifiedNotMerged(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	snaps := []db.CheckSnapshot{
		headSnap(t, ago(90*time.Minute), vcs.DisplayStatusPassing, greenCI(2, "")),
		headSnap(t, ago(45*time.Minute), vcs.DisplayStatusPassing, verifiedChecks()),
		headSnap(t, ago(time.Minute), vcs.DisplayStatusPassing, verifiedChecks()),
	}
	for _, tc := range []struct {
		name     string
		sentBack bool
		kind     pb.AttentionKind
	}{
		{"approved or never sent back", false, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"sent back", true, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 1
			mergeable := true
			entered := ago(7 * time.Hour)
			row := &models.Session{ID: "s", RepoID: "r", PRNumber: &n, State: machine.ReadyForReview, StateEnteredAt: &entered, UpdatedAt: entered}
			tr := status.NewDisplayTracker()
			tr.Set("s", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead, Mergeable: &mergeable})
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tr, nil)
			s.cronJobs = &attentionCronStore{jobs: []*domain.CronJob{{Prompt: "/boss-verify", IsEnabled: true}}}
			s.checkSnapshots = &fakeCheckSnapshotStore{snaps: newestFirst(snaps)}
			s.evidence = sentBackEvidence{head: timelineHead, sentBack: tc.sentBack}
			if got := listOneAttention(t, s); got.GetKind() != tc.kind {
				t.Fatalf("attention = %v, want %v", got, tc.kind)
			}
			if _, kind := assembleTimeline(timelineFacts{HasPR: true, HeadSHA: timelineHead, Snapshots: snaps, IsFactoryRepo: true, IsSentBack: tc.sentBack, Now: now}); kind != tc.kind {
				t.Fatalf("timeline kind = %v, want %v", kind, tc.kind)
			}
		})
	}
}

// sentBackEvidence reports a legacy send-back on head when sentBack is set.
type sentBackEvidence struct {
	stubEvidence
	head     string
	sentBack bool
}

func (e sentBackEvidence) SentBack(_, head string) bool { return e.sentBack && head == e.head }

// TestAttentionParkIsDatedFromThePark covers the session header reading "fix ·
// failed 7h ago" for a park minutes old: a verify park's attention is dated
// from the first poll of the newest unbroken run that saw the head parked,
// the time the timeline gives its park step, not from when the session's state
// began.
func TestAttentionParkIsDatedFromThePark(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	parked := greenCI(2, "needs human: ledger-open")
	for _, code := range []string{"ledger-open", "human-review:ui"} {
		t.Run(code, func(t *testing.T) {
			n := 1
			entered := ago(7 * time.Hour)
			row := &models.Session{ID: "s", RepoID: "r", PRNumber: &n, State: machine.ReadyForReview, StateEnteredAt: &entered, UpdatedAt: entered}
			tr := status.NewDisplayTracker()
			tr.Set("s", vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, VerifyReason: code, HeadSHA: timelineHead})
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tr, nil)
			snaps := []db.CheckSnapshot{
				headSnap(t, ago(6*time.Hour), vcs.DisplayStatusNeedsHuman, parked),
				headSnap(t, ago(3*time.Hour), vcs.DisplayStatusVerifying, greenCI(2, "verifying")),
				headSnap(t, ago(20*time.Minute), vcs.DisplayStatusNeedsHuman, parked),
				headSnap(t, ago(time.Minute), vcs.DisplayStatusNeedsHuman, parked),
			}
			s.checkSnapshots = &fakeCheckSnapshotStore{snaps: newestFirst(snaps)}
			got := listOneAttention(t, s)
			if len(got.GetParkCodes()) == 0 {
				t.Fatalf("attention = %v, want the park", got)
			}
			if want := ago(20 * time.Minute); !got.GetSince().AsTime().Equal(want) {
				t.Fatalf("since = %v, want the park's start %v", got.GetSince().AsTime(), want)
			}
			steps, _ := assembleTimeline(timelineFacts{HasPR: true, HeadSHA: timelineHead, Snapshots: snaps})
			if !slices.ContainsFunc(steps, func(st *pb.TimelineStep) bool { return st.GetAt().AsTime().Equal(got.GetSince().AsTime()) }) {
				t.Fatalf("no timeline step at the park's start %v: %v", got.GetSince().AsTime(), steps)
			}
		})
	}
}

// TestTimelineVerifiedNotMergedAnswersMerge checks the timeline answers the
// kind the session row does: in progress while verify may still merge the
// head, MERGE once the grace has run out.
func TestTimelineVerifiedNotMergedAnswersMerge(t *testing.T) {
	snaps := []db.CheckSnapshot{
		snap(t, at(0), vcs.DisplayStatusPassing, greenCI(2, "")),
		snap(t, at(5), vcs.DisplayStatusPassing, verifiedChecks()),
		snap(t, at(30), vcs.DisplayStatusPassing, verifiedChecks()),
	}
	for _, tc := range []struct {
		now  time.Time
		kind pb.AttentionKind
	}{
		{at(5).Add(attention.VerifyMergeGrace - time.Second), pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{at(5).Add(attention.VerifyMergeGrace), pb.AttentionKind_ATTENTION_KIND_MERGE},
	} {
		if _, kind := assembleTimeline(timelineFacts{HasPR: true, HeadSHA: timelineHead, Snapshots: snaps, IsFactoryRepo: true, Now: tc.now}); kind != tc.kind {
			t.Errorf("now %v: kind = %v, want %v", tc.now, kind, tc.kind)
		}
	}
	assertSteps(t, buildTimeline(timelineFacts{HasPR: true, HeadSHA: timelineHead, Snapshots: snaps, IsFactoryRepo: true, Now: at(5).Add(attention.VerifyMergeGrace)}), []wantStep{
		{at(0), "CI passed, 2 checks", "", done},
		{at(5), "Verified", "", current},
	})
}

// TestAttentionServesShouldShowDisplayLabel checks bossd marks a FIX row with
// the TUI's own rule (displaystatus.FixShowsLabel), so the web shows the same
// label: a blocked run whose PR is failing shows "⨯ failing", a blocked run
// that is only passing or idle under the errored recolor shows "fix", and no
// other kind is marked.
func TestAttentionServesShouldShowDisplayLabel(t *testing.T) {
	reason := "agent crashed"
	for _, tc := range []struct {
		name    string
		state   machine.State
		display vcs.DisplayStatus
		hasPR   bool
		park    string
		kind    pb.AttentionKind
		shown   bool
	}{
		{"blocked run, failing PR", machine.Blocked, vcs.DisplayStatusFailing, true, "", pb.AttentionKind_ATTENTION_KIND_FIX, true},
		{"blocked run, conflicting PR", machine.Blocked, vcs.DisplayStatusConflict, true, "", pb.AttentionKind_ATTENTION_KIND_FIX, true},
		{"blocked run, passing PR", machine.Blocked, vcs.DisplayStatusPassing, true, "", pb.AttentionKind_ATTENTION_KIND_FIX, false},
		{"blocked run, no PR", machine.Blocked, vcs.DisplayStatusUnspecified, false, "", pb.AttentionKind_ATTENTION_KIND_FIX, false},
		{"blocked run, park asking only for a sign-off", machine.Blocked, vcs.DisplayStatusNeedsHuman, true, "human-review:ui", pb.AttentionKind_ATTENTION_KIND_FIX, false},
		{"orphaned run, park asking only for a sign-off", machine.Orphaned, vcs.DisplayStatusNeedsHuman, true, "human-review:ui", pb.AttentionKind_ATTENTION_KIND_FIX, false},
		{"blocked run, blocked park", machine.Blocked, vcs.DisplayStatusNeedsHuman, true, "ledger-open", pb.AttentionKind_ATTENTION_KIND_FIX, true},
		{"failing PR, nothing to fix", machine.ReadyForReview, vcs.DisplayStatusFailing, true, "", pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := &models.Session{ID: "s", RepoID: "r", State: tc.state, BlockedReason: &reason, UpdatedAt: time.Now()}
			tr := status.NewDisplayTracker()
			if tc.hasPR {
				n := 1
				row.PRNumber = &n
				tr.Set("s", vcs.DisplayInfo{Status: tc.display, VerifyReason: tc.park, HeadSHA: timelineHead, HasFailures: tc.display == vcs.DisplayStatusFailing})
			}
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tr, nil)
			resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			got := onlySession(t, resp.Msg.Sessions)
			a := got.GetAttention()
			if a.GetKind() != tc.kind {
				t.Fatalf("attention = %v, want %v (display %q)", a, tc.kind, got.GetDisplayLabel())
			}
			if a.GetShouldShowDisplayLabel() != tc.shown {
				t.Fatalf("should_show_display_label = %v on %q (intent %v), want %v", a.GetShouldShowDisplayLabel(), got.GetDisplayLabel(), got.GetDisplayIntent(), tc.shown)
			}
			if tc.kind == pb.AttentionKind_ATTENTION_KIND_FIX && a.GetShouldShowDisplayLabel() != displaystatus.FixShowsLabel(got) {
				t.Fatalf("served %v, but the TUI's rule says %v for %q", a.GetShouldShowDisplayLabel(), displaystatus.FixShowsLabel(got), got.GetDisplayLabel())
			}
		})
	}
}

// countingAttentionSnapshots counts every snapshot port call, including reads
// other than the head walk, so an accidental bulk read cannot hide its cost.
type countingAttentionSnapshots struct {
	store       checkSnapshotReader
	calls, rows int
}

func (s *countingAttentionSnapshots) ListBySession(ctx context.Context, id string, limit int) ([]domain.CheckSnapshot, error) {
	s.calls++
	return s.store.ListBySession(ctx, id, limit)
}

func (s *countingAttentionSnapshots) ListBySessionHead(ctx context.Context, id, head string) ([]domain.CheckSnapshot, error) {
	s.calls++
	return s.store.ListBySessionHead(ctx, id, head)
}

func (s *countingAttentionSnapshots) ListBySessionHeadNewestFirst(ctx context.Context, id, head string, raw bool, visit func(domain.CheckSnapshot) bool) error {
	s.calls++
	return s.store.ListBySessionHeadNewestFirst(ctx, id, head, raw, func(snap domain.CheckSnapshot) bool {
		s.rows++
		return visit(snap)
	})
}

func (s *countingAttentionSnapshots) GetAttentionStartsBySessionHead(ctx context.Context, id, head string) (int, time.Time, time.Time, bool, error) {
	s.calls++
	return s.store.GetAttentionStartsBySessionHead(ctx, id, head)
}

func attentionSnapshotStore(t *testing.T) (*db.SQLiteCheckSnapshotStore, *models.Session) {
	t.Helper()
	conn, err := sqlutil.Open(filepath.Join(t.TempDir(), "bossd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	dbtest.Apply(t, conn)
	ctx := context.Background()
	repo, err := db.NewRepoStore(conn).Create(ctx, db.CreateRepoParams{DisplayName: "r", LocalPath: "/tmp/r", OriginURL: "https://github.com/acme/r.git", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt"})
	if err != nil {
		t.Fatal(err)
	}
	pr := 1
	row, err := db.NewSessionStore(conn).Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "attention", BranchName: "feat/attention", BaseBranch: "main", PRNumber: &pr})
	if err != nil {
		t.Fatal(err)
	}
	row.State = machine.ReadyForReview
	return db.NewCheckSnapshotStore(conn), row
}

func TestListSessionsSnapshotWorkIsConstant_Sessions_AC120(t *testing.T) {
	for _, parked := range []bool{true, false} {
		name := "verified"
		display, checks := vcs.DisplayStatusPassing, verifiedChecks()
		if parked {
			name, display, checks = "parked", vcs.DisplayStatusNeedsHuman, greenCI(2, "needs human: ledger-open")
		}
		t.Run(name, func(t *testing.T) {
			smallCalls := 0
			for _, count := range []int{3, 3000} {
				store, row := attentionSnapshotStore(t)
				start := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
				snaps := make([]domain.CheckSnapshot, count)
				for i := range snaps {
					snaps[i] = headSnap(t, start.Add(time.Duration(i)*time.Second), display, checks)
					snaps[i].SessionID = row.ID
					if err := store.Create(context.Background(), snaps[i]); err != nil {
						t.Fatal(err)
					}
				}
				wantPark, wantVerified := headSince(newestFirstSnapshots(snaps), newestFirstSnapshots(snaps))
				tracker := status.NewDisplayTracker()
				mergeable := true
				info := vcs.DisplayInfo{Status: display, HeadSHA: timelineHead, Mergeable: &mergeable}
				if parked {
					info.VerifyReason = "ledger-open"
				}
				tracker.Set(row.ID, info)
				srv := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tracker, nil)
				srv.cronJobs = &attentionCronStore{jobs: []*domain.CronJob{{Prompt: "/boss-verify", IsEnabled: true}}}
				counting := &countingAttentionSnapshots{store: store}
				srv.checkSnapshots = counting
				got := listOneAttention(t, srv)
				wantSince, wantKind := wantVerified, pb.AttentionKind_ATTENTION_KIND_MERGE
				if parked {
					wantSince, wantKind = wantPark, pb.AttentionKind_ATTENTION_KIND_FIX
				}
				if got.GetKind() != wantKind || !got.GetSince().AsTime().Equal(wantSince) {
					t.Fatalf("%d polls: attention %v, want kind %v since %v", count, got, wantKind, wantSince)
				}
				if counting.rows != 0 {
					t.Fatalf("%d polls: walked %d rows", count, counting.rows)
				}
				if count == 3 {
					smallCalls = counting.calls
				}
				if counting.calls == 0 || counting.calls != smallCalls {
					t.Fatalf("%d polls: %d calls, want %d", count, counting.calls, smallCalls)
				}
			}
		})
	}
}

func TestStoredHeadSinceUntrackedNewestMatchesWalk(t *testing.T) {
	store, row := attentionSnapshotStore(t)
	snaps := []domain.CheckSnapshot{
		headSnap(t, at(1), vcs.DisplayStatusNeedsHuman, verifiedChecks()),
		headSnap(t, at(3), vcs.DisplayStatusNeedsHuman, verifiedChecks()),
		headSnap(t, at(2), vcs.DisplayStatusPassing, greenCI(2, "")),
	}
	for _, snap := range snaps {
		snap.SessionID = row.ID
		if err := store.Create(context.Background(), snap); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _, tracked, err := store.GetAttentionStartsBySessionHead(context.Background(), row.ID, timelineHead)
	if err != nil || tracked {
		t.Fatalf("out-of-order insert: tracked %v, err %v", tracked, err)
	}
	slices.SortFunc(snaps, func(a, b domain.CheckSnapshot) int { return a.PolledAt.Compare(b.PolledAt) })
	wantPark, wantVerified := headSince(newestFirstSnapshots(snaps), newestFirstSnapshots(snaps))
	counting := &countingAttentionSnapshots{store: store}
	park, verified := storedHeadSince(context.Background(), counting, row.ID, timelineHead)
	if !park.Equal(wantPark) || !verified.Equal(wantVerified) || counting.rows == 0 {
		t.Fatalf("untracked read: park %v verified %v rows %d, want %v and %v with fallback walk", park, verified, counting.rows, wantPark, wantVerified)
	}
}

func (s *countingAttentionSnapshots) ListHeadStartsBySession(ctx context.Context, id string) ([]domain.CheckSnapshotHeadStart, error) {
	return s.store.ListHeadStartsBySession(ctx, id)
}
