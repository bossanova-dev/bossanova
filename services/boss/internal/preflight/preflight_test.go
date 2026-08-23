package preflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// wantIssue describes the Issue a checkAgentResolvable case must produce.
// A nil *wantIssue means "no issue"; an empty Title/Detail pair is not
// expressible, which is deliberate — every issue this check emits is
// user-facing text worth pinning.
type wantIssue struct {
	title  string
	detail string
}

// TestCheckAgentResolvableBranchesOnProbeOutcome is the BOS-976 seam test: the
// injected run seam drives every outcome the probe can produce, and each one is
// pinned on its RENDERED Title/Detail rather than on an internal branch flag.
// That is the point of the table — if the timeout and not-found messages are
// ever collapsed back into one Issue, two rows here disagree with each other and
// the test fails, which a branch-flag assertion would not catch.
//
// The not-found row's expectation is a literal, byte-for-byte copy of today's
// message rather than a re-derivation through agentNotFoundIssue, so a drift in
// that path fails here instead of silently agreeing with itself.
func TestCheckAgentResolvableBranchesOnProbeOutcome(t *testing.T) {
	const (
		shell    = "/bin/sh"
		agent    = "claude"
		worktree = "/tmp/bos976-worktree"
	)

	notFoundDetail := "Boss launches claude through your login shell (/bin/sh) in the worktree, " +
		"but it could not be resolved there. If you use a per-project version manager " +
		"(nodenv/asdf/mise) or a project-local install, make sure `claude` works when " +
		"you run it in:\n\n    /tmp/bos976-worktree\n\n" +
		`(checked: /bin/sh -l -c "command -v claude")`

	tests := []struct {
		name      string
		shell     string
		agent     string
		runErr    error
		wantRun   bool
		want      *wantIssue
		wantMatch []string // substrings the Detail must contain (timeout row)
	}{
		{
			name:  "timeout is reported as a timeout, not as a missing agent",
			shell: shell,
			agent: agent,
			// runShellWithTimeout wraps the sentinel with %w, so the call site sees
			// exactly this shape in production.
			runErr:  fmt.Errorf("shell command timed out after %s: %w", AgentResolveTimeout, context.DeadlineExceeded),
			wantRun: true,
			wantMatch: []string{
				"timed out",
				AgentResolveTimeout.String(),
				shell,
				agent,
			},
		},
		{
			name:    "a genuine non-zero command -v still reports not found",
			shell:   shell,
			agent:   agent,
			runErr:  errors.New("exit status 1"),
			wantRun: true,
			want: &wantIssue{
				title:  "claude was not found for this project",
				detail: notFoundDetail,
			},
		},
		{
			name:    "a resolving agent produces no issue",
			shell:   shell,
			agent:   agent,
			runErr:  nil,
			wantRun: true,
		},
		{
			name:    "an unsupported login shell is not probed at all",
			shell:   "/bin/tcsh",
			agent:   agent,
			runErr:  errors.New("must not be called"),
			wantRun: false,
		},
		{
			name:    "an unsafe agent name is rejected before the shell runs",
			shell:   shell,
			agent:   "bad;touch",
			runErr:  errors.New("must not be called"),
			wantRun: false,
			want: &wantIssue{
				title: "bad;touch is not a valid agent command",
				detail: "Boss cannot check the enabled agent provider \"bad;touch\" because its " +
					"command name is not safe to pass to shell preflight. Use only letters, " +
					"numbers, dot, underscore, and hyphen, starting with a letter or number.",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			got := checkAgentResolvable(tc.shell, tc.agent, worktree, func(string, string, string) error {
				called = true
				return tc.runErr
			})
			if called != tc.wantRun {
				t.Fatalf("probe invoked = %v, want %v", called, tc.wantRun)
			}
			switch {
			case tc.want != nil:
				if got == nil {
					t.Fatalf("want issue %q, got nil", tc.want.title)
				}
				if got.Title != tc.want.title {
					t.Errorf("Title = %q, want %q", got.Title, tc.want.title)
				}
				if got.Detail != tc.want.detail {
					t.Errorf("Detail =\n%q\nwant\n%q", got.Detail, tc.want.detail)
				}
			case len(tc.wantMatch) > 0:
				if got == nil {
					t.Fatal("want a timeout issue, got nil")
				}
				for _, want := range tc.wantMatch {
					if !strings.Contains(got.Title+"\n"+got.Detail, want) {
						t.Errorf("issue must mention %q: title=%q detail=%q", want, got.Title, got.Detail)
					}
				}
				// The two messages must stay distinguishable on BOTH fields — the
				// screen shows the Title in bold above the Detail, so a shared
				// Title would read as "not found" no matter what the body says.
				notFound := agentNotFoundIssue(tc.shell, tc.agent, worktree)
				if got.Title == notFound.Title {
					t.Errorf("timeout Title must differ from the not-found Title; both are %q", got.Title)
				}
				if got.Detail == notFound.Detail {
					t.Error("timeout Detail must differ from the not-found Detail")
				}
			default:
				if got != nil {
					t.Fatalf("want no issue, got %+v", got)
				}
			}
		})
	}
}

// TestAgentProbeTimedOutIssueNamesTheEffectiveTimeout pins that the message
// renders the timeout it was HANDED rather than a hardcoded literal. The odd
// duration is the point: it cannot coincide with AgentResolveTimeout, so a
// message that spelled the budget out in prose would fail here even while the
// production constant happened to agree with it.
func TestAgentProbeTimedOutIssueNamesTheEffectiveTimeout(t *testing.T) {
	issue := agentProbeTimedOutIssue("/bin/zsh", "codex", "/tmp/wt", 37*time.Second)
	if issue == nil {
		t.Fatal("agentProbeTimedOutIssue returned nil")
	}
	body := issue.Title + "\n" + issue.Detail
	if !strings.Contains(body, "37s") {
		t.Errorf("issue must name the effective timeout 37s: %q", body)
	}
	if strings.Contains(body, AgentResolveTimeout.String()) {
		t.Errorf("issue must not hardcode the default budget %s: %q", AgentResolveTimeout, body)
	}
	if !strings.Contains(issue.Detail, "/bin/zsh") {
		t.Errorf("issue Detail must name the login shell: %q", issue.Detail)
	}
	// Detail-only, deliberately. The acceptance criterion is that the DETAIL
	// names the elapsed budget, and the Title already contains it — so asserting
	// over Title+Detail (as the table row above does, where distinguishing the
	// two messages is the point) would stay green if the duration were dropped
	// from the body. This is the assertion that would actually fail.
	if !strings.Contains(issue.Detail, "37s") {
		t.Errorf("issue Detail must name the effective timeout 37s: %q", issue.Detail)
	}
}

// TestAgentResolveTimeoutBudget pins the probe budget itself. The 5s original
// was tighter than a healthy interactive rc that loads two version managers, so
// a slow-but-working shell was reported as a missing agent (BOS-976).
//
// It is also the value the slow-agent-probe proof scenario asserts on screen
// (`checking claude timed out after 20s`). That coupling is deliberate and it
// fails LOUDLY, not silently: changing the constant without updating the
// scenario turns the proof RED, because the scenario expects a literal the
// message no longer produces. This test is what names the constant as the thing
// to change, so the red proof is diagnosed rather than merely observed.
func TestAgentResolveTimeoutBudget(t *testing.T) {
	if AgentResolveTimeout != 20*time.Second {
		t.Fatalf("AgentResolveTimeout = %s, want 20s", AgentResolveTimeout)
	}
}

// TestClassifyProbeResultSucceedsAtTheDeadline is the BOS-976-inverse guard: a
// probe that ANSWERED just before its deadline expired must not be reported as a
// timeout. The old code read ctx.Err() without regard to the command's own
// status, so a shell that resolved the agent milliseconds before the budget ran
// out produced a blocking "checking claude timed out" screen for a user whose
// agent was there all along.
//
// Driven through the pure classifier rather than a real exec because that race
// window cannot be hit deterministically from the outside — the point is that
// the two inputs are read TOGETHER, and that is exactly what this pins.
func TestClassifyProbeResultSucceedsAtTheDeadline(t *testing.T) {
	if err := classifyProbeResult(nil, context.DeadlineExceeded, AgentResolveTimeout); err != nil {
		t.Fatalf("a successful probe must not be reported as a timeout; got %v", err)
	}
}

// TestClassifyProbeResultKeepsTheOtherThreeCorners covers the rest of the truth
// table, so the guard above cannot be satisfied by simply never reporting a
// timeout at all.
func TestClassifyProbeResultKeepsTheOtherThreeCorners(t *testing.T) {
	exit1 := errors.New("exit status 1")

	// Killed by the deadline: wrapped, so errors.Is finds the sentinel.
	err := classifyProbeResult(errors.New("signal: killed"), context.DeadlineExceeded, 20*time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a probe killed by the deadline must wrap DeadlineExceeded; got %v", err)
	}
	if !strings.Contains(err.Error(), "20s") {
		t.Errorf("timeout error should name the budget; got %v", err)
	}

	// Genuine non-zero exit inside budget: passed through untouched, so the
	// caller renders the not-found screen.
	if got := classifyProbeResult(exit1, nil, 20*time.Second); !errors.Is(got, exit1) {
		t.Errorf("a non-zero exit must pass through; got %v", got)
	}
	if errors.Is(classifyProbeResult(exit1, nil, 20*time.Second), context.DeadlineExceeded) {
		t.Error("a non-zero exit must not be reported as a timeout")
	}

	// Plain success.
	if got := classifyProbeResult(nil, nil, 20*time.Second); got != nil {
		t.Errorf("a clean probe must return nil; got %v", got)
	}
}

// TestRunShellWithTimeoutPassesASlowButSuccessfulProbe is the real-exec half of
// the guard above: a shell that takes its time and then answers cleanly, well
// inside its budget, must produce no error at all. Nothing is stubbed, so it
// also proves the success path never consults the deadline on its own.
func TestRunShellWithTimeoutPassesASlowButSuccessfulProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real shell that sleeps")
	}
	if err := runShellWithTimeout("/bin/sh", t.TempDir(), "sleep 0.2; command -v sh", 10*time.Second); err != nil {
		t.Fatalf("a slow but successful probe must not error; got %v", err)
	}
}

// TestRunShellWithTimeoutWrapsDeadlineExceeded is the probeRunner CONTRACT test.
// checkAgentResolvable's timeout branch fires on errors.Is(err,
// context.DeadlineExceeded) and on nothing else, and the table test above feeds
// it a HAND-BUILT error of that shape — so on its own that table only proves the
// test agrees with itself. This drives the production runner through a real exec
// and asserts the wrapping is genuinely there.
func TestRunShellWithTimeoutWrapsDeadlineExceeded(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real shell that hangs")
	}
	err := runShellWithTimeout("/bin/sh", t.TempDir(), "while true; do sleep 1; done", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error for a hanging shell")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a runner must WRAP context.DeadlineExceeded (probeRunner CONTRACT); got %#v", err)
	}
}

// TestCheckAgentResolvableReportsTimeoutThroughTheRealRunner closes the contract
// loop end to end: the production runner, a real login shell that will not
// answer, and the real classification. No error is hand-built anywhere, so a
// runner that stopped wrapping the sentinel fails HERE with the user-visible
// symptom — the not-found screen for a shell that is merely slow — instead of
// passing a table that supplies its own conforming error.
func TestCheckAgentResolvableReportsTimeoutThroughTheRealRunner(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real shell that hangs")
	}
	worktree := t.TempDir()
	// The seam receives the real `command -v <agent>` line and runs it behind a
	// blocking rc prologue — the shape of the BOS-976 incident, where the user's
	// interactive rc cost tens of seconds before `command -v` was ever reached.
	run := func(shell, wt, line string) error {
		return runShellWithTimeout(shell, wt, "while true; do sleep 1; done; "+line, 50*time.Millisecond)
	}

	issue := checkAgentResolvable("/bin/sh", "sh", worktree, run)
	if issue == nil {
		t.Fatal("a probe that outran its budget must block with an issue")
	}
	if issue.Title == agentNotFoundIssue("/bin/sh", "sh", worktree).Title {
		t.Fatalf("a slow shell must not be reported as a missing agent; got %q", issue.Title)
	}
	if !strings.Contains(issue.Title, "timed out") {
		t.Errorf("Title = %q, want the probe-timed-out screen", issue.Title)
	}
}

// TestCheckAgentsResolvableProbesConcurrently pins the WHOLE-STARTUP bound, not
// the per-probe one. Every probe owns a full AgentResolveTimeout and this call
// blocks boss's first frame, so a serial loop over two enabled agents costs the
// user 2 x 20s of blank screen.
//
// The assertion is a rendezvous rather than a wall-clock threshold, so it cannot
// flake on a loaded box: each probe parks until ALL of them have arrived. Under
// concurrency they all arrive and every probe returns nil; a serialised
// implementation strands the first probe against a barrier the others can never
// reach, which the guard turns into a failing error rather than a hang.
func TestCheckAgentsResolvableProbesConcurrently(t *testing.T) {
	agents := []string{"claude", "codex", "opencode"}

	var arrived sync.WaitGroup
	arrived.Add(len(agents))
	all := make(chan struct{})
	go func() {
		arrived.Wait()
		close(all)
	}()

	var mu sync.Mutex
	var probed []string
	run := func(_, _, line string) error {
		mu.Lock()
		probed = append(probed, line)
		mu.Unlock()
		arrived.Done()
		select {
		case <-all:
			return nil
		case <-time.After(10 * time.Second):
			// Only reachable if the probes did not overlap.
			return errors.New("probes ran serially: this one waited alone")
		}
	}

	start := time.Now()
	if issue := checkAgentsResolvable("/bin/sh", agents, t.TempDir(), run); issue != nil {
		t.Fatalf("probes did not overlap; got blocking issue %q", issue.Title)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("startup took %s, want roughly one probe budget", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(probed) != len(agents) {
		t.Errorf("probed %v, want one probe per agent %v", probed, agents)
	}
}

// TestCheckAgentsResolvableReportsFirstAgentInOrder pins that the blocking
// screen a user sees is chosen by the agents slice, not by whichever goroutine
// happens to finish first — concurrency must not make the rendered Issue
// nondeterministic.
func TestCheckAgentsResolvableReportsFirstAgentInOrder(t *testing.T) {
	run := func(_, _, line string) error {
		if strings.Contains(line, "claude") {
			time.Sleep(20 * time.Millisecond) // lose the race on purpose
		}
		return errors.New("exit status 1")
	}
	issue := checkAgentsResolvable("/bin/sh", []string{"claude", "codex"}, t.TempDir(), run)
	if issue == nil {
		t.Fatal("want a blocking issue when no agent resolves")
	}
	if !strings.Contains(issue.Title, "claude") {
		t.Errorf("Title = %q, want the FIRST agent (claude)", issue.Title)
	}
}

// TestCheckAgentsResolvableNoAgents guards the empty case: nothing enabled means
// nothing probed and no blocking screen.
func TestCheckAgentsResolvableNoAgents(t *testing.T) {
	run := func(_, _, _ string) error {
		t.Fatal("no agent is enabled; nothing should be probed")
		return nil
	}
	if issue := checkAgentsResolvable("/bin/sh", nil, t.TempDir(), run); issue != nil {
		t.Fatalf("want no issue for zero agents, got %+v", issue)
	}
}
