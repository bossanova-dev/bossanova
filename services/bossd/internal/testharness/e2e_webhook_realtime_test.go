package testharness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
)

type observedStatusChange struct {
	sessionID     string
	displayStatus pb.DisplayStatus
	hasFailures   bool
}

type statusChangeObserver struct {
	changes chan observedStatusChange
}

var _ pluginpkg.WorkflowService = (*statusChangeObserver)(nil)

func newStatusChangeObserver() *statusChangeObserver {
	return &statusChangeObserver{changes: make(chan observedStatusChange, 8)}
}

func (o *statusChangeObserver) GetInfo(context.Context) (*pb.PluginInfo, error) {
	return &pb.PluginInfo{Name: "status-change-observer"}, nil
}

func (o *statusChangeObserver) StartWorkflow(context.Context, *pb.StartWorkflowRequest) (*pb.StartWorkflowResponse, error) {
	return &pb.StartWorkflowResponse{}, nil
}

func (o *statusChangeObserver) PauseWorkflow(context.Context, string) (*pb.WorkflowStatusInfo, error) {
	return &pb.WorkflowStatusInfo{}, nil
}

func (o *statusChangeObserver) ResumeWorkflow(context.Context, string) (*pb.WorkflowStatusInfo, error) {
	return &pb.WorkflowStatusInfo{}, nil
}

func (o *statusChangeObserver) CancelWorkflow(context.Context, string) (*pb.WorkflowStatusInfo, error) {
	return &pb.WorkflowStatusInfo{}, nil
}

func (o *statusChangeObserver) GetWorkflowStatus(context.Context, string) (*pb.WorkflowStatusInfo, error) {
	return &pb.WorkflowStatusInfo{}, nil
}

func (o *statusChangeObserver) NotifyStatusChange(_ context.Context, sessionID string, displayStatus pb.DisplayStatus, hasFailures bool) error {
	o.changes <- observedStatusChange{
		sessionID:     sessionID,
		displayStatus: displayStatus,
		hasFailures:   hasFailures,
	}
	return nil
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "upstream", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}

func TestE2E_IssueComment_RefreshesDisplay(t *testing.T) {
	h := New(t)
	repoURL := "https://github.com/recurser/bossanova"
	repoID := h.SeedRepo(t, repoURL)
	sessionID := h.SeedSession(t, repoID, 345, pb.SessionState_SESSION_STATE_GREEN_DRAFT)

	h.PostGitHubWebhook(t, "issue_comment", fixture(t, "issue_comment_pr.json"), 345, repoURL)

	time.Sleep(100 * time.Millisecond)
	sess, err := h.Sessions.Get(h.Ctx(), sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got := pb.SessionState(sess.State); got != pb.SessionState_SESSION_STATE_GREEN_DRAFT {
		t.Errorf("state = %v, want GREEN_DRAFT (unchanged)", got)
	}

	if calls := h.Provider.CallCounts(); calls.GetPRStatus != 1 {
		t.Errorf("provider.GetPRStatus call count = %d, want 1 (refresh ran)", calls.GetPRStatus)
	}
}

func TestE2E_PullRequestSynchronize_ConflictDetectedRealtime(t *testing.T) {
	h := New(t)
	repoURL := "https://github.com/recurser/bossanova"
	repoID := h.SeedRepo(t, repoURL)

	// Let the harness's initial empty poll finish before seeding the session.
	time.Sleep(10 * time.Millisecond)

	sessionID := h.SeedSession(t, repoID, 345, pb.SessionState_SESSION_STATE_GREEN_DRAFT)
	payload := fixture(t, "pull_request_synchronize_conflict.json")

	start := time.Now()
	h.PostGitHubWebhook(t, "pull_request", payload, 345, repoURL)
	h.WaitForSessionState(t, sessionID, pb.SessionState_SESSION_STATE_FIXING_CHECKS, 200*time.Millisecond)

	latency := time.Since(start)
	t.Logf("pull_request synchronize conflict detected in %s", latency)

	if calls := h.Provider.CallCounts(); calls.GetPRStatus > 1 {
		t.Errorf("provider.GetPRStatus call count = %d, want <= 1 (refresh only; no polling)", calls.GetPRStatus)
	}
}

func TestE2E_PullRequestClosed_MergedRealtime(t *testing.T) {
	h := New(t)
	repoURL := "https://github.com/recurser/bossanova"
	repoID := h.SeedRepo(t, repoURL)
	sessionID := h.SeedSession(t, repoID, 345, pb.SessionState_SESSION_STATE_GREEN_DRAFT)

	h.PostGitHubWebhook(t, "pull_request", fixture(t, "pull_request_closed_merged.json"), 345, repoURL)

	h.WaitForSessionState(t, sessionID, pb.SessionState_SESSION_STATE_MERGED, 200*time.Millisecond)
}

func TestE2E_PullRequestClosed_UnmergedRealtime(t *testing.T) {
	h := New(t)
	repoURL := "https://github.com/recurser/bossanova"
	repoID := h.SeedRepo(t, repoURL)
	sessionID := h.SeedSession(t, repoID, 345, pb.SessionState_SESSION_STATE_GREEN_DRAFT)

	h.PostGitHubWebhook(t, "pull_request", fixture(t, "pull_request_closed_unmerged.json"), 345, repoURL)

	h.WaitForSessionState(t, sessionID, pb.SessionState_SESSION_STATE_CLOSED, 200*time.Millisecond)
}

func TestE2E_PullRequestReview_ChangesRequestedRealtime(t *testing.T) {
	h := New(t)
	repoURL := "https://github.com/recurser/bossanova"
	repoID := h.SeedRepo(t, repoURL)
	sessionID := h.SeedSession(t, repoID, 345, pb.SessionState_SESSION_STATE_READY_FOR_REVIEW)

	h.PostGitHubWebhook(t, "pull_request_review", fixture(t, "pull_request_review_changes_requested.json"), 345, repoURL)

	h.WaitForSessionState(t, sessionID, pb.SessionState_SESSION_STATE_FIXING_CHECKS, 200*time.Millisecond)
}

func TestE2E_CheckRunCompleted_FailureRealtime(t *testing.T) {
	h := New(t)
	repoURL := "https://github.com/recurser/bossanova"
	repoID := h.SeedRepo(t, repoURL)
	sessionID := h.SeedSession(t, repoID, 345, pb.SessionState_SESSION_STATE_AWAITING_CHECKS)

	h.PostGitHubWebhook(t, "check_run", fixture(t, "check_run_completed_failure.json"), 345, repoURL)

	h.WaitForSessionState(t, sessionID, pb.SessionState_SESSION_STATE_FIXING_CHECKS, 200*time.Millisecond)
}

func TestE2E_CheckRunCompleted_FailureNotifiesWorkflowPluginRealtime(t *testing.T) {
	observer := newStatusChangeObserver()
	h := NewWithOptions(t, Options{
		WorkflowServices: []pluginpkg.WorkflowService{observer},
	})
	repoURL := "https://github.com/recurser/bossanova"
	repoID := h.SeedRepo(t, repoURL)

	// Let the harness's initial empty poll finish before seeding the session.
	time.Sleep(10 * time.Millisecond)

	sessionID := h.SeedSession(t, repoID, 345, pb.SessionState_SESSION_STATE_AWAITING_CHECKS)
	failure := vcs.CheckConclusionFailure
	h.Provider.SetCheckResults(345, []vcs.CheckResult{{
		ID:         "ci",
		Name:       "ci",
		Status:     vcs.CheckStatusCompleted,
		Conclusion: &failure,
	}})

	for {
		select {
		case <-observer.changes:
		default:
			goto drained
		}
	}

drained:
	beforeCalls := h.Provider.CallCounts()
	h.PostGitHubWebhook(t, "check_run", fixture(t, "check_run_completed_failure.json"), 345, repoURL)

	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case got := <-observer.changes:
			if got.sessionID != sessionID {
				continue
			}
			if got.displayStatus != pb.DisplayStatus_DISPLAY_STATUS_FAILING {
				continue
			}
			goto notified
		case <-deadline:
			t.Fatal("workflow plugin did not receive failing check status notification within 500ms")
		}
	}

notified:
	h.WaitForSessionState(t, sessionID, pb.SessionState_SESSION_STATE_FIXING_CHECKS, 200*time.Millisecond)

	afterCalls := h.Provider.CallCounts()
	if got, want := afterCalls.GetCheckResults, beforeCalls.GetCheckResults+1; got != want {
		t.Fatalf("provider.GetCheckResults call count = %d, want %d (webhook refresh only)", got, want)
	}
}

func TestE2E_CheckSuiteCompleted_SuccessRealtime(t *testing.T) {
	h := New(t)
	repoURL := "https://github.com/recurser/bossanova"
	repoID := h.SeedRepo(t, repoURL)
	sessionID := h.SeedSession(t, repoID, 345, pb.SessionState_SESSION_STATE_AWAITING_CHECKS)

	h.PostGitHubWebhook(t, "check_suite", fixture(t, "check_suite_completed_success.json"), 345, repoURL)

	time.Sleep(100 * time.Millisecond)
	sess, err := h.Sessions.Get(h.Ctx(), sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got := pb.SessionState(sess.State); got != pb.SessionState_SESSION_STATE_AWAITING_CHECKS {
		t.Fatalf("state = %v, want AWAITING_CHECKS", got)
	}
}
