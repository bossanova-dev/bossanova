package sessionreason

import "fmt"

// FinalizeFailure formats the persisted blocked reason for a finalize
// pipeline failure. outcome is the models.CronJobOutcome string.
func FinalizeFailure(outcome string, err error) string {
	if err == nil {
		return fmt.Sprintf("finalize failed (%s)", outcome)
	}
	return fmt.Sprintf("finalize failed (%s): %s", outcome, err.Error())
}

// FinalizeRecovered is the blocked reason for a session left stuck in
// Finalizing after a daemon restart.
func FinalizeRecovered() string {
	return "interrupted during finalize after daemon restart"
}

// FixLoopExhausted is the blocked reason for a genuine fix-loop exhaustion
// (MaxAttempts reached). This preserves the original wording for the real
// case so the generic Blocked default no longer claims a fix loop ran.
func FixLoopExhausted() string {
	return "fix loop exhausted, needs human intervention"
}
