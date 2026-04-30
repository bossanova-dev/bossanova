package tuitest_test

import (
	"os"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const waitTimeout = 10 * time.Second

func TestMain(m *testing.M) {
	cleanup := tuitest.BuildBoss()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func testRepos() []*pb.Repo {
	return []*pb.Repo{
		{Id: "repo-1", DisplayName: "my-app", LocalPath: "/tmp/my-app", DefaultBaseBranch: "main", MergeStrategy: "merge"},
	}
}

func testMultiRepos() []*pb.Repo {
	return []*pb.Repo{
		{Id: "repo-1", DisplayName: "my-app", LocalPath: "/tmp/my-app", DefaultBaseBranch: "main", MergeStrategy: "merge"},
		{Id: "repo-2", DisplayName: "my-api", LocalPath: "/tmp/my-api", DefaultBaseBranch: "main", MergeStrategy: "squash"},
	}
}

func testSessions() []*pb.Session {
	return []*pb.Session{
		{
			Id:              "sess-aaa-111",
			RepoId:          "repo-1",
			RepoDisplayName: "my-app",
			Title:           "Add dark mode",
			BranchName:      "boss/add-dark-mode",
			State:           pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		},
		{
			Id:              "sess-bbb-222",
			RepoId:          "repo-1",
			RepoDisplayName: "my-app",
			Title:           "Fix login bug",
			BranchName:      "boss/fix-login-bug",
			State:           pb.SessionState_SESSION_STATE_AWAITING_CHECKS,
		},
	}
}

func testChats() []*pb.ClaudeChat {
	now := time.Now()
	return []*pb.ClaudeChat{
		{
			ClaudeId:  "claude-111",
			SessionId: "sess-aaa-111",
			Title:     "Initial implementation",
			CreatedAt: timestamppb.New(now), // most recent → sorted first by TUI
		},
		{
			ClaudeId:  "claude-222",
			SessionId: "sess-aaa-111",
			Title:     "Follow-up review",
			CreatedAt: timestamppb.New(now.Add(-time.Hour)), // older → sorted second
		},
	}
}

func testPRs() []*pb.PRSummary { //nolint:unused // available for future tests
	return []*pb.PRSummary{
		{
			Number:     42,
			Title:      "Add dark mode support",
			HeadBranch: "boss/add-dark-mode",
			State:      pb.PRState_PR_STATE_OPEN,
			Author:     "dave",
		},
	}
}
