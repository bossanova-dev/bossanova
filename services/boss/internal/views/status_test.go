package views

import (
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestRenderDisplayStatus_ReadsCompositeFields verifies that the new direct
// renderer reads DisplayLabel/DisplayIntent/DisplaySpinner from the session and
// produces output styled by intent — no recomposition on the client.
func TestRenderDisplayStatus_ReadsCompositeFields(t *testing.T) {
	sp := newStatusSpinner()

	cases := []struct {
		name         string
		sess         *pb.Session
		wantContains string // visible label substring (after stripping ANSI)
		wantStyle    string // ANSI prefix from styleForIntent
		wantSpinner  bool
	}{
		{
			name: "success/passing label",
			sess: &pb.Session{
				DisplayLabel:   "✓ passing",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
				DisplaySpinner: false,
			},
			wantContains: "✓ passing",
			wantSpinner:  false,
		},
		{
			name: "warning/idle label",
			sess: &pb.Session{
				DisplayLabel:  "idle",
				DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			},
			wantContains: "idle",
		},
		{
			name: "danger/failing label",
			sess: &pb.Session{
				DisplayLabel:  "⨯ failing",
				DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER,
			},
			wantContains: "⨯ failing",
		},
		{
			name: "muted/stopped label",
			sess: &pb.Session{
				DisplayLabel:  "stopped",
				DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_MUTED,
			},
			wantContains: "stopped",
		},
		{
			name: "info/running with spinner",
			sess: &pb.Session{
				DisplayLabel:   "running 2/5",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_INFO,
				DisplaySpinner: true,
			},
			wantContains: "running 2/5",
			wantSpinner:  true,
		},
		{
			name: "working with spinner",
			sess: &pb.Session{
				DisplayLabel:   "working",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
				DisplaySpinner: true,
			},
			wantContains: "working",
			wantSpinner:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderDisplayStatus(tc.sess, sp)
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("renderDisplayStatus output missing %q; got %q", tc.wantContains, got)
			}
			spinnerGlyph := sp.View()
			hasSpinner := strings.Contains(got, spinnerGlyph)
			if tc.wantSpinner && !hasSpinner {
				t.Errorf("expected spinner glyph %q in output; got %q", spinnerGlyph, got)
			}
			if !tc.wantSpinner && hasSpinner && spinnerGlyph != "" {
				t.Errorf("did not expect spinner glyph; got %q", got)
			}
		})
	}
}

// TestRenderDisplayStatus_ParityWithStyleForIntent confirms that the rendered
// output is byte-identical to the legacy path's "styleForIntent(intent).Render(label)"
// when no spinner is involved. Guards against accidental ANSI drift.
func TestRenderDisplayStatus_ParityWithStyleForIntent(t *testing.T) {
	sp := newStatusSpinner()
	intents := []pb.DisplayIntent{
		pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
		pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		pb.DisplayIntent_DISPLAY_INTENT_MUTED,
		pb.DisplayIntent_DISPLAY_INTENT_INFO,
		pb.DisplayIntent_DISPLAY_INTENT_UNSPECIFIED,
	}
	for _, intent := range intents {
		sess := &pb.Session{DisplayLabel: "x", DisplayIntent: intent}
		got := renderDisplayStatus(sess, sp)
		want := styleForIntent(intent).Render("x")
		if got != want {
			t.Errorf("intent=%v: got %q want %q", intent, got, want)
		}
	}
}

// TestRenderDisplayStatus_NilSession returns empty for safety.
func TestRenderDisplayStatus_NilSession(t *testing.T) {
	sp := newStatusSpinner()
	if got := renderDisplayStatus(nil, sp); got != "" {
		t.Errorf("expected empty render for nil session, got %q", got)
	}
}

func TestStyledPRStatus_MergedUsesLightCheck(t *testing.T) {
	sp := newStatusSpinner()
	got := styledPRStatus(&pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED}, sp)
	if !strings.Contains(got, "✓ merged") {
		t.Errorf("styledPRStatus output missing light check merged label; got %q", got)
	}
	if strings.Contains(got, "\u2714 merged") {
		t.Errorf("styledPRStatus output contains heavy check merged label; got %q", got)
	}
}

func TestStyledPRStatus_ConflictUsesFailureCross(t *testing.T) {
	sp := newStatusSpinner()
	got := styledPRStatus(&pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CONFLICT}, sp)
	if !strings.Contains(got, "⨯ conflict") {
		t.Errorf("styledPRStatus output missing failure cross conflict label; got %q", got)
	}
}

// TestRepairFailureHint covers the "⚠ repair failed (N×)" warning text
// that flags sessions where Phase 1c's RecordRepairOutcome captured a
// non-empty runner_error or exit_error.
func TestRepairFailureHint(t *testing.T) {
	cases := []struct {
		name string
		sess *pb.Session
		want string
	}{
		{
			name: "no attempts -> no hint",
			sess: &pb.Session{LastRepairAttemptCount: 0},
			want: "",
		},
		{
			name: "clean attempts -> no hint",
			sess: &pb.Session{LastRepairAttemptCount: 5},
			want: "",
		},
		{
			name: "first failed attempt",
			sess: &pb.Session{
				LastRepairAttemptCount: 1,
				LastRepairRunnerError:  "claude not on PATH",
			},
			want: "⚠ repair failed",
		},
		{
			name: "third failed attempt with exit error",
			sess: &pb.Session{
				LastRepairAttemptCount: 3,
				LastRepairExitError:    "exit status 1",
			},
			want: "⚠ repair failed (3×)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repairFailureHint(tc.sess); got != tc.want {
				t.Errorf("repairFailureHint = %q, want %q", got, tc.want)
			}
		})
	}
}
