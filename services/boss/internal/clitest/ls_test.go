package clitest_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
)

func TestCLI_Ls_Empty(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	res := h.Run("ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "No sessions found") {
		t.Errorf("expected 'No sessions found' message, got: %q", res.Stdout)
	}
}

func TestCLI_Ls_Default(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)
	res := h.Run("ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Assert each session's ID prefix and title appear in the output. Avoids
	// depending on full-string table formatting which may change harmlessly.
	for _, want := range []string{
		"sess-aaa", "Add dark mode",
		"sess-bbb", "Fix login bug",
		"sess-ccc", "Update auth",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q\n---\n%s", want, res.Stdout)
		}
	}
}

func TestCLI_Ls_FilterByRepo(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)
	res := h.Run("ls", "--repo", "repo-2")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	// repo-2 only has sess-ccc.
	if !strings.Contains(res.Stdout, "sess-ccc") {
		t.Errorf("expected sess-ccc in output, got: %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "sess-aaa") || strings.Contains(res.Stdout, "sess-bbb") {
		t.Errorf("unexpected repo-1 session in output: %q", res.Stdout)
	}
}

func TestCLI_Ls_FilterByState(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(testSessions()...),
	)
	res := h.Run("ls", "--state", "READY_FOR_REVIEW")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "sess-bbb") {
		t.Errorf("expected sess-bbb (READY_FOR_REVIEW) in output, got: %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "sess-aaa") {
		t.Errorf("unexpected IMPLEMENTING_PLAN session in output: %q", res.Stdout)
	}
}

func TestCLI_Ls_Archived(t *testing.T) {
	h := clitest.New(t,
		clitest.WithRepos(testRepos()...),
		clitest.WithSessions(append(testSessions(), archivedSession())...),
	)

	// Without --archived: archived session hidden.
	res := h.Run("ls")
	if strings.Contains(res.Stdout, "sess-zzz") {
		t.Errorf("archived session unexpectedly shown without --archived: %q", res.Stdout)
	}

	// With --archived: included.
	res = h.Run("ls", "--archived")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "sess-zzz") {
		t.Errorf("expected archived sess-zzz in output with --archived, got: %q", res.Stdout)
	}
}

// TestCLI_LS_HelpCarriesActivityCaveat proves the caveat reaches the surface a
// CLI caller actually reads. The same warning already lives in the MCP
// get_chat_statuses tool description; before this it existed on the CLI side
// only as a source comment, which is the failure this ticket names — a
// documented-but-unreadable caveat is not documented.
func TestCLI_LS_HelpCarriesActivityCaveat(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("ls", "--help")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	out := res.Stdout + res.Stderr
	for _, want := range []string{
		"last_agent_activity_at",
		"spinner_present",
		"last_substantive_output_at",
		"last_output_seeded",
		"boss chats --json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`boss ls --help` is missing %q; got:\n%s", want, out)
		}
	}
}
