package main

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// mockHostService is a test double for hostServiceClient that returns
// preconfigured responses without making gRPC calls.
type mockHostService struct {
	prs       []*bossanovav1.PRSummary
	closedPRs []*bossanovav1.PRSummary
	checks    map[int32][]*bossanovav1.CheckResult
	status    map[int32]*bossanovav1.PRStatus
	prErr     error
}

func (m *mockHostService) ListDependabotPRs(_ context.Context, _ string) ([]*bossanovav1.PRSummary, error) {
	if m.prErr != nil {
		return nil, m.prErr
	}
	return m.prs, nil
}

func (m *mockHostService) ListClosedDependabotPRs(_ context.Context, _ string) ([]*bossanovav1.PRSummary, error) {
	return m.closedPRs, nil
}

func (m *mockHostService) GetCheckResults(_ context.Context, _ string, prNumber int32) ([]*bossanovav1.CheckResult, error) {
	return m.checks[prNumber], nil
}

func (m *mockHostService) GetPRStatus(_ context.Context, _ string, prNumber int32) (*bossanovav1.PRStatus, error) {
	if s, ok := m.status[prNumber]; ok {
		return s, nil
	}
	return &bossanovav1.PRStatus{}, nil
}

// Compile-time check that both real and mock implement the hostClient interface.
var (
	_ hostClient = (*hostServiceClient)(nil)
	_ hostClient = (*lazyHostServiceClient)(nil)
	_ hostClient = (*mockHostService)(nil)
)

func newTestServer(mock *mockHostService) *server {
	logger := zerolog.New(os.Stderr).Level(zerolog.Disabled)
	return &server{
		host:   mock,
		logger: logger,
	}
}

func boolPtr(b bool) *bool { return &b }

func conclusionPtr(c bossanovav1.CheckConclusion) *bossanovav1.CheckConclusion { return &c }

func TestGetInfo(t *testing.T) {
	srv := newTestServer(nil)
	resp, err := srv.GetInfo(context.Background(), &bossanovav1.TaskSourceServiceGetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	info := resp.GetInfo()
	if info.GetName() != "dependabot" {
		t.Errorf("name = %q, want %q", info.GetName(), "dependabot")
	}
	if info.GetVersion() != "0.1.0" {
		t.Errorf("version = %q, want %q", info.GetVersion(), "0.1.0")
	}
	if len(info.GetCapabilities()) != 1 || info.GetCapabilities()[0] != "task_source" {
		t.Errorf("capabilities = %v, want [task_source]", info.GetCapabilities())
	}
}

func TestAggregateCheckResults(t *testing.T) {
	tests := []struct {
		name   string
		checks []*bossanovav1.CheckResult
		want   checksOverall
	}{
		{
			name: "all passed",
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)},
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)},
			},
			want: checksOverallPassed,
		},
		{
			name: "one failure",
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)},
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_FAILURE)},
			},
			want: checksOverallFailed,
		},
		{
			name: "still in progress",
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)},
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_IN_PROGRESS},
			},
			want: checksOverallPending,
		},
		{
			name: "queued",
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_QUEUED},
			},
			want: checksOverallPending,
		},
		{
			name: "neutral conclusion counts as pass",
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_NEUTRAL)},
			},
			want: checksOverallPassed,
		},
		{
			name: "skipped conclusion counts as pass",
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SKIPPED)},
			},
			want: checksOverallPassed,
		},
		{
			name: "cancelled conclusion counts as failure",
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_CANCELLED)},
			},
			want: checksOverallFailed,
		},
		{
			name: "timed out conclusion counts as failure",
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_TIMED_OUT)},
			},
			want: checksOverallFailed,
		},
		{
			name: "nil conclusion on completed check counts as failure",
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED},
			},
			want: checksOverallFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := aggregateCheckResults(tt.checks)
			if got != tt.want {
				t.Errorf("aggregateCheckResults() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpdateTaskStatus(t *testing.T) {
	srv := newTestServer(nil)
	resp, err := srv.UpdateTaskStatus(context.Background(), &bossanovav1.UpdateTaskStatusRequest{
		ExternalId: "dependabot:pr:https://github.com/foo/bar:42",
		Status:     bossanovav1.TaskItemStatus_TASK_ITEM_STATUS_COMPLETED,
		Details:    "merged",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestPollTasksEmptyRepo(t *testing.T) {
	srv := newTestServer(nil)
	resp, err := srv.PollTasks(context.Background(), &bossanovav1.PollTasksRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetTasks()) != 0 {
		t.Errorf("expected no tasks for empty repo URL, got %d", len(resp.GetTasks()))
	}
}

func TestClassifyPR(t *testing.T) {
	repoURL := "https://github.com/foo/bar"

	tests := []struct {
		name       string
		pr         *bossanovav1.PRSummary
		checks     []*bossanovav1.CheckResult
		status     *bossanovav1.PRStatus
		wantAction bossanovav1.TaskAction
		wantNil    bool
	}{
		{
			name: "checks passed + mergeable → AUTO_MERGE",
			pr: &bossanovav1.PRSummary{
				Number:     42,
				Title:      "Bump lodash from 4.17.20 to 4.17.21",
				HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.21",
				Author:     dependabotAuthor,
			},
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)},
			},
			status:     &bossanovav1.PRStatus{Mergeable: boolPtr(true)},
			wantAction: bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		},
		{
			name: "checks failed → CREATE_SESSION",
			pr: &bossanovav1.PRSummary{
				Number:     43,
				Title:      "Bump express from 4.0.0 to 5.0.0",
				HeadBranch: "dependabot/npm_and_yarn/express-5.0.0",
				Author:     dependabotAuthor,
			},
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_FAILURE)},
			},
			wantAction: bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
		},
		{
			name: "no checks yet → skip",
			pr: &bossanovav1.PRSummary{
				Number:     44,
				Title:      "Bump react from 17.0.0 to 18.0.0",
				HeadBranch: "dependabot/npm_and_yarn/react-18.0.0",
				Author:     dependabotAuthor,
			},
			checks:  nil,
			wantNil: true,
		},
		{
			name: "checks pending → skip",
			pr: &bossanovav1.PRSummary{
				Number:     45,
				Title:      "Bump vue from 2.0.0 to 3.0.0",
				HeadBranch: "dependabot/npm_and_yarn/vue-3.0.0",
				Author:     dependabotAuthor,
			},
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_IN_PROGRESS},
			},
			wantNil: true,
		},
		{
			name: "checks passed but not mergeable → skip",
			pr: &bossanovav1.PRSummary{
				Number:     46,
				Title:      "Bump angular from 15.0.0 to 16.0.0",
				HeadBranch: "dependabot/npm_and_yarn/angular-16.0.0",
				Author:     dependabotAuthor,
			},
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)},
			},
			status:  &bossanovav1.PRStatus{Mergeable: boolPtr(false)},
			wantNil: true,
		},
		{
			name: "checks passed + mergeable nil → AUTO_MERGE",
			pr: &bossanovav1.PRSummary{
				Number:     47,
				Title:      "Bump svelte from 3.0.0 to 4.0.0",
				HeadBranch: "dependabot/npm_and_yarn/svelte-4.0.0",
				Author:     dependabotAuthor,
			},
			checks: []*bossanovav1.CheckResult{
				{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)},
			},
			status:     &bossanovav1.PRStatus{},
			wantAction: bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockHostService{
				checks: map[int32][]*bossanovav1.CheckResult{
					tt.pr.GetNumber(): tt.checks,
				},
				status: map[int32]*bossanovav1.PRStatus{},
			}
			if tt.status != nil {
				mock.status[tt.pr.GetNumber()] = tt.status
			}

			srv := newTestServer(mock)
			task, err := srv.classifyPR(context.Background(), repoURL, tt.pr)
			if err != nil {
				t.Fatal(err)
			}

			if tt.wantNil {
				if task != nil {
					t.Errorf("expected nil task, got action=%v", task.GetAction())
				}
				return
			}
			if task == nil {
				t.Fatal("expected non-nil task")
			}
			if task.GetAction() != tt.wantAction {
				t.Errorf("action = %v, want %v", task.GetAction(), tt.wantAction)
			}
			if task.GetRepoOriginUrl() != repoURL {
				t.Errorf("repo = %q, want %q", task.GetRepoOriginUrl(), repoURL)
			}
			if task.GetExternalId() == "" {
				t.Error("expected non-empty external_id")
			}
		})
	}
}

// pollTasksWithMock exercises the full PollTasks path using the mock
// injected directly into the server via the hostClient interface.
func pollTasksWithMock(t *testing.T, mock *mockHostService, repoURL string) ([]*bossanovav1.TaskItem, error) {
	t.Helper()
	srv := newTestServer(mock)
	req := &bossanovav1.PollTasksRequest{}
	if repoURL != "" {
		req.RepoOriginUrl = &repoURL
	}
	resp, err := srv.PollTasks(context.Background(), req)
	if err != nil {
		return nil, err
	}
	return resp.GetTasks(), nil
}

func TestPollTasksMultiplePRsMixedStates(t *testing.T) {
	repoURL := "https://github.com/foo/bar"
	mock := &mockHostService{
		prs: []*bossanovav1.PRSummary{
			{Number: 10, Title: "Bump lodash from 4.17.20 to 4.17.21", HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.21", Author: dependabotAuthor},
			{Number: 11, Title: "Bump express from 4.0.0 to 5.0.0", HeadBranch: "dependabot/npm_and_yarn/express-5.0.0", Author: dependabotAuthor},
			{Number: 12, Title: "Bump react from 17.0.0 to 18.0.0", HeadBranch: "dependabot/npm_and_yarn/react-18.0.0", Author: dependabotAuthor},
		},
		checks: map[int32][]*bossanovav1.CheckResult{
			// PR 10: checks passed → AUTO_MERGE
			10: {{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)}},
			// PR 11: checks failed → CREATE_SESSION
			11: {{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_FAILURE)}},
			// PR 12: checks pending → skip
			12: {{Status: bossanovav1.CheckStatus_CHECK_STATUS_IN_PROGRESS}},
		},
		status: map[int32]*bossanovav1.PRStatus{
			10: {Mergeable: boolPtr(true)},
		},
	}

	tasks, err := pollTasksWithMock(t, mock, repoURL)
	if err != nil {
		t.Fatal(err)
	}

	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}

	// First task: lodash AUTO_MERGE.
	if tasks[0].GetAction() != bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE {
		t.Errorf("tasks[0] action = %v, want AUTO_MERGE", tasks[0].GetAction())
	}
	// Second task: express CREATE_SESSION.
	if tasks[1].GetAction() != bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION {
		t.Errorf("tasks[1] action = %v, want CREATE_SESSION", tasks[1].GetAction())
	}
}

func TestPollTasksNonDependabotPRsFiltered(t *testing.T) {
	repoURL := "https://github.com/foo/bar"
	mock := &mockHostService{
		prs: []*bossanovav1.PRSummary{
			{Number: 20, Title: "Add feature X", HeadBranch: "feat/x", Author: "human-dev"},
			{Number: 21, Title: "Bump lodash from 4.17.20 to 4.17.21", HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.21", Author: dependabotAuthor},
		},
		checks: map[int32][]*bossanovav1.CheckResult{
			21: {{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)}},
		},
		status: map[int32]*bossanovav1.PRStatus{
			21: {Mergeable: boolPtr(true)},
		},
	}

	tasks, err := pollTasksWithMock(t, mock, repoURL)
	if err != nil {
		t.Fatal(err)
	}

	// Only the dependabot PR should produce a task (non-dependabot filtered by ListDependabotPRs).
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].GetAction() != bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE {
		t.Errorf("action = %v, want AUTO_MERGE", tasks[0].GetAction())
	}
}

func TestPollTasksHostServiceError(t *testing.T) {
	repoURL := "https://github.com/foo/bar"
	mock := &mockHostService{
		prErr: fmt.Errorf("GitHub API rate limit exceeded"),
	}

	_, err := pollTasksWithMock(t, mock, repoURL)
	if err == nil {
		t.Fatal("expected error when host service fails")
	}
}

func TestPollTasksNoPRs(t *testing.T) {
	repoURL := "https://github.com/foo/bar"
	mock := &mockHostService{
		prs:    nil,
		checks: map[int32][]*bossanovav1.CheckResult{},
		status: map[int32]*bossanovav1.PRStatus{},
	}

	tasks, err := pollTasksWithMock(t, mock, repoURL)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for no PRs, got %d", len(tasks))
	}
}

func TestPollTasksCheckFailedProducesPlan(t *testing.T) {
	repoURL := "https://github.com/foo/bar"
	mock := &mockHostService{
		prs: []*bossanovav1.PRSummary{
			{Number: 99, Title: "Bump webpack from 4.0.0 to 5.0.0", HeadBranch: "dependabot/npm_and_yarn/webpack-5.0.0", Author: dependabotAuthor},
		},
		checks: map[int32][]*bossanovav1.CheckResult{
			99: {{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_FAILURE)}},
		},
		status: map[int32]*bossanovav1.PRStatus{},
	}

	tasks, err := pollTasksWithMock(t, mock, repoURL)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].GetPlan() == "" {
		t.Error("CREATE_SESSION task should have a non-empty plan")
	}
	if tasks[0].GetExistingBranch() != "dependabot/npm_and_yarn/webpack-5.0.0" {
		t.Errorf("existing_branch = %q, want dependabot branch", tasks[0].GetExistingBranch())
	}
}

func TestPollTasksAutoMergeLabels(t *testing.T) {
	repoURL := "https://github.com/foo/bar"
	mock := &mockHostService{
		prs: []*bossanovav1.PRSummary{
			{Number: 50, Title: "Bump lodash from 4.17.20 to 4.17.21", HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.21", Author: dependabotAuthor},
		},
		checks: map[int32][]*bossanovav1.CheckResult{
			50: {{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)}},
		},
		status: map[int32]*bossanovav1.PRStatus{
			50: {Mergeable: boolPtr(true)},
		},
	}

	tasks, err := pollTasksWithMock(t, mock, repoURL)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	labels := tasks[0].GetLabels()
	if len(labels) < 2 {
		t.Fatalf("expected at least 2 labels, got %v", labels)
	}
	if labels[0] != "dependabot" {
		t.Errorf("labels[0] = %q, want %q", labels[0], "dependabot")
	}
	if labels[1] != "lodash" {
		t.Errorf("labels[1] = %q, want %q", labels[1], "lodash")
	}
}

func TestPollTasksPreviouslyRejectedLibrary(t *testing.T) {
	repoURL := "https://github.com/foo/bar"

	t.Run("most recent closed → rejected", func(t *testing.T) {
		mock := &mockHostService{
			prs: []*bossanovav1.PRSummary{
				// Open PR for prisma — checks passed, mergeable.
				{Number: 100, Title: "Bump @prisma/client from 6.0.0 to 7.0.0", HeadBranch: "dependabot/npm_and_yarn/@prisma/client-7.0.0", Author: dependabotAuthor},
				// Open PR for lodash — checks passed, mergeable (not rejected).
				{Number: 101, Title: "Bump lodash from 4.17.20 to 4.17.21", HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.21", Author: dependabotAuthor},
			},
			closedPRs: []*bossanovav1.PRSummary{
				// Previously closed (rejected) prisma PR.
				{Number: 90, Title: "Bump @prisma/client from 5.0.0 to 6.0.0", HeadBranch: "dependabot/npm_and_yarn/@prisma/client-6.0.0", State: bossanovav1.PRState_PR_STATE_CLOSED, Author: dependabotAuthor},
			},
			checks: map[int32][]*bossanovav1.CheckResult{
				100: {{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)}},
				101: {{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)}},
			},
			status: map[int32]*bossanovav1.PRStatus{
				100: {Mergeable: boolPtr(true)},
				101: {Mergeable: boolPtr(true)},
			},
		}

		tasks, err := pollTasksWithMock(t, mock, repoURL)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 2 {
			t.Fatalf("expected 2 tasks, got %d", len(tasks))
		}

		// Prisma should be NOTIFY_USER (previously rejected).
		if tasks[0].GetAction() != bossanovav1.TaskAction_TASK_ACTION_NOTIFY_USER {
			t.Errorf("tasks[0] action = %v, want NOTIFY_USER", tasks[0].GetAction())
		}
		if tasks[0].GetTitle() != "Bump @prisma/client from 6.0.0 to 7.0.0" {
			t.Errorf("tasks[0] title = %q, want prisma PR", tasks[0].GetTitle())
		}

		// Lodash should be AUTO_MERGE (not rejected).
		if tasks[1].GetAction() != bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE {
			t.Errorf("tasks[1] action = %v, want AUTO_MERGE", tasks[1].GetAction())
		}
	})

	t.Run("closed then merged → not rejected", func(t *testing.T) {
		mock := &mockHostService{
			prs: []*bossanovav1.PRSummary{
				// Open PR for prisma — checks passed, mergeable.
				{Number: 100, Title: "Bump @prisma/client from 7.0.0 to 8.0.0", HeadBranch: "dependabot/npm_and_yarn/@prisma/client-8.0.0", Author: dependabotAuthor},
			},
			closedPRs: []*bossanovav1.PRSummary{
				// Older: closed (rejected) prisma PR.
				{Number: 80, Title: "Bump @prisma/client from 5.0.0 to 6.0.0", HeadBranch: "dependabot/npm_and_yarn/@prisma/client-6.0.0", State: bossanovav1.PRState_PR_STATE_CLOSED, Author: dependabotAuthor},
				// Newer: merged prisma PR — user accepted a later version.
				{Number: 90, Title: "Bump @prisma/client from 6.0.0 to 7.0.0", HeadBranch: "dependabot/npm_and_yarn/@prisma/client-7.0.0", State: bossanovav1.PRState_PR_STATE_MERGED, Author: dependabotAuthor},
			},
			checks: map[int32][]*bossanovav1.CheckResult{
				100: {{Status: bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS)}},
			},
			status: map[int32]*bossanovav1.PRStatus{
				100: {Mergeable: boolPtr(true)},
			},
		}

		tasks, err := pollTasksWithMock(t, mock, repoURL)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("expected 1 task, got %d", len(tasks))
		}

		// Prisma should be AUTO_MERGE (most recent PR was merged, not rejected).
		if tasks[0].GetAction() != bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE {
			t.Errorf("tasks[0] action = %v, want AUTO_MERGE", tasks[0].GetAction())
		}
	})
}
