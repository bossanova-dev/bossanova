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

func TestRenderArchivingStatus_ShowsSpinnerAndLabel(t *testing.T) {
	sp := newStatusSpinner()
	got := renderArchivingStatus(sp)
	if !strings.Contains(got, "archiving") {
		t.Fatalf("expected archiving label, got %q", got)
	}
	if !strings.Contains(got, sp.View()) {
		t.Fatalf("expected spinner frame %q in %q", sp.View(), got)
	}
}

// TestRepairFailureHint covers the "repair failed (N×)" warning text
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
			want: "repair failed",
		},
		{
			name: "third failed attempt with exit error",
			sess: &pb.Session{
				LastRepairAttemptCount: 3,
				LastRepairExitError:    "exit status 1",
			},
			want: "repair failed (3×)",
		},
		{
			name: "passing PR suppresses stale failed attempt",
			sess: &pb.Session{
				DisplayStatus:          pb.DisplayStatus_DISPLAY_STATUS_PASSING,
				LastRepairAttemptCount: 2,
				LastRepairExitError:    "exit status 1",
			},
			want: "",
		},
		{
			name: "merged PR suppresses stale failed attempt",
			sess: &pb.Session{
				DisplayStatus:          pb.DisplayStatus_DISPLAY_STATUS_MERGED,
				LastRepairAttemptCount: 3,
				LastRepairExitError:    "exit status 1",
			},
			want: "",
		},
		{
			name: "checking PR without known errors suppresses stale failed attempt",
			sess: &pb.Session{
				DisplayStatus:          pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
				LastRepairAttemptCount: 2,
				LastRepairExitError:    "exit status 1",
			},
			want: "",
		},
		{
			name: "checking PR with known failures keeps failed attempt",
			sess: &pb.Session{
				DisplayStatus:          pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
				DisplayHasFailures:     true,
				LastRepairAttemptCount: 2,
				LastRepairExitError:    "exit status 1",
			},
			want: "repair failed (2×)",
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

// TestRepairHint covers the blocked-vs-failure precedence in the
// combined repairHint helper (BOS-153 Task 6). A non-empty
// LastRepairBlockedReason always wins because the daemon clears the
// blocked pair on every real repair outcome, so it is the freshest
// repair-related state.
func TestRepairHint(t *testing.T) {
	cases := []struct {
		name string
		sess *pb.Session
		want string
	}{
		{
			name: "blocked_reason_renders_blocked_hint",
			sess: &pb.Session{
				LastRepairBlockedReason: "chat at prompt",
			},
			want: "repair blocked: chat at prompt",
		},
		{
			name: "blocked_takes_precedence_over_failure",
			sess: &pb.Session{
				LastRepairBlockedReason: "chat at prompt",
				LastRepairAttemptCount:  3,
				LastRepairRunnerError:   "exit status 1",
			},
			want: "repair blocked: chat at prompt",
		},
		{
			name: "no_blocked_reason_keeps_failure_hint",
			sess: &pb.Session{
				LastRepairAttemptCount: 2,
				LastRepairExitError:    "exit status 1",
			},
			want: "repair failed (2×)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repairHint(tc.sess); got != tc.want {
				t.Errorf("repairHint = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRepairBlockedHint_LongReasonTruncated verifies the blocked reason
// is truncated to 48 runes plus a trailing ellipsis so a verbose daemon
// message can't blow out the warning block.
func TestRepairBlockedHint_LongReasonTruncated(t *testing.T) {
	reason := strings.Repeat("x", 200)
	sess := &pb.Session{LastRepairBlockedReason: reason}
	got := repairBlockedHint(sess)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected trailing ellipsis, got %q", got)
	}
	body := strings.TrimPrefix(got, "repair blocked: ")
	if body == got {
		t.Fatalf("expected %q prefix, got %q", "repair blocked: ", got)
	}
	// 48 truncated runes + the ellipsis rune = 49.
	if n := len([]rune(body)); n != 49 {
		t.Errorf("truncated reason = %d runes, want 49", n)
	}
}

// TestRepairFailureHint_NoEmoji verifies that repairFailureHint never
// returns a string containing the U+26A0 WARNING SIGN (⚠).
func TestRepairFailureHint_NoEmoji(t *testing.T) {
	sess := &pb.Session{
		LastRepairAttemptCount: 3,
		LastRepairExitError:    "exit status 1",
	}
	got := repairFailureHint(sess)
	if strings.ContainsRune(got, '⚠') {
		t.Errorf("repairFailureHint contains ⚠ emoji: %q", got)
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
	wantPrefix := "repair failed (3×), retry in ~"
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
	want := "repair failed (2×)"
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

// TestAttentionWarningHint verifies that the hint returns the trimmed summary
// directly, without a ⚠ prefix.
func TestAttentionWarningHint(t *testing.T) {
	cases := []struct {
		name string
		sess *pb.Session
		want string
	}{
		{
			name: "nil session -> empty",
			sess: nil,
			want: "",
		},
		{
			name: "no attention -> empty",
			sess: &pb.Session{},
			want: "",
		},
		{
			name: "needs attention with summary",
			sess: &pb.Session{
				AttentionStatus: &pb.AttentionStatus{
					NeedsAttention: true,
					Summary:        "finalize failed: push rejected",
				},
			},
			want: "finalize failed: push rejected",
		},
		{
			name: "strips leading whitespace from summary",
			sess: &pb.Session{
				AttentionStatus: &pb.AttentionStatus{
					NeedsAttention: true,
					Summary:        "  finalize failed  ",
				},
			},
			want: "finalize failed",
		},
		{
			name: "summary that previously had ⚠ prefix is returned as-is trimmed",
			sess: &pb.Session{
				AttentionStatus: &pb.AttentionStatus{
					NeedsAttention: true,
					Summary:        "⚠ something failed",
				},
			},
			want: "⚠ something failed",
		},
		{
			name: "empty summary -> empty hint",
			sess: &pb.Session{
				AttentionStatus: &pb.AttentionStatus{
					NeedsAttention: true,
					Summary:        "",
				},
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := attentionWarningHint(tc.sess)
			if got != tc.want {
				t.Errorf("attentionWarningHint = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAttentionWarningHint_NoEmojiAdded verifies that attentionWarningHint
// never adds a ⚠ emoji to a summary that does not already contain one.
func TestAttentionWarningHint_NoEmojiAdded(t *testing.T) {
	sess := &pb.Session{
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Summary:        "finalize failed: push rejected",
		},
	}
	got := attentionWarningHint(sess)
	if strings.ContainsRune(got, '⚠') {
		t.Errorf("attentionWarningHint added ⚠ emoji unexpectedly: %q", got)
	}
}

// TestSessionWarningHints_Aggregates verifies that sessionWarningHints returns
// repair hint first, then attention hint, and that both are present.
func TestSessionWarningHints_Aggregates(t *testing.T) {
	sess := &pb.Session{
		LastRepairAttemptCount: 2,
		LastRepairExitError:    "exit status 1",
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Summary:        "finalize failed: push rejected",
		},
	}
	got := sessionWarningHints(sess)
	if len(got) != 2 {
		t.Fatalf("sessionWarningHints len = %d, want 2; hints: %v", len(got), got)
	}
	if !strings.Contains(got[0], "repair failed") {
		t.Errorf("first hint should be repair hint, got %q", got[0])
	}
	if !strings.Contains(got[1], "finalize failed") {
		t.Errorf("second hint should be attention hint, got %q", got[1])
	}
}

// TestSelectedSessionWarningBlock_WithHints verifies that the footer block
// contains the full hint text and is non-empty when the session has hints.
func TestSelectedSessionWarningBlock_WithHints(t *testing.T) {
	sess := &pb.Session{
		LastRepairAttemptCount: 1,
		LastRepairExitError:    "exit status 1",
	}
	got := selectedSessionWarningBlock(sess, 80)
	if got == "" {
		t.Fatal("selectedSessionWarningBlock returned empty for session with hints")
	}
	if !strings.Contains(got, "repair failed") {
		t.Errorf("selectedSessionWarningBlock output missing 'repair failed': %q", got)
	}
	if strings.ContainsRune(got, '⚠') {
		t.Errorf("selectedSessionWarningBlock contains ⚠ emoji: %q", got)
	}
}

// TestSelectedSessionWarningBlock_MultipleHints verifies that all hint lines
// appear in the block when a session has both repair and attention hints.
func TestSelectedSessionWarningBlock_MultipleHints(t *testing.T) {
	sess := &pb.Session{
		LastRepairAttemptCount: 2,
		LastRepairExitError:    "exit status 1",
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Summary:        "finalize failed: branch diverged",
		},
	}
	got := selectedSessionWarningBlock(sess, 80)
	if !strings.Contains(got, "repair failed") {
		t.Errorf("block missing repair hint: %q", got)
	}
	if !strings.Contains(got, "finalize failed") {
		t.Errorf("block missing attention hint: %q", got)
	}
}

// TestSelectedSessionWarningBlock_NoHints verifies that the block is empty
// when the session has no hints — no layout shift, no empty block.
func TestSelectedSessionWarningBlock_NoHints(t *testing.T) {
	sess := &pb.Session{
		DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING,
	}
	if got := selectedSessionWarningBlock(sess, 80); got != "" {
		t.Errorf("selectedSessionWarningBlock = %q, want empty for no-hint session", got)
	}
}

// TestSelectedSessionWarningBlock_NilSession verifies that a nil session
// returns an empty block (no panic).
func TestSelectedSessionWarningBlock_NilSession(t *testing.T) {
	if got := selectedSessionWarningBlock(nil, 80); got != "" {
		t.Errorf("selectedSessionWarningBlock = %q, want empty for nil session", got)
	}
}

func TestRenderSelectedText(t *testing.T) {
	got := renderSelectedText("my-repo")
	if !strings.Contains(got, "\x1b[1;38;2;76;167;248m") {
		t.Errorf("expected bold blue open SGR, got %q", got)
	}
	if !strings.Contains(got, "my-repo") {
		t.Errorf("expected visible text preserved, got %q", got)
	}
	if !strings.Contains(got, "\x1b[22;39m") {
		t.Errorf("expected close SGR resetting only bold+fg, got %q", got)
	}
	if renderSelectedText("") != "" {
		t.Errorf("expected empty input to yield empty output")
	}
}

func TestRenderSelectedTrackerLink_WithTrackerAndURL(t *testing.T) {
	url := "https://linear.app/x/issue/BOS-1"
	id := "BOS-1"
	sess := &pb.Session{TrackerId: &id, TrackerUrl: &url}
	got := renderSelectedTrackerLink(sess, "Fix the thing [BOS-1]")

	if !strings.Contains(got, "\x1b[1;38;2;76;167;248m") {
		t.Errorf("expected bold blue text SGR, got %q", got)
	}
	// tracker token underlined with pinned blue underline color
	if !strings.Contains(got, "\x1b[1;38;2;76;167;248;58;2;76;167;248;4m") {
		t.Errorf("expected blue underline SGR on tracker token, got %q", got)
	}
	// OSC 8 hyperlink to the tracker URL is preserved
	if !strings.Contains(got, "\x1b]8;;"+url+"\x1b\\") {
		t.Errorf("expected OSC 8 hyperlink open, got %q", got)
	}
	if !strings.Contains(got, "\x1b]8;;\x1b\\") {
		t.Errorf("expected OSC 8 hyperlink close, got %q", got)
	}
	// no strikethrough (SGR 9) anywhere
	if strings.Contains(got, ";9m") || strings.Contains(got, "[9m") {
		t.Errorf("did not expect strikethrough SGR, got %q", got)
	}
}

func TestRenderSelectedTrackerLink_NoTracker(t *testing.T) {
	sess := &pb.Session{}
	got := renderSelectedTrackerLink(sess, "Plain title")
	if !strings.Contains(got, "\x1b[1;38;2;76;167;248m") {
		t.Errorf("expected bold blue text SGR for plain title, got %q", got)
	}
	if !strings.Contains(got, "Plain title") {
		t.Errorf("expected visible text preserved, got %q", got)
	}
	if strings.Contains(got, "\x1b]8;;") {
		t.Errorf("did not expect an OSC 8 hyperlink for a title with no tracker id, got %q", got)
	}
}

func TestRenderSelectedPRLink(t *testing.T) {
	pr := int32(42)
	url := "https://github.com/x/y/pull/42"
	sess := &pb.Session{PrNumber: &pr, PrUrl: &url}
	got := renderSelectedPRLink(sess)
	if !strings.Contains(got, "#42") {
		t.Errorf("expected PR label, got %q", got)
	}
	if !strings.Contains(got, "\x1b[1;38;2;76;167;248;58;2;76;167;248;4m") {
		t.Errorf("expected bold blue underline SGR, got %q", got)
	}
	if !strings.Contains(got, "\x1b]8;;"+url+"\x1b\\") {
		t.Errorf("expected OSC 8 hyperlink, got %q", got)
	}
	if renderSelectedPRLink(&pb.Session{}) != "" {
		t.Errorf("expected empty output when PrNumber is nil")
	}
}
