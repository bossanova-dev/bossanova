// Package sessionreason owns structured helpers for session blocked reasons.
package sessionreason

import "strings"

const draftPRCreationFailurePrefix = "draft PR creation failed: "

// DraftPRCreationFailure formats the persisted blocked reason for a failed
// draft PR creation attempt.
func DraftPRCreationFailure(err error) string {
	if err == nil {
		return ""
	}
	return draftPRCreationFailurePrefix + err.Error()
}

// IsDraftPRCreationFailure reports whether a blocked reason was produced by
// DraftPRCreationFailure.
func IsDraftPRCreationFailure(reason *string) bool {
	return reason != nil && strings.HasPrefix(*reason, draftPRCreationFailurePrefix)
}

// draftPRCreationInFlightReason marks a session whose draft PR is being created
// right now by the background step StartSession spawns (BOS-540). It is a
// deliberately distinct string from DraftPRCreationFailure so the two states are
// never confused: "one is coming" must not read as "this failed". Only
// IsDraftPRCreationFailure gates the "? PR failed" display label, so an in-flight
// marker never renders as a failure.
const draftPRCreationInFlightReason = "draft PR creation in progress"

// DraftPRCreationInFlight is the reason persisted while the background draft PR
// create is running, so a user who does not yet see a PR knows one is coming.
// It is cleared when the PR lands, or replaced by DraftPRCreationFailure when
// the create fails.
func DraftPRCreationInFlight() string {
	return draftPRCreationInFlightReason
}

// IsDraftPRCreationInFlight reports whether a blocked reason is the in-flight
// marker written by DraftPRCreationInFlight.
func IsDraftPRCreationInFlight(reason *string) bool {
	return reason != nil && *reason == draftPRCreationInFlightReason
}
