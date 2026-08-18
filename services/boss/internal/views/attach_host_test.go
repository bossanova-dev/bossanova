package views

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	bosspty "github.com/recurser/boss/internal/pty"
	"github.com/recurser/boss/internal/remotehost"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/termnorm"
)

func mustBuildAttachCommand(t *testing.T, destination, tmuxName, worktreePath string) attachCommand {
	t.Helper()
	command, err := buildAttachCommand(destination, tmuxName, worktreePath)
	if err != nil {
		t.Fatalf("buildAttachCommand(%q, %q): %v", destination, tmuxName, err)
	}
	return command
}

func mustBuildAttachReproArgv(t *testing.T, destination, tmuxName string) []string {
	t.Helper()
	argv, err := buildAttachReproArgv(destination, tmuxName)
	if err != nil {
		t.Fatalf("buildAttachReproArgv(%q, %q): %v", destination, tmuxName, err)
	}
	return argv
}

func mustTmuxProbeCommandLine(t *testing.T, destination string) ([]string, time.Duration) {
	t.Helper()
	argv, timeout, err := tmuxProbeCommandLine(destination, "boss-chat-abc")
	if err != nil {
		t.Fatalf("tmuxProbeCommandLine(%q): %v", destination, err)
	}
	return argv, timeout
}

// TestBuildAttachCommand_RemoteRunsTmuxOverSSHWithNoLocalDir is the regression
// guard for the two bugs a naive --host attach has: it shells out to the local
// tmux (which knows nothing about the remote chat), and it sets the remote
// worktree path as a LOCAL working directory, which makes the process fail to
// start with a confusing chdir error. Both are asserted on the constructed
// command, because the live pane cannot be captured once tea.Exec takes the
// terminal.
func TestBuildAttachCommand_RemoteRunsTmuxOverSSHWithNoLocalDir(t *testing.T) {
	got := mustBuildAttachCommand(t, "deploy@bastion.example.com", "boss-chat-abc", "/home/deploy/worktrees/feature")

	want := []string{
		"ssh", "-t",
		// ssh's own escape character is off: with a pty allocated it scans every
		// line for a leading "~", so `~.` would kill the pane and `~~` would
		// silently eat a tilde out of anything pasted into the agent.
		"-e", "none",
		// Keepalives, or a half-open link leaves the pane frozen with no eject
		// and bosspty hands the same wedged process back on every later attach.
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
		// Through the remote LOGIN shell: `ssh host tmux …` runs in a
		// non-login shell, and a tmux off that reduced PATH (Homebrew's
		// /opt/homebrew/bin, exported from a ~/.zprofile zsh does not read
		// non-interactively) fails every attach with "command not found".
		"deploy@bastion.example.com", `exec "$SHELL" -lc 'sh -c "tmux -u attach -t boss-chat-abc"'`,
	}
	if !reflect.DeepEqual(got.argv, want) {
		t.Fatalf("remote attach argv = %q, want %q", got.argv, want)
	}
	if got.dir != "" {
		t.Fatalf("remote attach dir = %q, want empty: that path exists on the remote machine, and setting it locally makes the attach die in chdir", got.dir)
	}
}

// TestAttach_ModelExecsTheCommandItBuilt closes the gap between the builder and
// the process: buildAttachCommand is a pure function, so without this the two
// lines in Update that hand its argv and dir to exec could be reverted to the
// old local-only body with every other test still green — and the empty-Dir
// acceptance criterion is exactly about those two lines.
func TestAttach_ModelExecsTheCommandItBuilt(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")

	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	got, _ := driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1", WorktreePath: "/home/deploy/worktrees/feature"},
	})

	if got.pendingExec == nil || got.pendingExec.cmd == nil {
		t.Fatal("pendingExec.cmd = nil, want the staged process")
	}
	// Read back off the *exec.Cmd itself, never off a copy of the builder's
	// output: a copy would agree with itself while the exec quietly disagreed
	// with both, which is exactly the revert this test exists to catch.
	want := mustBuildAttachCommand(t, "deploy@bastion.example.com", "boss-test-chat", "/home/deploy/worktrees/feature")
	if !reflect.DeepEqual(got.pendingExec.cmd.Args, want.argv) {
		t.Fatalf("exec'd argv = %q, want the remote form %q", got.pendingExec.cmd.Args, want.argv)
	}
	if got.pendingExec.cmd.Dir != "" {
		t.Fatalf("exec'd dir = %q, want empty: the worktree is on the remote machine, so a local chdir to it fails to start the process",
			got.pendingExec.cmd.Dir)
	}
}

// TestAttach_LocalModelStillExecsInTheWorktree is the falsification half: with
// no --host destination the same path must still run the local tmux in the
// session's worktree.
func TestAttach_LocalModelStillExecsInTheWorktree(t *testing.T) {
	withHostDestination(t, "")

	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	got, _ := driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1", WorktreePath: "/Users/dev/worktrees/feature"},
	})

	if got.pendingExec == nil || got.pendingExec.cmd == nil {
		t.Fatal("pendingExec.cmd = nil, want the staged process")
	}
	want := []string{"tmux", "-u", "attach", "-t", "boss-test-chat"}
	if !reflect.DeepEqual(got.pendingExec.cmd.Args, want) {
		t.Fatalf("exec'd argv = %q, want the local form %q", got.pendingExec.cmd.Args, want)
	}
	if got.pendingExec.cmd.Dir != "/Users/dev/worktrees/feature" {
		t.Fatalf("exec'd dir = %q, want the session worktree", got.pendingExec.cmd.Dir)
	}
}

// TestDetachedAfterExec_CtrlXNeverCrossesTheConnection is the evidence for the
// detach criterion. Ctrl+X is read off LOCAL stdin by bosspty, so a detach must
// short-circuit before any remote probe: that is both what returns the user to
// the local TUI promptly and what leaves the remote tmux session untouched. A
// probe on this path would also answer "not alive" whenever the link is the
// thing that died, turning a detach into a spurious agent-exited screen.
func TestDetachedAfterExec_CtrlXNeverCrossesTheConnection(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	rec := withProbeRecorder(t)

	if !detachedAfterExec(true, nil, "boss-chat-abc") {
		t.Fatal("detachedAfterExec(intercepted) = false, want a detach")
	}
	if rec.name != "" {
		t.Fatalf("an intercepted Ctrl+X ran %q; the detach path must not touch the remote host", rec.name)
	}

	// The fallback half: without the keystroke the probe is what decides, so
	// tmux's own detach prefix still reads as a detach when the session lives.
	if detachedAfterExec(false, nil, "boss-chat-abc") {
		t.Fatal("detachedAfterExec(not intercepted) = true though the recorded probe never ran")
	}
	if rec.name != "ssh" {
		t.Fatalf("fallback probe ran %q, want ssh under a --host context", rec.name)
	}
}

// TestDetachedAfterExec_ClassifiesAllFourArms is the BOS-728 regression guard.
// A live tmux session used to be sufficient on its own, which made a --host
// attach whose tmux CLIENT died during startup indistinguishable from a detach:
// bossd creates the remote SESSION, so it is alive either way, and the detach
// verdict then short-circuits the whole error path in the agentFinishedMsg
// handler. A live session proves the work survived, not that the user was ever
// attached.
//
// All four arms are asserted together because the two failure directions are
// each other's falsification: without arm 2 a fix that read every
// non-intercepted exit as a failure would pass, and that regression (an error
// screen held after every ordinary detach) is worse than the bug being fixed.
func TestDetachedAfterExec_ClassifiesAllFourArms(t *testing.T) {
	// The status a tmux client that cannot start reports, propagated verbatim
	// by ssh — the concrete shape of "the client never ran".
	clientNeverStarted := &exec.ExitError{}

	for _, tc := range []struct {
		name         string
		intercepted  bool
		exitErr      error
		sessionAlive bool
		want         bool
		wantProbe    string // the probe argv[0], or "" for "no probe may run"
	}{
		{
			name:         "ctrl+x with a live session is a detach, decided locally",
			intercepted:  true,
			exitErr:      nil,
			sessionAlive: true,
			want:         true,
			wantProbe:    "",
		},
		{
			// The over-trigger guard: bosspty never saw this keystroke because
			// the user typed tmux's own prefix inside the pane, and the client
			// exited 0 on its way out.
			name:         "tmux's own detach prefix is still a detach",
			intercepted:  false,
			exitErr:      nil,
			sessionAlive: true,
			want:         true,
			wantProbe:    "ssh",
		},
		{
			// The bug. Same inputs as the row above but for the exit status.
			name:         "a client that never started is a failure, not a detach",
			intercepted:  false,
			exitErr:      clientNeverStarted,
			sessionAlive: true,
			want:         false,
			wantProbe:    "",
		},
		{
			name:         "a dead session with no interception is a failure, as before",
			intercepted:  false,
			exitErr:      nil,
			sessionAlive: false,
			want:         false,
			wantProbe:    "ssh",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withHostDestination(t, "deploy@bastion.example.com")
			rec := withProbeOutcome(t, tc.sessionAlive)

			if got := detachedAfterExec(tc.intercepted, tc.exitErr, "boss-chat-abc"); got != tc.want {
				t.Fatalf("detachedAfterExec(intercepted=%v, err=%v, alive=%v) = %v, want %v",
					tc.intercepted, tc.exitErr, tc.sessionAlive, got, tc.want)
			}
			if rec.name != tc.wantProbe {
				if tc.wantProbe == "" {
					t.Fatalf("the verdict ran %q; this arm is decided locally and must not cross the connection", rec.name)
				}
				t.Fatalf("probe ran %q, want %q", rec.name, tc.wantProbe)
			}
		})
	}
}

// TestBuildAttachCommand_LocalIsUnchanged pins the local form byte-for-byte,
// including the worktree cwd. --host support branches the attach path; it must
// not refactor it.
func TestBuildAttachCommand_LocalIsUnchanged(t *testing.T) {
	got := mustBuildAttachCommand(t, "", "boss-chat-abc", "/Users/dev/worktrees/feature")

	// -u is required, not incidental: the remote arm and bossd's own attach both
	// pass it, and omitting it here is what made the local attach the only path
	// that lost UTF-8 line-drawing under a bare locale.
	want := []string{"tmux", "-u", "attach", "-t", "boss-chat-abc"}
	if !reflect.DeepEqual(got.argv, want) {
		t.Fatalf("local attach argv = %q, want %q", got.argv, want)
	}
	if got.dir != "/Users/dev/worktrees/feature" {
		t.Fatalf("local attach dir = %q, want the session worktree", got.dir)
	}
}

// TestRemoteCommandBuildersRejectInvalidTmuxSessionNames closes the command
// execution seam before a remote-sourced name can reach either shell parser.
func TestRemoteCommandBuildersRejectInvalidTmuxSessionNames(t *testing.T) {
	for _, name := range []string{
		"boss-chat'quote", "boss-chat\\backslash", "boss-chat;semicolon",
		"boss-chat$dollar", "boss chat",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := buildAttachCommand("deploy@bastion", name, ""); err == nil || !strings.Contains(err.Error(), "invalid tmux session name") {
				t.Fatalf("buildAttachCommand(%q) error = %v, want clear rejection", name, err)
			}
			if _, err := buildAttachReproArgv("deploy@bastion", name); err == nil || !strings.Contains(err.Error(), "invalid tmux session name") {
				t.Fatalf("buildAttachReproArgv(%q) error = %v, want clear rejection", name, err)
			}
			if _, _, err := tmuxProbeCommandLine("deploy@bastion", name); err == nil || !strings.Contains(err.Error(), "invalid tmux session name") {
				t.Fatalf("tmuxProbeCommandLine(%q) error = %v, want clear rejection", name, err)
			}
		})
	}

	const valid = "boss-AbC12345-XyZ_6789"
	attach, err := buildAttachCommand("deploy@bastion", valid, "")
	if err != nil || !strings.Contains(attach.argv[len(attach.argv)-1], valid) {
		t.Fatalf("buildAttachCommand(%q) = (%q, %v), want unchanged valid name", valid, attach.argv, err)
	}
	repro, err := buildAttachReproArgv("deploy@bastion", valid)
	if err != nil || !strings.Contains(repro[len(repro)-1], valid) {
		t.Fatalf("buildAttachReproArgv(%q) = (%q, %v), want unchanged valid name", valid, repro, err)
	}
	probe, _, err := tmuxProbeCommandLine("deploy@bastion", valid)
	if err != nil || !strings.Contains(probe[len(probe)-1], valid) {
		t.Fatalf("tmuxProbeCommandLine(%q) = (%q, %v), want unchanged valid name", valid, probe, err)
	}
}

func TestRemoteTmuxCommand_RoundTripsThroughFishWithoutTouchingCanary(t *testing.T) {
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish not installed: wrapper shape is covered by golden argv tests")
	}

	dir := remoteStubDir(t)
	canary := filepath.Join(t.TempDir(), "must-survive")
	if err := os.WriteFile(canary, []byte("present"), 0o600); err != nil {
		t.Fatalf("write canary: %v", err)
	}
	name := "boss-chat-A1_b2"
	inner := "tmux -u attach -t " + name
	cmd := exec.Command(fish, "--no-config", "-c", remoteTmuxCommand(inner))
	cmd.Env = []string{"PATH=" + dir + ":/bin:/usr/bin", "SHELL=" + filepath.Join(dir, "loginshell")}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fish parsed remote command %q: %v (stderr: %s)", remoteTmuxCommand(inner), err, exitStderr(err))
	}
	if got := strings.Split(strings.TrimRight(string(out), "\n"), "\n"); !reflect.DeepEqual(got, []string{"-u", "attach", "-t", name}) {
		t.Fatalf("fish delivered tmux argv %q, want one payload word", got)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("fish parse touched canary %q: %v", canary, err)
	}
}

// remoteStubDir builds stand-ins for the far side of a --host attach: the remote
// login shell, the tmux it resolves, and ssh itself for a pasted reproduction.
//
// The login shell is a STUB rather than a real `sh -l`, which would look like
// the more faithful choice and is not. A real login shell re-derives PATH — on
// macOS /etc/profile runs path_helper, which reorders the system directories
// ahead of anything a test injected, so the assertion would quietly exercise the
// machine's own tmux instead of the stub (it did, and failed with tmux's exit 1)
// and would behave differently on Linux CI. Standing in for it keeps the test
// about the one property it asserts — whether the session name survives both
// shell parses as a single argument — while still checking that production asked
// for `-lc`.
func remoteStubDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"tmux": "#!/bin/sh\nprintf '%s\\n' \"$@\"\n",
		"loginshell": "#!/bin/sh\n" +
			"[ \"$1\" = \"-lc\" ] || { echo \"login shell invoked with $1, want -lc\" >&2; exit 64; }\n" +
			"exec /bin/sh -c \"$2\"\n",
		// Real ssh concatenates its trailing arguments and hands the result to
		// the remote login shell; the stub does the same with the last one.
		"ssh": "#!/bin/sh\nfor a in \"$@\"; do last=\"$a\"; done\nexec /bin/sh -c \"$last\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	return dir
}

// exitStderr surfaces a failed stub run's stderr, which carries the login
// shell's own complaint when production stops passing -lc.
func exitStderr(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return strings.TrimSpace(string(exitErr.Stderr))
	}
	return ""
}

// TestRemoteCommandsRunThroughTheLoginShell guards the fix itself. `ssh host
// tmux …` runs in a NON-login shell, so a tmux off that reduced PATH —
// canonically Homebrew's /opt/homebrew/bin, exported from a ~/.zprofile zsh does
// not read non-interactively — fails EVERY attach with "command not found".
//
// All three builders must agree. A probe resolving tmux differently from the
// attach would report a reachable session unreachable (or the reverse) purely
// because the two looked in different places, and a repro without the wrapper
// prints a command that fails on exactly the misconfigured host reading it.
func TestRemoteCommandsRunThroughTheLoginShell(t *testing.T) {
	remoteProbe, _ := mustTmuxProbeCommandLine(t, "deploy@bastion")
	for name, argv := range map[string][]string{
		"attach": mustBuildAttachCommand(t, "deploy@bastion", "boss-chat-abc", "").argv,
		"probe":  remoteProbe,
		"repro":  mustBuildAttachReproArgv(t, "deploy@bastion", "boss-chat-abc"),
	} {
		last := argv[len(argv)-1]
		if !strings.HasPrefix(last, `exec "$SHELL" -lc `) {
			t.Errorf("remote %s command = %q, want it routed through the remote login shell", name, last)
		}
		// Not ${SHELL:-…}: the POSIX default-value form is a syntax error in
		// fish ("${ is not a valid variable in fish", exit 127), and fish is a
		// real remote login shell — so the defensive-looking spelling is the
		// one that breaks a host that works today, guarding against nothing
		// sshd does not already set.
		if strings.Contains(last, "${SHELL") {
			t.Errorf("remote %s uses ${SHELL…} expansion, which fish rejects outright: %q", name, last)
		}
	}

	// The local forms stay untouched: a local tmux resolves on the user's own
	// PATH, and wrapping it would interpose a shell where exec needs none.
	localProbe, _ := mustTmuxProbeCommandLine(t, "")
	for name, argv := range map[string][]string{
		"attach": mustBuildAttachCommand(t, "", "boss-chat-abc", "").argv,
		"probe":  localProbe,
		"repro":  mustBuildAttachReproArgv(t, "", "boss-chat-abc"),
	} {
		if got := argv[0]; got != "tmux" {
			t.Errorf("local %s runs %q, want tmux directly", name, got)
		}
	}
}

// attachTuningArgs is the exact set of argv elements the reproduction is allowed
// to omit: the options that tune HOW the connection behaves without changing
// WHERE it goes or WHAT it runs. Anything else buildAttachCommand grows has to
// be declared here deliberately, which is the point — see the gate below.
var attachTuningArgs = map[string]bool{
	"-e": true, "none": true,
	"-o": true,
	fmt.Sprintf("ServerAliveInterval=%d", attachServerAliveInterval): true,
	fmt.Sprintf("ServerAliveCountMax=%d", attachServerAliveCountMax): true,
}

// TestAttachReproIsASubsequenceOfTheRealAttach is the drift gate between the two
// argv builders. The repro exists to be pasted and reproduce the failure on the
// machine the attach ran on, so anything that decides WHERE or HOW the command
// runs — a port, an identity file, a jump host, a different tmux subcommand —
// must appear in both.
//
// A bare subsequence check is NOT enough to say that, and claiming it would be
// worse than not checking: `-p 2222`, `-i <key>` and `-J <host>` all go BEFORE
// the destination, so they land in the middle and the repro stays a subsequence.
// The gate therefore names what may be dropped (attachTuningArgs) and fails on
// anything else, so a new option is red until someone decides which kind it is.
func TestAttachReproIsASubsequenceOfTheRealAttach(t *testing.T) {
	for _, destination := range []string{"", "deploy@bastion"} {
		full := mustBuildAttachCommand(t, destination, "boss-chat-abc", "").argv
		repro := mustBuildAttachReproArgv(t, destination, "boss-chat-abc")

		i := 0
		var dropped []string
		for _, arg := range full {
			if i < len(repro) && arg == repro[i] {
				i++
				continue
			}
			dropped = append(dropped, arg)
		}
		if i != len(repro) {
			// Report and move on: the checks below index off both slices and
			// only make sense once the repro really is a subsequence.
			t.Errorf("repro %q is not a subsequence of the attach %q (destination %q): the reproduction has drifted from the command it reproduces",
				repro, full, destination)
			continue
		}
		for _, arg := range dropped {
			if !attachTuningArgs[arg] {
				t.Errorf("the attach carries %q but the reproduction does not (destination %q); if it only tunes the connection add it to attachTuningArgs, otherwise the pasted command no longer reproduces the attach",
					arg, destination)
			}
		}
		// And the tail that names the machine and the session must be identical,
		// so a change there can never be absorbed as "just a dropped option".
		tail := full[len(full)-len(repro)+2:]
		if !reflect.DeepEqual(tail, repro[2:]) {
			t.Errorf("repro tail %q != attach tail %q (destination %q)", repro[2:], tail, destination)
		}
	}
}

// recordedProbe captures what tmuxSessionAlive would have executed, so both
// dispatch directions can be asserted without an ssh binary or a tmux server.
type recordedProbe struct {
	name string
	args []string
}

// withProbeRecorder swaps the liveness probe's command factory for one that
// records the argv and reports failure (the process never runs), restoring the
// real factory afterwards.
func withProbeRecorder(t *testing.T) *recordedProbe {
	t.Helper()
	rec := &recordedProbe{}
	previous := newTmuxProbeCommand
	t.Cleanup(func() { newTmuxProbeCommand = previous })
	newTmuxProbeCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		rec.name = name
		rec.args = append([]string(nil), args...)
		// A command that is certain not to exist: Run() fails without touching
		// any tmux server, and the test asserts the argv, not the outcome.
		return exec.CommandContext(ctx, "boss-test-probe-does-not-exist")
	}
	return rec
}

// TestDetachedAfterExec_LocalVerdictIsTheSameFunction pins the other half of
// "local behaviour is unchanged". The conjunct added for BOS-728 is the pane's
// own exit status, which is transport-independent for the same reason
// `intercepted` is — so the verdict must not acquire a --host branch, and a
// local attach must keep reading a clean exit over a live session as a detach.
// The local dispatch is asserted through the probe argv, so a classifier that
// quietly routed the local path through ssh would fail here rather than in
// production.
func TestDetachedAfterExec_LocalVerdictIsTheSameFunction(t *testing.T) {
	t.Run("a clean exit over a live local session is still a detach", func(t *testing.T) {
		withHostDestination(t, "")
		rec := withProbeOutcome(t, true)

		if !detachedAfterExec(false, nil, "boss-chat-abc") {
			t.Fatal("detachedAfterExec = false for a local tmux-prefix detach; an ordinary detach would be held behind an error screen")
		}
		if rec.name != "tmux" {
			t.Fatalf("local fallback probe ran %q, want tmux with no --host destination", rec.name)
		}
	})

	t.Run("a local client that never started is a failure too", func(t *testing.T) {
		withHostDestination(t, "")
		rec := withProbeOutcome(t, true)

		if detachedAfterExec(false, &exec.ExitError{}, "boss-chat-abc") {
			t.Fatal("detachedAfterExec = true for a local client that never started; the diagnostic is swallowed locally as well")
		}
		if rec.name != "" {
			t.Fatalf("a failed pane ran the %q probe; the exit status already settles it", rec.name)
		}
	})
}

// withProbeOutcome is withProbeRecorder's answering sibling: it records the
// argv the same way but exits with a status probeTmuxSession reads as a
// definite verdict, so a caller can drive "the session is alive" as well as
// "it is gone". withProbeRecorder's nonexistent binary can only ever produce
// not-alive, which cannot exercise the arms that turn on a LIVE session.
func withProbeOutcome(t *testing.T, alive bool) *recordedProbe {
	t.Helper()
	rec := &recordedProbe{}
	previous := newTmuxProbeCommand
	t.Cleanup(func() { newTmuxProbeCommand = previous })
	// The no-such-session status, not an arbitrary non-zero one: it is the only
	// code probeTmuxSession accepts as an answer, so a "gone" here is the same
	// gone production classifies rather than an unreachable probe wearing its
	// clothes.
	status := tmuxNoSuchSessionExitCode
	if alive {
		status = 0
	}
	newTmuxProbeCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		rec.name = name
		rec.args = append([]string(nil), args...)
		return exec.CommandContext(ctx, "sh", "-c", fmt.Sprintf("exit %d", status))
	}
	return rec
}

// TestAttach_HostClientThatNeverStartedRendersTheDiagnostic is the end-to-end
// half of BOS-728: the classification above only matters because of what it
// unlocks downstream. renderTmuxAttachDiagnostic was written for exactly this
// failure — it quotes tmux's captured startup output and names the REMOTE host
// as the one needing the terminfo entry — and the detach verdict meant it never
// ran. Asserting the rendered screen (not just the boolean) is what proves the
// diagnostic is reachable rather than merely correct.
func TestAttach_HostClientThatNeverStartedRendersTheDiagnostic(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withProbeOutcome(t, true) // bossd's remote session outlives its dead client

	failure := &exec.ExitError{}
	detached := detachedAfterExec(false, failure, "boss-chat-abc")
	if detached {
		t.Fatal("a client that never started was classified as a detach; the diagnostic below is unreachable")
	}

	client := &chatMutationSpy{}
	m := attachModelForTranscriptTest(client)
	m.attachDestination = "deploy@bastion.example.com"
	m.tmuxName = "boss-chat-abc"
	m.effectiveTERM = "xterm-ghostty"
	m.width = 100
	// startExecMsg clears this on the way into tea.Exec; without it View()
	// keeps rendering the launching screen and never reaches the error branch.
	m.launching = false

	updated, cmd := m.Update(agentFinishedMsg{err: failure, detached: detached})
	current := updated.(AttachModel)
	runAttachCmdGraph(t, cmd, func(msg tea.Msg) {
		next, _ := current.Update(msg)
		current = next.(AttachModel)
	})

	if current.detach {
		t.Fatal("detach = true after a failed attach; the TUI would bounce to the chat picker with nothing shown")
	}
	if !current.returned {
		t.Fatal("returned = false after a failed attach, want the error screen")
	}
	if current.agentErr != failure {
		t.Fatalf("agentErr = %v, want the pane's own exit error %v", current.agentErr, failure)
	}
	// describeLaunch hangs off this branch and is what fills launchInfo in.
	if current.launchInfo == nil {
		t.Fatal("launchInfo = nil; describeLaunch never ran, so the error screen has no reproduction command")
	}

	view := current.View().Content
	if !strings.Contains(view, "deploy@bastion.example.com") {
		t.Fatalf("rendered screen never names the remote host, so the terminfo advice points at the wrong machine:\n%s", view)
	}
	if !strings.Contains(view, "boss-chat-abc") {
		t.Fatalf("rendered screen never names the chat's tmux session:\n%s", view)
	}
}

// TestTmuxSessionAlive_RemoteProbeCrossesTheConnection is the load-bearing half
// of the liveness check: under --host the tmux server is on the other machine,
// so a local `tmux has-session` would report every remote chat as gone and route
// every resume-attach to errChatSessionGone.
func TestTmuxSessionAlive_RemoteProbeCrossesTheConnection(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	rec := withProbeRecorder(t)

	if tmuxSessionAlive("boss-chat-abc") {
		t.Fatal("probe reported alive though the recorded command never ran")
	}

	if rec.name != "ssh" {
		t.Fatalf("probe ran %q, want ssh under a --host context", rec.name)
	}
	want := []string{
		"-o", "BatchMode=yes",
		"deploy@bastion.example.com", `exec "$SHELL" -lc 'sh -c "tmux has-session -t boss-chat-abc"'`,
	}
	if !reflect.DeepEqual(rec.args, want) {
		t.Fatalf("remote probe args = %q, want %q", rec.args, want)
	}
}

// TestTmuxSessionAlive_LocalProbeIsUnchanged pins the local dispatch so the
// --host branch cannot quietly become the only path.
func TestTmuxSessionAlive_LocalProbeIsUnchanged(t *testing.T) {
	withHostDestination(t, "")
	rec := withProbeRecorder(t)

	if tmuxSessionAlive("boss-chat-abc") {
		t.Fatal("probe reported alive though the recorded command never ran")
	}

	if rec.name != "tmux" {
		t.Fatalf("probe ran %q, want tmux with no --host destination", rec.name)
	}
	want := []string{"has-session", "-t", "boss-chat-abc"}
	if !reflect.DeepEqual(rec.args, want) {
		t.Fatalf("local probe args = %q, want %q", rec.args, want)
	}
}

// TestTmuxProbeCommandLine_OnlyTheRemoteFormIsBounded documents why the two
// forms differ: a local probe talks to a unix socket and answers or does not,
// while the remote one crosses a network behind a live TUI and must not be able
// to hang the interface.
func TestTmuxProbeCommandLine_OnlyTheRemoteFormIsBounded(t *testing.T) {
	if _, timeout := mustTmuxProbeCommandLine(t, ""); timeout != 0 {
		t.Fatalf("local probe timeout = %v, want 0 (unchanged local behaviour)", timeout)
	}
	if _, timeout := mustTmuxProbeCommandLine(t, "host"); timeout <= 0 {
		t.Fatalf("remote probe timeout = %v, want a bound so a hung ssh cannot freeze the TUI", timeout)
	}
}

// TestProbeTmuxSession_OnlyTheRemoteFormAltersTheProcess pins the rest of
// "local behaviour is byte-for-byte unchanged": the local probe must keep the
// exact process shape it had before --host existed, with no stderr pipe and no
// WaitDelay whose expiry could turn a live local session into a gone one.
func TestProbeTmuxSession_OnlyTheRemoteFormAltersTheProcess(t *testing.T) {
	var captured *exec.Cmd
	previous := newTmuxProbeCommand
	t.Cleanup(func() { newTmuxProbeCommand = previous })
	newTmuxProbeCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		captured = exec.CommandContext(ctx, "sh", "-c", "exit 1")
		return captured
	}

	withHostDestination(t, "")
	probeTmuxSession("boss-chat-abc")
	if captured.Stderr != nil {
		t.Errorf("local probe Stderr = %v, want nil (the null device, as before --host)", captured.Stderr)
	}
	if captured.WaitDelay != 0 {
		t.Errorf("local probe WaitDelay = %v, want 0 (nothing to bound without a stderr pipe)", captured.WaitDelay)
	}

	withHostDestination(t, "deploy@bastion.example.com")
	probeTmuxSession("boss-chat-abc")
	if captured.Stderr == nil {
		t.Error("remote probe Stderr = nil; the shell's own message is half the missing-tmux classification")
	}
	if captured.WaitDelay != tmuxProbeWaitDelay {
		t.Errorf("remote probe WaitDelay = %v, want %v: a captured stderr pipe can outlive the killed ssh",
			captured.WaitDelay, tmuxProbeWaitDelay)
	}
}

// The exit statuses the classifier must NOT read as an answer. Production
// enumerates only the one status that is an answer (tmuxNoSuchSessionExitCode),
// so these live here: they are the real-world failures that would otherwise be
// mistaken for "your chat is gone", and each is a case in TestClassifyTmuxProbe.
const (
	sshTransportExitCode               = 255 // ssh's own failure
	remoteCommandNotFoundExitCode      = 127 // remote shell: tmux not on PATH
	remoteCommandNotExecutableExitCode = 126 // remote shell: found, not runnable
)

// withProbeExitStatus makes the liveness probe run a process that exits with the
// given status, so each failure mode is classified against the real
// *exec.ExitError the production code inspects rather than a hand-built one.
func withProbeExitStatus(t *testing.T, status int) {
	t.Helper()
	previous := newTmuxProbeCommand
	t.Cleanup(func() { newTmuxProbeCommand = previous })
	newTmuxProbeCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", fmt.Sprintf("exit %d", status))
	}
}

// TestClassifyTmuxProbe covers the distinction the remote probe exists to draw.
// A boolean probe answers "not alive" for an ssh that never reached the remote
// tmux server, and the resume guard then tells a user whose chat is perfectly
// alive that it is gone — the same silently-wrong-answer class as the transcript
// reads this ticket gates.
func TestClassifyTmuxProbe(t *testing.T) {
	exit := func(status int) error {
		// A real ExitError, produced the same way the probe produces one.
		return exec.Command("sh", "-c", fmt.Sprintf("exit %d", status)).Run()
	}
	// A real signal death, so ExitCode() is the genuine -1 rather than a
	// hand-built value that might not match what os/exec reports.
	signalKilled := func(t *testing.T) error {
		t.Helper()
		err := exec.Command("sh", "-c", "kill -TERM $$").Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != -1 {
			t.Fatalf("precondition: want a signal death reporting ExitCode() == -1, got %v", err)
		}
		return err
	}

	tests := []struct {
		name        string
		destination string
		runErr      error
		ctxErr      error
		stderr      string
		want        tmuxProbeResult
	}{
		{name: "local success is alive", want: tmuxProbeAlive},
		{name: "remote success is alive", destination: "deploy@bastion", want: tmuxProbeAlive},
		{
			name:   "local failure is gone, whatever the status",
			runErr: exit(sshTransportExitCode),
			want:   tmuxProbeGone,
		},
		{
			name:        "remote tmux answering 'no such session' is gone",
			destination: "deploy@bastion",
			runErr:      exit(1),
			stderr:      "can't find session: boss-chat-abc\n",
			want:        tmuxProbeGone,
		},
		{
			name:        "an ssh transport failure is unreachable, not gone",
			destination: "deploy@bastion",
			runErr:      exit(sshTransportExitCode),
			want:        tmuxProbeUnreachable,
		},
		{
			name:        "an expired bound is unreachable, not gone",
			destination: "deploy@bastion",
			runErr:      exit(1),
			ctxErr:      context.DeadlineExceeded,
			want:        tmuxProbeUnreachable,
		},
		{
			name:        "an ssh that never started is unreachable, not gone",
			destination: "deploy@bastion",
			runErr:      exec.ErrNotFound,
			want:        tmuxProbeUnreachable,
		},
		// A signal-killed ssh reports ExitCode() == -1 (SIGHUP when the terminal
		// closes, SIGTERM, the OOM killer). Enumerating the unreachable statuses
		// and defaulting to gone would land this — and every future status
		// nobody anticipated — on "your chat is gone, start a new one".
		{
			name:        "a signal-killed ssh is unreachable, not gone",
			destination: "deploy@bastion",
			runErr:      signalKilled(t),
			want:        tmuxProbeUnreachable,
		},
		// `ssh host cmd` is non-interactive and sources no login profile, so a
		// Homebrew tmux outside the default PATH is invisible to it. Reading
		// that as "gone" would tell a user with a perfectly live remote chat to
		// start a new one — and the new one would fail identically.
		{
			name:        "a remote tmux missing from the non-interactive PATH is unreachable, not gone",
			destination: "deploy@bastion",
			runErr:      exit(remoteCommandNotFoundExitCode),
			stderr:      "bash: line 1: tmux: command not found\n",
			want:        tmuxProbeUnreachable,
		},
		{
			name:        "a remote tmux that cannot be executed is unreachable, not gone",
			destination: "deploy@bastion",
			runErr:      exit(remoteCommandNotExecutableExitCode),
			want:        tmuxProbeUnreachable,
		},
		// Some shells and ssh wrappers report a status of their own choosing, so
		// the shell's own message is the fallback — the same pair
		// internal/remotehost matches.
		{
			name:        "a shell reporting 'command not found' under another status is unreachable",
			destination: "deploy@bastion",
			runErr:      exit(1),
			stderr:      "zsh:1: command not found: tmux\n",
			want:        tmuxProbeUnreachable,
		},
		{
			name:        "a shell reporting ': not found' under another status is unreachable",
			destination: "deploy@bastion",
			runErr:      exit(1),
			stderr:      "sh: 1: tmux: not found\n",
			want:        tmuxProbeUnreachable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyTmuxProbe(tt.destination, tt.runErr, tt.ctxErr, []byte(tt.stderr))
			if got != tt.want {
				t.Fatalf("classifyTmuxProbe(%q, %v, %v, %q) = %v, want %v",
					tt.destination, tt.runErr, tt.ctxErr, tt.stderr, got, tt.want)
			}
		})
	}
}

// TestRemoteCommandMissing_DoesNotSwallowTmuxsOwnMiss pins the one collision
// that would matter: tmux's "no such session" message must not read as "tmux is
// not installed", or a genuinely gone session would be reported unreachable and
// the resume guard would stop firing at all.
func TestRemoteCommandMissing_DoesNotSwallowTmuxsOwnMiss(t *testing.T) {
	for _, msg := range []string{
		"can't find session: boss-chat-abc",
		"session not found: boss-chat-abc",
		"no server running on /tmp/tmux-501/default",
		// `ssh host cmd` still sources ~/.bashrc, so a real "gone" answer often
		// arrives with unrelated rc-file noise ahead of it. Matching the bare
		// phrase anywhere in stderr would read this as "tmux is not installed"
		// and the BOS-108 resume guard would never fire for that user again.
		"bash: line 1: pyenv: command not found\ncan't find session: boss-chat-abc",
		"zsh:1: command not found: nvm\ncan't find session: boss-chat-abc",
	} {
		if remoteCommandMissing([]byte(msg)) {
			t.Fatalf("remoteCommandMissing(%q) = true; tmux's own answer must stay an answer", msg)
		}
	}
}

// TestAttach_UnreachableRemoteHostDoesNotClaimTheChatIsGone drives the resume
// guard end to end. errChatSessionGone reads "no longer available … start a new
// chat", so firing it because ssh could not connect invites the user to abandon
// a live remote chat and open a duplicate.
func TestAttach_UnreachableRemoteHostDoesNotClaimTheChatIsGone(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withProbeExitStatus(t, sshTransportExitCode)

	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "agent-1")
	got, _ := driveAttachReady(t, m, attachReadyMsg{session: &pb.Session{Id: "session-1"}})

	if got.err != nil {
		t.Fatalf("err = %v after an unreachable probe, want none: ssh failing to connect is not evidence the chat is gone", got.err)
	}
	if got.pendingExec == nil {
		t.Fatal("pendingExec = nil, want the attach to proceed so ssh's own error reaches the pane and the diagnostic screen")
	}
	// The wire between the remote context and the error screen. Without this,
	// dropping `m.attachDestination = remoteHostDestination()` leaves every
	// other test green while a --host failure prints a local `tmux attach -t
	// <name>` reproduction and aims the terminfo fix at the wrong machine.
	if got.attachDestination != "deploy@bastion.example.com" {
		t.Fatalf("attachDestination = %q, want the destination the attach went through", got.attachDestination)
	}
	repro := renderTmuxAttachDiagnostic(got.attachDestination, got.tmuxName, got.effectiveTERM, "")
	if !strings.Contains(repro, "ssh -t deploy@bastion.example.com") || !strings.Contains(repro, "tmux -u attach") {
		t.Fatalf("diagnostic does not reproduce the ssh transport that ran; got:\n%s", repro)
	}
}

// TestAttach_RemoteGoneStillShowsChatSessionGone is the falsification half: when
// the remote tmux server does answer "no such session", the guard must still
// fire, or a resume-attach dumps a raw tmux error into the terminal.
func TestAttach_RemoteGoneStillShowsChatSessionGone(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withProbeExitStatus(t, 1)

	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "agent-1")
	got, _ := driveAttachReady(t, m, attachReadyMsg{session: &pb.Session{Id: "session-1"}})

	if !errors.Is(got.err, errChatSessionGone) {
		t.Fatalf("err = %v, want errChatSessionGone when the remote tmux server reports the session missing", got.err)
	}
}

// transcriptSpy counts the local-filesystem transcript reads the --host gates
// must skip. Counting the reads (rather than watching for a crash) is the whole
// point: reading a path that does not exist does not crash, it quietly answers
// "no title" and "transcript absent" — which is exactly the bug.
type transcriptSpy struct {
	titleCalls   int
	absenceCalls int
}

func withTranscriptSpy(t *testing.T, title string, absent bool) *transcriptSpy {
	t.Helper()
	spy := &transcriptSpy{}
	previousTitle, previousAbsence := agentChatTitle, agentTranscriptAbsentOrEmpty
	t.Cleanup(func() {
		agentChatTitle, agentTranscriptAbsentOrEmpty = previousTitle, previousAbsence
	})
	agentChatTitle = func(string, string) string {
		spy.titleCalls++
		return title
	}
	agentTranscriptAbsentOrEmpty = func(string, string) bool {
		spy.absenceCalls++
		return absent
	}
	return spy
}

// chatMutationSpy records the two RPCs a mistaken local transcript read would
// trigger. DeleteChat is the dangerous one: an absent local transcript reads as
// "this chat was never used".
type chatMutationSpy struct {
	stubClient
	titleUpdates int
	deletes      int
	// lastTitle records the title of the most recent UpdateChatTitle call, so a
	// test can assert *what* was written and not merely that a write happened
	// (BOS-839). Purely additive: the counters above are unchanged.
	lastTitle string
	// chats is the stored chat list the detach path's fresh stored-title read
	// answers with (BOS-869). Additive: a zero-valued spy returns no chats,
	// which is itself one of the fail-closed cases.
	chats []*pb.ClaudeChat
	// listChatsErr makes that read fail — the other fail-closed half.
	listChatsErr error
	// listChatsCalls proves the read happens only inside the title-write branch:
	// the orphan reap must not fire it. The embedded stubClient.ListChats panics,
	// so before BOS-869 no fixture in this file could reach it at all.
	listChatsCalls int
}

func (s *chatMutationSpy) UpdateChatTitle(_ context.Context, _ string, title string) error {
	s.titleUpdates++
	s.lastTitle = title
	return nil
}

func (s *chatMutationSpy) DeleteChat(context.Context, string) error {
	s.deletes++
	return nil
}

// ListChats answers the detach path's fresh stored-title read (BOS-869).
func (s *chatMutationSpy) ListChats(context.Context, string) ([]*pb.ClaudeChat, error) {
	s.listChatsCalls++
	if s.listChatsErr != nil {
		return nil, s.listChatsErr
	}
	return s.chats, nil
}

func (s *chatMutationSpy) ReportChatStatus(context.Context, []*pb.ChatStatusReport) error {
	return nil
}

func (s *chatMutationSpy) DescribeChatLaunch(context.Context, string) (*pb.DescribeChatLaunchResponse, error) {
	return &pb.DescribeChatLaunchResponse{Argv: []string{"claude"}}, nil
}

func (s *chatMutationSpy) DescribeChatMCP(context.Context, string) (*pb.DescribeChatMCPResponse, error) {
	return nil, nil
}

func attachModelForTranscriptTest(client *chatMutationSpy) AttachModel {
	m := NewAttachModel(client, context.Background(), bosspty.NewManager(), "session-1", "agent-1")
	// A remote worktree path: on this machine it names nothing, which is the
	// condition that makes the local reads answer wrongly rather than fail.
	m.session = &pb.Session{Id: "session-1", WorktreePath: "/home/deploy/worktrees/feature"}
	m.agentSessionID = "agent-1"
	return m
}

// TestAttach_DetachWritesDerivedTitleOnlyOverAPlaceholder pins writer 5 of the
// five chat-title writers. It is the successor to BOS-839's
// TestAttach_DetachClobbersUserRenameWithDerivedTitle, which characterized this
// writer as the one hole in the project's title-precedence rule; BOS-869 closed
// the hole and this test asserts the guarded behaviour instead. Renamed, not
// deleted — the rename is the record that the hole was closed deliberately.
//
// The title AttachModel.updateChatTitle derives is NOT a user-set name.
// agentChatTitle resolves to agent.ChatTitle -> parseSessionMeta
// (services/boss/internal/agent/title.go), which scans the JSONL and returns the
// FIRST USER MESSAGE. It is therefore non-authoritative, exactly as BOS-611
// ratified: an auto-derived title must never replace a real name.
//
// So the detach path now carries a placeholder guard. Immediately before the
// write it re-reads the stored title (a fresh ListChats on the session, matched
// on the agent session id) and writes only when that title is still a
// placeholder — isPlaceholderChatTitle's exact match on "" and "New chat", the
// same predicate the picker's backfillTitles uses. The read is fresh rather than
// captured at attach time because a chat can be renamed mid-attach: by the agent
// in the attached pane through the MCP update_chat_title tool, or by `boss chat
// rename` and the web UI from a second terminal.
//
// It fails CLOSED. A read error, or no chat matching the agent session id, skips
// the write: skipping costs nothing durable (the daemon poller and the chat
// picker both still backfill a placeholder title) while clobbering destroys a
// name the user typed.
//
// The final case is the falsification half for the read itself: the orphan-reap
// branch is untouched and must not fire the stored-title read at all.
func TestAttach_DetachWritesDerivedTitleOnlyOverAPlaceholder(t *testing.T) {
	const derived = "First user prompt"

	storedChat := func(title string) []*pb.ClaudeChat {
		return []*pb.ClaudeChat{{AgentSessionId: "agent-1", Title: title}}
	}

	tests := []struct {
		name string
		// derivedTitle is what the JSONL read answers; transcriptAbsent is the
		// orphan-reap probe's answer, consulted only when derivedTitle is "".
		derivedTitle     string
		transcriptAbsent bool
		// chats / listChatsErr configure the fresh stored-title read.
		chats        []*pb.ClaudeChat
		listChatsErr error

		wantTitleUpdates   int
		wantLastTitle      string
		wantDeletes        int
		wantListChatsCalls int
	}{
		{
			// The behaviour BOS-869 changed: a real name survives the detach.
			name:               "user rename is not clobbered",
			derivedTitle:       derived,
			chats:              storedChat("Manual investigation"),
			wantTitleUpdates:   0,
			wantListChatsCalls: 1,
		},
		{
			// The positive half. Without it the guard test above passes
			// vacuously — a writer that never writes would satisfy it.
			name:               "placeholder New chat is still backfilled",
			derivedTitle:       derived,
			chats:              storedChat("New chat"),
			wantTitleUpdates:   1,
			wantLastTitle:      derived,
			wantListChatsCalls: 1,
		},
		{
			// An empty stored title is a placeholder, not an unknown.
			name:               "empty stored title is a placeholder",
			derivedTitle:       derived,
			chats:              storedChat(""),
			wantTitleUpdates:   1,
			wantLastTitle:      derived,
			wantListChatsCalls: 1,
		},
		{
			name:               "read error fails closed",
			derivedTitle:       derived,
			listChatsErr:       errors.New("daemon unreachable"),
			wantTitleUpdates:   0,
			wantListChatsCalls: 1,
		},
		{
			name:               "no chat matching the agent session fails closed",
			derivedTitle:       derived,
			chats:              []*pb.ClaudeChat{{AgentSessionId: "agent-2", Title: "New chat"}},
			wantTitleUpdates:   0,
			wantListChatsCalls: 1,
		},
		{
			// Untouched neighbour: the reap keys on a confirmed-absent
			// transcript and never consults the stored title, so the read must
			// not fire outside the write branch.
			name:               "orphan reap is untouched and never reads the stored title",
			derivedTitle:       "",
			transcriptAbsent:   true,
			chats:              storedChat("Manual investigation"),
			wantTitleUpdates:   0,
			wantDeletes:        1,
			wantListChatsCalls: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Local destination: the remote-host short-circuit must not be what
			// makes any of these pass, since that gate is what the sibling test
			// below measures.
			withHostDestination(t, "")
			spy := withTranscriptSpy(t, tc.derivedTitle, tc.transcriptAbsent)
			client := &chatMutationSpy{chats: tc.chats, listChatsErr: tc.listChatsErr}
			m := attachModelForTranscriptTest(client)

			cmd := m.updateChatTitle()
			if cmd == nil {
				t.Fatal("updateChatTitle returned nil locally, want the title-write cmd (the detach path must be reached for this test to mean anything)")
			}
			runAttachCmdGraph(t, cmd, func(tea.Msg) {})

			if spy.titleCalls != 1 {
				t.Fatalf("agent.ChatTitle called %d time(s) locally, want 1", spy.titleCalls)
			}
			// Behaviour first, mechanism second: when the guard is missing the
			// headline failure should name the clobber, not the absent read.
			if client.titleUpdates != tc.wantTitleUpdates {
				t.Errorf("UpdateChatTitle called %d time(s), want %d", client.titleUpdates, tc.wantTitleUpdates)
			}
			if client.lastTitle != tc.wantLastTitle {
				t.Errorf("UpdateChatTitle title = %q, want %q", client.lastTitle, tc.wantLastTitle)
			}
			if client.deletes != tc.wantDeletes {
				t.Errorf("DeleteChat called %d time(s), want %d", client.deletes, tc.wantDeletes)
			}
			if client.listChatsCalls != tc.wantListChatsCalls {
				t.Errorf("ListChats called %d time(s), want %d — the stored-title read fires once, and only inside the title-write branch", client.listChatsCalls, tc.wantListChatsCalls)
			}
		})
	}
}

// TestAttach_RemoteHostNeverReadsTheLocalTranscript is the highest-value guard
// in this change. Under --host the orphan reap would delete the record of a live
// remote chat, because the transcript it looks for is on the other machine. The
// spy is configured to answer "no title, transcript absent" — precisely the
// answers a nonexistent local path produces — so a missing gate reaches
// DeleteChat rather than merely not crashing.
func TestAttach_RemoteHostNeverReadsTheLocalTranscript(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	spy := withTranscriptSpy(t, "", true)
	client := &chatMutationSpy{}
	m := attachModelForTranscriptTest(client)

	// t.Error, not t.Fatal: the DeleteChat assertion below is the one that
	// describes the damage, and it must still run when this one trips.
	if cmd := m.updateChatTitle(); cmd != nil {
		t.Error("updateChatTitle returned a cmd under a --host context, want nil so no local transcript read is scheduled")
	}

	// Drive the whole error-exit graph too: the orphan reap hangs off
	// agentFinishedMsg, and a nil cmd inside tea.Batch/tea.Sequence must not
	// resurrect it by another route.
	updated, cmd := m.Update(agentFinishedMsg{err: &exec.ExitError{}, detached: false})
	current := updated.(AttachModel)
	runAttachCmdGraph(t, cmd, func(msg tea.Msg) {
		next, _ := current.Update(msg)
		current = next.(AttachModel)
	})

	if spy.titleCalls != 0 {
		t.Fatalf("agent.ChatTitle called %d time(s) under --host, want 0", spy.titleCalls)
	}
	if spy.absenceCalls != 0 {
		t.Fatalf("agent.TranscriptAbsentOrEmpty called %d time(s) under --host, want 0", spy.absenceCalls)
	}
	if client.deletes != 0 {
		t.Fatalf("DeleteChat called %d time(s) under --host — a live remote chat's record would be destroyed", client.deletes)
	}
	if client.titleUpdates != 0 {
		t.Fatalf("UpdateChatTitle called %d time(s) under --host, want 0", client.titleUpdates)
	}
	// The error screen still has to arrive: gating the transcript reads must not
	// take the launch diagnostic down with it.
	if current.launchInfo == nil {
		t.Fatal("launchInfo = nil after an error exit under --host; the diagnostic must survive the gate")
	}
}

// TestAttach_LocalStillReapsTheOrphan is the falsification half: with no --host
// destination the exact same setup must reach both reads and delete the orphan.
// Without it, a gate that accidentally fired always would still pass the test
// above.
func TestAttach_LocalStillReapsTheOrphan(t *testing.T) {
	withHostDestination(t, "")
	spy := withTranscriptSpy(t, "", true)
	client := &chatMutationSpy{}
	m := attachModelForTranscriptTest(client)

	cmd := m.updateChatTitle()
	if cmd == nil {
		t.Fatal("updateChatTitle returned nil locally, want the title/orphan cmd")
	}
	runAttachCmdGraph(t, cmd, func(tea.Msg) {})

	if spy.titleCalls != 1 {
		t.Fatalf("agent.ChatTitle called %d time(s) locally, want 1", spy.titleCalls)
	}
	if spy.absenceCalls != 1 {
		t.Fatalf("agent.TranscriptAbsentOrEmpty called %d time(s) locally, want 1", spy.absenceCalls)
	}
	if client.deletes != 1 {
		t.Fatalf("DeleteChat called %d time(s) locally, want 1 (unchanged orphan reap)", client.deletes)
	}
}

// TestRenderTmuxAttachDiagnostic_RemoteReproducesTheSSHTransport keeps the error
// screen honest. A local `tmux attach -t <name>` printed under a --host failure
// is a command that cannot work where the user would paste it, and it aims the
// terminfo fix at the wrong machine.
func TestRenderTmuxAttachDiagnostic_RemoteReproducesTheSSHTransport(t *testing.T) {
	out := renderTmuxAttachDiagnostic(
		"deploy@bastion.example.com",
		"boss-chat-abc",
		"xterm-256color",
		"missing or unsuitable terminal: xterm-ghostty\n",
	)

	for _, want := range []string{
		// The TERM prefix and the transport, then the tmux invocation inside
		// remoteTmuxCommand's login-shell wrapper — the diagnostic reproduces
		// the wrapper too, or it would print a command that fails on exactly
		// the misconfigured host most likely to be reading this screen.
		"TERM=xterm-256color ssh -t deploy@bastion.example.com",
		`exec "$SHELL" -lc`,
		"tmux -u attach -t boss-chat-abc",
		"install the entry on deploy@bastion.example.com",
		"missing or unsuitable terminal: xterm-ghostty",
		"boss fix-terminal",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("remote diagnostic missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "install the entry on this host") {
		t.Fatalf("remote diagnostic points terminfo at the local host; got:\n%s", out)
	}
}

// TestAttach_ErrorViewNeverShowsTheForwardedSocketDial covers the third view the
// ticket names: attach is where a --host user spends the session, so a tunnel
// that drops here is exactly the "session looks frozen" report. The absence
// assertion is the guard — before this, the raw dial error was rendered whole.
func TestAttach_ErrorViewNeverShowsTheForwardedSocketDial(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withHostTunnelReason(t, "remotehost: ssh tunnel to deploy@bastion.example.com exited: signal: terminated")

	m := AttachModel{err: errForwardedSocketDial, width: 100}
	rendered := normalizedRender(m.View().Content)

	assertNoForwardedSocketDial(t, "attach error view", rendered)
	if !strings.Contains(rendered, "Reconnecting to deploy@bastion.example.com") {
		t.Fatalf("attach error view %q must show the reconnecting affordance naming the destination", rendered)
	}
	if !strings.Contains(rendered, "signal: terminated") {
		t.Fatalf("attach error view %q must report why the tunnel went down", rendered)
	}
	if !strings.Contains(rendered, "[esc] back") {
		t.Fatalf("attach error view %q must keep its action bar", rendered)
	}
}

// TestAttach_ErrorViewKeepsLocalErrorsVerbatim is acceptance criterion 6 for
// attach: a local run still shows the error it was given.
func TestAttach_ErrorViewKeepsLocalErrorsVerbatim(t *testing.T) {
	withHostDestination(t, "")
	withHostTunnelReason(t, "")

	m := AttachModel{err: errForwardedSocketDial, width: 100}
	rendered := normalizedRender(m.View().Content)
	if !strings.Contains(rendered, "dial unix") {
		t.Fatalf("local attach error view %q must keep showing the real error", rendered)
	}
	if strings.Contains(rendered, "Reconnecting to") {
		t.Fatalf("local attach error view %q must not claim an ssh tunnel is being redialled", rendered)
	}
}

// probeRecord is what recordingTerminfoProber captures: the destinations a
// prober was built for, and the TERMs those probers were actually asked about.
//
// Recording the TERM matters as much as the destination. The value crosses a
// seam nothing else observes — termnorm.NormalizeWith strips the `TERM=` prefix
// and hands the remainder to the prober — and a prober stub that discards its
// argument agrees with any value at all. Were the un-trimmed `TERM=xterm-ghostty`
// ever passed through, remotehost.probeableTerm would reject the `=`, EVERY
// --host attach would silently downgrade to the fallback (the blanket downgrade
// the plan explicitly rejected), and an argument-blind stub would stay green.
type probeRecord struct {
	destinations []string
	terms        []string
}

// recordingTerminfoProber replaces newRemoteTerminfoProber for one test and
// records what it was asked, handing back a prober that always answers `answer`.
//
// The recorder is what makes the local assertions non-vacuous: everything a real
// remote prober goes on to do is an ssh call a local test never makes, so
// "nothing crashed" cannot tell "no prober was built" apart from "one was built
// and never used" — and the second of those is the bug this ticket fixes.
func recordingTerminfoProber(t *testing.T, answer bool) *probeRecord {
	t.Helper()
	orig := newRemoteTerminfoProber
	t.Cleanup(func() { newRemoteTerminfoProber = orig })

	rec := &probeRecord{}
	newRemoteTerminfoProber = func(destination string) termnorm.Prober {
		rec.destinations = append(rec.destinations, destination)
		return func(term string) bool {
			rec.terms = append(rec.terms, term)
			return answer
		}
	}
	return rec
}

// assertProbedFor pins BOTH halves of the remote probe's inputs: exactly one
// prober built, for that destination, asked about exactly that TERM.
//
// The TERM half is the one that would otherwise go unchecked anywhere on the
// branch — see probeRecord. It is asserted as the BARE value: termnorm strips
// the `TERM=` prefix before calling the prober, and passing the prefixed form
// through would make remotehost.probeableTerm reject the `=` and downgrade every
// single --host attach.
func assertProbedFor(t *testing.T, rec *probeRecord, destination, term string) {
	t.Helper()
	if want := []string{destination}; !reflect.DeepEqual(rec.destinations, want) {
		t.Fatalf("newRemoteTerminfoProber built for %q, want exactly %q", rec.destinations, want)
	}
	if want := []string{term}; !reflect.DeepEqual(rec.terms, want) {
		t.Fatalf("the prober was asked about %q, want exactly %q (the bare TERM, not the TERM= entry)",
			rec.terms, want)
	}
}

// envTERM returns the TERM value carried by env, or "" when it has none.
func envTERM(env []string) string {
	for _, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			return strings.TrimPrefix(e, "TERM=")
		}
	}
	return ""
}

// TestAttachTmuxEnv_LocalAttachNeverBuildsARemoteProber pins the local half of
// the fix: an ordinary attach must resolve terminfo exactly as it did before,
// and must not so much as construct the ssh-backed prober.
//
// The env comparison against a plain termnorm.Normalize is what proves
// byte-for-byte parity, and it holds whether or not xterm-ghostty resolves on
// the machine running the test, so it is not environment-dependent.
func TestAttachTmuxEnv_LocalAttachNeverBuildsARemoteProber(t *testing.T) {
	// Answering false is deliberate: were the local path to reach this fake, the
	// TERM would be downgraded and the parity assertion below would notice.
	built := recordingTerminfoProber(t, false)

	base := []string{"PATH=/bin", "TERM=xterm-ghostty"}
	env, eff := attachTmuxEnv(base, "")

	if len(built.destinations) != 0 {
		t.Fatalf("newRemoteTerminfoProber built probers for %q on a local attach, want none", built.destinations)
	}
	want := termnorm.Normalize(base)
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("local env = %q, want %q (byte-for-byte the pre-existing local normalization)", env, want)
	}
	if wantEff := envTERM(want); eff != wantEff {
		t.Fatalf("local effective TERM = %q, want %q", eff, wantEff)
	}
}

// TestHostTerminfoProber_EmptyDestinationIsTheLocalProbe asserts the selection
// itself, not just its outcome: "" must hand back termnorm.Resolvable, the very
// function the local path used before this ticket existed.
func TestHostTerminfoProber_EmptyDestinationIsTheLocalProbe(t *testing.T) {
	orig := newRemoteTerminfoProber
	t.Cleanup(func() { newRemoteTerminfoProber = orig })
	newRemoteTerminfoProber = func(destination string) termnorm.Prober {
		t.Fatalf("hostTerminfoProber(%q) built a remote prober for the local case", destination)
		return nil
	}

	got := reflect.ValueOf(hostTerminfoProber("")).Pointer()
	want := reflect.ValueOf(termnorm.Resolvable).Pointer()
	if got != want {
		t.Fatal("hostTerminfoProber(\"\") did not return termnorm.Resolvable; the local attach must keep probing this host")
	}
}

// TestRemoteTerminfoProber_CarriesTheDestinationAndTheTunnelsControlMaster runs
// the REAL remoteTerminfoProber body, which no other test does.
//
// Every other test in this file stubs newRemoteTerminfoProber, so the one place
// that builds remotehost.Options — and the only place the tunnel's control path
// is wired into the probe — never executes. Deleting `ControlPath:
// remoteHostControlPath()` would leave the whole suite green while every probe
// stopped riding the master and started opening its own BatchMode connection,
// which for a password or uncached-passphrase user fails outright. That is
// acceptance criterion 6's first clause, and it was asserted only by inspection.
//
// Reading the control path at PROBE time rather than at construction is asserted
// too: a prober built before the tunnel came up must still find the master.
func TestRemoteTerminfoProber_CarriesTheDestinationAndTheTunnelsControlMaster(t *testing.T) {
	var got []remotehost.Options
	previous := remoteTerminfoPresent
	t.Cleanup(func() { remoteTerminfoPresent = previous })
	remoteTerminfoPresent = func(_ context.Context, opts remotehost.Options, _ string) bool {
		got = append(got, opts)
		return true
	}

	withHostControlPath(t, "")
	prober := remoteTerminfoProber("deploy@bastion.example.com")
	// Built with no master, then the tunnel comes up before the probe runs.
	withHostControlPath(t, "/tmp/boss-ctl.sock")
	prober("xterm-ghostty")

	if len(got) != 1 {
		t.Fatalf("remotehost.TerminfoPresent called %d time(s), want 1", len(got))
	}
	if got[0].Destination != "deploy@bastion.example.com" {
		t.Errorf("Options.Destination = %q, want the --host destination verbatim", got[0].Destination)
	}
	if got[0].ControlPath != "/tmp/boss-ctl.sock" {
		t.Errorf("Options.ControlPath = %q, want the tunnel's master read at probe time; "+
			"without it the probe opens its own BatchMode connection and a password-auth user's probe is refused",
			got[0].ControlPath)
	}
}

// TestAttach_RemoteAttachProbesTheDestinationItAttachesTo is the wire between
// the production Update path and the fix.
//
// attachTmuxEnv's destination argument is passed at exactly one production call
// site. Reverting it to `attachTmuxEnv(os.Environ(), "")` disables the entire
// ticket under --host — every remote attach would go back to probing the local
// machine — and nothing else in the package observes the resulting env, so the
// suite would stay green. The recorded destination is the assertion rather than
// the env's TERM: on a machine whose os.Environ() carries no TERM, or one whose
// TERM resolves locally, the local and remote answers coincide and an env-only
// check would be vacuous.
func TestAttach_RemoteAttachProbesTheDestinationItAttachesTo(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	built := recordingTerminfoProber(t, true)

	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	got, _ := driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1", WorktreePath: "/home/deploy/worktrees/feature"},
	})

	if want := []string{"deploy@bastion.example.com"}; !reflect.DeepEqual(built.destinations, want) {
		t.Fatalf("the attach probed terminfo for %q, want %q — the machine it is about to run tmux on",
			built.destinations, want)
	}
	if got.pendingExec == nil {
		t.Fatal("pendingExec = nil, want the staged process")
	}
}

// TestAttach_LocalAttachProbesNoDestination is the falsification half: without
// --host the same path must build no remote prober at all.
func TestAttach_LocalAttachProbesNoDestination(t *testing.T) {
	withHostDestination(t, "")
	built := recordingTerminfoProber(t, true)

	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	if _, _ = driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1", WorktreePath: "/Users/dev/worktrees/feature"},
	}); len(built.destinations) != 0 {
		t.Fatalf("a local attach probed %q, want no remote probe at all", built.destinations)
	}
}

// TestAttachTmuxEnv_RemoteAttachProbesTheHostAndItsAnswerDecides is the bug this
// ticket is about. Under --host the tmux client runs on the remote machine, so a
// TERM that resolves locally and is absent there kills the pane with "missing or
// unsuitable terminal". The fake answers false while the local machine may well
// resolve xterm-ghostty, which is what proves the REMOTE answer drove the result.
func TestAttachTmuxEnv_RemoteAttachProbesTheHostAndItsAnswerDecides(t *testing.T) {
	built := recordingTerminfoProber(t, false)

	env, eff := attachTmuxEnv([]string{"PATH=/bin", "TERM=xterm-ghostty"}, "deploy@bastion.example.com")

	assertProbedFor(t, built, "deploy@bastion.example.com", "xterm-ghostty")
	if eff != termnorm.FallbackTERM {
		t.Fatalf("effective TERM = %q, want %q — the remote host has no xterm-ghostty entry", eff, termnorm.FallbackTERM)
	}
	if got := envTERM(env); got != termnorm.FallbackTERM {
		t.Fatalf("env carries TERM=%q, want TERM=%s; the tmux child reads the env, not the diagnostic string", got, termnorm.FallbackTERM)
	}
}

// TestAttachTmuxEnv_RemoteHostWithTheEntryKeepsTheRealTERM guards the other
// direction: the fix must not blanket-downgrade every --host attach to
// xterm-256color, which would silently cost users their real terminal's
// capabilities on hosts that have the entry.
func TestAttachTmuxEnv_RemoteHostWithTheEntryKeepsTheRealTERM(t *testing.T) {
	built := recordingTerminfoProber(t, true)

	env, eff := attachTmuxEnv([]string{"PATH=/bin", "TERM=xterm-ghostty"}, "deploy@bastion.example.com")

	assertProbedFor(t, built, "deploy@bastion.example.com", "xterm-ghostty")
	if eff != "xterm-ghostty" {
		t.Fatalf("effective TERM = %q, want xterm-ghostty kept as-is when the remote host has the entry", eff)
	}
	if got := envTERM(env); got != "xterm-ghostty" {
		t.Fatalf("env carries TERM=%q, want TERM=xterm-ghostty", got)
	}
}
