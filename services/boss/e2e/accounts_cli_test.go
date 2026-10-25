package e2e_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestLayout_Account_Ls_FullIDAndAlignedColumns(t *testing.T) {
	accounts := []*pb.AgentAccount{
		{
			Id:       "8078890d6b0affc7",
			Provider: "claude",
			Label:    "agent.yuki",
			Status:   "active",
		},
		{
			Id:       "6aaff35db711eee5",
			Provider: "codex",
			Label:    "dave@kamik.ai",
			Status:   "active",
		},
	}
	h := clitest.New(t, clitest.WithAgentAccounts(accounts...))
	res := h.Run("account", "ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header plus 2 account rows, got %d lines:\n%s", len(lines), res.Stdout)
	}
	var yukiRow string
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == h.AccountIDs["8078890d6b0affc7"] {
			yukiRow = line
		}
	}
	if yukiRow == "" {
		t.Fatalf("full account id not found; stdout:\n%s", res.Stdout)
	}
	if !strings.Contains(yukiRow, "agent.yuki     active") {
		t.Fatalf("agent.yuki row does not align label before status:\n%s", res.Stdout)
	}
}

func TestJourney_Accounts_AC1_Account_Ls_Empty(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("account", "ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "No accounts") {
		t.Errorf("expected empty-state message, got %q", res.Stdout)
	}
}
