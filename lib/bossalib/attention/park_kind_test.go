package attention_test

import (
	"slices"
	"testing"

	"github.com/recurser/bossalib/attention"
)

// verifyGateHumanReasons is skills-toolbox/verify-gate.mjs HUMAN_REASONS, split
// by what each asks of the operator: the policy codes ask for a sign-off on
// work verify passed, the rest report a failure or a gap.
var (
	signOffCodes = []string{"always-human-path", "human-review:database", "human-review:security", "human-review:ui"}
	failureCodes = []string{"no-receipt", "repair-exhausted", "ledger-open", "extension-failed", "no-session", "human-review:unproven"}
)

func TestIsSignOffPark(t *testing.T) {
	for _, code := range signOffCodes {
		if !attention.IsSignOffPark([]string{code}) {
			t.Errorf("IsSignOffPark(%q) = false, want a sign-off", code)
		}
	}
	if !attention.IsSignOffPark([]string{"human-review:ui,always-human-path", "human-review:security"}) {
		t.Error("IsSignOffPark(all policy codes) = false")
	}
	for _, code := range failureCodes {
		if attention.IsSignOffPark([]string{code}) {
			t.Errorf("IsSignOffPark(%q) = true, want a failure", code)
		}
		if attention.IsSignOffPark([]string{"human-review:ui", code}) {
			t.Errorf("IsSignOffPark(human-review:ui + %q) = true; one failure makes the park a failure", code)
		}
	}
	for _, raw := range [][]string{nil, {""}, {"brand-new"}, {"human-review:ui,brand-new"}} {
		if attention.IsSignOffPark(raw) {
			t.Errorf("IsSignOffPark(%q) = true; no code or an unknown code is not a sign-off anyone can give", raw)
		}
	}
}

func TestParkFailureCodesKeepReportOrder(t *testing.T) {
	got := attention.ParkFailureCodes([]string{"human-review:ui,extension-failed", "no-receipt", "brand-new"})
	if want := []string{"no-receipt", "extension-failed", "brand-new"}; !slices.Equal(got, want) {
		t.Fatalf("ParkFailureCodes = %v, want %v", got, want)
	}
}

func TestEveryKnownParkCodeHasPlainWords(t *testing.T) {
	unknown := attention.ParkCodePlainWords("brand-new")
	for _, code := range append(slices.Clone(signOffCodes), failureCodes...) {
		if got := attention.ParkCodePlainWords(code); got == "" || got == unknown {
			t.Errorf("ParkCodePlainWords(%q) = %q, want the code's own plain words", code, got)
		}
	}
	if unknown == "" {
		t.Error("an unknown code renders no words")
	}
}
