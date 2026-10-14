package attention

import "strings"

// Alert keys of the fix rows bossd derives from session facts, matched against
// Input.DismissedAlertKey. A key names the kind of fault, not one occurrence,
// so the same fault recurring stays dismissed while a different one shows
// again (the rule resumepolicy.AlertKey follows for resume alerts).
const (
	// RepairFailedAlertKey is an automatic repair that ran and failed.
	RepairFailedAlertKey = "repair:failed"
	// RepairRefusedAlertKey is an automatic repair the daemon refused to start.
	RepairRefusedAlertKey = "repair:refused"
	// BlockedAlertKey is a session parked in the Blocked state.
	BlockedAlertKey = "blocked"
)

// RotationAlertKey is the key of a rotation incident the daemon gave up on:
// "rotation:<trigger>", the trigger being the RotationTrigger enum name with
// its ROTATION_TRIGGER_ prefix dropped, lower-cased (for example
// "rotation:auth_invalidated").
func RotationAlertKey(trigger string) string {
	return "rotation:" + strings.ToLower(strings.TrimPrefix(strings.TrimSpace(trigger), "ROTATION_TRIGGER_"))
}
