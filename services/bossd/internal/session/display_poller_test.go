package session

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/status"
)

func TestDisplayPoller_PollsSessionWithPR(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewPRTracker()
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
	}
	if e.Status != vcs.PRDisplayStatusPassing {
		t.Errorf("Status = %d, want %d (Passing)", e.Status, vcs.PRDisplayStatusPassing)
	}
}

func TestDisplayPoller_SkipsSessionWithoutPR(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewPRTracker()
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
	tracker := status.NewPRTracker()
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

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry, got nil")
	}
	if e.Status != vcs.PRDisplayStatusMerged {
		t.Errorf("Status = %d, want %d (Merged)", e.Status, vcs.PRDisplayStatusMerged)
	}
}

func TestDisplayPoller_FailingChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewPRTracker()
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
	}
	if e.Status != vcs.PRDisplayStatusFailing {
		t.Errorf("Status = %d, want %d (Failing)", e.Status, vcs.PRDisplayStatusFailing)
	}
}

func TestDisplayPoller_ChangesRequested(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewPRTracker()
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
	}
	if e.Status != vcs.PRDisplayStatusRejected {
		t.Errorf("Status = %d, want %d (Rejected)", e.Status, vcs.PRDisplayStatusRejected)
	}
}

func TestDisplayPoller_GracefulDegradation_CheckResultsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewPRTracker()
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

	// Should still have an entry — graceful degradation means it continues
	// with nil checks and computes status from PR alone.
	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry despite check results error, got nil")
	}
	if e.Status != vcs.PRDisplayStatusIdle {
		t.Errorf("Status = %d, want %d (Idle — no checks available)", e.Status, vcs.PRDisplayStatusIdle)
	}
}

func TestDisplayPoller_GracefulDegradation_ReviewCommentsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewPRTracker()
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

	// Should still compute status — reviews error means nil reviews, so Passing.
	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry despite review comments error, got nil")
	}
	if e.Status != vcs.PRDisplayStatusPassing {
		t.Errorf("Status = %d, want %d (Passing — reviews unavailable)", e.Status, vcs.PRDisplayStatusPassing)
	}
}

func TestDisplayPoller_DraftPR_SkipsChecksAndReviews(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	vp := newMockVCSProvider()
	tracker := status.NewPRTracker()
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
	}
	if e.Status != vcs.PRDisplayStatusDraft {
		t.Errorf("Status = %d, want %d (Draft)", e.Status, vcs.PRDisplayStatusDraft)
	}

	// Verify no check or review API calls were made.
	if vp.getCheckResultsCalls != 0 {
		t.Errorf("GetCheckResults called %d times, want 0 for draft PR", vp.getCheckResultsCalls)
	}
	if vp.getReviewCommentsCalls != 0 {
		t.Errorf("GetReviewComments called %d times, want 0 for draft PR", vp.getReviewCommentsCalls)
	}
}

func boolPtr(b bool) *bool { return &b }
