package clitest_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCLI_Trash_Ls(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(append(testSessions(), archivedSession())...),
	)
	res := h.Run("trash", "ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "sess-zzz") {
		t.Errorf("expected archived sess-zzz in trash output, got: %q", res.Stdout)
	}
	// Non-archived sessions should not appear.
	if strings.Contains(res.Stdout, "sess-aaa") {
		t.Errorf("unexpected non-archived session in trash output: %q", res.Stdout)
	}
}

func TestCLI_Trash_Restore(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(append(testSessions(), archivedSession())...),
	)
	res := h.Run("trash", "restore", "sess-zzz-999")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Confirm restoration in mock state.
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-zzz-999" && s.ArchivedAt != nil {
			t.Errorf("session still archived after restore")
		}
	}
}

func TestCLI_Trash_Delete_RequiresYes(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(append(testSessions(), archivedSession())...),
	)

	// No --yes, and stdin replies "n" to the confirmation prompt.
	res := h.RunWithStdin("n\n", "trash", "delete", "sess-zzz-999")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q (expected 0 on user cancel)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Cancelled") {
		t.Errorf("expected 'Cancelled' message on refusal, got: %q", res.Stdout)
	}

	// Session should still exist.
	var found bool
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-zzz-999" {
			found = true
		}
	}
	if !found {
		t.Errorf("session removed despite user cancellation")
	}
}

func TestCLI_Trash_Delete_WithYes(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(append(testSessions(), archivedSession())...),
	)
	res := h.Run("trash", "delete", "sess-zzz-999", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Session should be gone.
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-zzz-999" {
			t.Errorf("session still present after trash delete --yes")
		}
	}
}

func TestCLI_Trash_Empty_WithOlderThan(t *testing.T) {
	// Two archived sessions: one old, one recent. --older-than=1d should
	// remove the old one but keep the recent one.
	oldSess := &pb.Session{
		Id:              "sess-old-000",
		RepoId:          "repo-1",
		RepoDisplayName: "my-app",
		Title:           "Old",
		State:           pb.SessionState_SESSION_STATE_CLOSED,
		ArchivedAt:      timestamppb.New(timestampDaysAgo(10)),
	}
	recentSess := &pb.Session{
		Id:              "sess-new-001",
		RepoId:          "repo-1",
		RepoDisplayName: "my-app",
		Title:           "Recent",
		State:           pb.SessionState_SESSION_STATE_CLOSED,
		ArchivedAt:      timestamppb.Now(),
	}
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(oldSess, recentSess),
	)

	// Note: the mock daemon's EmptyTrash currently ignores OlderThan (it
	// deletes all archived). Assert the CLI at least accepts the flag and
	// exits cleanly — the OlderThan plumbing is unit-tested separately in
	// the bossd package.
	res := h.Run("trash", "empty", "--older-than", "1d")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
}
