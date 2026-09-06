package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestRunnerUnavailableSentinelMessages is the plugin half of the two-sided
// drift pin. The bytes are the wire contract with bossd, which duplicates the
// marker in services/bossd/internal/accountwiring/credcheck.go because the two
// modules must not share a package. Changing the wording here without changing
// the host's copy would silently degrade a PATH fault back to
// transient/unclassified -- the exact BOS-1172 regression -- with nothing else
// failing, so this test exists to fail first.
//
// If this test fails, update runnerUnavailableMarkers in credcheck.go and the
// codexRunnerUnavailableSentinel / codexRunnerNotExecutableSentinel constants
// in credcheck_test.go to match.
func TestRunnerUnavailableSentinelMessages(t *testing.T) {
	for _, tc := range []struct {
		got  error
		want string
	}{
		{ErrRunnerUnavailable, "could not run the codex binary: the login shell exited 127 without executing it"},
		{ErrRunnerNotExecutable, "could not run the codex binary: the login shell exited 126 without executing it"},
	} {
		if tc.got.Error() != tc.want {
			t.Errorf("sentinel drift:\n got  %q\n want %q\nupdate the host's copied marker in credcheck.go too", tc.got.Error(), tc.want)
		}
		if !strings.Contains(tc.got.Error(), runnerUnavailableMarker) {
			t.Errorf("%q no longer carries the host-matched marker %q", tc.got.Error(), runnerUnavailableMarker)
		}
		// The wording must name the binary without claiming it is missing:
		// 127 also covers a login shell that failed to start.
		for _, forbidden := range []string{"not installed", "not found", "does not exist", "missing"} {
			if strings.Contains(tc.got.Error(), forbidden) {
				t.Errorf("%q over-claims with %q; exit 127 is not exclusively a missing binary", tc.got.Error(), forbidden)
			}
		}
	}

	// The sentinels must stay distinguishable from the auth sentinel they sit
	// beside, in both directions -- the host matches both by substring.
	if strings.Contains(ErrAuthRequired.Error(), runnerUnavailableMarker) {
		t.Errorf("the auth sentinel %q now carries the non-execution marker", ErrAuthRequired.Error())
	}
	for _, e := range []error{ErrRunnerUnavailable, ErrRunnerNotExecutable} {
		if detectAuthFailure([]byte(e.Error())) {
			t.Errorf("the non-execution sentinel %q now trips the auth detector", e.Error())
		}
	}
}

// TestDetectNonExecutionIgnoresOtherExits pins the branch's own boundary: only
// the shell's reserved non-execution codes qualify. Codex's ordinary failures
// exit 1, and a nil error is a clean run.
func TestDetectNonExecutionIgnoresOtherExits(t *testing.T) {
	if got := detectNonExecution(nil); got != nil {
		t.Errorf("detectNonExecution(nil) = %v, want nil", got)
	}
	if got := detectNonExecution(errors.New("boom")); got != nil {
		t.Errorf("detectNonExecution(non-exit error) = %v, want nil", got)
	}
	for _, code := range []int{0, 1, 2, 125, 128, 130} {
		err := exec.Command("/bin/sh", "-c", "exit "+strconv.Itoa(code)).Run()
		if code == 0 && err != nil {
			t.Fatalf("exit 0 produced %v", err)
		}
		if got := detectNonExecution(err); got != nil {
			t.Errorf("detectNonExecution(exit %d) = %v, want nil", code, got)
		}
	}
	for code, want := range map[int]error{127: ErrRunnerUnavailable, 126: ErrRunnerNotExecutable} {
		err := exec.Command("/bin/sh", "-c", "exit "+strconv.Itoa(code)).Run()
		got := detectNonExecution(err)
		if !errors.Is(got, want) {
			t.Errorf("detectNonExecution(exit %d) = %v, want %v", code, got, want)
		}
	}
}

// TestExitStatusReportsNonExecution drives the real PostExit hook through a
// login shell that exits without ever running codex (BOS-1172).
//
// 127 and 126 are the shell's own reserved codes -- codex never ran, so the
// exit status is not codex's and says nothing about the stored credential.
// The daemon has no numeric exit code to read (AgentRunnerService.ExitStatus
// carries only a string), so the message here IS the wire contract.
func TestExitStatusReportsNonExecution(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   string
	}{
		{"command not found", "exit 127", "exited 127"},
		{"not executable", "exit 126", "exited 126"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "agent.log")
			r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeCodexShell(t, tc.script)))
			srv := &Server{logger: zerolog.Nop(), runner: r}

			start, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
				WorkDir: dir, SessionId: "sid-" + tc.name, LogPath: logPath,
			})
			if err != nil {
				t.Fatalf("StartRun: %v", err)
			}
			exit := waitExit(t, srv, start.SessionId)

			got := exit.GetExitError()
			if !strings.Contains(got, "could not run the codex binary") {
				t.Fatalf("ExitError = %q, want it to name the binary that could not be run", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("ExitError = %q, want it to carry %q", got, tc.want)
			}
			// The bare exec message is what the daemon could not classify.
			if strings.Contains(got, "exit status") {
				t.Fatalf("ExitError = %q, want the sentinel to replace the bare exec error", got)
			}
			// Non-execution is not a credential verdict, so it must not be
			// reported through the auth failure class.
			if fc := exit.GetFailureClass(); fc == "auth_invalidated" {
				t.Fatalf("FailureClass = %q, a binary that never ran cannot have been rejected by the provider", fc)
			}
		})
	}
}

// TestNonExecutionWinsOverAnAuthShapedTail pins the PostExit precedence.
//
// A process that exited 127 never started, so an auth marker in the tail came
// from the shell or a wrapper, not from codex refusing a credential. Reading
// one as ErrAuthRequired would bench a working account for a PATH fault --
// the expensive direction of this mistake. The structural exit code therefore
// outranks the substring match on agent-authored log text.
func TestNonExecutionWinsOverAnAuthShapedTail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(
		fakeCodexShell(t, "echo 'HTTP error: 401 Unauthorized'; exit 127")))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	start, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: dir, SessionId: "sid-nonexec-auth", LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	exit := waitExit(t, srv, start.SessionId)

	if got := exit.GetExitError(); !strings.Contains(got, "could not run the codex binary") {
		t.Fatalf("ExitError = %q, want the non-execution sentinel to win over the auth marker", got)
	}
	if fc := exit.GetFailureClass(); fc == "auth_invalidated" {
		t.Fatalf("FailureClass = %q, want a 127 exit never to bench the credential", fc)
	}
}

// TestOrdinaryFailureKeepsItsClassification is the mirror image: the new
// branch must only fire for the shell's own non-execution codes. An ordinary
// non-zero exit still reaches the auth and usage-cap detectors.
func TestOrdinaryFailureKeepsItsClassification(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r := NewRunner(zerolog.Nop(), WithCommandFactory(
		fakeCodexShell(t, "echo 'HTTP error: 401 Unauthorized'; exit 1")))
	srv := &Server{logger: zerolog.Nop(), runner: r}

	start, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir: dir, SessionId: "sid-ordinary", LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	exit := waitExit(t, srv, start.SessionId)

	if fc := exit.GetFailureClass(); fc != "auth_invalidated" {
		t.Fatalf("FailureClass = %q, want auth_invalidated for an ordinary exit with an auth marker", fc)
	}
	if got := exit.GetExitError(); strings.Contains(got, "could not run the codex binary") {
		t.Fatalf("ExitError = %q, want an ordinary failure not to claim non-execution", got)
	}
}
