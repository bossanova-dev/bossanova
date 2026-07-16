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

// TestRenderDisplayStatus_UsageLimitedLabel guards the no-client-change path for
// BOS-167: the session badge renders the backend-computed usage-limited label
// (with reset ETA) in the warn style straight from the composite display
// fields, so surfacing the limited state needed no client logic change.
func TestRenderDisplayStatus_UsageLimitedLabel(t *testing.T) {
	sp := newStatusSpinner()
	const label = "usage-limited (resets ~15:00)"
	sess := &pb.Session{
		DisplayLabel:  label,
		DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
	}
	got := renderDisplayStatus(sess, sp)
	if !strings.Contains(got, label) {
		t.Errorf("renderDisplayStatus missing %q; got %q", label, got)
	}
	if want := styleStatusWarning.Render(label); got != want {
		t.Errorf("usage-limited label not warn-styled: got %q want %q", got, want)
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

// TestSessionWarningHintsIncludesSetupError verifies that a non-empty
// SetupError on the session surfaces a "setup script failed" hint (BOS-…
// tmux launch/attach failure capture, Task 7). SetupError is a plain
// proto3 string field (not a pointer), so it is set directly rather than
// via proto.String.
func TestSessionWarningHintsIncludesSetupError(t *testing.T) {
	sess := &pb.Session{SetupError: "npm install failed: ENOENT"}
	hints := sessionWarningHints(sess)
	joined := strings.Join(hints, "\n")
	if !strings.Contains(joined, "setup script failed") {
		t.Fatalf("hints missing setup-error surface; got %q", joined)
	}
	if !strings.Contains(joined, "npm install failed: ENOENT") {
		t.Fatalf("hints missing setup-error detail; got %q", joined)
	}
}

// TestSessionWarningHintsIncludesSetupError_FirstLineOnly verifies a
// multi-line SetupError is truncated to its first line so a verbose
// setup-script failure can't blow out the warning block.
func TestSessionWarningHintsIncludesSetupError_FirstLineOnly(t *testing.T) {
	sess := &pb.Session{SetupError: "npm install failed: ENOENT\nfull stack trace here\nmore detail"}
	hints := sessionWarningHints(sess)
	joined := strings.Join(hints, "\n")
	if !strings.Contains(joined, "setup script failed: npm install failed: ENOENT") {
		t.Fatalf("hints missing truncated setup-error; got %q", joined)
	}
	if strings.Contains(joined, "full stack trace") {
		t.Fatalf("hints should not contain lines after the first; got %q", joined)
	}
}

// TestChatStartErrorHintFromChats verifies that a chat with a non-empty
// StartError (a tmux launch failure stamped by the daemon) surfaces a
// "chat failed to start" hint truncated to the error's first line.
// ClaudeChat.StartError is a plain proto3 string field (not a pointer), so
// it is set directly via a struct literal rather than via proto.String.
func TestChatStartErrorHintFromChats(t *testing.T) {
	chats := []*pb.ClaudeChat{
		{StartError: "missing or unsuitable terminal: xterm-ghostty\nfull tmux stderr here"},
	}
	got := chatStartErrorHint(chats)
	if !strings.Contains(got, "failed to start") {
		t.Fatalf("chatStartErrorHint missing 'failed to start'; got %q", got)
	}
	if !strings.Contains(got, "missing or unsuitable terminal: xterm-ghostty") {
		t.Fatalf("chatStartErrorHint missing StartError first line; got %q", got)
	}
	if strings.Contains(got, "full tmux stderr") {
		t.Fatalf("chatStartErrorHint should not contain lines after the first; got %q", got)
	}
}

// TestChatStartErrorHintEmptyWhenNoStartError verifies that chats with no
// StartError set (the common case), a nil chats slice, and a nil entry in
// the slice all yield an empty hint — no false positive, no panic.
func TestChatStartErrorHintEmptyWhenNoStartError(t *testing.T) {
	if got := chatStartErrorHint(nil); got != "" {
		t.Errorf("chatStartErrorHint(nil) = %q, want empty", got)
	}
	chats := []*pb.ClaudeChat{nil, {AgentSessionId: "sess-1"}}
	if got := chatStartErrorHint(chats); got != "" {
		t.Errorf("chatStartErrorHint = %q, want empty for chats with no StartError", got)
	}
}

// TestSelectedSessionWarningBlock_ChatStartError verifies that a chat-level
// StartError renders in the selected-session warning block alongside
// session-level hints such as SetupError.
func TestSelectedSessionWarningBlock_ChatStartError(t *testing.T) {
	sess := &pb.Session{SetupError: "npm install failed: ENOENT"}
	chats := []*pb.ClaudeChat{{StartError: "missing or unsuitable terminal: xterm-ghostty"}}
	got := selectedSessionWarningBlock(sess, chats, 80)
	if !strings.Contains(got, "setup script failed") {
		t.Errorf("block missing setup-error hint: %q", got)
	}
	if !strings.Contains(got, "chat failed to start: missing or unsuitable terminal: xterm-ghostty") {
		t.Errorf("block missing chat start-error hint: %q", got)
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
	got := selectedSessionWarningBlock(sess, nil, 80)
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
	got := selectedSessionWarningBlock(sess, nil, 80)
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
	if got := selectedSessionWarningBlock(sess, nil, 80); got != "" {
		t.Errorf("selectedSessionWarningBlock = %q, want empty for no-hint session", got)
	}
}

// TestSelectedSessionWarningBlock_NilSession verifies that a nil session
// returns an empty block (no panic).
func TestSelectedSessionWarningBlock_NilSession(t *testing.T) {
	if got := selectedSessionWarningBlock(nil, nil, 80); got != "" {
		t.Errorf("selectedSessionWarningBlock = %q, want empty for nil session", got)
	}
}

// TestSelectedSessionWarningBlock_NilSessionWithChatStartError guards the
// DisplayStatus dereference: chatStartErrorHint is session-independent, so a
// nil session paired with a chat-level StartError produces a non-empty hint
// set that reaches the styling branch — which must not dereference nil.
func TestSelectedSessionWarningBlock_NilSessionWithChatStartError(t *testing.T) {
	chats := []*pb.ClaudeChat{{StartError: "missing or unsuitable terminal: xterm-ghostty"}}
	got := selectedSessionWarningBlock(nil, chats, 80)
	if !strings.Contains(got, "chat failed to start: missing or unsuitable terminal: xterm-ghostty") {
		t.Errorf("block missing chat start-error hint for nil session: %q", got)
	}
}

func rotationSession(evs ...*pb.RotationEvent) *pb.Session {
	return &pb.Session{RotationEvents: evs}
}

// TestRotationExhaustedHint covers the parked-timer badge: an unexpired
// exhausted episode surfaces a countdown, an expired one clears, and
// non-exhausted / no-history sessions stay silent. Assertions are on the
// rendered hint string.
func TestRotationExhaustedHint(t *testing.T) {
	now := time.Date(2026, 7, 6, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name         string
		sess         *pb.Session
		wantContains string // "" means the hint must be empty
	}{
		{
			name: "exhausted with future reset",
			sess: rotationSession(&pb.RotationEvent{
				Outcome: pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED,
				ResetAt: timestamppb.New(now.Add(30 * time.Minute)),
			}),
			wantContains: "all accounts limited, resumes in ~",
		},
		{
			name: "exhausted with expired reset",
			sess: rotationSession(&pb.RotationEvent{
				Outcome: pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED,
				ResetAt: timestamppb.New(now.Add(-1 * time.Minute)),
			}),
			wantContains: "",
		},
		{
			name: "exhausted without reset time",
			sess: rotationSession(&pb.RotationEvent{
				Outcome: pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED,
			}),
			wantContains: "all accounts limited",
		},
		{
			name: "newest is rotated",
			sess: rotationSession(&pb.RotationEvent{
				Outcome:     pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
				FromAccount: "acct-a",
				ToAccount:   "acct-b",
			}),
			wantContains: "",
		},
		{name: "no events", sess: rotationSession(), wantContains: ""},
		{name: "nil session", sess: nil, wantContains: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rotationExhaustedHint(tc.sess, now)
			if tc.wantContains == "" {
				if got != "" {
					t.Errorf("rotationExhaustedHint = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("rotationExhaustedHint = %q, want contains %q", got, tc.wantContains)
			}
		})
	}
}

// TestRotationExhaustedHint_WiredIntoWarnings verifies the exhausted badge
// flows through the rendered warning block operators actually see.
func TestRotationExhaustedHint_WiredIntoWarnings(t *testing.T) {
	sess := rotationSession(&pb.RotationEvent{
		Outcome: pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED,
		ResetAt: timestamppb.New(time.Now().Add(45 * time.Minute)),
	})
	got := selectedSessionWarningBlock(sess, nil, 80)
	if !strings.Contains(got, "all accounts limited, resumes in ~") {
		t.Errorf("warning block missing exhausted hint: %q", got)
	}
}

// TestRotationHistoryBlock covers the rendered rotation-history block: a
// rotated event shows the account transition, and a session with no history
// renders nothing.
func TestRotationHistoryBlock(t *testing.T) {
	cases := []struct {
		name         string
		sess         *pb.Session
		wantContains string
	}{
		{
			name: "rotated latest",
			sess: rotationSession(&pb.RotationEvent{
				Outcome:     pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
				FromAccount: "acct-a",
				ToAccount:   "acct-b",
				CreatedAt:   timestamppb.New(time.Now()),
			}),
			wantContains: "acct-a switched to acct-b",
		},
		{name: "no events", sess: rotationSession(), wantContains: ""},
		{name: "nil session", sess: nil, wantContains: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rotationHistoryBlock(tc.sess, 80)
			if tc.wantContains == "" {
				if got != "" {
					t.Errorf("rotationHistoryBlock = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, "Rotation history") {
				t.Errorf("rotationHistoryBlock missing header: %q", got)
			}
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("rotationHistoryBlock = %q, want contains %q", got, tc.wantContains)
			}
		})
	}
}

// TestRotationHistoryBlockKeepsRotatedDetailSuffix preserves information such
// as whether a switch resumed the prior conversation or when the capped account
// resets. The first event exercises the LEGACY-row shim: manual-switch audits
// written before the daemon split the pane notice from the audit Detail stored
// the full "switched to <account> — resumed" sentence; new daemon rows carry
// only the suffix (the second event's shape), which passes through untouched.
func TestRotationHistoryBlockKeepsRotatedDetailSuffix(t *testing.T) {
	sess := rotationSession(
		&pb.RotationEvent{
			Outcome:     pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
			FromAccount: "acct-a",
			ToAccount:   "acct-b",
			Detail:      "switched to acct-b — resumed",
			CreatedAt:   timestamppb.New(time.Now()),
		},
		&pb.RotationEvent{
			Outcome:     pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
			FromAccount: "acct-c",
			ToAccount:   "acct-d",
			Detail:      "resets 15:04",
			CreatedAt:   timestamppb.New(time.Now()),
		},
	)
	got := rotationHistoryBlock(sess, 80)
	for _, want := range []string{
		"acct-a switched to acct-b — resumed",
		"acct-c switched to acct-d — resets 15:04",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rotationHistoryBlock = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "acct-a switched to acct-b — switched to acct-b") {
		t.Errorf("rotationHistoryBlock = %q, should not repeat the switch label", got)
	}
}

// TestRotationEventLabel pins the per-outcome phrasing.
func TestRotationEventLabel(t *testing.T) {
	cases := []struct {
		outcome pb.RotationOutcome
		want    string
	}{
		{pb.RotationOutcome_ROTATION_OUTCOME_ROTATED, "acct-a switched to acct-b"},
		{pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_DISABLED, "rotation disabled — status only"},
		{pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY, "agent cannot rotate — status only"},
		{pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT, "no eligible account — status only"},
		{pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED, "all accounts limited"},
		{pb.RotationOutcome_ROTATION_OUTCOME_FAILED, "switch to acct-b failed"},
		{pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED, "rotation event"},
	}
	for _, tc := range cases {
		ev := &pb.RotationEvent{Outcome: tc.outcome, FromAccount: "acct-a", ToAccount: "acct-b"}
		if got := rotationEventLabel(ev); got != tc.want {
			t.Errorf("rotationEventLabel(%v) = %q, want %q", tc.outcome, got, tc.want)
		}
	}

	// Legacy ROTATED rows with an unresolved (empty) from-side drop the leading
	// space rather than render " switched to acct-b".
	emptyFrom := &pb.RotationEvent{
		Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
		ToAccount: "acct-b",
	}
	if got, want := rotationEventLabel(emptyFrom), "switched to acct-b"; got != want {
		t.Errorf("rotationEventLabel(empty from) = %q, want %q", got, want)
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
