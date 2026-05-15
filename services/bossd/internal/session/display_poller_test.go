package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
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
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry, got nil")
		return
	}
	if e.Status != vcs.DisplayStatusDraft {
		t.Errorf("Status = %d, want %d (Draft)", e.Status, vcs.DisplayStatusDraft)
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

	poller.recordRefresh("owner/repo", now)

	if got := poller.intervalFor("owner/repo", now.Add(time.Minute)); got != webhookHealthyInterval {
		t.Fatalf("intervalFor = %s, want %s", got, webhookHealthyInterval)
	}
}

func TestPollIntervalRevertsAfterStaleWebhook(t *testing.T) {
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

	poller.recordRefresh("owner/repo", now.Add(-webhookHealthyWindow-time.Nanosecond))

	if got := poller.intervalFor("owner/repo", now); got != configured {
		t.Fatalf("intervalFor = %s, want %s", got, configured)
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

	poller.recordRefresh("owner/repo", time.Now())
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
