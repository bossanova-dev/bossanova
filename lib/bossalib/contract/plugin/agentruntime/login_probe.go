package agentruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/recurser/bossalib/contract/plugin/loginshell"
)

// loginProbeWaitDelay bounds how long cmd.Wait waits after cancellation for a
// child holding stdout/stderr pipes open to close them.
const loginProbeWaitDelay = 2 * time.Second

// LoginStatusProbe configures RunLoginStatus.
type LoginStatusProbe struct {
	// Shell is the login shell path (e.g. "/bin/zsh") passed to loginshell.Wrap.
	// Empty runs Argv directly without a shell wrapper.
	Shell string
	// Argv is the CLI status command and its arguments.
	Argv []string
	// Factory builds the exec.Cmd; nil defaults to exec.CommandContext.
	Factory CommandFactory
	// Timeout bounds the probe command when > 0.
	Timeout time.Duration
}

// LoginStatusOutput holds the result of running a login status command.
//
// When the process exits with a non-zero exit code and the probe context did
// not expire, ExitCode holds the exit code and Err is nil. Launch failures,
// signal terminations, and context cancellation/deadlines populate Err.
type LoginStatusOutput struct {
	Output   []byte
	ExitCode int
	Err      error
}

// RunLoginStatus runs probe.Argv wrapped in probe.Shell inside os.TempDir(),
// capturing combined stdout and stderr under probe.Timeout.
func RunLoginStatus(ctx context.Context, probe LoginStatusProbe) LoginStatusOutput {
	if len(probe.Argv) == 0 {
		return LoginStatusOutput{Err: errors.New("login status probe requires non-empty argv")}
	}

	wrapped := loginshell.Wrap(probe.Shell, loginshell.Flags(probe.Shell), probe.Argv)
	if len(wrapped) == 0 {
		return LoginStatusOutput{Err: errors.New("login status probe produced empty argv")}
	}

	probeCtx := ctx
	if probe.Timeout > 0 {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(ctx, probe.Timeout)
		defer cancel()
	}

	factory := probe.Factory
	if factory == nil {
		factory = exec.CommandContext
	}

	cmd := factory(probeCtx, wrapped[0], wrapped[1:]...)
	cmd.Dir = os.TempDir()
	cmd.WaitDelay = loginProbeWaitDelay

	out, err := cmd.CombinedOutput()
	if err == nil {
		return LoginStatusOutput{Output: out}
	}
	if probeCtx.Err() != nil {
		return LoginStatusOutput{Output: out, Err: probeCtx.Err()}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return LoginStatusOutput{Output: out, ExitCode: code}
		}
		return LoginStatusOutput{Output: out, ExitCode: exitErr.ExitCode(), Err: err}
	}
	return LoginStatusOutput{Output: out, Err: err}
}
