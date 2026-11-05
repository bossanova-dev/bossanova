package plugin

import "strings"

const (
	// ChatRunIncompleteError marks an interactive run without explicit completion.
	ChatRunIncompleteError = "interactive chat did not report completion"
	// ChatRunIdleError marks a run stopped by the idle watchdog.
	ChatRunIdleError = "agent went idle without completing"
)

// ChatRunRetryable reports whether the agent never attempted the work, so a
// same-input retry stays allowed. In particular, the idle watchdog (BOS-153)
// shares the incomplete interactive run's retry contract across host and plugin.
func ChatRunRetryable(exitErr string) bool {
	return strings.Contains(exitErr, ChatRunIncompleteError) || strings.Contains(exitErr, ChatRunIdleError)
}
