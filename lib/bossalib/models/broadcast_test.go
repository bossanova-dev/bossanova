package models

import (
	"errors"
	"strings"
	"testing"
)

// TestBroadcastStateVocabularyIsComplete pins the claim BroadcastStates makes
// about itself: that it is the single list every validation path derives from.
// A constant declared without being appended to the list would be rejected by
// Valid and unparseable by ParseBroadcastState — the store would then fail to
// scan a row it wrote itself. Failure is loud rather than silent, so this is a
// fast signal rather than a safety net; it also pins that the list carries no
// duplicates and that every member round-trips through its parser.
func TestBroadcastStateVocabularyIsComplete(t *testing.T) {
	declared := map[string]bool{}
	for _, s := range BroadcastStates() {
		if declared[s.String()] {
			t.Errorf("BroadcastStates lists %q twice", s)
		}
		declared[s.String()] = true
	}
	for _, want := range []BroadcastState{
		BroadcastStatePending,
		BroadcastStateResolved,
		BroadcastStateCompleted,
		BroadcastStateExpired,
		BroadcastStateCanceled,
	} {
		if !declared[want.String()] {
			t.Errorf("BroadcastStates is missing the %q constant", want)
		}
		if !want.Valid() {
			t.Errorf("%q.Valid() = false, want true", want)
		}
		got, err := ParseBroadcastState(want.String())
		if err != nil {
			t.Errorf("ParseBroadcastState(%q): %v", want, err)
		}
		if got != want {
			t.Errorf("ParseBroadcastState(%q) = %q, want round trip", want, got)
		}
	}
}

func TestBroadcastDeliveryStateVocabularyIsComplete(t *testing.T) {
	declared := map[string]bool{}
	for _, s := range BroadcastDeliveryStates() {
		if declared[s.String()] {
			t.Errorf("BroadcastDeliveryStates lists %q twice", s)
		}
		declared[s.String()] = true
	}
	for _, want := range []BroadcastDeliveryState{
		BroadcastDeliveryStatePending,
		BroadcastDeliveryStateLeased,
		BroadcastDeliveryStateDelivered,
		BroadcastDeliveryStateFailed,
		BroadcastDeliveryStateSkipped,
	} {
		if !declared[want.String()] {
			t.Errorf("BroadcastDeliveryStates is missing the %q constant", want)
		}
		if !want.Valid() {
			t.Errorf("%q.Valid() = false, want true", want)
		}
		got, err := ParseBroadcastDeliveryState(want.String())
		if err != nil {
			t.Errorf("ParseBroadcastDeliveryState(%q): %v", want, err)
		}
		if got != want {
			t.Errorf("ParseBroadcastDeliveryState(%q) = %q, want round trip", want, got)
		}
	}
}

// TestParseBroadcastStatesRejectUnknown asserts membership is exact — no
// trimming, no case folding — so the stored value is always canonical, and that
// the error names the offending input.
func TestParseBroadcastStatesRejectUnknown(t *testing.T) {
	for _, bad := range []string{"", "Pending", " pending", "pending ", "quantum"} {
		got, err := ParseBroadcastState(bad)
		if !errors.Is(err, ErrUnknownBroadcastState) {
			t.Errorf("ParseBroadcastState(%q) err = %v, want ErrUnknownBroadcastState", bad, err)
		}
		if got != "" {
			t.Errorf("ParseBroadcastState(%q) = %q, want the zero value on error", bad, got)
		}
		if err != nil && !strings.Contains(err.Error(), bad) {
			t.Errorf("ParseBroadcastState(%q) err = %q, want it to name the input", bad, err)
		}
	}
	for _, bad := range []string{"", "Leased", " leased", "tachyon"} {
		got, err := ParseBroadcastDeliveryState(bad)
		if !errors.Is(err, ErrUnknownBroadcastDeliveryState) {
			t.Errorf("ParseBroadcastDeliveryState(%q) err = %v, want ErrUnknownBroadcastDeliveryState", bad, err)
		}
		if got != "" {
			t.Errorf("ParseBroadcastDeliveryState(%q) = %q, want the zero value on error", bad, got)
		}
	}
}

// TestBroadcastStatesIsNotAliased guards against the accessor handing out the
// package's own backing array: a caller mutating the returned slice must not be
// able to corrupt the vocabulary for everyone else.
func TestBroadcastStatesIsNotAliased(t *testing.T) {
	first := BroadcastStates()
	first[0] = BroadcastState("clobbered")
	if !BroadcastStatePending.Valid() {
		t.Error("mutating the returned slice broke BroadcastStates for other callers")
	}

	firstDelivery := BroadcastDeliveryStates()
	firstDelivery[0] = BroadcastDeliveryState("clobbered")
	if !BroadcastDeliveryStatePending.Valid() {
		t.Error("mutating the returned slice broke BroadcastDeliveryStates for other callers")
	}
}
