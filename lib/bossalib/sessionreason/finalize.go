package sessionreason

import (
	"fmt"
	"strings"
)

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

// RunBlocked is the blocked reason finalize gives a session whose unattended
// run reported that it ended BLOCKED on a person with reason.
func RunBlocked(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "no reason given"
	}
	return "run blocked: " + reason
}

// blockedLabels are the leading labels a sentence that already opens with
// "Run blocked: " would repeat.
var blockedLabels = []string{"run blocked:", "blocked:"}

// WithoutBlockedLabel returns reason trimmed, less every leading "run blocked:"
// or "blocked:" label in any case, so a sentence that prefixes "Run blocked: "
// says it once. A reason that is nothing but labels is returned trimmed.
func WithoutBlockedLabel(reason string) string {
	reason = strings.TrimSpace(reason)
	rest := reason
	for {
		cut := false
		for _, label := range blockedLabels {
			if len(rest) >= len(label) && strings.EqualFold(rest[:len(label)], label) {
				rest, cut = strings.TrimSpace(rest[len(label):]), true
			}
		}
		if !cut {
			break
		}
	}
	if rest == "" {
		return reason
	}
	return rest
}
