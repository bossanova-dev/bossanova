package views

import (
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
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

// TestRepairFailureHint_RetryInSuffix covers the "retry in ~Xm" tail
// that surfaces the exponential-backoff window. Without this suffix
// the operator sees "repair failed (5×)" with no clue why no new
// attempt is firing — the wait could be 16m and the next sweep is in
// 1m, but the UI doesn't show that.
func TestRepairFailureHint_RetryInSuffix(t *testing.T) {
	// Attempt #3 → wait 4m. LastRepairStartedAt 1m ago → 3m remaining.
	startedAt := time.Now().Add(-1 * time.Minute)
	sess := &pb.Session{
		LastRepairAttemptCount: 3,
		LastRepairExitError:    "exit status 1",
		LastRepairStartedAt:    timestamppb.New(startedAt),
	}
	got := repairFailureHint(sess)
	// Allow slight skew from time.Now() between test and func: assert
	// the prefix and a 3m-ish suffix.
	wantPrefix := "⚠ repair failed (3×), retry in ~"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("repairFailureHint = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.HasSuffix(got, "m") {
		t.Errorf("repairFailureHint retry suffix should round to minutes, got %q", got)
	}
}

// TestRepairFailureHint_NoRetryInWhenElapsed covers the case where the
// backoff window has already elapsed — the next sweep will fire
// imminently and adding "retry in ~0m" would be noise. The hint
// degrades to the base "repair failed (N×)" label.
func TestRepairFailureHint_NoRetryInWhenElapsed(t *testing.T) {
	// Attempt #2 → wait 2m. Started 5m ago → window long elapsed.
	startedAt := time.Now().Add(-5 * time.Minute)
	sess := &pb.Session{
		LastRepairAttemptCount: 2,
		LastRepairExitError:    "exit status 1",
		LastRepairStartedAt:    timestamppb.New(startedAt),
	}
	got := repairFailureHint(sess)
	want := "⚠ repair failed (2×)"
	if got != want {
		t.Errorf("repairFailureHint = %q, want %q", got, want)
	}
}

// TestRepairRetryRemaining pins the schedule that mirrors the repair
// plugin's cooldownFor(). If the two ever drift, the TUI estimate will
// silently mislead operators about when the next attempt will fire.
func TestRepairRetryRemaining(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		count     int32
		startedAt time.Time
		wantMin   time.Duration // wait derived for that count
	}{
		{"count 0 returns 0", 0, now, 0},
		{"count 1 waits 1m", 1, now, time.Minute},
		{"count 2 waits 2m", 2, now, 2 * time.Minute},
		{"count 3 waits 4m", 3, now, 4 * time.Minute},
		{"count 4 waits 8m", 4, now, 8 * time.Minute},
		{"count 5 waits 16m", 5, now, 16 * time.Minute},
		{"count 6 caps at 30m", 6, now, 30 * time.Minute},
		{"count 1000 still 30m", 1000, now, 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := repairRetryRemaining(tc.count, tc.startedAt)
			// `repairRetryRemaining` subtracts time.Since(startedAt) — for
			// startedAt=now that's ~0, so got ≈ wantMin within a few ms.
			if tc.wantMin == 0 {
				if got != 0 {
					t.Errorf("got %s, want 0", got)
				}
				return
			}
			diff := tc.wantMin - got
			if diff < 0 {
				diff = -diff
			}
			if diff > time.Second {
				t.Errorf("got %s, want ~%s (diff %s)", got, tc.wantMin, diff)
			}
		})
	}
}
