package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/bossalib/attention"
	"github.com/recurser/bossalib/displaystatus"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// servedVerifyPark is a verify park as bossd serves it for codes: the display
// composite, the verify-park summary and the attention all come from the
// producers, so the row renders whatever tier they decide.
func servedVerifyPark(codes ...string) *pb.Session {
	s := &pb.Session{
		Id: "park", Title: "Parked change", State: pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		DisplayStatus:   pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN,
		AttentionStatus: &pb.AttentionStatus{NeedsAttention: true, Reason: pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT, Summary: displaystatus.VerifyParkSummary(strings.Join(codes, ","))},
	}
	out := displaystatus.Compute(displaystatus.Input{Session: s, ParkCodes: codes})
	s.DisplayLabel, s.DisplayIntent, s.DisplaySpinner = out.Label, out.Intent, out.Spinner
	s.Attention = attention.Compute(attention.Input{HasPR: true, PRState: attention.PRStateOpen, ParkCodes: codes})
	return s
}

// TestHomeVerifyParkTierByMix renders the parks the operator saw read red
// "blocked" on green PRs, which only need a person's look, and two parks that
// are still blocked. A look-only park reads the amber "review" cell, its hint
// is amber and starts "review", and nothing on the row says "needs sign-off";
// a blocked park keeps the red "blocked" cell and a red hint.
func TestHomeVerifyParkTierByMix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		codes   []string
		blocked bool
	}{
		{"#3162 no-receipt + UI", []string{"no-receipt", "human-review:ui"}, false},
		{"#3184 UI + unproven", []string{"human-review:ui", "human-review:unproven"}, false},
		{"#3189 unproven", []string{"human-review:unproven"}, false},
		{"no-receipt", []string{"no-receipt"}, false},
		{"open checklist + no-receipt", []string{"ledger-open", "no-receipt"}, true},
		{"repair exhausted + UI", []string{"repair-exhausted", "human-review:ui"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHomeModel(nil, context.Background(), nil)
			h.Width, h.Height = 160, 30
			s := servedVerifyPark(tc.codes...)
			wantCell, wantHint, wantPrefix := ui.StyleStatusWarning.Render("review"), ui.StyleStatusWarning, "review: "
			if tc.blocked {
				wantCell, wantHint, wantPrefix = ui.StyleStatusDanger.Render("blocked"), ui.StyleStatusDanger, "blocked: "
			}
			if got := h.RenderSessionStatus(s); got != wantCell {
				t.Errorf("status cell = %q, want %q", got, wantCell)
			}
			if got := ui.RenderDisplayStatus(s, h.Spinner); got != wantCell {
				t.Errorf("display status cell = %q, want %q", got, wantCell)
			}
			var park *ui.SessionHint
			for _, hint := range ui.SessionWarningHints(s) {
				if strings.HasPrefix(hint.Text, "review: ") || strings.HasPrefix(hint.Text, "blocked: ") {
					park = &hint
				}
			}
			if park == nil || !strings.HasPrefix(park.Text, wantPrefix) {
				t.Fatalf("park hint = %v, want one starting %q", park, wantPrefix)
			}
			if got := ui.WarningHintStyle(s, *park).Render(park.Text); got != wantHint.Render(park.Text) {
				t.Errorf("park hint %q styled %q, want %q", park.Text, got, wantHint.Render(park.Text))
			}
			h.Sessions = []*pb.Session{s}
			h.BuildTableRows()
			if screen := ansi.Strip(h.Table.View()); strings.Contains(screen, displaystatus.NeedsSignOffLabel) {
				t.Errorf("row shows %q:\n%s", displaystatus.NeedsSignOffLabel, screen)
			}
		})
	}
}
