package clitest_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// agentTestSessions returns two sessions with explicit AgentName values used
// by the ls AGENT-column tests. The agent values are set per-test by mutating
// the returned slice in place.
func agentTestSessions(agents ...string) []*pb.Session {
	base := []*pb.Session{
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
	}
	for i, a := range agents {
		if i >= len(base) {
			break
		}
		base[i].AgentName = a
	}
	return base
}

// TestCLI_Ls_AgentColumnHidden_WhenAllMatchDefault verifies that the ls table
// keeps its compact 5-column shape when every session's agent matches the
// user's Settings.DefaultAgent. We assert on the column header rather than
// the row values because "claude" can legitimately appear in row data
// (e.g. inside a branch name) — only the AGENT header proves the column
// itself was rendered.
func TestCLI_Ls_AgentColumnHidden_WhenAllMatchDefault(t *testing.T) {
	home := t.TempDir()
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(agentTestSessions("claude", "claude")...),
		clitest.WithEnv("HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")),
	)
	res := h.Run("ls")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stdout, "AGENT") {
		t.Errorf("AGENT column should be hidden when all sessions match default agent; got:\n%s", res.Stdout)
	}
	// Sanity-check that other expected columns are present.
	for _, want := range []string{"ID", "TITLE", "STATE", "BRANCH", "PR"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("expected column %q in output; got:\n%s", want, res.Stdout)
		}
	}
}

// TestCLI_Ls_AgentColumnShown_WhenSessionDeviates verifies that the AGENT
// column appears as soon as at least one session uses a non-default agent.
// We seed the daemon with two sessions — one "claude", one "opencode" — and
// rely on the default fallback (DefaultAgent="claude" when settings are absent
// or the file omits the field) so the deviation triggers the column.
func TestCLI_Ls_AgentColumnShown_WhenSessionDeviates(t *testing.T) {
	home := t.TempDir()
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(agentTestSessions("claude", "opencode")...),
		clitest.WithEnv("HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")),
	)
	res := h.Run("ls")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "AGENT") {
		t.Errorf("expected AGENT column header in output; got:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "opencode") {
		t.Errorf("expected non-default agent value 'opencode' in output; got:\n%s", res.Stdout)
	}
}

// TestCLI_Ls_AgentColumnRespectsCustomDefault verifies that the column hides
// when DefaultAgent is set to something other than "claude" but every session
// matches that custom default. This guards against a regression where the
// hide/show logic accidentally hard-codes "claude".
func TestCLI_Ls_AgentColumnRespectsCustomDefault(t *testing.T) {
	home := t.TempDir()
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(agentTestSessions("opencode", "opencode")...),
		clitest.WithEnv("HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")),
	)

	// Set DefaultAgent=opencode via the settings command so the on-disk
	// settings.json has the user's preferred default.
	if res := h.Run("settings", "--default-agent", "opencode"); res.ExitCode != 0 {
		t.Fatalf("settings --default-agent: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	res := h.Run("ls")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stdout, "AGENT") {
		t.Errorf("AGENT column should be hidden when all sessions match custom DefaultAgent=opencode; got:\n%s", res.Stdout)
	}
}
