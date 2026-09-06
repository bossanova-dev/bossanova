package views

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/sessionreason"
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
		{
			name: "info/waiting with spinner",
			sess: &pb.Session{
				DisplayLabel:   "waiting",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_INFO,
				DisplaySpinner: true,
			},
			wantContains: "waiting",
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
	joined := strings.Join(sessionWarningHintTexts(sess), "\n")
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
	joined := strings.Join(sessionWarningHintTexts(sess), "\n")
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
	if !strings.Contains(got[0].Text, "repair failed") {
		t.Errorf("first hint should be repair hint, got %q", got[0].Text)
	}
	if !strings.Contains(got[1].Text, "finalize failed") {
		t.Errorf("second hint should be attention hint, got %q", got[1].Text)
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

// TestRotationRespawnCapHint_WiredIntoWarnings proves a respawn-cap-exhausted
// newest event surfaces a needs-attention warning line (BOS-482), while a
// benign respawned-same-account event does not (it is a self-heal, not a wedge).
func TestRotationRespawnCapHint_WiredIntoWarnings(t *testing.T) {
	capped := rotationSession(&pb.RotationEvent{
		Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED,
		CreatedAt: timestamppb.New(time.Now()),
	})
	got := selectedSessionWarningBlock(capped, nil, 80)
	if !strings.Contains(got, "respawn cap reached") {
		t.Errorf("warning block missing respawn-cap hint: %q", got)
	}

	healed := rotationSession(&pb.RotationEvent{
		Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT,
		ToAccount: "acct-b",
		CreatedAt: timestamppb.New(time.Now()),
	})
	if hint := rotationRespawnCapHint(healed); hint != "" {
		t.Errorf("respawned-same-account should not warn, got %q", hint)
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
			got := rotationHistoryBlock(tc.sess, 80, time.Now())
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
	got := rotationHistoryBlock(sess, 80, time.Now())
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

// TestRotationEventTime pins the date-aware per-row timestamp: an event from
// the same calendar day as now renders time-only (15:04); an event from any
// other day is prefixed with an ISO date (2006-01-02 15:04).
func TestRotationEventTime(t *testing.T) {
	now := time.Date(2026, 7, 18, 14, 34, 0, 0, time.Local)
	today := time.Date(2026, 7, 18, 9, 5, 0, 0, time.Local)
	if got, want := rotationEventTime(today, now), "09:05"; got != want {
		t.Errorf("today: rotationEventTime = %q, want %q", got, want)
	}
	older := time.Date(2026, 7, 17, 14, 34, 0, 0, time.Local)
	if got, want := rotationEventTime(older, now), "2026-07-17 14:34"; got != want {
		t.Errorf("not-today: rotationEventTime = %q, want %q", got, want)
	}
}

// TestRotationHistoryBlockUnspecifiedDropsPrefix proves a BOS-409 stale-port
// event (recorded UNSPECIFIED with the whole message in Detail) renders as
// "<time> <detail>" with no "rotation event — " prefix and no double space.
func TestRotationHistoryBlockUnspecifiedDropsPrefix(t *testing.T) {
	now := time.Date(2026, 7, 18, 14, 34, 0, 0, time.Local)
	sess := rotationSession(&pb.RotationEvent{
		Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED,
		Detail:    "stale failover-proxy port (BOS-409)",
		CreatedAt: timestamppb.New(now),
	})
	got := rotationHistoryBlock(sess, 80, now)
	if !strings.Contains(got, "14:34 stale failover-proxy port (BOS-409)") {
		t.Errorf("rotationHistoryBlock = %q, want time + detail with no label", got)
	}
	if strings.Contains(got, "rotation event") {
		t.Errorf("rotationHistoryBlock = %q, should not contain the dropped generic prefix", got)
	}
}

// TestRotationHistoryBlockDatesNonTodayEvents proves a not-today event carries
// the ISO date prefix in the assembled row.
func TestRotationHistoryBlockDatesNonTodayEvents(t *testing.T) {
	now := time.Date(2026, 7, 18, 14, 34, 0, 0, time.Local)
	older := time.Date(2026, 7, 17, 8, 2, 0, 0, time.Local)
	sess := rotationSession(&pb.RotationEvent{
		Outcome:     pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
		FromAccount: "acct-a",
		ToAccount:   "acct-b",
		CreatedAt:   timestamppb.New(older),
	})
	got := rotationHistoryBlock(sess, 80, now)
	if !strings.Contains(got, "2026-07-17 08:02 acct-a switched to acct-b") {
		t.Errorf("rotationHistoryBlock = %q, want date-prefixed not-today row", got)
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
		{pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT, "refreshed auth in place on acct-b"},
		{pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED, "auth-wedge respawn cap reached"},
		{pb.RotationOutcome_ROTATION_OUTCOME_FAILED, "switch to acct-b failed"},
		{pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED, ""},
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

// --- BOS-474: exact-escape goldens for the OSC 8 link renderers ------------
//
// These pin the FULL byte sequence each link renderer emits (raw ANSI + OSC 8
// envelope) so the osc8Link extraction is provably byte-stable. The wants are
// written as literals — never composed from the production constants — so a
// change to those constants fails here instead of silently agreeing with
// itself.

func TestLinkRenderers_ExactEscapes(t *testing.T) {
	const (
		prURL      = "https://github.com/acme/widgets/pull/42"
		trackerURL = "https://linear.app/acme/issue/BOS-474"
		title      = "Ship [BOS-474] endpoints"
	)
	pr := int32(42)
	trackerID := "BOS-474"
	linked := &pb.Session{
		PrNumber:   &pr,
		PrUrl:      strPtr(prURL),
		TrackerId:  &trackerID,
		TrackerUrl: strPtr(trackerURL),
	}
	unlinked := &pb.Session{PrNumber: &pr, TrackerId: &trackerID}

	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "pr link with url",
			got:  renderPRLink(linked),
			want: "\x1b]8;;" + prURL + "\x1b\\" + "\x1b[4m#42\x1b[24m" + "\x1b]8;;\x1b\\",
		},
		{
			name: "pr link without url",
			got:  renderPRLink(unlinked),
			want: "\x1b[4m#42\x1b[24m",
		},
		{
			name: "muted pr link with url",
			got:  renderMutedPRLink(linked),
			want: "\x1b]8;;" + prURL + "\x1b\\" +
				"\x1b[38;2;98;98;98;58;2;98;98;98;9;4m#42\x1b[39;59;29;24m" +
				"\x1b]8;;\x1b\\",
		},
		{
			name: "muted pr link without url",
			got:  renderMutedPRLink(unlinked),
			want: "\x1b[38;2;98;98;98;58;2;98;98;98;9;4m#42\x1b[39;59;29;24m",
		},
		{
			name: "selected pr link with url",
			got:  renderSelectedPRLink(linked),
			want: "\x1b]8;;" + prURL + "\x1b\\" +
				"\x1b[1;38;2;76;167;248;58;2;76;167;248;4m#42\x1b[22;39;59;24m" +
				"\x1b]8;;\x1b\\",
		},
		{
			name: "selected pr link without url",
			got:  renderSelectedPRLink(unlinked),
			want: "\x1b[1;38;2;76;167;248;58;2;76;167;248;4m#42\x1b[22;39;59;24m",
		},
		{
			name: "tracker link with url",
			got:  renderTrackerLink(linked, title),
			want: "Ship " +
				"\x1b]8;;" + trackerURL + "\x1b\\" + "\x1b[4m[BOS-474]\x1b[24m" + "\x1b]8;;\x1b\\" +
				" endpoints",
		},
		{
			name: "tracker link without url",
			got:  renderTrackerLink(unlinked, title),
			want: "Ship " + "\x1b[4m[BOS-474]\x1b[24m" + " endpoints",
		},
		{
			name: "muted tracker link with url",
			got:  renderMutedTrackerLink(linked, title),
			want: "\x1b[38;2;98;98;98;9mShip \x1b[39;29m" +
				"\x1b]8;;" + trackerURL + "\x1b\\" +
				"\x1b[38;2;98;98;98;58;2;98;98;98;9;4m[BOS-474]\x1b[39;59;29;24m" +
				"\x1b]8;;\x1b\\" +
				"\x1b[38;2;98;98;98;9m endpoints\x1b[39;29m",
		},
		{
			name: "muted tracker link without url",
			got:  renderMutedTrackerLink(unlinked, title),
			want: "\x1b[38;2;98;98;98;9mShip \x1b[39;29m" +
				"\x1b[38;2;98;98;98;58;2;98;98;98;9;4m[BOS-474]\x1b[39;59;29;24m" +
				"\x1b[38;2;98;98;98;9m endpoints\x1b[39;29m",
		},
		{
			name: "selected tracker link with url",
			got:  renderSelectedTrackerLink(linked, title),
			want: "\x1b[1;38;2;76;167;248mShip \x1b[22;39m" +
				"\x1b]8;;" + trackerURL + "\x1b\\" +
				"\x1b[1;38;2;76;167;248;58;2;76;167;248;4m[BOS-474]\x1b[22;39;59;24m" +
				"\x1b]8;;\x1b\\" +
				"\x1b[1;38;2;76;167;248m endpoints\x1b[22;39m",
		},
		{
			name: "selected tracker link without url",
			got:  renderSelectedTrackerLink(unlinked, title),
			want: "\x1b[1;38;2;76;167;248mShip \x1b[22;39m" +
				"\x1b[1;38;2;76;167;248;58;2;76;167;248;4m[BOS-474]\x1b[22;39;59;24m" +
				"\x1b[1;38;2;76;167;248m endpoints\x1b[22;39m",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("escape sequence drift\n got: %q\nwant: %q", tc.got, tc.want)
			}
		})
	}
}

// --- BOS-474: endpoint link rendering --------------------------------------

func TestSessionEndpointLabels(t *testing.T) {
	cases := []struct {
		name string
		sess *pb.Session
		want string
	}{
		{name: "nil session", sess: nil, want: ""},
		{name: "no endpoints", sess: &pb.Session{}, want: ""},
		{
			name: "single endpoint",
			sess: &pb.Session{HttpEndpoints: []*pb.HttpEndpoint{
				{Port: 3000, Url: "http://localhost:3000"},
			}},
			want: ":3000",
		},
		{
			name: "multiple endpoints",
			sess: &pb.Session{HttpEndpoints: []*pb.HttpEndpoint{
				{Port: 3000, Url: "http://localhost:3000"},
				{Port: 5173, Url: "https://localhost:5173"},
			}},
			want: ":3000 :5173",
		},
		{
			name: "invalid url still labelled",
			sess: &pb.Session{HttpEndpoints: []*pb.HttpEndpoint{
				{Port: 3000, Url: "ftp://localhost:3000"},
			}},
			want: ":3000",
		},
		{
			name: "zero port dropped",
			sess: &pb.Session{HttpEndpoints: []*pb.HttpEndpoint{
				{Port: 0, Url: "http://localhost"},
				{Port: 8080, Url: "http://localhost:8080"},
			}},
			want: ":8080",
		},
		{
			name: "nil entry dropped",
			sess: &pb.Session{HttpEndpoints: []*pb.HttpEndpoint{
				nil,
				{Port: 8080, Url: "http://localhost:8080"},
			}},
			want: ":8080",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionEndpointLabels(tc.sess); got != tc.want {
				t.Errorf("sessionEndpointLabels = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderSessionEndpoints_ExactEscapes(t *testing.T) {
	const (
		mutedLink  = "\x1b[38;2;98;98;98;58;2;98;98;98;4m"
		mutedLinkX = "\x1b[39;59;24m"
		mutedOpen  = "\x1b[38;2;98;98;98m"
		mutedClose = "\x1b[39m"
	)
	cases := []struct {
		name string
		sess *pb.Session
		want string
	}{
		{name: "nil session", sess: nil, want: ""},
		{name: "no endpoints", sess: &pb.Session{}, want: ""},
		{
			name: "single http endpoint is clickable",
			sess: &pb.Session{HttpEndpoints: []*pb.HttpEndpoint{
				{Port: 3000, Url: "http://localhost:3000"},
			}},
			want: "\x1b]8;;http://localhost:3000\x1b\\" + mutedLink + ":3000" + mutedLinkX + "\x1b]8;;\x1b\\",
		},
		{
			name: "two endpoints joined by a muted space",
			sess: &pb.Session{HttpEndpoints: []*pb.HttpEndpoint{
				{Port: 3000, Url: "http://localhost:3000"},
				{Port: 5173, Url: "https://127.0.0.1:5173"},
			}},
			want: "\x1b]8;;http://localhost:3000\x1b\\" + mutedLink + ":3000" + mutedLinkX + "\x1b]8;;\x1b\\" +
				mutedOpen + " " + mutedClose +
				"\x1b]8;;https://127.0.0.1:5173\x1b\\" + mutedLink + ":5173" + mutedLinkX + "\x1b]8;;\x1b\\",
		},
		{
			name: "non-http scheme renders muted but not clickable",
			sess: &pb.Session{HttpEndpoints: []*pb.HttpEndpoint{
				{Port: 3000, Url: "ftp://localhost:3000"},
			}},
			want: mutedOpen + ":3000" + mutedClose,
		},
		{
			name: "empty url renders muted but not clickable",
			sess: &pb.Session{HttpEndpoints: []*pb.HttpEndpoint{
				{Port: 3000},
			}},
			want: mutedOpen + ":3000" + mutedClose,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderSessionEndpoints(tc.sess); got != tc.want {
				t.Errorf("renderSessionEndpoints\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestIsHTTPEndpointURL rejects anything that must never become an OSC 8
// target: non-HTTP schemes, relative/hostless URLs, unparseable strings, and
// (critically) strings carrying control bytes that would terminate or forge
// the escape envelope.
func TestIsHTTPEndpointURL(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"http://localhost:3000", true},
		{"https://127.0.0.1:5173", true},
		{"http://localhost:3000/path?q=1", true},
		{"", false},
		{"localhost:3000", false},
		{"//localhost:3000", false},
		{"ftp://localhost:3000", false},
		{"file:///etc/passwd", false},
		{"javascript:alert(1)", false},
		{"http://", false},
		{"http://loc\x1balhost", false},
		{"http://localhost\x07", false},
		{"http://localhost\n:3000", false},
		// A raw space makes the URI invalid per RFC 3986 even though url.Parse
		// accepts it — the terminal would render a dead link.
		{"http://local host:3000", false},
		{"http://localhost:3000/a b", false},
		{"http://localhost:3000/a%20b", true},
		{"HTTP://localhost:3000", true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if got := isHTTPEndpointURL(tc.raw); got != tc.want {
				t.Errorf("isHTTPEndpointURL(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestRenderSessionEndpoints_NeverEmitsUnsafeTarget is the security-shaped
// backstop for the table above: whatever the daemon hands us, the rendered
// string must never contain an OSC 8 introducer whose target carries a raw
// ESC or BEL byte.
func TestRenderSessionEndpoints_NeverEmitsUnsafeTarget(t *testing.T) {
	hostile := []string{
		"http://localhost:3000\x1b]8;;http://evil\x1b\\",
		"http://localhost\x07",
		"http://localhost\n",
	}
	for _, raw := range hostile {
		sess := &pb.Session{HttpEndpoints: []*pb.HttpEndpoint{{Port: 3000, Url: raw}}}
		got := renderSessionEndpoints(sess)
		if strings.Contains(got, "\x1b]8;;") {
			t.Errorf("hostile url %q produced an OSC 8 envelope: %q", raw, got)
		}
		if !strings.Contains(got, ":3000") {
			t.Errorf("hostile url %q dropped the visible label: %q", raw, got)
		}
	}
}

// TestSessionSubRowCount verifies the single accounting function every Home
// row/height/cursor helper routes through.
func TestSessionSubRowCount(t *testing.T) {
	endpoints := []*pb.HttpEndpoint{{Port: 3000, Url: "http://localhost:3000"}}
	cases := []struct {
		name string
		sess *pb.Session
		want int
	}{
		{name: "nil", sess: nil, want: 0},
		{name: "bare session", sess: &pb.Session{}, want: 0},
		{name: "endpoints only", sess: &pb.Session{HttpEndpoints: endpoints}, want: 1},
		{name: "warning only", sess: &pb.Session{SetupError: "boom"}, want: 1},
		{
			name: "endpoints and warning",
			sess: &pb.Session{HttpEndpoints: endpoints, SetupError: "boom"},
			want: 2,
		},
		{
			name: "unrenderable endpoints add no row",
			sess: &pb.Session{HttpEndpoints: []*pb.HttpEndpoint{{Port: 0, Url: "http://localhost"}}},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionSubRowCount(tc.sess, ""); got != tc.want {
				t.Errorf("sessionSubRowCount = %d, want %d", got, tc.want)
			}
		})
	}
}

// --- BOS-855: live activity owns the row, a past outcome goes recessive ---

// draftPRFailureReason builds the blocked reason bossd persists when the
// background draft-PR create fails.
func draftPRFailureReason(detail string) *string {
	r := sessionreason.DraftPRCreationFailure(errors.New(detail))
	return &r
}

// assertFadedStylesDiffer is the anti-vacuous guard the BOS-855 style
// assertions lean on. Every style comparison below asserts equality against
// styleStatusDangerFaded AND inequality against styleStatusDanger; under an
// ANSI-stripping colour profile the equality half alone would pass for any
// input, so this fails loudly rather than letting the suite go quiet.
func assertFadedStylesDiffer(t *testing.T) {
	t.Helper()
	const probe = "probe"
	if styleStatusDangerFaded.Render(probe) == styleStatusDanger.Render(probe) {
		t.Fatalf("styleStatusDangerFaded and styleStatusDanger render identically (%q) — the colour profile is stripping styling, so every style assertion in this file would be vacuous",
			styleStatusDanger.Render(probe))
	}
}

// TestWarningHintStyle_DemotableFadesOnlyOnLiveRow pins the core gate: the same
// demotable hint fades on a row whose composite is live and stays at full danger
// on a row whose composite is not.
func TestWarningHintStyle_DemotableFadesOnlyOnLiveRow(t *testing.T) {
	assertFadedStylesDiffer(t)
	const text = "finalize failed (pr_skipped_no_github)"
	hint := sessionHint{Text: text, Demotable: true}

	live := &pb.Session{DisplayLabel: "working"}
	if got, want := warningHintStyle(live, hint).Render(text), styleStatusDangerFaded.Render(text); got != want {
		t.Errorf("live row: rendered %q, want the faded style %q", got, want)
	}
	if got, unwanted := warningHintStyle(live, hint).Render(text), styleStatusDanger.Render(text); got == unwanted {
		t.Errorf("live row: rendered at FULL danger intensity %q", got)
	}

	notLive := &pb.Session{DisplayLabel: "idle"}
	if got, want := warningHintStyle(notLive, hint).Render(text), styleStatusDanger.Render(text); got != want {
		t.Errorf("non-live row: rendered %q, want full danger %q", got, want)
	}
	if got, unwanted := warningHintStyle(notLive, hint).Render(text), styleStatusDangerFaded.Render(text); got == unwanted {
		t.Errorf("non-live row: rendered faded %q", got)
	}
}

// TestWarningHintStyle_ExemptHintsStayLoud walks the exemption set on a live
// row. Each of these is either the row's only carrier of its signal or an
// impeachment of the liveness claim the label makes, so none may fade.
func TestWarningHintStyle_ExemptHintsStayLoud(t *testing.T) {
	assertFadedStylesDiffer(t)
	respawnCapEvent := []*pb.RotationEvent{{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED}}
	exhaustedEvent := []*pb.RotationEvent{{
		Outcome: pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED,
		ResetAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
	}}

	tests := []struct {
		name     string
		sess     *pb.Session
		chats    []*pb.ClaudeChat
		contains string
	}{
		{
			name: "agent stalled",
			sess: &pb.Session{DisplayLabel: "working", AttentionStatus: &pb.AttentionStatus{
				NeedsAttention: true,
				Reason:         pb.AttentionReason_ATTENTION_REASON_AGENT_STALLED,
				Summary:        "agent reports working but has made no progress — check the pane",
			}},
			contains: "no progress",
		},
		{
			name: "agent auth failed",
			sess: &pb.Session{DisplayLabel: "working", AttentionStatus: &pb.AttentionStatus{
				NeedsAttention: true,
				Reason:         pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED,
				Summary:        "agent is not logged in — run /login in the pane",
			}},
			contains: "not logged in",
		},
		{
			name:     "rotation respawn cap",
			sess:     &pb.Session{DisplayLabel: "working", RotationEvents: respawnCapEvent},
			contains: "respawn cap reached",
		},
		{
			name:     "rotation exhausted",
			sess:     &pb.Session{DisplayLabel: "working", RotationEvents: exhaustedEvent},
			contains: "all accounts limited",
		},
		{
			name:     "setup error",
			sess:     &pb.Session{DisplayLabel: "working", SetupError: "npm install failed: ENOENT"},
			contains: "setup script failed",
		},
		{
			name:     "draft PR creation failure",
			sess:     &pb.Session{DisplayLabel: "working", BlockedReason: draftPRFailureReason("gh pr create: auth required")},
			contains: "draft PR creation failed",
		},
		{
			name:     "chat start error",
			sess:     &pb.Session{DisplayLabel: "working"},
			chats:    []*pb.ClaudeChat{{StartError: "missing or unsuitable terminal: xterm-ghostty"}},
			contains: "chat failed to start",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hints := sessionWarningHints(tt.sess)
			if hint := chatStartErrorHint(tt.chats); hint != "" {
				hints = append(hints, sessionHint{Text: hint})
			}
			var found bool
			for _, h := range hints {
				if !strings.Contains(h.Text, tt.contains) {
					continue
				}
				found = true
				if h.Demotable {
					t.Errorf("hint %q is demotable, want exempt", h.Text)
				}
				got := warningHintStyle(tt.sess, h).Render(h.Text)
				if want := styleStatusDanger.Render(h.Text); got != want {
					t.Errorf("hint %q rendered %q, want full danger %q", h.Text, got, want)
				}
				if unwanted := styleStatusDangerFaded.Render(h.Text); got == unwanted {
					t.Errorf("hint %q rendered faded on a live row", h.Text)
				}
			}
			if !found {
				t.Fatalf("no hint containing %q was produced; hints: %+v", tt.contains, hints)
			}
		})
	}
}

// TestSelectedSessionWarningBlock_MixedDemotableAndExempt is the mixed case: one
// faded line and one full-intensity line on the same row, never two of either.
func TestSelectedSessionWarningBlock_MixedDemotableAndExempt(t *testing.T) {
	assertFadedStylesDiffer(t)
	sess := &pb.Session{
		DisplayLabel:           "working",
		LastRepairAttemptCount: 1,
		LastRepairExitError:    "exit status 1",
		SetupError:             "npm install failed: ENOENT",
	}
	hints := sessionWarningHints(sess)
	if len(hints) != 2 {
		t.Fatalf("sessionWarningHints len = %d, want 2 (one demotable repair hint, one exempt setup hint): %+v", len(hints), hints)
	}
	var faded, loud int
	for _, h := range hints {
		got := warningHintStyle(sess, h).Render(h.Text)
		switch {
		case got == styleStatusDangerFaded.Render(h.Text):
			faded++
		case got == styleStatusDanger.Render(h.Text):
			loud++
		default:
			t.Fatalf("hint %q rendered with neither style: %q", h.Text, got)
		}
	}
	if faded != 1 || loud != 1 {
		t.Fatalf("faded = %d, full-intensity = %d, want exactly one of each", faded, loud)
	}
}

// TestWarningHintStyle_MergedRowStillFades keeps BOS-246 intact and pins the
// overlap: a merged row fades regardless of demotability, a merged AND live row
// fades exactly once (there is no third, double-faded tier), and a row that is
// neither renders full danger.
func TestWarningHintStyle_MergedRowStillFades(t *testing.T) {
	assertFadedStylesDiffer(t)
	const text = "finalize failed (pr_skipped_no_github)"
	exempt := sessionHint{Text: text}
	demotable := sessionHint{Text: text, Demotable: true}

	merged := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED}
	closed := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CLOSED}
	mergedLive := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED, DisplayLabel: "working"}
	neither := &pb.Session{DisplayLabel: "idle"}

	for _, sess := range []*pb.Session{merged, closed, mergedLive} {
		for _, h := range []sessionHint{exempt, demotable} {
			if got, want := warningHintStyle(sess, h).Render(text), styleStatusDangerFaded.Render(text); got != want {
				t.Errorf("merged/closed row (%v, label %q): rendered %q, want faded %q",
					sess.GetDisplayStatus(), sess.GetDisplayLabel(), got, want)
			}
		}
	}
	// Fading twice would have to produce a different string; it does not.
	if got, want := warningHintStyle(mergedLive, demotable).Render(text), styleStatusDangerFaded.Render(text); got != want {
		t.Errorf("merged AND live row rendered %q, want a single fade %q", got, want)
	}
	if got, want := warningHintStyle(neither, exempt).Render(text), styleStatusDanger.Render(text); got != want {
		t.Errorf("neither merged nor live: rendered %q, want full danger %q", got, want)
	}
}

// TestHomeTableAndSelectedBlockAgreeOnStyle asserts the two render sites resolve
// the SAME style for the same session — the split that made the list and the
// detail view disagree before the shared gate existed.
func TestHomeTableAndSelectedBlockAgreeOnStyle(t *testing.T) {
	assertFadedStylesDiffer(t)
	sessions := []*pb.Session{
		{Id: "s1", Title: "live row", DisplayLabel: "working", AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS,
			Summary:        "finalize failed (pr_skipped_no_github)",
		}},
		{Id: "s2", Title: "idle row", DisplayLabel: "idle", AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS,
			Summary:        "finalize failed (pr_skipped_no_github)",
		}},
		{Id: "s3", Title: "exempt row", DisplayLabel: "working", SetupError: "npm install failed: ENOENT"},
	}
	for _, sess := range sessions {
		for _, h := range sessionWarningHints(sess) {
			tableStyled := warningHintStyle(sess, h).Render(h.Text)
			block := selectedSessionWarningBlock(sess, nil, len(h.Text))
			if !strings.Contains(block, strings.TrimSpace(tableStyled)) {
				t.Errorf("session %s: selected block %q does not carry the home table's styling of %q (%q)",
					sess.GetId(), block, h.Text, tableStyled)
			}
		}
	}
}

// TestAttentionIndicator_FadedOnlyWhenEveryHintIsDemotable covers the mixed case
// specifically: one demotable plus one exempt hint keeps the "!" loud.
func TestAttentionIndicator_FadedOnlyWhenEveryHintIsDemotable(t *testing.T) {
	assertFadedStylesDiffer(t)
	attention := func() *pb.AttentionStatus {
		return &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS,
			Summary:        "finalize failed (pr_skipped_no_github)",
		}
	}
	allDemotable := &pb.Session{DisplayLabel: "working", AttentionStatus: attention()}
	mixed := &pb.Session{DisplayLabel: "working", AttentionStatus: attention(), SetupError: "npm install failed: ENOENT"}
	notLive := &pb.Session{DisplayLabel: "idle", AttentionStatus: attention()}

	if got, want := renderAttentionIndicator(allDemotable), styleStatusDangerFaded.Render("!"); got != want {
		t.Errorf("all-demotable row: indicator = %q, want faded %q", got, want)
	}
	if got, want := renderAttentionIndicator(mixed), styleStatusDanger.Render("!"); got != want {
		t.Errorf("mixed row: indicator = %q, want FULL intensity %q — one exempt hint keeps the ! loud", got, want)
	}
	if got, want := renderAttentionIndicator(notLive), styleStatusDanger.Render("!"); got != want {
		t.Errorf("non-live row: indicator = %q, want full intensity %q", got, want)
	}

	// A merged/closed row fades its "!" even when a hint is EXEMPT, because
	// attentionIndicatorDemoted routes through hintDemoted, whose BOS-246
	// finished arm short-circuits ahead of the demotable check. That extends
	// BOS-246 — which faded the hint lines but left the "!" loud — and it is the
	// intended reading: a bright "!" above an entirely faded hint block is the
	// same self-contradiction this work exists to remove. Pinned so the
	// extension cannot be undone silently.
	// Same shape as `mixed` above — whose "!" stays loud — but merged.
	mergedExempt := &pb.Session{
		DisplayLabel:    "working",
		DisplayStatus:   pb.DisplayStatus_DISPLAY_STATUS_MERGED,
		AttentionStatus: attention(),
		SetupError:      "npm install failed: ENOENT",
	}
	if got, want := renderAttentionIndicator(mergedExempt), styleStatusDangerFaded.Render("!"); got != want {
		t.Errorf("merged row with an exempt hint: indicator = %q, want faded %q", got, want)
	}
	if unwanted := styleStatusDanger.Render("!"); renderAttentionIndicator(mergedExempt) == unwanted {
		t.Error("merged row rendered a full-intensity ! above an entirely faded hint block")
	}
}

// TestDraftPRFailure_SuccessCompositeWithExemptHint is the sole-carrier case
// that forces the exemption. The background create never transitions the
// session, so displaystatus's errored recolor never fires and the composite is a
// plain green "working" — a FADED hint here would leave the row strictly quieter
// than the "? PR failed" it replaced.
func TestDraftPRFailure_SuccessCompositeWithExemptHint(t *testing.T) {
	assertFadedStylesDiffer(t)
	sess := &pb.Session{
		Id:            "sess-draft-pr",
		State:         pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		BlockedReason: draftPRFailureReason("gh pr create: authentication required"),
	}
	composite := displaystatus.Compute(displaystatus.Input{
		Session:    sess,
		ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING,
	})
	if composite.Label != "working" || composite.Intent != pb.DisplayIntent_DISPLAY_INTENT_SUCCESS {
		t.Fatalf("composite = %+v, want a SUCCESS \"working\" label", composite)
	}
	sess.DisplayLabel, sess.DisplayIntent, sess.DisplaySpinner = composite.Label, composite.Intent, composite.Spinner

	hints := sessionWarningHints(sess)
	if len(hints) != 1 {
		t.Fatalf("sessionWarningHints = %+v, want exactly one hint carrying the failure", hints)
	}
	if !strings.Contains(hints[0].Text, "draft PR creation failed") {
		t.Fatalf("hint = %q, want it to carry the draft-PR failure text", hints[0].Text)
	}
	if hints[0].Demotable {
		t.Fatal("the draft-PR failure hint is demotable; it is the row's ONLY alarm and must be exempt")
	}
	if got, want := warningHintStyle(sess, hints[0]).Render(hints[0].Text), styleStatusDanger.Render(hints[0].Text); got != want {
		t.Errorf("hint rendered %q, want full danger %q", got, want)
	}
	if unwanted := styleStatusDangerFaded.Render(hints[0].Text); warningHintStyle(sess, hints[0]).Render(hints[0].Text) == unwanted {
		t.Error("hint rendered faded on a row that is never recolored")
	}
	if got := sessionSubRowCount(sess, ""); got != 1 {
		t.Errorf("sessionSubRowCount = %d, want 1 (the hint adds a row)", got)
	}
}

// TestDraftPRFailureHint_TruncatesMultilineError pins the firstLine + rune
// truncation: an embedded multi-line `gh pr create` error must not put a newline
// in a table cell nor push the NAME column to its width cap via nameWidthLabels.
func TestDraftPRFailureHint_TruncatesMultilineError(t *testing.T) {
	long := "fatal: could not read Username for 'https://github.com': terminal prompts disabled\nhint: see the docs\nhint: and more"
	sess := &pb.Session{BlockedReason: draftPRFailureReason(long)}
	got := draftPRFailureHint(sess)
	if strings.Contains(got, "\n") {
		t.Fatalf("hint contains a newline: %q", got)
	}
	if strings.Contains(got, "hint: see the docs") {
		t.Fatalf("hint kept text after the first line: %q", got)
	}
	if n := len([]rune(got)); n > hintReasonMaxRunes+1 {
		t.Fatalf("hint is %d runes (%q), want at most %d plus the ellipsis", n, got, hintReasonMaxRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a truncated hint should end in an ellipsis: %q", got)
	}
	if !strings.HasPrefix(got, "draft PR creation failed") {
		t.Fatalf("hint lost its identifying prefix: %q", got)
	}
	// A short reason passes through untouched.
	short := &pb.Session{BlockedReason: draftPRFailureReason("no remote")}
	if got, want := draftPRFailureHint(short), "draft PR creation failed: no remote"; got != want {
		t.Errorf("short hint = %q, want %q", got, want)
	}
	if got := draftPRFailureHint(&pb.Session{}); got != "" {
		t.Errorf("draftPRFailureHint with no blocked reason = %q, want empty", got)
	}
	if got := draftPRFailureHint(nil); got != "" {
		t.Errorf("draftPRFailureHint(nil) = %q, want empty", got)
	}
}

// transientDraftPRFailureReason builds the BOS-877 transient blocked reason for
// a test session, mirroring draftPRFailureReason's terminal counterpart.
func transientDraftPRFailureReason(detail string) *string {
	r := sessionreason.DraftPRCreationTransientFailure(errors.New(detail))
	return &r
}

// TestDraftPRFailureHint_TransientRendersAPurposeWrittenString is the BOS-877
// fix. The incident's reason was `Permission denied (publickey)`, which the old
// hint truncated onto the row verbatim and sent the operator hunting through
// ~/.ssh for a key bug that did not exist. A transient failure gets a fixed
// sentence naming the real condition instead of a truncated transport error.
func TestDraftPRFailureHint_TransientRendersAPurposeWrittenString(t *testing.T) {
	sess := &pb.Session{
		BlockedReason: transientDraftPRFailureReason("exit status 128: git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository."),
	}
	const want = "PR retrying — GitHub was unreachable"
	if got := draftPRFailureHint(sess); got != want {
		t.Fatalf("draftPRFailureHint = %q, want %q", got, want)
	}
	// The fixed string must stay inside the hint cap. Nothing enforces that at
	// runtime — draftPRFailureHint returns the constant directly and truncates
	// only the terminal case — so an over-length reword would render whole and
	// drag the NAME column toward its width cap. This assertion is the only
	// guard.
	if n := len([]rune(want)); n > hintReasonMaxRunes {
		t.Fatalf("the transient hint is %d runes, over the %d-rune cap", n, hintReasonMaxRunes)
	}
	if strings.Contains(draftPRFailureHint(sess), "Permission denied") {
		t.Fatal("the transient hint still leaks the raw SSH error the fix exists to hide")
	}
}

// TestDraftPRFailureHint_TerminalIsUnchanged pins the other half of the branch:
// a terminal failure keeps today's truncated first line byte-for-byte, because
// that raw text is genuinely the operator's next step there.
func TestDraftPRFailureHint_TerminalIsUnchanged(t *testing.T) {
	sess := &pb.Session{BlockedReason: draftPRFailureReason("ERROR: Repository not found.")}
	if got, want := draftPRFailureHint(sess), "draft PR creation failed: ERROR: Repository not …"; got != want {
		t.Fatalf("draftPRFailureHint = %q, want the existing truncated first line %q", got, want)
	}
	if got := draftPRFailureHint(&pb.Session{}); got != "" {
		t.Errorf("draftPRFailureHint with no draft-PR failure = %q, want empty", got)
	}
	if got := draftPRFailureHint(nil); got != "" {
		t.Errorf("draftPRFailureHint(nil) = %q, want empty", got)
	}
}

// TestTransientDraftPRFailureHintStaysExempt guards the boundary the plan draws
// around the recessive treatment: the transient hint is still the row's only
// alarm (the failure never transitions the session, and a live WORKING chat
// outranks the draft-PR branch, so the composite stays a plain SUCCESS
// "working"), so it must not become demotable as a side effect of reading more
// calmly.
func TestTransientDraftPRFailureHintStaysExempt(t *testing.T) {
	assertFadedStylesDiffer(t)
	sess := &pb.Session{
		Id:             "sess-draft-pr-transient",
		State:          pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		DisplayLabel:   "working",
		DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
		DisplaySpinner: true,
		BlockedReason:  transientDraftPRFailureReason("git@github.com: Permission denied (publickey)."),
	}
	hints := sessionWarningHints(sess)
	if len(hints) != 1 {
		t.Fatalf("sessionWarningHints = %+v, want exactly one hint", hints)
	}
	if hints[0].Text != "PR retrying — GitHub was unreachable" {
		t.Fatalf("hint = %q, want the transient string", hints[0].Text)
	}
	if hints[0].Demotable {
		t.Fatal("the transient draft-PR hint is demotable; it is still the row's only alarm")
	}
}

// TestTransientDraftPRFailureHintDoesNotResurrectTheDuplicate pins the BOS-855
// de-duplication against the substitution BOS-877 introduced. The dedup above
// keys off the ATTENTION hint's own prefix, not off equality with the rendered
// draft-PR hint — which is the only reason it still fires now that the transient
// branch renders a string bearing no resemblance to the reason. Switch that test
// to equality and this row would show the replacement string AND the raw reason
// it exists to suppress, with nothing else failing.
func TestTransientDraftPRFailureHintDoesNotResurrectTheDuplicate(t *testing.T) {
	assertFadedStylesDiffer(t)
	reason := transientDraftPRFailureReason("git@github.com: Permission denied (publickey)")
	sess := &pb.Session{
		Id:            "sess-draft-pr-transient-blocked",
		State:         pb.SessionState_SESSION_STATE_BLOCKED,
		DisplayLabel:  "blocked",
		DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		BlockedReason: reason,
		AttentionStatus: &pb.AttentionStatus{
			// NeedsAttention is load-bearing, not decoration: attentionWarningHint
			// returns "" without it, so the dedup branch would never be reached and
			// this test would pass with the de-duplication deleted outright.
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS,
			Summary:        *reason,
		},
	}
	hints := sessionWarningHints(sess)
	if len(hints) != 1 {
		t.Fatalf("sessionWarningHints = %+v, want exactly one hint — the attention copy of the same reason must stay deduplicated", hints)
	}
	if hints[0].Text != transientDraftPRHint {
		t.Fatalf("hint = %q, want the transient string %q", hints[0].Text, transientDraftPRHint)
	}
}

// TestAttentionHintDemotable_ClassifiesEveryReason is the exhaustive-enum guard.
// AttentionReason is actively growing and both recent additions were exemptions,
// so a new value arriving unclassified must red here rather than silently fade
// on every live row.
func TestAttentionHintDemotable_ClassifiesEveryReason(t *testing.T) {
	want := map[pb.AttentionReason]bool{
		pb.AttentionReason_ATTENTION_REASON_UNSPECIFIED:                 false,
		pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS:        true,
		pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT:        true,
		pb.AttentionReason_ATTENTION_REASON_REVIEW_REQUESTED:            true,
		pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE: true,
		pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED:           false,
		pb.AttentionReason_ATTENTION_REASON_AGENT_STALLED:               false,
	}
	for name, value := range pb.AttentionReason_value {
		reason := pb.AttentionReason(value)
		expected, ok := want[reason]
		if !ok {
			t.Errorf("AttentionReason %s is not classified for the BOS-855 recessive treatment — add it to attentionHintDemotable AND to this table, deciding deliberately whether a live row may fade it", name)
			continue
		}
		if got := attentionHintDemotable(reason); got != expected {
			t.Errorf("attentionHintDemotable(%s) = %v, want %v", name, got, expected)
		}
	}
	// The default arm stays loud for a value the switch has never seen.
	if attentionHintDemotable(pb.AttentionReason(9999)) {
		t.Error("attentionHintDemotable default arm is demotable; an unclassified reason must stay at full intensity")
	}
}

// TestAttentionWarningHint_InFlightMarkerTreatedAsAbsent closes the TUI/web
// divergence: the transient draft-PR progress marker must not paint a red
// warning across every healthy new session, but a session that genuinely needs a
// human must not lose its flag either.
func TestAttentionWarningHint_InFlightMarkerTreatedAsAbsent(t *testing.T) {
	inFlight := &pb.Session{AttentionStatus: &pb.AttentionStatus{
		NeedsAttention: true,
		Summary:        sessionreason.DraftPRCreationInFlight(),
	}}
	if got := attentionWarningHint(inFlight); got != needsAttentionFallback {
		t.Errorf("exact in-flight marker = %q, want the generic fallback %q", got, needsAttentionFallback)
	}
	// Not an exact match — left alone.
	contains := &pb.Session{AttentionStatus: &pb.AttentionStatus{
		NeedsAttention: true,
		Summary:        "stuck: draft PR creation in progress for 40 minutes",
	}}
	if got, want := attentionWarningHint(contains), "stuck: draft PR creation in progress for 40 minutes"; got != want {
		t.Errorf("summary merely containing the phrase = %q, want %q (only an exact match is filtered)", got, want)
	}
}

// TestSessionWarningHints_EdgeCasesProduceNoRow pins the empty/nil surface: no
// hint, no styled empty row, no panic.
func TestSessionWarningHints_EdgeCasesProduceNoRow(t *testing.T) {
	cases := []struct {
		name string
		sess *pb.Session
	}{
		{"nil session", nil},
		{"nil attention status", &pb.Session{DisplayLabel: "working"}},
		{"needs attention with empty summary", &pb.Session{AttentionStatus: &pb.AttentionStatus{NeedsAttention: true}}},
		{"needs attention with whitespace summary", &pb.Session{AttentionStatus: &pb.AttentionStatus{NeedsAttention: true, Summary: "   \t "}}},
		{"empty display label", &pb.Session{DisplayLabel: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionWarningHints(tc.sess); len(got) != 0 {
				t.Errorf("sessionWarningHints = %+v, want none", got)
			}
			if got := sessionWarningHintTexts(tc.sess); len(got) != 0 {
				t.Errorf("sessionWarningHintTexts = %v, want none", got)
			}
			if got := selectedSessionWarningBlock(tc.sess, nil, 80); got != "" {
				t.Errorf("selectedSessionWarningBlock = %q, want an empty string (no styled empty row)", got)
			}
			if got := sessionSubRowCount(tc.sess, ""); got != 0 {
				t.Errorf("sessionSubRowCount = %d, want 0", got)
			}
		})
	}
}

// TestSessionSubRowCount_AgreesWithBuildTableRows keeps the auxiliary-row
// accounting honest across the return-type change: the count must equal the
// number of extra rows buildTableRows actually emits, for zero, one and several
// hints.
func TestSessionSubRowCount_AgreesWithBuildTableRows(t *testing.T) {
	sessions := []*pb.Session{
		{Id: "s0", Title: "no hints", DisplayLabel: "idle"},
		{Id: "s1", Title: "one hint", DisplayLabel: "working", SetupError: "npm install failed"},
		{Id: "s2", Title: "several hints", DisplayLabel: "working",
			SetupError:             "npm install failed",
			LastRepairAttemptCount: 2,
			LastRepairExitError:    "exit status 1",
			BlockedReason:          draftPRFailureReason("gh pr create: auth required"),
		},
	}
	wantExtra := 0
	for _, sess := range sessions {
		wantExtra += sessionSubRowCount(sess, "")
	}
	if wantExtra == 0 {
		t.Fatal("fixture produced no auxiliary rows; the test would be vacuous")
	}

	home := HomeModel{sessions: sessions, width: 200, height: 60, spinner: newStatusSpinner()}
	home.buildTableRows()
	gotRows := len(home.table.Rows())
	if want := len(sessions) + wantExtra; gotRows != want {
		t.Fatalf("buildTableRows emitted %d rows, want %d (%d primary + %d auxiliary)",
			gotRows, want, len(sessions), wantExtra)
	}
	if got, want := sessionSubRowCount(sessions[0], ""), 0; got != want {
		t.Errorf("no-hint session sub-rows = %d, want %d", got, want)
	}
	if got, want := sessionSubRowCount(sessions[1], ""), 1; got != want {
		t.Errorf("one-hint session sub-rows = %d, want %d", got, want)
	}
	if got, want := sessionSubRowCount(sessions[2], ""), 3; got != want {
		t.Errorf("several-hint session sub-rows = %d, want %d", got, want)
	}
}

func authDraftPRFailureReason(detail string) *string {
	r := sessionreason.DraftPRCreationAuthFailure(errors.New(detail))
	return &r
}

// TestDraftPRFailureHint_AuthRendersAPurposeWrittenString is the 2026-09-03
// fix, and it is BOS-877's lesson applied to a second cause. That incident's
// reason was `Permission denied (publickey)`, truncated onto the row and sending
// an operator hunting a key bug that did not exist. This one's was
// `HTTP 401: Requires authentication`, truncated to "draft PR creation failed:
// creat…" — which names nothing at all, because the 26-rune prefix consumes most
// of the 48-rune budget before the error starts. An auth failure gets a fixed
// sentence naming the condition instead.
func TestDraftPRFailureHint_AuthRendersAPurposeWrittenString(t *testing.T) {
	sess := &pb.Session{
		BlockedReason: authDraftPRFailureReason(
			"create PR: HTTP 401: Requires authentication (https://api.github.com/graphql)\nTry authenticating with:  gh auth login",
		),
	}
	const want = "PR blocked — bossd's gh cannot authenticate"
	if got := draftPRFailureHint(sess); got != want {
		t.Fatalf("draftPRFailureHint = %q, want %q", got, want)
	}
	// Same guard the transient constant carries, and for the same reason:
	// draftPRFailureHint returns this constant directly and truncates only the
	// generic terminal case, so an over-length reword renders whole and drags the
	// NAME column toward its width cap. This assertion is the only thing that
	// catches it.
	if n := len([]rune(want)); n > hintReasonMaxRunes {
		t.Fatalf("the auth hint is %d runes, over the %d-rune cap", n, hintReasonMaxRunes)
	}
	// The raw transport error must not reach the row — hiding it is the point.
	if strings.Contains(draftPRFailureHint(sess), "401") {
		t.Fatal("the auth hint still leaks the raw 401 the fix exists to hide")
	}
}

// TestDraftPRFailureHint_AuthAndTransientDoNotCollide guards the branch order in
// draftPRFailureHint. The two markers are mutually exclusive by construction, so
// each reason must reach exactly its own constant and the generic terminal case
// must still truncate as before.
func TestDraftPRFailureHint_AuthAndTransientDoNotCollide(t *testing.T) {
	auth := &pb.Session{BlockedReason: authDraftPRFailureReason("HTTP 401: Requires authentication")}
	transient := &pb.Session{BlockedReason: transientDraftPRFailureReason("remote end hung up")}
	terminal := &pb.Session{BlockedReason: draftPRFailureReason("ERROR: Repository not found")}

	if got := draftPRFailureHint(auth); got != "PR blocked — bossd's gh cannot authenticate" {
		t.Fatalf("auth hint = %q", got)
	}
	if got := draftPRFailureHint(transient); got != transientDraftPRHint {
		t.Fatalf("transient hint = %q, want %q", got, transientDraftPRHint)
	}
	// The generic terminal branch is unchanged: still prefix + truncation.
	got := draftPRFailureHint(terminal)
	if !strings.HasPrefix(got, "draft PR creation failed: ") {
		t.Fatalf("terminal hint = %q, want the untouched prefixed form", got)
	}
	if len([]rune(got)) > hintReasonMaxRunes+1 {
		t.Fatalf("terminal hint = %q, %d runes — truncation regressed", got, len([]rune(got)))
	}
}
