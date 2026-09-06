package main

import (
	"errors"
	"os/exec"
)

// Shell-level exit codes that mean codex never ran.
//
// POSIX shells reserve both: 127 when the command could not be found (and,
// notably, when the shell itself failed to start), 126 when the target exists
// but could not be executed -- not executable, or a bad interpreter line.
// Neither is codex's own exit status, because codex never ran to produce one.
const (
	shellExitCommandNotFound = 127
	shellExitNotExecutable   = 126
)

// runnerUnavailableMarker is the fixed, host-matched half of the non-execution
// sentinels below.
//
// The message IS the wire format. AgentRunnerService.ExitStatus carries only a
// string exit_error (proto/bossanova/v1/plugin.proto) -- there is no numeric
// exit code on that RPC -- so the host has nothing to read but this text, and
// recognising the code has to happen here, on the side of the boundary that
// actually sees the process status.
//
// bossd DUPLICATES this literal in services/bossd/internal/accountwiring/
// credcheck.go rather than importing it, because a plugin binary and the host
// must not share packages (CLAUDE.md, "Module boundaries") -- the same
// coupling, for the same reason, as ErrAuthRequired in auth.go.
//
// The pin is two-sided: TestRunnerUnavailableSentinelMessages freezes these
// bytes here, and TestClassifyVerificationRunnerUnavailableSentinel freezes
// the copy on the host. Reword one side without the other and a test fails
// rather than the classification silently degrading to transient/unclassified.
const runnerUnavailableMarker = "could not run the codex binary"

// ErrRunnerUnavailable and ErrRunnerNotExecutable are the typed sentinels
// surfaced via Runner.ExitError when the login shell exited without running
// codex at all.
//
// Deliberately worded "could not run", never "is not installed": exit 127 is
// not exclusively a missing binary -- a login shell that fails to start, or a
// version-manager shim that exits 127 for its own reasons, produces the same
// code. Naming the binary is what an operator needs to stop looking at the
// credential; diagnosing why it was unresolvable is their environment, not
// the daemon's to guess at.
var (
	ErrRunnerUnavailable   unavailableErr = runnerUnavailableMarker + ": the login shell exited 127 without executing it"
	ErrRunnerNotExecutable unavailableErr = runnerUnavailableMarker + ": the login shell exited 126 without executing it"
)

// unavailableErr is a typed string error so callers can errors.Is these
// sentinels without depending on pointer identity, mirroring authErr.
type unavailableErr string

func (e unavailableErr) Error() string { return string(e) }

// detectNonExecution maps a subprocess exit error onto a non-execution
// sentinel, or nil when the process actually ran.
//
// It reads the exit STATUS rather than matching log text: a process that never
// started produced no log of its own, and the shell's stderr is not codex's
// prose to classify.
func detectNonExecution(err error) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return nil
	}
	switch exitErr.ExitCode() {
	case shellExitCommandNotFound:
		return ErrRunnerUnavailable
	case shellExitNotExecutable:
		return ErrRunnerNotExecutable
	default:
		return nil
	}
}
