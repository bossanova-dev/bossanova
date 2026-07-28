package sessionports

import (
	"context"
	"time"
)

// commandProcessSource is the production ProcessSource. It snapshots the
// process graph with `ps` (portable across macOS and Linux) and reads per-PID
// birth identities through the platform-specific readProcessIdentity (sysctl
// on macOS, /proc/<pid>/stat on Linux).
type commandProcessSource struct {
	timeout time.Duration
}

// newCommandProcessSource builds a ProcessSource backed by live commands.
func newCommandProcessSource() *commandProcessSource {
	return &commandProcessSource{timeout: defaultProcessTimeout}
}

// Snapshot runs `ps -axo pid=,ppid=` and parses the pid -> ppid graph.
func (s *commandProcessSource) Snapshot(ctx context.Context) (map[int]int, error) {
	out, err := runCommand(ctx, s.timeout, "ps", "-axo", "pid=,ppid=")
	if err != nil {
		return nil, err
	}
	return parsePS(out)
}

// Identity reads pid's birth identity via the platform adapter.
func (s *commandProcessSource) Identity(ctx context.Context, pid int) (Identity, error) {
	return readProcessIdentity(ctx, pid)
}
