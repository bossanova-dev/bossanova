//go:build darwin

package sessionports

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// lsofSocketSource is the macOS SocketSource. It runs a single lsof invocation
// per batch and parses its field output with the portable parseLsof.
type lsofSocketSource struct {
	timeout time.Duration
}

func newPlatformSocketSource() SocketSource {
	return &lsofSocketSource{timeout: defaultSocketTimeout}
}

// Listeners runs
//
//	lsof -nP -a -p <csv> -iTCP -sTCP:LISTEN -Fpnt
//
// once for the whole batch. The -t (type) field is included beyond the plan's
// original -Fpn so IPv4 and IPv6 wildcard binds ("*:8080", byte-identical in
// the name field) can be told apart — required by the IPv4/IPv6 acceptance
// criterion.
//
// lsof exits non-zero (status 1) whenever some — or all — of the requested
// PIDs have no matching listening socket. That is the normal "nothing is
// listening" result, not a failure, so a clean process exit (even non-zero,
// even with empty output) is authoritative complete evidence. Only three
// things degrade completeness: a timeout/cancellation (truncated, untrusted →
// GlobalIncomplete), lsof failing to execute at all (missing binary →
// source error), and per-PID malformed rows (scoped by parseLsof).
func (s *lsofSocketSource) Listeners(ctx context.Context, pids []int) (SocketScan, error) {
	if len(pids) == 0 {
		return SocketScan{}, nil
	}
	strs := make([]string, len(pids))
	for i, pid := range pids {
		strs[i] = strconv.Itoa(pid)
	}
	args := []string{"-nP", "-a", "-p", strings.Join(strs, ","), "-iTCP", "-sTCP:LISTEN", "-Fpnt"}
	out, err := runCommand(ctx, s.timeout, "lsof", args...)
	return classifyLsofResult(out, err, ctx.Err())
}
