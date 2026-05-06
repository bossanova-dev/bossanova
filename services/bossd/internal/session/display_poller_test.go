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

	poller := NewDisplayPoller(sessions, repos, vp, tracker, 50*time.Millisecond, logger)
	poller.Run(ctx)

	time.Sleep(150 * time.Millisecond)

	e := tracker.Get("sess-1")
	if e == nil {
		t.Fatal("expected tracker entry, got nil")
	}
	if e.Status != vcs.DisplayStatusMerged {
		t.Errorf("Status = %d, want %d (Merged)", e.Status, vcs.DisplayStatusMerged)
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

func boolPtr(b bool) *bool { return &b }
