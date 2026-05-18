package upstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/recurser/bossalib/vcs"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()

	payload, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return payload
}

func mutateJSONFixture(t *testing.T, payload []byte, mutate func(map[string]any)) []byte {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	mutate(body)

	mutated, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return mutated
}

func TestTranslateWebhook_PullRequestSynchronizeConflict(t *testing.T) {
	events, pr, err := TranslateWebhook("pull_request", loadFixture(t, "pull_request_synchronize_conflict.json"))
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	want := []vcs.Event{vcs.ConflictDetected{PRID: 345}}
	if pr != 345 {
		t.Fatalf("TranslateWebhook() pr = %d, want 345", pr)
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("TranslateWebhook() events = %#v, want %#v", events, want)
	}
}

func TestTranslateWebhook_PullRequestClosedMerged(t *testing.T) {
	events, pr, err := TranslateWebhook("pull_request", loadFixture(t, "pull_request_closed_merged.json"))
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	want := []vcs.Event{vcs.PRMerged{PRID: 345}}
	if pr != 345 {
		t.Fatalf("TranslateWebhook() pr = %d, want 345", pr)
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("TranslateWebhook() events = %#v, want %#v", events, want)
	}
}

func TestTranslateWebhook_PullRequestClosedUnmerged(t *testing.T) {
	events, pr, err := TranslateWebhook("pull_request", loadFixture(t, "pull_request_closed_unmerged.json"))
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	want := []vcs.Event{vcs.PRClosed{PRID: 345}}
	if pr != 345 {
		t.Fatalf("TranslateWebhook() pr = %d, want 345", pr)
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("TranslateWebhook() events = %#v, want %#v", events, want)
	}
}

func TestTranslateWebhook_PullRequestReviewChangesRequested(t *testing.T) {
	events, pr, err := TranslateWebhook("pull_request_review", loadFixture(t, "pull_request_review_changes_requested.json"))
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	if pr != 345 {
		t.Fatalf("TranslateWebhook() pr = %d, want 345", pr)
	}
	if len(events) != 1 {
		t.Fatalf("TranslateWebhook() events length = %d, want 1", len(events))
	}

	review, ok := events[0].(vcs.ReviewSubmitted)
	if !ok {
		t.Fatalf("TranslateWebhook() event type = %T, want vcs.ReviewSubmitted", events[0])
	}
	if review.PRID != 345 {
		t.Fatalf("ReviewSubmitted.PRID = %d, want 345", review.PRID)
	}
	if review.State != vcs.ReviewStateChangesRequested {
		t.Fatalf("ReviewSubmitted.State = %v, want %v", review.State, vcs.ReviewStateChangesRequested)
	}
	if len(review.Comments) != 1 {
		t.Fatalf("ReviewSubmitted.Comments length = %d, want 1", len(review.Comments))
	}
	comment := review.Comments[0]
	if comment.Body != "needs work" {
		t.Fatalf("ReviewSubmitted.Comments[0].Body = %q, want needs work", comment.Body)
	}
	if comment.Author != "reviewer" {
		t.Fatalf("ReviewSubmitted.Comments[0].Author = %q, want reviewer", comment.Author)
	}
	if comment.State != vcs.ReviewStateChangesRequested {
		t.Fatalf("ReviewSubmitted.Comments[0].State = %v, want %v", comment.State, vcs.ReviewStateChangesRequested)
	}
	if comment.Path != nil {
		t.Fatalf("ReviewSubmitted.Comments[0].Path = %v, want nil", comment.Path)
	}
	if comment.Line != nil {
		t.Fatalf("ReviewSubmitted.Comments[0].Line = %v, want nil", comment.Line)
	}
}

func TestTranslateWebhook_PullRequestReviewStates(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  vcs.ReviewState
	}{
		{
			name:  "approved",
			state: "approved",
			want:  vcs.ReviewStateApproved,
		},
		{
			name:  "commented",
			state: "commented",
			want:  vcs.ReviewStateCommented,
		},
		{
			name:  "dismissed",
			state: "dismissed",
			want:  vcs.ReviewStateDismissed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
				review := body["review"].(map[string]any)
				review["state"] = tt.state
			})

			events, pr, err := TranslateWebhook("pull_request_review", payload)
			if err != nil {
				t.Fatalf("TranslateWebhook() error = %v", err)
			}

			if pr != 345 {
				t.Fatalf("TranslateWebhook() pr = %d, want 345", pr)
			}
			if len(events) != 1 {
				t.Fatalf("TranslateWebhook() events length = %d, want 1", len(events))
			}

			review, ok := events[0].(vcs.ReviewSubmitted)
			if !ok {
				t.Fatalf("TranslateWebhook() event type = %T, want vcs.ReviewSubmitted", events[0])
			}
			if review.PRID != 345 {
				t.Fatalf("ReviewSubmitted.PRID = %d, want 345", review.PRID)
			}
			if review.State != tt.want {
				t.Fatalf("ReviewSubmitted.State = %v, want %v", review.State, tt.want)
			}
		})
	}
}

func TestTranslateWebhook_PullRequestReviewUnknownState(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
		review := body["review"].(map[string]any)
		review["state"] = "unknown"
	})

	events, pr, err := TranslateWebhook("pull_request_review", payload)
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	if pr != 345 {
		t.Fatalf("TranslateWebhook() pr = %d, want 345", pr)
	}
	if len(events) != 0 {
		t.Fatalf("TranslateWebhook() events length = %d, want 0", len(events))
	}
}

func TestTranslateWebhook_PullRequestReviewMissingPRNumber(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "pull_request_review_changes_requested.json"), func(body map[string]any) {
		pullRequest := body["pull_request"].(map[string]any)
		delete(pullRequest, "number")
	})

	events, pr, err := TranslateWebhook("pull_request_review", payload)
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	if pr != 0 {
		t.Fatalf("TranslateWebhook() pr = %d, want 0", pr)
	}
	if len(events) != 0 {
		t.Fatalf("TranslateWebhook() events length = %d, want 0", len(events))
	}
}

func TestTranslateWebhook_IssueCommentOnPR(t *testing.T) {
	events, pr, err := TranslateWebhook("issue_comment", loadFixture(t, "issue_comment_pr.json"))
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	if pr != 345 {
		t.Fatalf("TranslateWebhook() pr = %d, want 345", pr)
	}
	if len(events) != 0 {
		t.Fatalf("TranslateWebhook() events length = %d, want 0", len(events))
	}
}

func TestTranslateWebhook_IssueCommentPlainIssue(t *testing.T) {
	events, pr, err := TranslateWebhook("issue_comment", loadFixture(t, "issue_comment_plain_issue.json"))
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	if pr != 0 {
		t.Fatalf("TranslateWebhook() pr = %d, want 0", pr)
	}
	if len(events) != 0 {
		t.Fatalf("TranslateWebhook() events length = %d, want 0", len(events))
	}
}

func TestTranslateWebhook_CheckRunCompletedFailure(t *testing.T) {
	events, pr, err := TranslateWebhook("check_run", loadFixture(t, "check_run_completed_failure.json"))
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	if pr != 345 {
		t.Fatalf("TranslateWebhook() pr = %d, want 345", pr)
	}
	if len(events) != 1 {
		t.Fatalf("TranslateWebhook() events length = %d, want 1", len(events))
	}

	failed, ok := events[0].(vcs.ChecksFailed)
	if !ok {
		t.Fatalf("TranslateWebhook() event type = %T, want vcs.ChecksFailed", events[0])
	}
	if failed.PRID != 345 {
		t.Fatalf("ChecksFailed.PRID = %d, want 345", failed.PRID)
	}
	if len(failed.FailedChecks) != 1 {
		t.Fatalf("ChecksFailed.FailedChecks length = %d, want 1", len(failed.FailedChecks))
	}
	check := failed.FailedChecks[0]
	if check.ID != "11111" {
		t.Fatalf("ChecksFailed.FailedChecks[0].ID = %q, want 11111", check.ID)
	}
	if check.Name != "lint" {
		t.Fatalf("ChecksFailed.FailedChecks[0].Name = %q, want lint", check.Name)
	}
	if check.Status != vcs.CheckStatusCompleted {
		t.Fatalf("ChecksFailed.FailedChecks[0].Status = %v, want %v", check.Status, vcs.CheckStatusCompleted)
	}
	if check.Conclusion == nil || *check.Conclusion != vcs.CheckConclusionFailure {
		t.Fatalf("ChecksFailed.FailedChecks[0].Conclusion = %v, want %v", check.Conclusion, vcs.CheckConclusionFailure)
	}
}

func TestTranslateWebhook_CheckSuiteCompletedSuccess(t *testing.T) {
	events, pr, err := TranslateWebhook("check_suite", loadFixture(t, "check_suite_completed_success.json"))
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	if pr != 345 {
		t.Fatalf("TranslateWebhook() pr = %d, want 345", pr)
	}
	if len(events) != 0 {
		t.Fatalf("TranslateWebhook() events length = %d, want 0", len(events))
	}
}

func TestTranslateWebhook_CheckSuiteCompletedFailure(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "check_suite_completed_success.json"), func(body map[string]any) {
		checkSuite := body["check_suite"].(map[string]any)
		checkSuite["id"] = 22222
		checkSuite["status"] = "completed"
		checkSuite["conclusion"] = "failure"
		checkSuite["app"] = map[string]any{"name": "ci"}
	})

	events, pr, err := TranslateWebhook("check_suite", payload)
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	if pr != 345 {
		t.Fatalf("TranslateWebhook() pr = %d, want 345", pr)
	}
	if len(events) != 1 {
		t.Fatalf("TranslateWebhook() events length = %d, want 1", len(events))
	}
	failed, ok := events[0].(vcs.ChecksFailed)
	if !ok {
		t.Fatalf("TranslateWebhook() event type = %T, want vcs.ChecksFailed", events[0])
	}
	if failed.PRID != 345 {
		t.Fatalf("ChecksFailed.PRID = %d, want 345", failed.PRID)
	}
	if len(failed.FailedChecks) != 1 {
		t.Fatalf("ChecksFailed.FailedChecks length = %d, want 1", len(failed.FailedChecks))
	}
	check := failed.FailedChecks[0]
	if check.ID != "22222" {
		t.Fatalf("ChecksFailed.FailedChecks[0].ID = %q, want 22222", check.ID)
	}
	if check.Name != "ci" {
		t.Fatalf("ChecksFailed.FailedChecks[0].Name = %q, want ci", check.Name)
	}
	if check.Status != vcs.CheckStatusCompleted {
		t.Fatalf("ChecksFailed.FailedChecks[0].Status = %v, want %v", check.Status, vcs.CheckStatusCompleted)
	}
	if check.Conclusion == nil || *check.Conclusion != vcs.CheckConclusionFailure {
		t.Fatalf("ChecksFailed.FailedChecks[0].Conclusion = %v, want %v", check.Conclusion, vcs.CheckConclusionFailure)
	}
}

func TestTranslateWebhook_CheckSuiteCompletedTimedOut(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "check_suite_completed_success.json"), func(body map[string]any) {
		checkSuite := body["check_suite"].(map[string]any)
		checkSuite["id"] = 33333
		checkSuite["status"] = "completed"
		checkSuite["conclusion"] = "timed_out"
		checkSuite["app"] = map[string]any{"name": "ci"}
	})

	events, pr, err := TranslateWebhook("check_suite", payload)
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	if pr != 345 {
		t.Fatalf("TranslateWebhook() pr = %d, want 345", pr)
	}
	if len(events) != 1 {
		t.Fatalf("TranslateWebhook() events length = %d, want 1", len(events))
	}
	failed, ok := events[0].(vcs.ChecksFailed)
	if !ok {
		t.Fatalf("TranslateWebhook() event type = %T, want vcs.ChecksFailed", events[0])
	}
	if failed.PRID != 345 {
		t.Fatalf("ChecksFailed.PRID = %d, want 345", failed.PRID)
	}
	if len(failed.FailedChecks) != 1 {
		t.Fatalf("ChecksFailed.FailedChecks length = %d, want 1", len(failed.FailedChecks))
	}
	check := failed.FailedChecks[0]
	if check.ID != "33333" {
		t.Fatalf("ChecksFailed.FailedChecks[0].ID = %q, want 33333", check.ID)
	}
	if check.Name != "ci" {
		t.Fatalf("ChecksFailed.FailedChecks[0].Name = %q, want ci", check.Name)
	}
	if check.Status != vcs.CheckStatusCompleted {
		t.Fatalf("ChecksFailed.FailedChecks[0].Status = %v, want %v", check.Status, vcs.CheckStatusCompleted)
	}
	if check.Conclusion == nil || *check.Conclusion != vcs.CheckConclusionTimedOut {
		t.Fatalf("ChecksFailed.FailedChecks[0].Conclusion = %v, want %v", check.Conclusion, vcs.CheckConclusionTimedOut)
	}
}

func TestTranslateWebhook_CheckSuiteCompletedSuccessWithoutPR(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "check_suite_completed_success.json"), func(body map[string]any) {
		checkSuite := body["check_suite"].(map[string]any)
		checkSuite["pull_requests"] = []any{}
	})

	events, pr, err := TranslateWebhook("check_suite", payload)
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	if pr != 0 {
		t.Fatalf("TranslateWebhook() pr = %d, want 0", pr)
	}
	if len(events) != 0 {
		t.Fatalf("TranslateWebhook() events length = %d, want 0", len(events))
	}
}

func TestTranslateWebhook_CheckRunCompletedFailureWithoutPR(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "check_run_completed_failure.json"), func(body map[string]any) {
		checkRun := body["check_run"].(map[string]any)
		checkRun["pull_requests"] = []any{}
	})

	events, pr, err := TranslateWebhook("check_run", payload)
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	if pr != 0 {
		t.Fatalf("TranslateWebhook() pr = %d, want 0", pr)
	}
	if len(events) != 0 {
		t.Fatalf("TranslateWebhook() events length = %d, want 0", len(events))
	}
}

func TestTranslateWebhook_CheckRunCompletedTimedOut(t *testing.T) {
	payload := mutateJSONFixture(t, loadFixture(t, "check_run_completed_failure.json"), func(body map[string]any) {
		checkRun := body["check_run"].(map[string]any)
		checkRun["conclusion"] = "timed_out"
	})

	events, pr, err := TranslateWebhook("check_run", payload)
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}

	if pr != 345 {
		t.Fatalf("TranslateWebhook() pr = %d, want 345", pr)
	}
	if len(events) != 1 {
		t.Fatalf("TranslateWebhook() events length = %d, want 1", len(events))
	}

	failed, ok := events[0].(vcs.ChecksFailed)
	if !ok {
		t.Fatalf("TranslateWebhook() event type = %T, want vcs.ChecksFailed", events[0])
	}
	if len(failed.FailedChecks) != 1 {
		t.Fatalf("ChecksFailed.FailedChecks length = %d, want 1", len(failed.FailedChecks))
	}
	check := failed.FailedChecks[0]
	if check.Conclusion == nil || *check.Conclusion != vcs.CheckConclusionTimedOut {
		t.Fatalf("ChecksFailed.FailedChecks[0].Conclusion = %v, want %v", check.Conclusion, vcs.CheckConclusionTimedOut)
	}
}

func TestTranslateWebhook_MalformedPayload(t *testing.T) {
	_, _, err := TranslateWebhook("pull_request", []byte("{not json"))
	if err == nil {
		t.Fatal("TranslateWebhook() error = nil, want non-nil")
	}
}

func TestTranslateWebhook_UnknownEventType(t *testing.T) {
	events, pr, err := TranslateWebhook("unknown_event_type_xyz", []byte("{}"))
	if err == nil {
		t.Fatal("TranslateWebhook() error = nil, want non-nil")
	}
	if events != nil {
		t.Fatalf("TranslateWebhook() events = %#v, want nil", events)
	}
	if pr != 0 {
		t.Fatalf("TranslateWebhook() pr = %d, want 0", pr)
	}
}

func TestTranslateWebhook_KnownButUnhandled(t *testing.T) {
	events, pr, err := TranslateWebhook("push", []byte(`{"ref":"refs/heads/main","repository":{"id":1}}`))
	if err != nil {
		t.Fatalf("TranslateWebhook() error = %v", err)
	}
	if events != nil {
		t.Fatalf("TranslateWebhook() events = %#v, want nil", events)
	}
	if pr != 0 {
		t.Fatalf("TranslateWebhook() pr = %d, want 0", pr)
	}
}
