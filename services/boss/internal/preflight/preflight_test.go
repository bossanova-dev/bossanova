package preflight

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCheckShellTools_AllPresent verifies that on a normal dev/CI host
// (where bash and tee are both on PATH) the check returns nil — the
// blocking preflight screen would otherwise fire on every boss launch.
func TestCheckShellTools_AllPresent(t *testing.T) {
	if issue := CheckShellTools(); issue != nil {
		t.Fatalf("CheckShellTools returned issue on normal host: title=%q detail=%q",
			issue.Title, issue.Detail)
	}
}

// TestCheckShellTools_BothMissing simulates a system without bash or tee
// by emptying PATH for the duration of the test. The check must report
// both tools and recommend the matching install command.
func TestCheckShellTools_BothMissing(t *testing.T) {
	t.Setenv("PATH", "")
	issue := CheckShellTools()
	if issue == nil {
		t.Fatal("expected issue when PATH is empty; got nil")
	}
	if !strings.Contains(issue.Title, "bash") || !strings.Contains(issue.Title, "tee") {
		t.Errorf("title should mention both missing tools; got %q", issue.Title)
	}
	if !strings.Contains(issue.Detail, "tee") {
		t.Errorf("detail should reference tee; got %q", issue.Detail)
	}
}

// TestCheckShellTools_SingleMissing pins the exact boundary at the
// `len(missing) > 1` branch (preflight.go:51). With exactly one tool
// missing (len(missing) == 1) the title must be the single-tool form
// ("tee is not installed"), not the combined "bash and tee are not
// installed". A boundary mutant that flips `> 1` to `>= 1` would emit
// the combined title here, so this case fails against the mutant and
// passes against the real code.
func TestCheckShellTools_SingleMissing(t *testing.T) {
	// Build a PATH that contains bash but not tee so exactly one tool
	// (tee) is reported missing.
	dir := t.TempDir()
	bash := filepath.Join(dir, "bash")
	if err := os.WriteFile(bash, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing fake bash: %v", err)
	}
	t.Setenv("PATH", dir)

	issue := CheckShellTools()
	if issue == nil {
		t.Fatal("expected issue when tee is missing; got nil")
	}
	if issue.Title != "tee is not installed" {
		t.Errorf("title should be single-tool form %q; got %q",
			"tee is not installed", issue.Title)
	}
	if strings.Contains(issue.Title, "bash and tee") {
		t.Errorf("title must not use combined form when only one tool is missing; got %q", issue.Title)
	}
}

func TestCheckAgentResolvable_BlocksWhenMissing(t *testing.T) {
	issue := checkAgentResolvable("/bin/sh", "definitely-not-a-real-agent-xyz", t.TempDir(), runShell)
	if issue == nil {
		t.Fatalf("expected a blocking issue for a missing agent")
	}
	if !strings.Contains(issue.Detail, "definitely-not-a-real-agent-xyz") {
		t.Fatalf("issue must name the agent: %q", issue.Detail)
	}
}

func TestCheckAgentResolvable_PassesWhenPresent(t *testing.T) {
	if issue := checkAgentResolvable("/bin/sh", "sh", t.TempDir(), runShell); issue != nil {
		t.Fatalf("sh should resolve, got: %+v", issue)
	}
}

func TestCheckAgentResolvable_BlocksInvalidAgentNameBeforeShell(t *testing.T) {
	called := false
	issue := checkAgentResolvable("/bin/sh", "bad;touch", t.TempDir(), func(string, string, string) error {
		called = true
		return nil
	})
	if issue == nil {
		t.Fatalf("expected a blocking issue for an invalid agent name")
	}
	if called {
		t.Fatal("runner should not be invoked for an invalid agent name")
	}
	if !strings.Contains(issue.Title, "bad;touch") || !strings.Contains(issue.Detail, "bad;touch") {
		t.Fatalf("issue must name the invalid agent: title=%q detail=%q", issue.Title, issue.Detail)
	}
}

func TestCheckAgentResolvable_SkipsUnsupportedLoginShell(t *testing.T) {
	called := false
	issue := checkAgentResolvable("/bin/tcsh", "codex", t.TempDir(), func(string, string, string) error {
		called = true
		t.Fatal("runner should not be invoked for an unsupported login shell")
		return nil
	})

	if issue != nil {
		t.Fatalf("unsupported login shell should not block preflight, got: %#v", issue)
	}
	if called {
		t.Fatal("runner was invoked for an unsupported login shell")
	}
}

func TestDaemonIssueRemediationUsesStaticDaemonCommands(t *testing.T) {
	issue := DaemonIssue(errors.New("dial unix: connection refused"))
	for _, want := range []string{"boss daemon restart", "boss daemon status", "boss daemon install", "bossd"} {
		if !strings.Contains(issue.Detail, want) {
			t.Fatalf("DaemonIssue detail missing %q: %s", want, issue.Detail)
		}
	}
}

func TestRunShellWithTimeoutReturnsErrorForHangingShell(t *testing.T) {
	err := runShellWithTimeout("/bin/sh", t.TempDir(), "while true; do sleep 1; done", 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error for hanging shell command")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error should be explicit, got: %v", err)
	}
}

// TestCheckTerminalOKWhenTermResolves verifies the common case: $TERM has a
// terminfo entry, so no issue is reported.
func TestCheckTerminalOKWhenTermResolves(t *testing.T) {
	if got := checkTerminal("xterm-ghostty", func(term string) bool { return true }); got != nil {
		t.Fatalf("want nil issue, got %+v", got)
	}
}

// TestCheckTerminalOKWhenFallbackResolves covers the common ghostty-missing
// case: $TERM has no terminfo entry, but xterm-256color does, so the CLI's
// auto-fallback (Task 2) covers it and this must not block.
func TestCheckTerminalOKWhenFallbackResolves(t *testing.T) {
	// ghostty missing but xterm-256color present → auto-fallback covers it → no block.
	probe := func(term string) bool { return term == "xterm-256color" }
	if got := checkTerminal("xterm-ghostty", probe); got != nil {
		t.Fatalf("want nil issue (fallback available), got %+v", got)
	}
}

// TestCheckTerminalIssueWhenNothingResolves covers the rare truly-broken box
// where neither $TERM nor the xterm-256color fallback resolves.
func TestCheckTerminalIssueWhenNothingResolves(t *testing.T) {
	probe := func(term string) bool { return false }
	got := checkTerminal("xterm-ghostty", probe)
	if got == nil {
		t.Fatal("want an Issue when no terminal resolves")
	}
	if !strings.Contains(got.Detail, "xterm-256color") {
		t.Fatalf("Issue.Detail should mention the fallback remediation; got %q", got.Detail)
	}
}
