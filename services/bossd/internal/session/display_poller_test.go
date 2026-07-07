package session

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

func TestDisplayPoller_PollsSessionWithPR(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	prNum := 42
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	// Configure mock: all checks passing.
	success := vcs.CheckConclusionSuccess
	vp.nextCheckResults = []vcs.CheckResult{
		{Status: vcs.CheckStatusCompleted, Conclusion: &success},
	}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true)}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	// Wait for at least one poll cycle.
	time.Sleep(150 * time.Millisecond)

	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry for sess-1, got nil")
		return
	}
	if e.Status != vcs.DisplayStatusPassing {
		t.Errorf("Status = %d, want %d (Passing)", e.Status, vcs.DisplayStatusPassing)
	}
}

func TestDisplayPoller_ShowsRebaseConflictOnlyForRebaseStrategy(t *testing.T) {
	ctx := context.Background()
	success := vcs.CheckConclusionSuccess

	tests := []struct {
		name              string
		mergeStrategy     models.MergeStrategy
		prStatus          *vcs.PRStatus
		allowedStrategies []string
		wantStatus        vcs.DisplayStatus
	}{
		{
			name:          "rebase strategy",
			mergeStrategy: models.MergeStrategyRebase,
			wantStatus:    vcs.DisplayStatusConflict,
		},
		{
			name:          "merge strategy",
			mergeStrategy: models.MergeStrategyMerge,
			wantStatus:    vcs.DisplayStatusPassing,
		},
		{
			name:          "review required block",
			mergeStrategy: models.MergeStrategyRebase,
			prStatus: &vcs.PRStatus{
				State:               vcs.PRStateOpen,
				Mergeable:           boolPtr(true),
				Rebaseable:          boolPtr(false),
				MergeStateStatus:    vcs.MergeStateStatusBlocked,
				ReviewDecisionState: vcs.ReviewStateRequired,
			},
			wantStatus: vcs.DisplayStatusReview,
		},
		{
			name: "plain merge conflict with no allowed strategies",
			prStatus: &vcs.PRStatus{
				State:      vcs.PRStateOpen,
				Mergeable:  boolPtr(false),
				Rebaseable: boolPtr(false),
			},
			allowedStrategies: []string{},
			wantStatus:        vcs.DisplayStatusConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			vp := newMockVCSProvider()
			if tt.allowedStrategies != nil {
				vp.allowedStrategies = tt.allowedStrategies
			}
			tracker := status.NewDisplayTracker()
			logger := zerolog.Nop()

			prNum := 42
			repos.repos["repo-1"] = &models.Repo{
				ID:            "repo-1",
				OriginURL:     "owner/repo",
				MergeStrategy: tt.mergeStrategy,
			}
			sessions.sessions["sess-1"] = &models.Session{
				ID:       "sess-1",
				RepoID:   "repo-1",
				PRNumber: &prNum,
			}
			vp.nextPRStatus = tt.prStatus
			if vp.nextPRStatus == nil {
				vp.nextPRStatus = &vcs.PRStatus{
					State:      vcs.PRStateOpen,
					Mergeable:  boolPtr(true),
					Rebaseable: boolPtr(false),
				}
			}
			vp.nextCheckResults = []vcs.CheckResult{
				{Status: vcs.CheckStatusCompleted, Conclusion: &success},
			}

			poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
			poller.poll(ctx)

			e := tracker.Get("sess-1")
			if e == nil {
				t.Fatal("expected tracker entry for sess-1, got nil")
			}
			if e.Status != tt.wantStatus {
				t.Fatalf("Status = %d, want %d", e.Status, tt.wantStatus)
			}
		})
	}
}

func TestDisplayPollerDoesNotShowConflictForUnstablePR(t *testing.T) {
	ctx := context.Background()
	mergeable := true
	rebaseable := false

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	repo := &models.Repo{ID: "repo-1", OriginURL: "owner/repo", MergeStrategy: models.MergeStrategyRebase}
	repos.repos["repo-1"] = repo
	prNumber := 42
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		PRNumber:   &prNumber,
		BranchName: "feature",
	}
	vp.nextPRStatus = &vcs.PRStatus{
		State:            vcs.PRStateOpen,
		Mergeable:        &mergeable,
		Rebaseable:       &rebaseable,
		MergeStateStatus: vcs.MergeStateStatusUnstable,
		HeadSHA:          "abc123",
	}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
	poller.pollSession(ctx, repo, "sess-1", prNumber)

	entry := tracker.Get("sess-1")
	if entry == nil {
		t.Fatalf("missing display entry")
	}
	if entry.Status == vcs.DisplayStatusConflict {
		t.Fatalf("display status = conflict for UNSTABLE PR")
	}
}

func TestDisplayPoller_SkipsSessionWithoutPR(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: nil, // No PR.
	}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	e := tracker.Get("sess-1")
	if e != nil {
		t.Errorf("expected no tracker entry for session without PR, got %v", e)
	}
}

func TestDisplayPoller_MergedPR(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	prNum := 10
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged}
	vp.checkResultsErr = fmt.Errorf("no checks reported")
	vp.reviewCommentsErr = fmt.Errorf("reviews should not be fetched")

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry, got nil")
		return
	}
	if e.Status != vcs.DisplayStatusMerged {
		t.Errorf("Status = %d, want %d (Merged)", e.Status, vcs.DisplayStatusMerged)
	}
	if vp.getCheckResultsCalls != 0 {
		t.Errorf("GetCheckResults called %d times, want 0 for merged PR", vp.getCheckResultsCalls)
	}
	if vp.getReviewCommentsCalls != 0 {
		t.Errorf("GetReviewComments called %d times, want 0 for merged PR", vp.getReviewCommentsCalls)
	}
}

func TestDisplayPoller_MergedPRPersistsSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	snapshots := newMockCheckSnapshotStore()
	logger := zerolog.Nop()

	prNum := 10
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "abc123"}
	vp.checkResultsErr = fmt.Errorf("no checks reported")
	vp.reviewCommentsErr = fmt.Errorf("reviews should not be fetched")

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.SetSnapshotStore(snapshots)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	snaps := snapshots.all()
	if len(snaps) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snaps))
	}
	if snaps[0].SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", snaps[0].SessionID)
	}
	if snaps[0].HeadSHA != "abc123" {
		t.Errorf("HeadSHA = %q, want abc123", snaps[0].HeadSHA)
	}
	if snaps[0].ComputedStatus != int(vcs.DisplayStatusMerged) {
		t.Errorf("ComputedStatus = %d, want %d (Merged)", snaps[0].ComputedStatus, vcs.DisplayStatusMerged)
	}
	if vp.getCheckResultsCalls != 0 {
		t.Errorf("GetCheckResults called %d times, want 0 for merged PR", vp.getCheckResultsCalls)
	}
	if vp.getReviewCommentsCalls != 0 {
		t.Errorf("GetReviewComments called %d times, want 0 for merged PR", vp.getReviewCommentsCalls)
	}
}

func TestDisplayPoller_ClosedPRSkipsChecksAndReviews(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	prNum := 10
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateClosed}
	vp.checkResultsErr = fmt.Errorf("no checks reported")
	vp.reviewCommentsErr = fmt.Errorf("reviews should not be fetched")

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry, got nil")
		return
	}
	if e.Status != vcs.DisplayStatusClosed {
		t.Errorf("Status = %d, want %d (Closed)", e.Status, vcs.DisplayStatusClosed)
	}
	if vp.getCheckResultsCalls != 0 {
		t.Errorf("GetCheckResults called %d times, want 0 for closed PR", vp.getCheckResultsCalls)
	}
	if vp.getReviewCommentsCalls != 0 {
		t.Errorf("GetReviewComments called %d times, want 0 for closed PR", vp.getReviewCommentsCalls)
	}
}

func TestDisplayPoller_ClosedPRIsTerminalAfterSnapshot(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	snapshots := newMockCheckSnapshotStore()
	logger := zerolog.Nop()

	prNum := 10
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateClosed, HeadSHA: "closed-sha"}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 0, logger)
	poller.SetSnapshotStore(snapshots)
	poller.poll(ctx)
	poller.poll(ctx)

	if got := len(vp.getPRStatusPRNumbers); got != 1 {
		t.Fatalf("GetPRStatus calls = %d, want 1", got)
	}
	snaps := snapshots.all()
	if len(snaps) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snaps))
	}
	if snaps[0].ComputedStatus != int(vcs.DisplayStatusClosed) {
		t.Errorf("ComputedStatus = %d, want %d (Closed)", snaps[0].ComputedStatus, vcs.DisplayStatusClosed)
	}
}

func TestDisplayPoller_FailingChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	prNum := 10
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	failure := vcs.CheckConclusionFailure
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true)}
	vp.nextCheckResults = []vcs.CheckResult{
		{Status: vcs.CheckStatusCompleted, Conclusion: &failure},
	}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry, got nil")
		return
	}
	if e.Status != vcs.DisplayStatusFailing {
		t.Errorf("Status = %d, want %d (Failing)", e.Status, vcs.DisplayStatusFailing)
	}
}

func TestDisplayPoller_ChangesRequested(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	prNum := 10
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	success := vcs.CheckConclusionSuccess
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true)}
	vp.nextCheckResults = []vcs.CheckResult{
		{Status: vcs.CheckStatusCompleted, Conclusion: &success},
	}
	vp.nextReviewComments = []vcs.ReviewComment{
		{State: vcs.ReviewStateChangesRequested},
	}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry, got nil")
		return
	}
	if e.Status != vcs.DisplayStatusRejected {
		t.Errorf("Status = %d, want %d (Rejected)", e.Status, vcs.DisplayStatusRejected)
	}
}

func TestDisplayPoller_CodexBotCommentedReviewCommentsRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	prNum := 10
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	success := vcs.CheckConclusionSuccess
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true), LatestReviewState: vcs.ReviewStateCommented}
	vp.nextCheckResults = []vcs.CheckResult{
		{Status: vcs.CheckStatusCompleted, Conclusion: &success},
	}
	vp.nextReviewComments = []vcs.ReviewComment{
		{Author: "chatgpt-codex-connector[bot]", Body: "handle the nil case", State: vcs.ReviewStateChangesRequested},
	}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry, got nil")
		return
	}
	if e.Status != vcs.DisplayStatusRejected {
		t.Errorf("Status = %d, want %d (Rejected)", e.Status, vcs.DisplayStatusRejected)
	}
}

func TestDisplayPoller_CheckResultsError_NoUpdate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	prNum := 10
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true)}
	vp.checkResultsErr = fmt.Errorf("API rate limited")

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	// Falling back to "Idle" on a transient API error silently disables the
	// repair plugin (which only triggers on FAILING/CONFLICT/REJECTED). The
	// poller must skip the update and let the next cycle retry.
	if e := tracker.Get("sess-1"); e != nil {
		t.Errorf("expected no tracker entry when GetCheckResults errored, got status=%d", e.Status)
	}
}

func TestDisplayPoller_CheckResultsError_PreservesPrevious(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	prNum := 10
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	// Seed a "Failing" entry to simulate a previous successful poll.
	tracker.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusFailing})

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true)}
	vp.checkResultsErr = fmt.Errorf("API rate limited")

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("previous entry must be preserved on API error, got nil")
		return
	}
	if e.Status != vcs.DisplayStatusFailing {
		t.Errorf("Status = %d, want %d (Failing — previous status sticks on error)", e.Status, vcs.DisplayStatusFailing)
	}
}

// TestDisplayPoller_ClearsStaleDraftWhenReadyWithNoChecks reproduces WON-1118:
// a PR created as draft (tracker holds "Draft") becomes ready-for-review, but
// its head commit has no CI checks. With the GetCheckResults fix, the provider
// returns an empty (non-error) check set, so the poller must recompute and
// clear the stale "Draft" rather than leaving it frozen for hours.
func TestDisplayPoller_ClearsStaleDraftWhenReadyWithNoChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	prNum := 299
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	// Seed a stale "Draft" entry from when the PR really was a draft.
	tracker.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusDraft})

	// PR is now ready (Draft=false) and mergeable, with no checks reported.
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Draft: false, Mergeable: boolPtr(true)}
	vp.nextCheckResults = []vcs.CheckResult{}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry, got nil")
		return
	}
	if e.Status == vcs.DisplayStatusDraft {
		t.Errorf("Status still Draft (%d) — stale draft was not cleared", e.Status)
	}
	if e.Status != vcs.DisplayStatusIdle {
		t.Errorf("Status = %d, want %d (Idle — ready PR with no checks)", e.Status, vcs.DisplayStatusIdle)
	}
}

func TestDisplayPoller_ReviewCommentsError_NoUpdate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	prNum := 10
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	success := vcs.CheckConclusionSuccess
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true)}
	vp.nextCheckResults = []vcs.CheckResult{
		{Status: vcs.CheckStatusCompleted, Conclusion: &success},
	}
	vp.reviewCommentsErr = fmt.Errorf("API error")

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	// Without reviews we cannot tell apart "Passing" from "Rejected" — so
	// the poller must skip the update rather than misclassify a rejected
	// PR as passing.
	if e := tracker.Get("sess-1"); e != nil {
		t.Errorf("expected no tracker entry when GetReviewComments errored, got status=%d", e.Status)
	}
}

func TestDisplayPoller_DraftPR_SkipsChecksAndReviews(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	snapshots := newMockCheckSnapshotStore()
	logger := zerolog.Nop()

	prNum := 10
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	// PR is draft — checks and reviews should NOT be fetched.
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Draft: true}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.SetSnapshotStore(snapshots)
	if err := poller.RefreshPR(ctx, "owner/repo", prNum); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}

	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry, got nil")
		return
	}
	if e.Status != vcs.DisplayStatusDraft {
		t.Errorf("Status = %d, want %d (Draft)", e.Status, vcs.DisplayStatusDraft)
	}
	snaps := snapshots.all()
	if len(snaps) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snaps))
	}
	if snaps[0].SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", snaps[0].SessionID)
	}
	if snaps[0].ComputedStatus != int(vcs.DisplayStatusDraft) {
		t.Errorf("ComputedStatus = %d, want %d (Draft)", snaps[0].ComputedStatus, vcs.DisplayStatusDraft)
	}

	// Verify no check or review API calls were made.
	if vp.getCheckResultsCalls != 0 {
		t.Errorf("GetCheckResults called %d times, want 0 for draft PR", vp.getCheckResultsCalls)
	}
	if vp.getReviewCommentsCalls != 0 {
		t.Errorf("GetReviewComments called %d times, want 0 for draft PR", vp.getReviewCommentsCalls)
	}
}

func TestRefreshPRTargetsOnlyMatchingSessions(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	repos.repos["repo-a"] = &models.Repo{ID: "repo-a", OriginURL: "git@github.com:owner/repo-a.git"}
	repos.repos["repo-b"] = &models.Repo{ID: "repo-b", OriginURL: "git@github.com:owner/repo-b.git"}

	sessions.sessions["s1"] = &models.Session{ID: "s1", RepoID: "repo-a", PRNumber: intPtr(42)}
	sessions.sessions["s2"] = &models.Session{ID: "s2", RepoID: "repo-a", PRNumber: intPtr(99)}
	sessions.sessions["s3"] = &models.Session{ID: "s3", RepoID: "repo-b", PRNumber: intPtr(42)}

	success := vcs.CheckConclusionSuccess
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, HeadSHA: "abc", Mergeable: boolPtr(true)}
	vp.nextCheckResults = []vcs.CheckResult{
		{Status: vcs.CheckStatusCompleted, Conclusion: &success},
	}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
	if err := poller.RefreshPR(ctx, "https://github.com/owner/repo-a", 42); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}

	e := tracker.Get("s1")
	if e == nil {
		t.Fatal("expected tracker entry for s1, got nil")
	}
	if e.HeadSHA != "abc" {
		t.Errorf("s1 HeadSHA = %q, want abc", e.HeadSHA)
	}
	if tracker.Get("s2") != nil {
		t.Fatalf("expected no tracker entry for s2")
	}
	if tracker.Get("s3") != nil {
		t.Fatalf("expected no tracker entry for s3")
	}
	if len(vp.getPRStatusPRNumbers) != 1 || vp.getPRStatusPRNumbers[0] != 42 {
		t.Fatalf("GetPRStatus PR numbers = %v, want [42]", vp.getPRStatusPRNumbers)
	}
}

func TestRefreshPRClosedDraftOverwritesPreviousDraftStatus(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "https://github.com/IKHOR/wondercanvas-mono"}
	sessions.sessions["sess-203"] = &models.Session{ID: "sess-203", RepoID: "repo-1", PRNumber: intPtr(203)}
	tracker.Set("sess-203", vcs.DisplayInfo{Status: vcs.DisplayStatusDraft})
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateClosed, Draft: true, HeadSHA: "closed-sha"}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
	if err := poller.RefreshPR(ctx, "git@github.com:IKHOR/wondercanvas-mono.git", 203); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}

	entry := tracker.Get("sess-203")
	if entry == nil {
		t.Fatal("expected tracker entry for sess-203, got nil")
	}
	if entry.Status != vcs.DisplayStatusClosed {
		t.Fatalf("display status = %v, want Closed", entry.Status)
	}
	if entry.HeadSHA != "closed-sha" {
		t.Fatalf("HeadSHA = %q, want closed-sha", entry.HeadSHA)
	}
}

func TestRefreshPRClosedPRIsTerminalAfterSnapshot(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	snapshots := newMockCheckSnapshotStore()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", PRNumber: intPtr(42)}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateClosed, HeadSHA: "closed-sha"}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
	poller.SetSnapshotStore(snapshots)
	if err := poller.RefreshPR(ctx, "owner/repo", 42); err != nil {
		t.Fatalf("first RefreshPR returned error: %v", err)
	}
	if err := poller.RefreshPR(ctx, "owner/repo", 42); err != nil {
		t.Fatalf("second RefreshPR returned error: %v", err)
	}

	if got := len(vp.getPRStatusPRNumbers); got != 1 {
		t.Fatalf("GetPRStatus calls = %d, want 1", got)
	}
	snaps := snapshots.all()
	if len(snaps) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snaps))
	}
	if snaps[0].ComputedStatus != int(vcs.DisplayStatusClosed) {
		t.Errorf("ComputedStatus = %d, want %d (Closed)", snaps[0].ComputedStatus, vcs.DisplayStatusClosed)
	}
}

func TestRefreshPRLogsFetchedClosedDraftStatus(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	var logs bytes.Buffer
	logger := zerolog.New(&logs)

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", PRNumber: intPtr(42)}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateClosed, Draft: true}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
	if err := poller.RefreshPR(ctx, "owner/repo", 42); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}

	got := logs.String()
	for _, want := range []string{
		`"session_id":"sess-1"`,
		`"repo_origin_url":"owner/repo"`,
		`"pr_number":42`,
		`"pr_state":"closed"`,
		`"pr_draft":true`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("log output missing %s: %s", want, got)
		}
	}
}

func TestRefreshPRReturnsErrorForUnknownRepo(t *testing.T) {
	ctx := context.Background()
	poller := NewDisplayPoller(
		newMockSessionStore(),
		newMockRepoStore(),
		newMockVCSProvider(),
		status.NewDisplayTracker(),
		time.Minute,
		zerolog.Nop(),
	)

	if err := poller.RefreshPR(ctx, "owner/missing", 42); err == nil {
		t.Fatal("expected error for unknown repo")
	}
}

func TestPollIntervalStretchesAfterRecentWebhookRefresh(t *testing.T) {
	poller := NewDisplayPoller(
		newMockSessionStore(),
		newMockRepoStore(),
		newMockVCSProvider(),
		status.NewDisplayTracker(),
		30*time.Second,
		zerolog.Nop(),
	)
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	poller.recordRefresh("sess-1", now)

	if got := poller.intervalFor("sess-1", now.Add(time.Minute)); got != webhookHealthyInterval {
		t.Fatalf("intervalFor = %s, want %s", got, webhookHealthyInterval)
	}
}

func TestPollIntervalReturnsConfiguredAfterWebhookWindow(t *testing.T) {
	configured := 30 * time.Second
	poller := NewDisplayPoller(
		newMockSessionStore(),
		newMockRepoStore(),
		newMockVCSProvider(),
		status.NewDisplayTracker(),
		configured,
		zerolog.Nop(),
	)
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	poller.recordRefresh("sess-1", now.Add(-webhookHealthyWindow-time.Nanosecond))

	if got := poller.intervalFor("sess-1", now); got != configured {
		t.Fatalf("intervalFor = %s, want %s", got, configured)
	}
}

func TestShouldPollSessionBackoffIsPerSessionNotPerRepo(t *testing.T) {
	configured := 30 * time.Second
	poller := NewDisplayPoller(
		newMockSessionStore(),
		newMockRepoStore(),
		newMockVCSProvider(),
		status.NewDisplayTracker(),
		configured,
		zerolog.Nop(),
	)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	poller.markPolled("sess-A", now)
	poller.markPolled("sess-B", now)

	// Session A received a webhook; it should back off to the healthy interval.
	poller.recordRefresh("sess-A", now)
	if got := poller.shouldPollSession("sess-A", now.Add(time.Minute)); got {
		t.Fatal("sess-A shouldPollSession = true, want false (webhook healthy)")
	}

	// Session B shares the same repo but got no webhook of its own; it must keep
	// the configured (fast) interval rather than being starved by sess-A's webhook.
	if got := poller.shouldPollSession("sess-B", now.Add(time.Minute)); !got {
		t.Fatalf("sess-B shouldPollSession = false, want true after configured interval %s", configured)
	}
}

func TestPollIntervalSkipsRecentlyPolledSessionWhenWebhookHealthy(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	prNum := 42
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	success := vcs.CheckConclusionSuccess
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true)}
	vp.nextCheckResults = []vcs.CheckResult{
		{Status: vcs.CheckStatusCompleted, Conclusion: &success},
	}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 30*time.Second, logger)
	poller.poll(ctx)
	if len(vp.getPRStatusPRNumbers) != 1 {
		t.Fatalf("first poll GetPRStatus calls = %d, want 1", len(vp.getPRStatusPRNumbers))
	}

	poller.recordRefresh("sess-1", time.Now())
	poller.poll(ctx)

	if len(vp.getPRStatusPRNumbers) != 1 {
		t.Fatalf("second poll GetPRStatus calls = %d, want still 1", len(vp.getPRStatusPRNumbers))
	}
}

func TestPollIntervalRefreshPRSuppressesImmediateScheduledPoll(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	prNum := 42
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: &prNum,
	}

	success := vcs.CheckConclusionSuccess
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true)}
	vp.nextCheckResults = []vcs.CheckResult{
		{Status: vcs.CheckStatusCompleted, Conclusion: &success},
	}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 30*time.Second, logger)
	if err := poller.RefreshPR(ctx, "owner/repo", prNum); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}
	if len(vp.getPRStatusPRNumbers) != 1 {
		t.Fatalf("RefreshPR GetPRStatus calls = %d, want 1", len(vp.getPRStatusPRNumbers))
	}

	poller.poll(ctx)

	if len(vp.getPRStatusPRNumbers) != 1 {
		t.Fatalf("scheduled poll GetPRStatus calls = %d, want still 1", len(vp.getPRStatusPRNumbers))
	}
}

func boolPtr(b bool) *bool { return &b }

// TestDisplayPollerAutoUnblocksStaleFixLoopExhausted is BOS-235 acceptance
// criterion 4: a session sitting in Blocked with the FixLoopExhausted reason
// whose live PR is observed clean + green + mergeable is auto-unblocked — it
// leaves Blocked, its blocked_reason is cleared, and attempt_count resets to 0.
// The stale tracker entry (a non-Passing status) must not prevent the
// downgrade, because the display poller reads live PR state.
func TestDisplayPollerAutoUnblocksStaleFixLoopExhausted(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	var logs bytes.Buffer
	logger := zerolog.New(&logs)

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	blockedReason := sessionreason.FixLoopExhausted()
	staleSHA := "sha-stale"
	sessions.sessions["sess-1"] = &models.Session{
		ID:                 "sess-1",
		RepoID:             "repo-1",
		PRNumber:           intPtr(42),
		State:              machine.Blocked,
		BlockedReason:      &blockedReason,
		AttemptCount:       machine.MaxAttempts,
		LastAttemptHeadSHA: &staleSHA,
	}

	// Live PR: open, mergeable, all checks passed → computes Passing.
	success := vcs.CheckConclusionSuccess
	vp.nextCheckResults = []vcs.CheckResult{{Status: vcs.CheckStatusCompleted, Conclusion: &success}}
	vp.nextReviewComments = nil
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true), HeadSHA: "sha-green"}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
	if err := poller.RefreshPR(ctx, "owner/repo", 42); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}

	sess := sessions.sessions["sess-1"]
	if sess.State == machine.Blocked {
		t.Fatalf("session still Blocked; want auto-unblocked (state=%v)", sess.State)
	}
	if sess.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil (cleared)", *sess.BlockedReason)
	}
	if sess.AttemptCount != 0 {
		t.Fatalf("AttemptCount = %d, want 0 after unblock", sess.AttemptCount)
	}
	if sess.LastAttemptHeadSHA != nil {
		t.Fatalf("LastAttemptHeadSHA = %q, want nil (cleared)", *sess.LastAttemptHeadSHA)
	}
	if !strings.Contains(logs.String(), "fix_loop_exhausted cleared") {
		t.Fatalf("log output missing %q: %s", "fix_loop_exhausted cleared", logs.String())
	}
}

// TestDisplayPollerLeavesOtherBlockedReasonsUntouched pins the narrowness of
// the auto-unblock: a session Blocked for any reason OTHER than
// FixLoopExhausted (a genuine human-required block) must be left alone even
// when the live PR is clean + green + mergeable.
func TestDisplayPollerLeavesOtherBlockedReasonsUntouched(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	humanReason := "needs human: risky migration"
	sessions.sessions["sess-1"] = &models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		PRNumber:      intPtr(42),
		State:         machine.Blocked,
		BlockedReason: &humanReason,
		AttemptCount:  2,
	}

	success := vcs.CheckConclusionSuccess
	vp.nextCheckResults = []vcs.CheckResult{{Status: vcs.CheckStatusCompleted, Conclusion: &success}}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true), HeadSHA: "sha-green"}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
	if err := poller.RefreshPR(ctx, "owner/repo", 42); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Blocked {
		t.Fatalf("state = %v, want Blocked (non-fix-loop reason must be untouched)", sess.State)
	}
	if sess.BlockedReason == nil || *sess.BlockedReason != humanReason {
		t.Fatalf("BlockedReason = %v, want %q (untouched)", sess.BlockedReason, humanReason)
	}
}

// TestDisplayPollerReconcilesBlockedSessionOnMergedPR covers the BOS-246 fix:
// a session wedged in Blocked (e.g. by a non-gating finalize failure) whose PR
// is later observed merged is reconciled to Merged, clearing blocked_reason and
// attempt_count. The reconcile is deliberately NOT gated on the block reason — a
// merged PR is terminal truth for any block.
func TestDisplayPollerReconcilesBlockedSessionOnMergedPR(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	var logs bytes.Buffer
	logger := zerolog.New(&logs)

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	blockedReason := "finalize failed: worktree has uncommitted changes"
	staleSHA := "sha-stale"
	sessions.sessions["sess-1"] = &models.Session{
		ID:                 "sess-1",
		RepoID:             "repo-1",
		PRNumber:           intPtr(42),
		State:              machine.Blocked,
		BlockedReason:      &blockedReason,
		AttemptCount:       machine.MaxAttempts,
		LastAttemptHeadSHA: &staleSHA,
	}

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "sha-merged"}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
	notifier := &mockCompletionNotifier{}
	poller.SetCompletionNotifier(notifier)
	if err := poller.RefreshPR(ctx, "owner/repo", 42); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Merged {
		t.Fatalf("state = %v, want Merged (reconciled)", sess.State)
	}
	if sess.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil (cleared)", *sess.BlockedReason)
	}
	if sess.AttemptCount != 0 {
		t.Fatalf("AttemptCount = %d, want 0 (cleared)", sess.AttemptCount)
	}
	if sess.LastAttemptHeadSHA != nil {
		t.Fatalf("LastAttemptHeadSHA = %q, want nil (cleared)", *sess.LastAttemptHeadSHA)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 notifier call, got %d", len(notifier.calls))
	}
	if notifier.calls[0].sessionID != "sess-1" {
		t.Errorf("notified session = %q, want sess-1", notifier.calls[0].sessionID)
	}
	if notifier.calls[0].outcome != models.TaskMappingStatusCompleted {
		t.Errorf("notified outcome = %v, want Completed", notifier.calls[0].outcome)
	}
	if !strings.Contains(logs.String(), "reconciled") {
		t.Fatalf("log output missing reconcile line: %s", logs.String())
	}
}

// TestDisplayPollerReconcilesBlockedSessionOnClosedPR is the closed-PR mirror:
// a Blocked session whose PR is closed without merging reconciles to Closed with
// its block reason cleared.
func TestDisplayPollerReconcilesBlockedSessionOnClosedPR(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	blockedReason := "finalize failed: mark PR ready"
	sessions.sessions["sess-1"] = &models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		PRNumber:      intPtr(42),
		State:         machine.Blocked,
		BlockedReason: &blockedReason,
		AttemptCount:  2,
	}

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateClosed, HeadSHA: "sha-closed"}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
	notifier := &mockCompletionNotifier{}
	poller.SetCompletionNotifier(notifier)
	if err := poller.RefreshPR(ctx, "owner/repo", 42); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Closed {
		t.Fatalf("state = %v, want Closed (reconciled)", sess.State)
	}
	if sess.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil (cleared)", *sess.BlockedReason)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 notifier call, got %d", len(notifier.calls))
	}
	if notifier.calls[0].sessionID != "sess-1" {
		t.Errorf("notified session = %q, want sess-1", notifier.calls[0].sessionID)
	}
	if notifier.calls[0].outcome != models.TaskMappingStatusFailed {
		t.Errorf("notified outcome = %v, want Failed", notifier.calls[0].outcome)
	}
}

// TestDisplayPollerReconcilesNonTerminalSessionOnMergedPR covers the missed
// webhook fallback: if a linked PR is already merged, polling terminal truth must
// advance stale non-terminal session state too.
func TestDisplayPollerReconcilesNonTerminalSessionOnMergedPR(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	sessions.sessions["sess-1"] = &models.Session{
		ID:       "sess-1",
		RepoID:   "repo-1",
		PRNumber: intPtr(42),
		State:    machine.ImplementingPlan,
	}

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "sha-merged"}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
	notifier := &mockCompletionNotifier{}
	poller.SetCompletionNotifier(notifier)
	if err := poller.RefreshPR(ctx, "owner/repo", 42); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Merged {
		t.Fatalf("state = %v, want Merged (reconciled)", sess.State)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 notifier call, got %d", len(notifier.calls))
	}
	if notifier.calls[0].sessionID != "sess-1" {
		t.Errorf("notified session = %q, want sess-1", notifier.calls[0].sessionID)
	}
	if notifier.calls[0].outcome != models.TaskMappingStatusCompleted {
		t.Errorf("notified outcome = %v, want Completed", notifier.calls[0].outcome)
	}
}

// TestDisplayPollerClearsStaleBlockReasonOnAlreadyTerminalSession covers the
// upgrade case: a session that a pre-fix PRClosed webhook advanced to Closed while
// the old handler wrote only State is already terminal but still carries a stale
// blocked_reason (which web sessionWarningHints surfaces). The reconcile must clear
// the residual metadata in place even though the session is no longer Blocked.
func TestDisplayPollerClearsStaleBlockReasonOnAlreadyTerminalSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	var logs bytes.Buffer
	logger := zerolog.New(&logs)

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	blockedReason := "finalize failed: mark PR ready"
	staleSHA := "sha-stale"
	sessions.sessions["sess-1"] = &models.Session{
		ID:                 "sess-1",
		RepoID:             "repo-1",
		PRNumber:           intPtr(42),
		State:              machine.Closed, // already terminal from a pre-fix webhook
		BlockedReason:      &blockedReason,
		AttemptCount:       2,
		LastAttemptHeadSHA: &staleSHA,
	}

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateClosed, HeadSHA: "sha-closed"}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
	notifier := &mockCompletionNotifier{}
	poller.SetCompletionNotifier(notifier)
	if err := poller.RefreshPR(ctx, "owner/repo", 42); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}

	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Closed {
		t.Fatalf("state = %v, want Closed (unchanged; no transition on already-terminal row)", sess.State)
	}
	if sess.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil (cleared on terminal row)", *sess.BlockedReason)
	}
	if len(notifier.calls) != 0 {
		t.Fatalf("expected no notifier calls for already-terminal cleanup, got %d", len(notifier.calls))
	}
	if sess.AttemptCount != 0 {
		t.Fatalf("AttemptCount = %d, want 0 (cleared)", sess.AttemptCount)
	}
	if sess.LastAttemptHeadSHA != nil {
		t.Fatalf("LastAttemptHeadSHA = %q, want nil (cleared)", *sess.LastAttemptHeadSHA)
	}
	if !strings.Contains(logs.String(), "cleared stale block reason") {
		t.Fatalf("log output missing clear line: %s", logs.String())
	}
}

// TestDisplayPollerRetriesTerminalReconcileAfterStoreError covers the retry
// window: a transient store error while reconciling a wedged Blocked session must
// NOT leave a terminal tracker entry, or poll() / RefreshPR() would skip the row
// forever (until daemon restart). The next poll must retry and reconcile cleanly.
func TestDisplayPollerRetriesTerminalReconcileAfterStoreError(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewDisplayTracker()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	blockedReason := "finalize failed: worktree dirty"
	sessions.sessions["sess-1"] = &models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		PRNumber:      intPtr(42),
		State:         machine.Blocked,
		BlockedReason: &blockedReason,
		AttemptCount:  2,
	}

	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: "sha-merged"}

	// Fail the first Update (the reconcile persist), succeed thereafter.
	failedOnce := false
	sessions.updateHook = func(_ string, _ db.UpdateSessionParams) error {
		if !failedOnce {
			failedOnce = true
			return fmt.Errorf("transient store error")
		}
		return nil
	}

	poller := NewDisplayPoller(sessions, repos, vp, tracker, time.Minute, logger)
	notifier := &mockCompletionNotifier{}
	poller.SetCompletionNotifier(notifier)

	// First refresh: reconcile Update fails → tracker must NOT be marked terminal.
	if err := poller.RefreshPR(ctx, "owner/repo", 42); err != nil {
		t.Fatalf("RefreshPR returned error: %v", err)
	}
	if entry := tracker.Get("sess-1"); entry != nil && isTerminalDisplayStatus(entry.Status) {
		t.Fatalf("tracker marked terminal despite reconcile store error; next poll would skip the retry")
	}
	if sessions.sessions["sess-1"].State != machine.Blocked {
		t.Fatalf("state = %v, want still Blocked after failed reconcile", sessions.sessions["sess-1"].State)
	}
	if len(notifier.calls) != 0 {
		t.Fatalf("expected no notifier call after failed reconcile, got %d", len(notifier.calls))
	}

	// Second refresh: Update now succeeds → session reconciles to Merged + cleared.
	if err := poller.RefreshPR(ctx, "owner/repo", 42); err != nil {
		t.Fatalf("RefreshPR (retry) returned error: %v", err)
	}
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.Merged {
		t.Fatalf("state = %v, want Merged after retry", sess.State)
	}
	if sess.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil after retry", *sess.BlockedReason)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 notifier call after successful retry, got %d", len(notifier.calls))
	}
}

func intPtr(i int) *int { return &i }

type mockCheckSnapshotStore struct {
	mu    sync.Mutex
	snaps []db.CheckSnapshot
}

func newMockCheckSnapshotStore() *mockCheckSnapshotStore {
	return &mockCheckSnapshotStore{}
}

func (m *mockCheckSnapshotStore) Insert(_ context.Context, snap db.CheckSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snaps = append(m.snaps, snap)
	return nil
}

func (m *mockCheckSnapshotStore) RecentBySession(_ context.Context, sessionID string, limit int) ([]db.CheckSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []db.CheckSnapshot
	for i := len(m.snaps) - 1; i >= 0; i-- {
		if m.snaps[i].SessionID == sessionID {
			out = append(out, m.snaps[i])
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *mockCheckSnapshotStore) all() []db.CheckSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]db.CheckSnapshot, len(m.snaps))
	copy(out, m.snaps)
	return out
}
