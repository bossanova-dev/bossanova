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
