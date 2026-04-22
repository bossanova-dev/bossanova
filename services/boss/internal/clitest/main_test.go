package clitest_test

import (
	"os"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func timestampDaysAgo(days int) time.Time {
	return time.Now().Add(-time.Duration(days) * 24 * time.Hour)
}

func TestMain(m *testing.M) {
	cleanup := tuitest.BuildBoss()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func testRepos() []*pb.Repo {
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
			State:           pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		},
		{
			Id:              "sess-ccc-333",
			RepoId:          "repo-2",
			RepoDisplayName: "my-api",
			Title:           "Update auth",
			BranchName:      "boss/update-auth",
			State:           pb.SessionState_SESSION_STATE_AWAITING_CHECKS,
		},
	}
}

func archivedSession() *pb.Session {
	return &pb.Session{
		Id:              "sess-zzz-999",
		RepoId:          "repo-1",
		RepoDisplayName: "my-app",
		Title:           "Old cleanup",
		BranchName:      "boss/old-cleanup",
		State:           pb.SessionState_SESSION_STATE_CLOSED,
		ArchivedAt:      timestamppb.Now(),
	}
}
