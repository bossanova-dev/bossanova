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
	if want := "Approved #42 on head abcdef01 (record: https://x/9). It waives:\n" +
		"  The PR checklist has open items: unmet acceptance criteria, open follow-ups or questions, a do-not-merge marker, or an unreadable section\n" +
		"This overrode a blocking issue. A new push is judged afresh.\n"; res.Stdout != want {
		t.Fatalf("stdout = %q, want %q", res.Stdout, want)
	}
	calls := h.Daemon.ApproveSessionPRCalls()
	if len(calls) != 1 || calls[0].GetId() != "sess-pr-42" || calls[0].GetIsDryRun() {
		t.Fatalf("ApproveSessionPRCalls = %v", calls)
	}
}

func TestCLI_PrApprove_DaemonTimeoutSaysAnApprovalMayBePosted(t *testing.T) {
	h := prHarness(t)
	h.Daemon.SetVerifyError(connect.CodeDeadlineExceeded, "verify-gate timed out")

	res := h.Run("pr", "approve", "42")
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "an approval may already be posted") || strings.Contains(res.Stderr, "re-run") {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
}

func TestCLI_PrVerbs_RefuseInsideAnAgentChat(t *testing.T) {
	h := prHarness(t, clitest.WithEnv("BOSS_AGENT_SESSION_ID=chat-xyz"))
	h.Daemon.SetApproveResult(&pb.VerifyApproveResult{IsApproved: true})

	res := h.Run("pr", "approve", "42")
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "your own terminal") {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if n := len(h.Daemon.ApproveSessionPRCalls()); n != 0 {
		t.Fatalf("an agent chat reached the daemon's approve verb %d times", n)
	}
}

// TestCLI_PrSendBack_IsGone proves the retired verb no longer runs and reaches
// no daemon RPC.
func TestCLI_PrSendBack_IsGone(t *testing.T) {
	h := prHarness(t)
	res := h.Run("pr", "send-back", "42", "fix it")
	if strings.Contains(res.Stdout+res.Stderr, "send-back") || strings.Contains(res.Stdout+res.Stderr, "Sent #42 back") {
		t.Fatalf("boss pr still knows send-back: stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	if n := len(h.Daemon.ApproveSessionPRCalls()) + len(h.Daemon.OperatorMergeCalls()); n != 0 {
		t.Fatalf("a removed verb reached the daemon %d times", n)
	}
}
