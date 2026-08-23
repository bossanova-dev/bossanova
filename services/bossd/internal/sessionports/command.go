package sessionports

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

// Default bounds for the external commands this package runs. They are small:
// a process snapshot or a listener probe that takes seconds is already a
// failure we would rather report as incomplete than block a scan on.
const (
	defaultProcessTimeout = 5 * time.Second
	defaultSocketTimeout  = 8 * time.Second
	commandWaitDelay      = 2 * time.Second
)

// runCommand runs name+args under a timeout derived from ctx and returns the
// command's stdout. The child context guarantees the subprocess is killed and
// reaped on timeout or parent cancellation, so no subprocess or goroutine
// leaks. stdout is returned even on a non-zero exit (tools like lsof exit 1
// when some PIDs have no matching files yet still print usable output); the
// caller decides whether the accompanying error is fatal.
func runCommand(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.WaitDelay = commandWaitDelay
	err := cmd.Run()
	if cctx.Err() != nil {
		// The derived deadline/cancellation fired: surface it so callers can
		// distinguish an untrustworthy timeout/cancel (whatever bytes we
		// captured are a truncated prefix — degrade to incomplete) from a
		// benign non-zero exit that still produced usable output. cmd.Run
		// otherwise reports the kill only as "signal: killed", which is
		// indistinguishable from a real failure on the parent context.
		return stdout.Bytes(), cctx.Err()
	}
	return stdout.Bytes(), err
}
