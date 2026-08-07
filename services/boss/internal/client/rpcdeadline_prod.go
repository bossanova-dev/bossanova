//go:build !e2e

package client

import "time"

// e2eRPCDeadlineOverride always declines in production builds: the only thing
// that sets a production boss's unary RPC bounds is defaultRPCDeadline and
// slowRPCDeadline (plus the slowProcedures set) in rpcdeadline.go, this file's
// untagged sibling. The e2e-tagged variant in rpcdeadline_e2e.go reads
// BOSS_RPC_DEADLINE_E2E so a proof scenario can watch a wedged daemon's bound
// fire and recover inside one step's timeout budget.
// Keeping the env read behind the build tag means no environment variable can
// shorten — or lengthen — the RPC bound of a boss a user actually runs, so a
// stray export can neither strand the TUI on a hung daemon nor make it give up
// on a daemon that was merely busy.
func e2eRPCDeadlineOverride() (time.Duration, bool) { return 0, false }
