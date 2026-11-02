package attention_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestVerifyWaitLabel(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{reason: "base-ci-red", want: "main red"},
		{reason: "stack-busy", want: "tests busy"},
		{reason: "merge-state-unknown", want: "waiting: merge"},
		{reason: "merge-state-blocked", want: "waiting: merge"},
		{reason: "merge-state-draft", want: "waiting: draft"},
		{reason: "ci-not-settled", want: "waiting: CI"},
		{reason: "ci-not-green-on-head", want: "waiting: CI"},
		{reason: "ci-absent-gate", want: "waiting: CI"},
		{reason: "ci-not-requested", want: "waiting: CI"},
		{reason: "base-ci-unreadable", want: "waiting: main"},
		{reason: "base-ci-pending", want: "waiting: main"},
		{reason: "receipt-absent", want: "waiting: build"},
		{reason: "pr-draft", want: "waiting: draft"},
		{reason: "pr-unreadable", want: "waiting: PR"},
		{reason: "pr-not-open", want: "waiting: PR"},
		{reason: "head-unknown", want: "waiting: PR"},
		{reason: "head-mismatch", want: "waiting: PR"},
		{reason: "worktree-head-mismatch", want: "waiting: PR"},
		{reason: "review-unreadable", want: "waiting: retry"},
		{reason: "config-unreadable", want: "waiting: retry"},
		{reason: "some-new-code", want: "waiting: retry"},
		{reason: "  merge-state-unknown ", want: "waiting: merge"},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			got := attention.VerifyWaitLabel(tt.reason)
			if got != tt.want {
				t.Fatalf("VerifyWaitLabel(%q) = %q, want %q", tt.reason, got, tt.want)
			}
			if !slices.Contains(attention.VerifyWaitLabels(), got) {
				t.Fatalf("VerifyWaitLabel(%q) = %q, not in VerifyWaitLabels %v", tt.reason, got, attention.VerifyWaitLabels())
			}
		})
	}
}

func TestVerifyWaitLabels(t *testing.T) {
	labels := attention.VerifyWaitLabels()
	if len(labels) == 0 || !slices.IsSorted(labels) || len(slices.Compact(slices.Clone(labels))) != len(labels) {
		t.Fatalf("labels not sorted, distinct and non-empty: %v", labels)
	}
	for _, label := range labels {
		if label == "waiting" || label == attention.VerifyingLabel || label == attention.WaitingForVerifyLabel {
			t.Errorf("wait label %q collides with another label", label)
		}
		if !strings.HasPrefix(label, "waiting") && !slices.Contains(attention.WaitLabels(), label) {
			t.Errorf("wait label %q neither starts with waiting nor is an existing wait label", label)
		}
		if !attention.IsWaitLabel(label) {
			t.Errorf("wait label %q is not styled as a wait by IsWaitLabel", label)
		}
		if !attention.IsVerifyWaitLabel(label) {
			t.Errorf("IsVerifyWaitLabel(%q) = false", label)
		}
	}
	for _, label := range []string{"waiting", "verifying", "queued", "waiting: merge-state-unknown", "working", ""} {
		if attention.IsVerifyWaitLabel(label) {
			t.Errorf("IsVerifyWaitLabel(%q) = true", label)
		}
	}
}

// verifyPendingPR is a factory repo's green PR that verify admitted, whose
// head carries a pending boss/verify status.
func verifyPendingPR(reason string) attention.Input {
	in := unverifiedPR()
	in.IsFactoryRepo = true
	in.VerifyAdmission = attention.Admission{State: attention.AdmissionAdmitted}
	in.VerifyPending = true
	in.VerifyWaitReason = reason
	in.ChatStatus = pb.ChatStatus_CHAT_STATUS_IDLE
	return in
}

func TestCompute_VerifyPendingProgressLabel(t *testing.T) {
	tests := []struct {
		name string
		in   func() attention.Input
		want string
	}{
		{name: "wait with mergeability unknown shows the wait label", in: func() attention.Input {
			in := verifyPendingPR("merge-state-unknown")
			in.Mergeable = nil
			return in
		}, want: "waiting: merge"},
		{name: "wait with mergeability known shows the wait label", in: func() attention.Input { return verifyPendingPR("ci-not-settled") }, want: "waiting: CI"},
		{name: "claim shows verifying", in: func() attention.Input { return verifyPendingPR("") }, want: attention.VerifyingLabel},
		{name: "claim with mergeability unknown shows verifying", in: func() attention.Input {
			in := verifyPendingPR("")
			in.Mergeable = nil
			return in
		}, want: attention.VerifyingLabel},
		{name: "claim without admission shows verifying", in: func() attention.Input {
			in := verifyPendingPR("")
			in.VerifyAdmission = attention.Admission{}
			return in
		}, want: attention.VerifyingLabel},
		{name: "admitted head with no pending status stays queued", in: func() attention.Input {
			in := verifyPendingPR("")
			in.VerifyPending = false
			return in
		}, want: attention.WaitingForVerifyLabel},
		{name: "a live working chat outranks the verify label", in: func() attention.Input {
			in := verifyPendingPR("merge-state-unknown")
			in.ChatStatus = pb.ChatStatus_CHAT_STATUS_WORKING
			return in
		}, want: "working"},
		{name: "a draft PR does not read the verify label", in: func() attention.Input {
			in := verifyPendingPR("merge-state-unknown")
			in.PRDraft = true
			return in
		}, want: "idle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			att := attention.Compute(tt.in())
			if att.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS || att.GetProgressLabel() != tt.want {
				t.Fatalf("got kind %v label %q, want IN_PROGRESS %q", att.GetKind(), att.GetProgressLabel(), tt.want)
			}
		})
	}
}
