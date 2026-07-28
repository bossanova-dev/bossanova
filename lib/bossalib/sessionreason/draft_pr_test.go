package sessionreason

import (
	"errors"
	"testing"
)

func TestDraftPRCreationFailure(t *testing.T) {
	reason := DraftPRCreationFailure(errors.New("create draft PR: authentication required"))
	if reason == "" {
		t.Fatal("DraftPRCreationFailure() = empty string")
	}
	if !IsDraftPRCreationFailure(&reason) {
		t.Fatalf("IsDraftPRCreationFailure(%q) = false, want true", reason)
	}
}

// TestDraftPRCreationInFlightIsDistinctFromFailure pins the BOS-540 split: the
// in-flight marker must never satisfy IsDraftPRCreationFailure, because that
// predicate is what renders the "? PR failed" display label.
func TestDraftPRCreationInFlightIsDistinctFromFailure(t *testing.T) {
	inFlight := DraftPRCreationInFlight()
	if inFlight == "" {
		t.Fatal("DraftPRCreationInFlight() = empty string")
	}
	if !IsDraftPRCreationInFlight(&inFlight) {
		t.Fatalf("IsDraftPRCreationInFlight(%q) = false, want true", inFlight)
	}
	if IsDraftPRCreationFailure(&inFlight) {
		t.Fatalf("IsDraftPRCreationFailure(%q) = true, want false", inFlight)
	}

	failure := DraftPRCreationFailure(errors.New("boom"))
	if IsDraftPRCreationInFlight(&failure) {
		t.Fatalf("IsDraftPRCreationInFlight(%q) = true, want false", failure)
	}
	if IsDraftPRCreationInFlight(nil) {
		t.Fatal("IsDraftPRCreationInFlight(nil) = true, want false")
	}
}

// TestDraftPRCreationInFlightWireValue pins the literal because it is mirrored
// outside Go: services/web/src/sessionStatus.ts filters exactly this string out
// of the session warning hints, which otherwise render every healthy session
// create as a red error for the duration of the background create. There is no
// generated constant shared across that boundary, so changing this string means
// changing the web copy in the same commit — this test is the reminder.
func TestDraftPRCreationInFlightWireValue(t *testing.T) {
	const wantMirroredInWeb = "draft PR creation in progress"
	if got := DraftPRCreationInFlight(); got != wantMirroredInWeb {
		t.Fatalf("DraftPRCreationInFlight() = %q, want %q; update DRAFT_PR_IN_FLIGHT_REASON in services/web/src/sessionStatus.ts to match", got, wantMirroredInWeb)
	}
}

func TestIsDraftPRCreationFailure(t *testing.T) {
	reason := "CI failed"
	if IsDraftPRCreationFailure(&reason) {
		t.Fatalf("IsDraftPRCreationFailure(%q) = true, want false", reason)
	}
	if IsDraftPRCreationFailure(nil) {
		t.Fatal("IsDraftPRCreationFailure(nil) = true, want false")
	}
}
