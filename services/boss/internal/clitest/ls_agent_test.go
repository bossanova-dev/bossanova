package clitest_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// agentTestSessions returns two sessions with explicit AgentName values used
// by the ls session-list tests. The session list intentionally omits an AGENT
// column because a session may have multiple chats with different agents.
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
		t.Errorf("AGENT column should be hidden from session list; got:\n%s", res.Stdout)
	}
	// Sanity-check that other expected columns are present.
	for _, want := range []string{"ID", "TITLE", "STATE", "BRANCH", "PR"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("expected column %q in output; got:\n%s", want, res.Stdout)
		}
	}
}

func TestCLI_Ls_AgentColumnHidden_WhenSessionDeviates(t *testing.T) {
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
	if strings.Contains(res.Stdout, "AGENT") {
		t.Errorf("AGENT column should be hidden from session list; got:\n%s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "opencode") {
		t.Errorf("session list should not render session agent values; got:\n%s", res.Stdout)
	}
}

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
		t.Errorf("AGENT column should be hidden from session list; got:\n%s", res.Stdout)
	}
}
