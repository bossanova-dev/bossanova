package session

import (
	"fmt"
	"time"
)

// AutoRotateNotice renders the in-chat notice for an automatic account rotation
// (Epic 4.3, decision D4): "switched to <label> — previous account resets
// ~15:04". A zero resetAt (the usage-limit banner carried no parseable reset
// time) omits the reset clause. The notice is informational only — the
// interrupted prompt is NEVER auto-resent (D12); the user reads this and decides
// what, if anything, to redo.
func AutoRotateNotice(label string, resetAt time.Time) string {
	if resetAt.IsZero() {
		return fmt.Sprintf("switched to %s — previous account usage-limited", label)
	}
	return fmt.Sprintf("switched to %s — previous account resets ~%s",
		label, resetAt.Local().Format("15:04"))
}
