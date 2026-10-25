package e2e_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
)

func TestJourney_Settings_AC1_Settings_Show(t *testing.T) {
	h := clitest.New(t)
	worktree := t.TempDir()
	if configured := h.Run("settings", "--worktree-dir", worktree, "--default-agent", "stub", "--no-managed-accounts"); !configured.Success() {
		t.Fatalf("configure settings: %+v", configured)
	}
	res := h.Run("settings")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"Worktree dir:", worktree, "Default agent:    stub", "Managed accounts: false"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q", want)
		}
	}
}
