package attention_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/recurser/bossalib/attention"
)

func TestVerifyHoldLabels(t *testing.T) {
	labels := attention.VerifyHoldLabels()
	if !slices.IsSorted(labels) || len(slices.Compact(slices.Clone(labels))) != len(labels) {
		t.Fatalf("labels not sorted and distinct: %v", labels)
	}
	for _, reason := range []string{"ticket-not-in-review", "draft", "attached", "ambiguous-pr", "claimed", "no-session", "session-has-ticket", "brand-new-code", ""} {
		in := unverifiedPR()
		in.IsFactoryRepo = true
		in.VerifyAdmission = attention.Admission{State: attention.AdmissionPending, Reason: reason}
		label := attention.Compute(in).GetProgressLabel()
		if !attention.IsVerifyHoldLabel(label) || !slices.Contains(labels, label) || strings.Contains(label, "outside verify") {
			t.Fatalf("reason %q: unexpected hold label %q", reason, label)
		}
	}
	for _, label := range labels {
		if attention.IsWaitLabel(label) || label == attention.VerifyUnknownLabel {
			t.Errorf("hold label collides with another progress label: %q", label)
		}
	}
	for _, label := range []string{"outside verify", "outside verify: draft", "outside verify: ticket not in review"} {
		if !attention.IsVerifyHoldLabel(label) {
			t.Errorf("legacy label %q not recognised", label)
		}
	}
	for _, label := range []string{"queued", "verify unknown", "working", "idle", "main red", "tests busy", "stopped", "waiting", "initializing", "checking 1/2", "outside verify unrelated", "outside verify:draft"} {
		if attention.IsVerifyHoldLabel(label) {
			t.Errorf("other label %q recognised as hold", label)
		}
	}
}

func TestCompute_VerifyAdmissionLabels_Sessions_AC105(t *testing.T) {
	for _, reason := range []string{"", "brand-new-code"} {
		in := unverifiedPR()
		in.IsFactoryRepo = true
		in.VerifyAdmission = attention.Admission{State: attention.AdmissionPending, Reason: reason}
		if got := attention.Compute(in).GetProgressLabel(); got != attention.VerifyHoldFallbackLabel {
			t.Fatalf("reason %q: got %q", reason, got)
		}
	}
}
