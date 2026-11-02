package config

import "time"

// PRWaitDefault is how long a PR wait (boss pr wait, WaitPRState) blocks when
// the caller names no timeout.
const PRWaitDefault = 5 * time.Minute

// PRWaitCeiling caps one PR wait. It stays under the 570 s chunk ci-wait
// already fits into one agent tool call, so a wait plus the client's grace
// still fits a 10-minute agent Bash call.
const PRWaitCeiling = 8 * time.Minute

// PRWaitTimeout resolves a requested PR wait timeout in seconds: 0 (or less)
// is PRWaitDefault and anything above PRWaitCeiling is the ceiling. bossd and
// the boss client both apply it, so the client's deadline matches the wait the
// daemon runs.
func PRWaitTimeout(seconds int32) time.Duration {
	if seconds <= 0 {
		return PRWaitDefault
	}
	return min(time.Duration(seconds)*time.Second, PRWaitCeiling)
}
