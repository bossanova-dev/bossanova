package clitest_test

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func prHarness(t *testing.T, opts ...clitest.Option) *clitest.Harness {
	t.Helper()
	pr := int32(42)
	sess := &pb.Session{
		Id:            "sess-pr-42",
		RepoId:        "repo-1",
		Title:         "Parked change",
		PrNumber:      &pr,
		RepoOriginUrl: "https://github.com/acme/app",
		WorktreePath:  "/nonexistent/on/this/host",
	}
	return clitest.New(t, append([]clitest.Option{
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(append(testSessions(), sess)...),
	}, opts...)...)
}

// TestCLI_PrApprove_RunsOnTheDaemon proves approve reaches the daemon by session
// id: the worktree path does not exist on this host, which the old local
// toolbox run could never have tolerated.
func TestCLI_PrApprove_RunsOnTheDaemon(t *testing.T) {
	h := prHarness(t)
	h.Daemon.SetApproveResult(&pb.VerifyApproveResult{IsApproved: true, Waives: "ledger-open", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: "ok"})

	res := h.Run("pr", "approve", "42")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if want := "Approved #42: waived ledger-open on head abcdef01 (record: https://x/9). Verify will re-check and merge.\n" +
		"  ledger-open: review left must-fix findings open\n" +
		"Approving overrode ledger-open (review left must-fix findings open); if that was not intended, boss pr send-back 42 \"<note>\" revokes the approval and sends the head back.\n"; res.Stdout != want {
		t.Fatalf("stdout = %q, want %q", res.Stdout, want)
	}
	calls := h.Daemon.ApproveSessionPRCalls()
	if len(calls) != 1 || calls[0].GetId() != "sess-pr-42" || calls[0].GetIsDryRun() {
		t.Fatalf("ApproveSessionPRCalls = %v", calls)
	}
}

func TestCLI_PrApprove_DaemonTimeoutSaysReRun(t *testing.T) {
	h := prHarness(t)
	h.Daemon.SetVerifyError(connect.CodeDeadlineExceeded, "verify-gate timed out")

	res := h.Run("pr", "approve", "42")
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "re-run approve to confirm") {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
}

func TestCLI_PrVerbs_RefuseInsideAnAgentChat(t *testing.T) {
	h := prHarness(t, clitest.WithEnv("BOSS_AGENT_SESSION_ID=chat-xyz"))
	h.Daemon.SetApproveResult(&pb.VerifyApproveResult{IsApproved: true})

	for _, args := range [][]string{{"pr", "approve", "42"}, {"pr", "send-back", "42", "fix it"}} {
		res := h.Run(args...)
		if res.ExitCode == 0 || !strings.Contains(res.Stderr, "your own terminal") {
			t.Fatalf("%v: exit=%d stderr=%q", args, res.ExitCode, res.Stderr)
		}
	}
	if n := len(h.Daemon.ApproveSessionPRCalls()) + len(h.Daemon.SendBackSessionPRCalls()); n != 0 {
		t.Fatalf("an agent chat reached the daemon's verify verbs %d times", n)
	}
}
