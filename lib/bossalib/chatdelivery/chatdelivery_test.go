package chatdelivery

import (
	"strings"
	"testing"
)

// TestGuidanceSentencesAreDisjoint is the invariant the whole scheme rests on.
// Surfaces recover a delivery_state their converter dropped by substring-matching
// notice_text against these two constants, and the two call for OPPOSITE actions
// — resend vs do not resend. If either sentence ever became a substring of the
// other, a match would be ambiguous and the guidance printed to the operator
// could be the exact inverse of the truth. That failure is invisible to every
// other test here, because each one feeds a notice built from the constant it
// expects; only this test constrains the pair.
func TestGuidanceSentencesAreDisjoint(t *testing.T) {
	if QueuedGuidance == ResendGuidance {
		t.Fatal("the two guidance sentences are identical; a matcher cannot tell the states apart")
	}
	if strings.Contains(QueuedGuidance, ResendGuidance) {
		t.Error("QueuedGuidance contains ResendGuidance; an unconfirmed matcher would fire on a queued notice")
	}
	if strings.Contains(ResendGuidance, QueuedGuidance) {
		t.Error("ResendGuidance contains QueuedGuidance; a queued matcher would fire on an unconfirmed notice")
	}
}

func TestSplitGuidance(t *testing.T) {
	tests := []struct {
		name         string
		notice       string
		wantDetail   string
		wantGuidance string
	}{
		{
			name:         "queued notice yields the queued guidance",
			notice:       "submit verification timed out for pane boss-c1; " + QueuedGuidance,
			wantDetail:   "submit verification timed out for pane boss-c1",
			wantGuidance: QueuedGuidance,
		},
		{
			name:         "unconfirmed notice yields the resend guidance",
			notice:       "submit verification timed out for pane boss-c1; " + ResendGuidance,
			wantDetail:   "submit verification timed out for pane boss-c1",
			wantGuidance: ResendGuidance,
		},
		{
			// A notice with no guidance is not a delivery whose outcome needs
			// qualifying — a mechanically-handled message, say. The empty
			// guidance is how a caller tells that apart from a delivery, so it
			// must not be filled in with a default.
			name:         "a notice carrying no guidance is returned whole",
			notice:       "  switched to account acme  ",
			wantDetail:   "switched to account acme",
			wantGuidance: "",
		},
		{
			name:         "guidance alone leaves an empty detail",
			notice:       QueuedGuidance,
			wantDetail:   "",
			wantGuidance: QueuedGuidance,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detail, guidance := SplitGuidance(tc.notice)
			if detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", detail, tc.wantDetail)
			}
			if guidance != tc.wantGuidance {
				t.Errorf("guidance = %q, want %q", guidance, tc.wantGuidance)
			}
		})
	}
}

// TestSplitResendGuidance_LeavesQueuedNoticeWhole pins the narrowing that makes
// the older, resend-only callers safe. Such a caller renders whatever guidance it
// is handed under a "check the pane before resending" framing; handing it the
// queued sentence would tell an operator to resend a message the agent already
// holds, which runs it twice. Returning the notice whole with an empty guidance
// makes it fall through to the generic rendering instead.
func TestSplitResendGuidance_LeavesQueuedNoticeWhole(t *testing.T) {
	notice := "submit verification timed out for pane boss-c1; " + QueuedGuidance

	detail, guidance := SplitResendGuidance(notice)
	if guidance != "" {
		t.Errorf("guidance = %q, want empty: a resend-only caller must not claim a queued notice", guidance)
	}
	if detail != notice {
		t.Errorf("detail = %q, want the whole notice %q", detail, notice)
	}

	// The unconfirmed case it does own still splits.
	unconfirmed := "submit verification timed out for pane boss-c1; " + ResendGuidance
	detail, guidance = SplitResendGuidance(unconfirmed)
	if guidance != ResendGuidance {
		t.Errorf("guidance = %q, want ResendGuidance", guidance)
	}
	if detail != "submit verification timed out for pane boss-c1" {
		t.Errorf("detail = %q, want the leading detail with its separator trimmed", detail)
	}
}
