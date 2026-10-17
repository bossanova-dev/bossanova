package attention

import pb "github.com/recurser/bossalib/gen/bossanova/v1"

// Titles of the timeline steps the daemon writes for the events of a rotation
// incident, after the incident's failure step.
const (
	RotationResumedTitle = "Resumed automatically"
	RotationWaitingTitle = "Waiting for the usage limit to reset"
	RotationStoppedTitle = "Stopped resuming automatically"
)

// rotationFailureTitles title the failure step that opens an incident, keyed by
// the RotationTrigger enum names that form incidents.
var rotationFailureTitles = map[string]string{
	pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED.String(): "Lost its login",
	pb.RotationTrigger_ROTATION_TRIGGER_USAGE_LIMITED.String():    "Hit its usage limit",
}

// rotationGiveUps are the rotation outcomes after which the daemon stopped
// trying: the operator must act. EXHAUSTED is absent because the session
// resumes itself when the limit resets.
var rotationGiveUps = map[string]bool{
	pb.RotationOutcome_ROTATION_OUTCOME_FAILED.String():                          true,
	pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_DISABLED.String():            true,
	pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY.String():       true,
	pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT.String(): true,
	pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED.String():           true,
}

// IsRotationGiveUp reports whether a rotation outcome (its RotationOutcome
// enum name) is one after which the daemon stopped trying.
func IsRotationGiveUp(outcome string) bool {
	return rotationGiveUps[outcome]
}

// RotationFailureTitle titles the failure step of an incident opened by
// trigger (its RotationTrigger enum name); "" for a trigger that opens none,
// such as a manual switch.
func RotationFailureTitle(trigger string) string {
	return rotationFailureTitles[trigger]
}

// RotationOutcomeTitle titles the step an incident event with outcome (its
// RotationOutcome enum name) adds after the failure step; "" when it adds none.
func RotationOutcomeTitle(outcome string) string {
	switch {
	case outcome == pb.RotationOutcome_ROTATION_OUTCOME_ROTATED.String(),
		outcome == pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT.String():
		return RotationResumedTitle
	case outcome == pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED.String():
		return RotationWaitingTitle
	case IsRotationGiveUp(outcome):
		return RotationStoppedTitle
	default:
		return ""
	}
}

// NarratesRotation reports whether a daemon timeline step titled title can be
// the daemon's narration of a rotation event with trigger and outcome (enum
// names): the failure step of the incident the event belongs to, or the step
// its outcome adds. An event outside any incident is narrated by no step.
func NarratesRotation(title, trigger, outcome string) bool {
	failure := RotationFailureTitle(trigger)
	if failure == "" || title == "" {
		return false
	}
	return title == failure || title == RotationOutcomeTitle(outcome)
}
