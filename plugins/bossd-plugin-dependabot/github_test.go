package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestFilterDependabotPRs(t *testing.T) {
	prs := []*bossanovav1.PRSummary{
		nil,
		{Number: 1, Author: "human-dev"},
		{Number: 2, Author: dependabotAuthor},
		{Number: 3, Author: "dependabot[bot]"},
		{Number: 4, Author: dependabotAuthor},
	}

	got := filterDependabotPRs(prs)
	if len(got) != 2 {
		t.Fatalf("filterDependabotPRs returned %d PRs, want 2", len(got))
	}

	wantNumbers := []int32{2, 4}
	for i, want := range wantNumbers {
		if got[i].GetNumber() != want {
			t.Errorf("filtered PR %d number = %d, want %d", i, got[i].GetNumber(), want)
		}
	}
}

// TestPollTasksReusesClosedHistory pins the closed-history cache: a repeat poll
// with an unchanged open dependabot set reuses the history, and a PR leaving
// the set, the history aging past closedHistoryMaxAge, or a failed fetch each
// lead to a fresh ListClosedPRs. A failed refetch still checks against the last
// cached history.
func TestPollTasksReusesClosedHistory(t *testing.T) {
	repoURL := "https://github.com/owner/repo"
	lodash := &bossanovav1.PRSummary{Number: 100, Title: "Bump lodash from 4.17.20 to 4.17.21", HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.21", Author: dependabotAuthor}
	prisma := &bossanovav1.PRSummary{Number: 101, Title: "Bump @prisma/client from 6.0.0 to 7.0.0", HeadBranch: "dependabot/npm_and_yarn/@prisma/client-7.0.0", Author: dependabotAuthor}
	rejectedPrisma := &bossanovav1.PRSummary{Number: 90, Title: "Bump @prisma/client from 5.0.0 to 6.0.0", HeadBranch: "dependabot/npm_and_yarn/@prisma/client-6.0.0", State: bossanovav1.PRState_PR_STATE_CLOSED, Author: dependabotAuthor}

	mock := &mockHostService{
		prs:       []*bossanovav1.PRSummary{lodash, prisma},
		closedPRs: []*bossanovav1.PRSummary{rejectedPrisma},
	}
	srv := newTestServer(mock)
	clock := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return clock }
	poll := func() []*bossanovav1.TaskItem {
		t.Helper()
		resp, err := srv.PollTasks(t.Context(), &bossanovav1.PollTasksRequest{RepoOriginUrl: &repoURL})
		if err != nil {
			t.Fatalf("PollTasks: %v", err)
		}
		return resp.GetTasks()
	}
	wantCalls := func(step string, want int) {
		t.Helper()
		if mock.closedCalls != want {
			t.Fatalf("%s: ListClosedPRs calls = %d, want %d", step, mock.closedCalls, want)
		}
	}

	poll()
	wantCalls("first poll", 1)

	clock = clock.Add(2 * time.Minute)
	tasks := poll()
	wantCalls("repeat poll, same open set", 1)
	if len(tasks) != 1 || tasks[0].GetAction() != bossanovav1.TaskAction_TASK_ACTION_NOTIFY_USER {
		t.Fatalf("repeat poll tasks = %v, want the cached history to still flag prisma as rejected", tasks)
	}

	mock.prs = []*bossanovav1.PRSummary{prisma}
	clock = clock.Add(2 * time.Minute)
	poll()
	wantCalls("open PR left the set", 2)

	clock = clock.Add(closedHistoryMaxAge - time.Second)
	poll()
	wantCalls("just under max age", 2)

	clock = clock.Add(time.Second)
	poll()
	wantCalls("max age reached", 3)

	mock.closedErr = errors.New("gh: HTTP 502")
	mock.prs = []*bossanovav1.PRSummary{lodash, prisma}
	tasks = poll()
	wantCalls("open set changed, fetch fails", 4)
	if !hasNotifyFor(tasks, prisma.GetNumber()) {
		t.Fatalf("failed-refetch poll tasks = %v, want the last cached history to still flag prisma as rejected", tasks)
	}
	mock.closedErr = nil
	poll()
	wantCalls("failed fetch was not cached", 5)
	poll()
	wantCalls("successful refetch is cached", 5)
}

func hasNotifyFor(tasks []*bossanovav1.TaskItem, prNumber int32) bool {
	for _, task := range tasks {
		if task.GetAction() == bossanovav1.TaskAction_TASK_ACTION_NOTIFY_USER && strings.HasSuffix(task.GetExternalId(), fmt.Sprintf(":%d", prNumber)) {
			return true
		}
	}
	return false
}
