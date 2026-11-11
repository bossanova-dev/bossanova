package main

import (
	"context"
	"fmt"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestBotRebaseClassification_Sessions_AC248203(t *testing.T) {
	for _, tc := range []struct {
		name     string
		authors  []*pb.PRCommitAuthor
		complete bool
		head     string
		want     bool
	}{
		{"login", []*pb.PRCommitAuthor{{Login: "dependabot[bot]"}}, true, "abc", true},
		{"email", []*pb.PRCommitAuthor{{Email: "49699333+dependabot[bot]@users.noreply.github.com"}}, true, "abc", true},
		{"human", []*pb.PRCommitAuthor{{Login: "dependabot[bot]"}, {Login: "human"}}, true, "abc", false},
		{"incomplete", []*pb.PRCommitAuthor{{Login: "dependabot[bot]"}}, false, "abc", false},
		{"empty", nil, true, "abc", false},
		{"no head", []*pb.PRCommitAuthor{{Login: "dependabot[bot]"}}, true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := &pb.PRSummary{Number: 42, Title: "Bump lodash", HeadSha: tc.head, IsConflicted: true, HasAllCommitAuthors: tc.complete, CommitAuthors: tc.authors}
			host := &mockHostService{status: map[int32]*pb.PRStatus{42: {Mergeable: boolPtr(false)}}, checks: map[int32][]*pb.CheckResult{42: {{Status: pb.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: conclusionPtr(pb.CheckConclusion_CHECK_CONCLUSION_SUCCESS)}}}}
			task, err := newTestServer(host).classifyPR(context.Background(), "https://github.com/org/repo", "org/repo", pr)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want {
				if task == nil || task.GetAction() != pb.TaskAction_TASK_ACTION_REQUEST_REBASE || task.GetExternalId() != "dependabot:rebase:https://github.com/org/repo:abc:42" {
					t.Fatalf("task = %v", task)
				}
			} else if task != nil {
				t.Fatalf("unsafe task = %v", task)
			}
			host.status[42].Mergeable = boolPtr(true)
			pr.IsConflicted = false
			task, err = newTestServer(host).classifyPR(context.Background(), "https://github.com/org/repo", "org/repo", pr)
			if err != nil || task == nil || task.GetAction() != pb.TaskAction_TASK_ACTION_AUTO_MERGE || task.GetIsBotRebaseRequestable() != tc.want {
				t.Fatalf("green task=%v err=%v", task, err)
			}
		})
	}
}

func TestRebaseClassificationAvoidsStatusFallback_Sessions_AC248204(t *testing.T) {
	for _, snapshot := range []string{"UNKNOWN mergeability", "truncated reviews", "conclusive conflict"} {
		for _, failing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failing=%t", snapshot, failing), func(t *testing.T) {
				check := &pb.CheckResult{Status: pb.CheckStatus_CHECK_STATUS_IN_PROGRESS}
				if failing {
					check.Status = pb.CheckStatus_CHECK_STATUS_COMPLETED
					check.Conclusion = conclusionPtr(pb.CheckConclusion_CHECK_CONCLUSION_FAILURE)
				}
				pr := &pb.PRSummary{Number: 42, HeadSha: "abc", HasAllCommitAuthors: true, CommitAuthors: []*pb.PRCommitAuthor{{Login: "dependabot[bot]"}}, IsConflicted: snapshot == "conclusive conflict"}
				host := &mockHostService{checks: map[int32][]*pb.CheckResult{42: {check}}}
				task, err := newTestServer(host).classifyPR(t.Context(), "https://github.com/org/repo", "org/repo", pr)
				if err != nil {
					t.Fatal(err)
				}
				if host.statusCalls != 0 {
					t.Fatalf("status calls=%d; pending/failing classification must not trigger per-PR status fallbacks", host.statusCalls)
				}
				switch {
				case pr.IsConflicted:
					if task == nil || task.GetAction() != pb.TaskAction_TASK_ACTION_REQUEST_REBASE {
						t.Fatalf("conflicted task=%v", task)
					}
				case failing:
					if task == nil || task.GetAction() != pb.TaskAction_TASK_ACTION_CREATE_SESSION {
						t.Fatalf("failing task=%v", task)
					}
				default:
					if task != nil {
						t.Fatalf("pending task=%v", task)
					}
				}
			})
		}
	}
}
