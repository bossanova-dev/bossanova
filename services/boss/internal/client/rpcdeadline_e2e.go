//go:build e2e

package client

import (
	"math"
	"os"
	"strings"
	"time"
)

// envRPCDeadlineE2E carries a time.ParseDuration string that replaces
// defaultRPCDeadline for this process. The proof harness sets it (via a fixture
// preset's DefaultEnv) so a scenario can wedge the mock daemon, watch the bound
// fire, and watch the TUI recover — all inside a single step's timeout budget.
const envRPCDeadlineE2E = "BOSS_RPC_DEADLINE_E2E"

// e2eRPCDeadlineOverride resolves the process-wide unary RPC bound from the
// environment. It reports ok=false — leaving the production default in place —
// for anything that is not a strictly positive duration, because a zero or
// negative bound would expire every RPC before it left the process and a
// typo'd value must not silently disarm the bound the ticket exists to add.
//
// The env read lives behind the e2e build tag on purpose (see rpcdeadline_prod.go).
func e2eRPCDeadlineOverride() (time.Duration, bool) {
	return parseE2ERPCDeadline(os.Getenv(envRPCDeadlineE2E))
}

// parseE2ERPCDeadline is the pure half of e2eRPCDeadlineOverride, split out so
// the accept/reject table is testable without mutating process state.
func parseE2ERPCDeadline(raw string) (time.Duration, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, false
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, false
	}
	// The override scales the slow bound by slowDeadlineRatio, and
	// time.ParseDuration happily accepts values near the time.Duration ceiling.
	// Multiplying one of those would wrap negative and expire every slow RPC
	// the instant it was issued — the exact disarming this parser rejects
	// zero and garbage to prevent. Refuse rather than wrap.
	if d > math.MaxInt64/slowDeadlineRatio {
		return 0, false
	}
	return d, true
}
