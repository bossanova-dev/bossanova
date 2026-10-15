package attention_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// verifyGateHumanReasons is skills-toolbox/verify-gate.mjs HUMAN_REASONS, split
// by what each asks of the operator: the policy codes ask for a sign-off on
// work verify passed, the rest report a failure or a gap.
var (
	signOffCodes = []string{"always-human-path", "human-review:database", "human-review:security", "human-review:ui"}
	failureCodes = []string{"no-receipt", "repair-exhausted", "ledger-open", "extension-failed", "no-session", "worktree-diverged", "worktree-unsynced", "human-review:unproven"}
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

// TestComputeAnswersAParkByWhatItAsks checks that a park asking only for a
// sign-off is a REVIEW offering APPROVE, and that a park with any failure code,
// alone or beside every sign-off code, is a blocked FIX that never offers
// APPROVE.
func TestComputeAnswersAParkByWhatItAsks(t *testing.T) {
	approves := func(a *pb.SessionAttention) bool {
		return slices.Contains(a.GetActions(), pb.AttentionAction_ATTENTION_ACTION_APPROVE)
	}
	for _, code := range signOffCodes {
		got := attention.Compute(attention.Input{ParkCodes: []string{code}})
		if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW || !approves(got) {
			t.Errorf("sign-off park %q = %v %v, want REVIEW offering APPROVE", code, got.GetKind(), got.GetActions())
		}
	}
	for _, failure := range append(slices.Clone(failureCodes), "brand-new") {
		for _, park := range [][]string{{failure}, append(slices.Clone(signOffCodes), failure)} {
			got := attention.Compute(attention.Input{ParkCodes: park})
			if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || approves(got) {
				t.Errorf("blocked park %q = %v %v, want FIX without APPROVE", park, got.GetKind(), got.GetActions())
			}
			if !slices.Contains(got.GetActions(), pb.AttentionAction_ATTENTION_ACTION_SEND_BACK) {
				t.Errorf("blocked park %q actions %v lack SEND_BACK", park, got.GetActions())
			}
			if strings.Contains(got.GetSentence(), "approve") {
				t.Errorf("blocked park %q sentence %q asks for approval", park, got.GetSentence())
			}
			if got.GetAlertKey() != "" || got.GetTargetChatId() != "" {
				t.Errorf("blocked park %q carries fix alert fields (%q, %q)", park, got.GetAlertKey(), got.GetTargetChatId())
			}
		}
	}
}
