package sessionreason

import (
	"errors"
	"strings"
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

// TestDraftPRCreationTransientFailureStillIsAFailure pins the BOS-877
// compatibility invariant. The transient form NESTS inside the terminal
// prefix, so every existing consumer of IsDraftPRCreationFailure —
// displaystatus's "? PR failed" label, the TUI hint, the apiversion
// down-convert transform — keeps matching it. A sibling prefix would have made
// all of them silently stop matching on exactly the failures the operator most
// needs to see.
func TestDraftPRCreationTransientFailureStillIsAFailure(t *testing.T) {
	reason := DraftPRCreationTransientFailure(errors.New("git@github.com: Permission denied (publickey)"))
	if reason == "" {
		t.Fatal("DraftPRCreationTransientFailure() = empty string")
	}
	if !IsDraftPRCreationFailure(&reason) {
		t.Fatalf("IsDraftPRCreationFailure(%q) = false, want true: the transient form must nest inside the terminal prefix", reason)
	}
	if !IsDraftPRCreationTransientFailure(&reason) {
		t.Fatalf("IsDraftPRCreationTransientFailure(%q) = false, want true", reason)
	}
	if !strings.Contains(reason, "Permission denied (publickey)") {
		t.Fatalf("transient reason dropped the underlying error: %q", reason)
	}
}

// TestDraftPRCreationTransientFailureNilError pins the nil contract, which
// matches DraftPRCreationFailure's: a nil error produces no reason at all
// rather than a marker with an empty tail.
func TestDraftPRCreationTransientFailureNilError(t *testing.T) {
	if got := DraftPRCreationTransientFailure(nil); got != "" {
		t.Fatalf("DraftPRCreationTransientFailure(nil) = %q, want empty", got)
	}
}

// TestIsDraftPRCreationTransientFailureRejectsTheOtherForms is the negative half
// of the BOS-877 split: only the transient constructor's output may satisfy the
// predicate. A terminal failure must stay terminal (the operator really does
// need to read the raw error), and the in-flight marker is not a failure at all.
func TestIsDraftPRCreationTransientFailureRejectsTheOtherForms(t *testing.T) {
	terminal := DraftPRCreationFailure(errors.New("ERROR: Repository not found"))
	if IsDraftPRCreationTransientFailure(&terminal) {
		t.Fatalf("IsDraftPRCreationTransientFailure(%q) = true, want false", terminal)
	}
	inFlight := DraftPRCreationInFlight()
	if IsDraftPRCreationTransientFailure(&inFlight) {
		t.Fatalf("IsDraftPRCreationTransientFailure(%q) = true, want false", inFlight)
	}
	if IsDraftPRCreationTransientFailure(nil) {
		t.Fatal("IsDraftPRCreationTransientFailure(nil) = true, want false")
	}
	unrelated := "CI failed"
	if IsDraftPRCreationTransientFailure(&unrelated) {
		t.Fatalf("IsDraftPRCreationTransientFailure(%q) = true, want false", unrelated)
	}
}

// TestDraftPRCreationTransientMarkerIsASentenceFragment guards the risk the plan
// calls out: a marker short enough to occur at the start of a real git error
// would misfire. The predicate anchors the marker immediately after the failure
// prefix, which removes the mid-sentence half of that risk outright (the last
// corpus entry below is the pin for it); the leading-position half remains, and
// a full fragment with the em dash is what bounds it. Keep it long.
func TestDraftPRCreationTransientMarkerIsASentenceFragment(t *testing.T) {
	const want = "GitHub was temporarily unreachable — retrying: "
	if transientMarker != want {
		t.Fatalf("transientMarker = %q, want %q; shortening it widens the Contains match", transientMarker, want)
	}
	// The equality above is only a change detector — it re-asserts the literal
	// against itself and would still pass if the marker were a word long. These
	// assert the property that actually bounds the misfire: no error text a
	// terminal draft-PR failure can carry may contain the marker, or a TERMINAL
	// reason built from it would answer true to IsDraftPRCreationTransientFailure.
	//
	// The corpus is deliberately adversarial — it includes the wordings closest to
	// the marker's own vocabulary (GitHub, unreachable, retrying) as well as the
	// real stderr from the 2026-08-13 degradation that motivated BOS-877.
	for _, msg := range []string{
		"ERROR: Repository not found.",
		"ssh: Permission denied",
		"exit status 128: git@github.com: Permission denied (publickey)",
		"fatal: unable to access 'https://github.com/o/r.git': Could not resolve host: github.com",
		"gh: GitHub is unreachable, retrying",
		"GitHub was temporarily unreachable, retrying",
		"pull request create failed: retrying: GitHub was temporarily unreachable",
		"remote: GitHub found 1 vulnerability; retrying is not required",
		// The discriminating case: the marker verbatim, but mid-error rather than
		// where the constructor writes it. An unanchored Contains test accepts this
		// as transient and hides the raw error from the operator; the anchored
		// HasPrefix test refuses it.
		"gh pr create: remote said: GitHub was temporarily unreachable — retrying: giving up",
	} {
		reason := DraftPRCreationFailure(errors.New(msg))
		if IsDraftPRCreationTransientFailure(&reason) {
			t.Errorf("a TERMINAL reason built from %q answers IsDraftPRCreationTransientFailure = true; the marker is short enough to occur inside a real error", msg)
		}
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
