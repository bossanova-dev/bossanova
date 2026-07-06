package views

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Status constants (previously from bosspty package).
const (
	statusWorking  = "working"
	statusIdle     = "idle"
	statusQuestion = "question"
	statusStopped  = "stopped"
	statusLimited  = "limited"
)

// newStatusSpinner creates an unstyled spinner for status display.
// Color is applied at the call site so the entire cell has a single ANSI wrap.
func newStatusSpinner() spinner.Model {
	return spinner.New(spinner.WithSpinner(spinner.Dot))
}

// chatStatusString converts a protobuf ChatStatus enum to a bosspty.Status* string.
func chatStatusString(s pb.ChatStatus) string {
	switch s {
	case pb.ChatStatus_CHAT_STATUS_WORKING:
		return statusWorking
	case pb.ChatStatus_CHAT_STATUS_IDLE:
		return statusIdle
	case pb.ChatStatus_CHAT_STATUS_QUESTION:
		return statusQuestion
	case pb.ChatStatus_CHAT_STATUS_LIMITED:
		return statusLimited
	default:
		return statusStopped
	}
}

// renderArchivingStatus renders the optimistic "archiving" status with an
// animated spinner, used while a session archive is in flight and across the
// navigation that follows it. Mirrors the in-progress "checking" styling in
// renderDisplayStatus so the home list and the chatpicker banner match.
func renderArchivingStatus(sp spinner.Model) string {
	return styleStatusWarning.Render(sp.View() + "archiving")
}

// styledPRStatus returns a styled label for a PR display status.
// Returns "" for unspecified/unknown statuses.
func styledPRStatus(sess *pb.Session, sp spinner.Model) string {
	switch sess.DisplayStatus {
	case pb.DisplayStatus_DISPLAY_STATUS_MERGED:
		return styleStatusMuted.Render("✓ merged")
	case pb.DisplayStatus_DISPLAY_STATUS_CLOSED:
		return styleStatusMuted.Render("closed")
	case pb.DisplayStatus_DISPLAY_STATUS_APPROVED:
		return styleStatusSuccess.Render("✓ approved")
	case pb.DisplayStatus_DISPLAY_STATUS_PASSING:
		return styleStatusSuccess.Render("✓ passing")
	case pb.DisplayStatus_DISPLAY_STATUS_REVIEW:
		return styleStatusSuccess.Render("✓ review")
	case pb.DisplayStatus_DISPLAY_STATUS_FAILING:
		return styleStatusDanger.Render("⨯ failing")
	case pb.DisplayStatus_DISPLAY_STATUS_CONFLICT:
		return styleStatusDanger.Render("⨯ conflict")
	case pb.DisplayStatus_DISPLAY_STATUS_REJECTED:
		return styleStatusDanger.Render("⨯ rejected")
	case pb.DisplayStatus_DISPLAY_STATUS_DRAFT:
		return styleStatusMuted.Render("draft")
	case pb.DisplayStatus_DISPLAY_STATUS_CHECKING:
		s := styleStatusWarning
		if sess.DisplayHasChangesRequested || sess.DisplayHasFailures {
			s = styleStatusDanger
		}
		return s.Render(sp.View() + "checking")
	default:
		return ""
	}
}

// renderDisplayStatus renders the unified STATUS column directly from the
// composite display fields (DisplayLabel/DisplayIntent/DisplaySpinner) computed
// by bossd. Clients no longer recompose — they just style.
func renderDisplayStatus(sess *pb.Session, sp spinner.Model) string {
	if sess == nil {
		return ""
	}
	label := sess.GetDisplayLabel()
	if sess.GetDisplaySpinner() {
		label = sp.View() + label
	}
	return styleForIntent(sess.GetDisplayIntent()).Render(label)
}

func sessionWarningHints(sess *pb.Session) []string {
	if sess == nil {
		return nil
	}
	hints := make([]string, 0, 3)
	if hint := repairHint(sess); hint != "" {
		hints = append(hints, hint)
	}
	if hint := attentionWarningHint(sess); hint != "" {
		hints = append(hints, hint)
	}
	if hint := rotationExhaustedHint(sess, time.Now()); hint != "" {
		hints = append(hints, hint)
	}
	return hints
}

// rotationExhaustedHint returns "all accounts limited, resumes in ~Xm" when the
// session's newest rotation event is an unexpired exhausted episode — every
// account the agent can rotate to is usage-limited (BOS-176). Empty when there
// is no rotation history, the newest event is not an exhausted outcome, or the
// parked reset time has already passed (the operator should resume). The
// countdown mirrors the repair retry-in hint so the operator sees why the
// session is parked rather than a bare "limited" with no ETA.
func rotationExhaustedHint(sess *pb.Session, now time.Time) string {
	if sess == nil {
		return ""
	}
	evs := sess.GetRotationEvents()
	if len(evs) == 0 {
		return ""
	}
	latest := evs[0] // hydrated newest-first
	if latest.GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED {
		return ""
	}
	reset := latest.GetResetAt()
	if reset == nil {
		return "all accounts limited"
	}
	remaining := reset.AsTime().Sub(now)
	if remaining <= 0 {
		return ""
	}
	return fmt.Sprintf("all accounts limited, resumes in ~%s", shortDuration(remaining))
}

// rotationHistoryBlock renders the session's recent rotation decisions
// (newest-first, capped) as a muted block, e.g.
// "15:02 acct-a → acct-b rotated". Returns "" when the session has no
// rotation history so callers can skip rendering entirely (no layout shift).
func rotationHistoryBlock(sess *pb.Session, width int) string {
	if sess == nil {
		return ""
	}
	evs := sess.GetRotationEvents()
	if len(evs) == 0 {
		return ""
	}
	const maxRows = 5
	lines := make([]string, 0, maxRows+1)
	lines = append(lines, "Rotation history")
	for i, ev := range evs {
		if i == maxRows {
			break
		}
		line := fmt.Sprintf("%s %s",
			ev.GetCreatedAt().AsTime().Local().Format("15:04"),
			rotationEventLabel(ev))
		if detail := strings.TrimSpace(ev.GetDetail()); detail != "" {
			line += " — " + detail
		}
		lines = append(lines, line)
	}
	return styleStatusMuted.Width(width).Render(strings.Join(lines, "\n"))
}

// rotationEventLabel renders a single rotation decision as a short,
// human-readable phrase keyed off its outcome (BOS-176).
func rotationEventLabel(ev *pb.RotationEvent) string {
	switch ev.GetOutcome() {
	case pb.RotationOutcome_ROTATION_OUTCOME_ROTATED:
		return fmt.Sprintf("%s → %s rotated", ev.GetFromAccount(), ev.GetToAccount())
	case pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_DISABLED:
		return "rotation disabled — status only"
	case pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY:
		return "agent cannot rotate — status only"
	case pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED:
		return "all accounts limited"
	case pb.RotationOutcome_ROTATION_OUTCOME_FAILED:
		return fmt.Sprintf("switch to %s failed", ev.GetToAccount())
	default:
		return "rotation event"
	}
}

func attentionWarningHint(sess *pb.Session) string {
	if sess == nil || sess.GetAttentionStatus() == nil || !sess.GetAttentionStatus().GetNeedsAttention() {
		return ""
	}
	summary := strings.TrimSpace(sess.GetAttentionStatus().GetSummary())
	return summary
}

// repairFailureHint returns a short suffix like "repair failed (5×,
// retry in ~16m)" when the session's last repair attempt failed. Empty
// when there has been no attempt or the last attempt was clean. Kept
// distinct from the main STATUS label so the existing `failing` /
// `repairing` rendering stays intact and the hint reads as auxiliary
// context.
//
// The "retry in" suffix surfaces the exponential-backoff window the
// repair plugin enforces — without it the operator would see "repair
// failed (5×)" with no clue why no new attempt is firing. Estimate is
// based on the default 1-minute base cooldown; operators who tune
// CooldownMinutes will see a slightly inaccurate ETA but still in the
// right ballpark.
// repairHint returns the single repair-related warning suffix to show
// for a session, blocked-reason first, else the failure counter. The
// blocked hint takes precedence because the daemon clears the blocked
// pair on every real repair outcome (BOS-153 Task 4) — so a non-empty
// blocked reason IS the latest repair-related state and needs no
// timestamp comparison against the failure fields.
func repairHint(sess *pb.Session) string {
	if hint := repairBlockedHint(sess); hint != "" {
		return hint
	}
	return repairFailureHint(sess)
}

// repairBlockedHint returns a short suffix like
// "repair blocked: <reason>" when the daemon most recently refused to
// start or displace a repair chat (e.g. the live pane is at a prompt
// and can't be safely reclaimed). Empty when there is no blocked
// reason. The reason is truncated to ~48 runes with a trailing ellipsis
// so a long daemon message can't blow out the warning block. Rune-based
// truncation avoids splitting a multibyte character mid-sequence.
func repairBlockedHint(sess *pb.Session) string {
	reason := sess.GetLastRepairBlockedReason()
	if reason == "" {
		return ""
	}
	if runes := []rune(reason); len(runes) > 48 {
		reason = string(runes[:48]) + "…"
	}
	return "repair blocked: " + reason
}

func repairFailureHint(sess *pb.Session) string {
	count := sess.GetLastRepairAttemptCount()
	if count == 0 {
		return ""
	}
	if sess.GetLastRepairRunnerError() == "" && sess.GetLastRepairExitError() == "" {
		return ""
	}
	if repairFailureResolved(sess) {
		return ""
	}
	base := fmt.Sprintf("repair failed (%d×)", count)
	if count == 1 {
		base = "repair failed"
	}
	startedAt := sess.GetLastRepairStartedAt()
	if startedAt == nil {
		return base
	}
	remaining := repairRetryRemaining(count, startedAt.AsTime())
	if remaining <= 0 {
		return base
	}
	return fmt.Sprintf("%s, retry in ~%s", base, shortDuration(remaining))
}

func repairFailureResolved(sess *pb.Session) bool {
	switch sess.GetDisplayStatus() {
	case pb.DisplayStatus_DISPLAY_STATUS_MERGED,
		pb.DisplayStatus_DISPLAY_STATUS_CLOSED:
		return true
	case pb.DisplayStatus_DISPLAY_STATUS_PASSING:
		return true
	case pb.DisplayStatus_DISPLAY_STATUS_CHECKING:
		return !sess.GetDisplayHasFailures() && !sess.GetDisplayHasChangesRequested()
	default:
		return false
	}
}

// repairRetryRemaining mirrors the repair plugin's cooldownFor()
// schedule so the TUI can show an accurate retry-in estimate without
// importing from plugins/bossd-plugin-repair. Schedule with the 1-min
// base: 1m, 2m, 4m, 8m, 16m, then capped at 30m. attemptCount<=0
// returns 0 (no failures recorded).
func repairRetryRemaining(attemptCount int32, startedAt time.Time) time.Duration {
	if attemptCount <= 0 {
		return 0
	}
	const base = time.Minute
	const maxWait = 30 * time.Minute
	shift := int(attemptCount) - 1
	if shift > 16 {
		shift = 16
	}
	wait := base << uint(shift)
	if wait <= 0 || wait > maxWait {
		wait = maxWait
	}
	return wait - time.Since(startedAt)
}

// shortDuration renders a Duration as the most informative compact
// label for the repair retry-in hint: "Xm" for >= 1 minute, "Xs"
// otherwise. The repair backoff lives entirely in minute-or-larger
// land so the seconds branch is mostly defensive.
func shortDuration(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
}

// selectedSessionWarningBlock returns a full-width, danger-styled block
// containing all warning hints for the given session, joined by newlines.
// Returns "" when the session has no hints, so callers can skip rendering
// entirely (no empty block, no layout shift).
func selectedSessionWarningBlock(sess *pb.Session, width int) string {
	hints := sessionWarningHints(sess)
	if len(hints) == 0 {
		return ""
	}
	// Dim a resolved session's residual warning so it no longer alarms; mirrors
	// the merged/closed hint fade in the home table (BOS-246).
	style := styleStatusDanger
	if sess.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_MERGED ||
		sess.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CLOSED {
		style = styleStatusDangerFaded
	}
	return style.Width(width).Render(strings.Join(hints, "\n"))
}

// styleForIntent maps a DisplayIntent to its lipgloss style for the TUI.
func styleForIntent(intent pb.DisplayIntent) lipgloss.Style {
	switch intent {
	case pb.DisplayIntent_DISPLAY_INTENT_SUCCESS:
		return styleStatusSuccess
	case pb.DisplayIntent_DISPLAY_INTENT_WARNING:
		return styleStatusWarning
	case pb.DisplayIntent_DISPLAY_INTENT_DANGER:
		return styleStatusDanger
	case pb.DisplayIntent_DISPLAY_INTENT_INFO:
		return styleStatusInfo
	default:
		return styleStatusMuted
	}
}

// renderClaudeStatus returns a styled status string for a Claude process
// (working/idle/stopped) without PR display context.
func renderClaudeStatus(status string, sp spinner.Model) string {
	switch status {
	case statusQuestion:
		return styleStatusWarning.Render("? question")
	case statusLimited:
		return styleStatusWarning.Render("limited")
	case statusWorking:
		return styleStatusSuccess.Render(sp.View() + "working")
	case statusIdle:
		return styleStatusWarning.Render("idle")
	default:
		return styleStatusMuted.Render("stopped")
	}
}

// renderChatStartFailed returns a styled status string for a chat that
// never came up because StartTmuxChat hit a failure before SendPlan
// succeeded (e.g. SendPlan timeout when claude bailed with its
// "--print needs stdin or prompt" error, ConfigureFinalizeHook RPC
// failure). The chat row is preserved with StartError set so the
// operator can see what was attempted — this is the status badge that
// surfaces that.
func renderChatStartFailed() string {
	return styleStatusDanger.Render("× failed")
}

// renderPRLink returns an underlined, OSC 8 hyperlinked PR label (e.g. "#12")
// that opens the PR URL on cmd+click. Returns plain label if no URL is available.
// Uses raw ANSI underline escapes (not lipgloss) so the table's row-level
// foreground color is inherited rather than overridden.
func renderPRLink(sess *pb.Session) string {
	if sess == nil || sess.PrNumber == nil {
		return ""
	}
	label := fmt.Sprintf("#%d", *sess.PrNumber)
	underlined := "\x1b[4m" + label + "\x1b[24m"
	if sess.PrUrl != nil && *sess.PrUrl != "" {
		return fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", *sess.PrUrl, underlined)
	}
	return underlined
}

// renderMutedPRLink returns a muted, strikethrough, underlined, OSC 8
// hyperlinked PR label for merged/closed rows. Uses raw ANSI escapes (not
// lipgloss) to avoid SGR resets that break the OSC 8 hyperlink context.
func renderMutedPRLink(sess *pb.Session) string {
	if sess == nil || sess.PrNumber == nil {
		return ""
	}
	label := fmt.Sprintf("#%d", *sess.PrNumber)
	// SGR 38;2;98;98;98 = muted gray foreground (#626262)
	// SGR 58;2;98;98;98 = matching muted gray underline color (otherwise the
	// underline picks up whatever SGR 58 was last set, e.g. the row-selected
	// highlight color, and visually mismatches the strikethrough).
	// SGR 9 = strikethrough, SGR 4 = underline
	styled := "\x1b[38;2;98;98;98;58;2;98;98;98;9;4m" + label + "\x1b[39;59;29;24m"
	if sess.PrUrl != nil && *sess.PrUrl != "" {
		return fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", *sess.PrUrl, styled)
	}
	return styled
}

// renderTrackerLink replaces the [tracker_id] portion of a session title with
// an OSC 8 hyperlinked + underlined version. Returns the original title if
// the session has no tracker ID or the ID is not found in the title.
func renderTrackerLink(sess *pb.Session, title string) string {
	if sess == nil || sess.TrackerId == nil || *sess.TrackerId == "" {
		return title
	}
	target := "[" + *sess.TrackerId + "]"
	idx := strings.Index(title, target)
	if idx < 0 {
		return title
	}
	underlined := "\x1b[4m" + target + "\x1b[24m"
	var linked string
	if sess.TrackerUrl != nil && *sess.TrackerUrl != "" {
		linked = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", *sess.TrackerUrl, underlined)
	} else {
		linked = underlined
	}
	return title[:idx] + linked + title[idx+len(target):]
}

// SGR envelopes for merged/closed title text. Raw ANSI (not lipgloss) so they
// can bracket an OSC 8 hyperlink without the lipgloss Render path mangling the
// hyperlink envelope.
const (
	// 38;2;98;98;98 = muted gray fg (#626262); 9 = strikethrough.
	mutedStrikeOpen  = "\x1b[38;2;98;98;98;9m"
	mutedStrikeClose = "\x1b[39;29m"
	// Same as above with 4 = underline. 58;2;98;98;98 pins the underline
	// color to muted gray so it matches the strikethrough — otherwise the
	// underline inherits whatever SGR 58 was last set (e.g. the highlight
	// color from a selected row), producing a mismatched colored line.
	mutedStrikeUnderlineOpen  = "\x1b[38;2;98;98;98;58;2;98;98;98;9;4m"
	mutedStrikeUnderlineClose = "\x1b[39;59;29;24m"
)

// renderMutedTrackerLink returns the full title styled muted + strikethrough
// for merged/closed rows. If the title contains the session's [tracker_id], it
// is additionally underlined and wrapped in an OSC 8 hyperlink to the tracker
// URL. Styling is done with raw ANSI rather than lipgloss so the OSC 8 envelope
// survives intact — lipgloss.Render on a string containing OSC 8 strips the
// leading ESC bytes and leaves the payload visible.
func renderMutedTrackerLink(sess *pb.Session, title string) string {
	wrap := func(s string) string {
		if s == "" {
			return ""
		}
		return mutedStrikeOpen + s + mutedStrikeClose
	}
	if sess == nil || sess.TrackerId == nil || *sess.TrackerId == "" {
		return wrap(title)
	}
	target := "[" + *sess.TrackerId + "]"
	idx := strings.Index(title, target)
	if idx < 0 {
		return wrap(title)
	}
	styledTarget := mutedStrikeUnderlineOpen + target + mutedStrikeUnderlineClose
	linked := styledTarget
	if sess.TrackerUrl != nil && *sess.TrackerUrl != "" {
		linked = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", *sess.TrackerUrl, styledTarget)
	}
	return wrap(title[:idx]) + linked + wrap(title[idx+len(target):])
}

// SGR envelopes for selected-row text. Raw ANSI (not lipgloss) so they can
// bracket an OSC 8 hyperlink without lipgloss's Render path mangling the
// hyperlink envelope. 1 = bold, 38;2;76;167;248 = selection blue fg (#4CA7F8).
// The selected row is bold (see bossTableStyles().Selected); the table applies
// blue at the row level, but a styled leading cell (the attention "!") emits a
// full reset that cancels it for later cells — so we re-assert blue here.
const (
	selectedTextOpen  = "\x1b[1;38;2;76;167;248m"
	selectedTextClose = "\x1b[22;39m"
	// Same as above plus 58;2;76;167;248 = blue underline color, 4 = underline.
	// Pinning the underline color stops it inheriting whatever SGR 58 was last
	// set (e.g. a previous row's highlight) and mismatching the text.
	selectedUnderlineOpen  = "\x1b[1;38;2;76;167;248;58;2;76;167;248;4m"
	selectedUnderlineClose = "\x1b[22;39;59;24m"
)

// renderSelectedText wraps plain (non-hyperlinked) cell text in the selected
// row's bold blue styling. Returns "" unchanged.
func renderSelectedText(s string) string {
	if s == "" {
		return ""
	}
	return selectedTextOpen + s + selectedTextClose
}

// renderSelectedTrackerLink returns the full title in the selected row's bold
// blue. If the title contains the session's [tracker_id], that token is also
// underlined and wrapped in an OSC 8 hyperlink to the tracker URL. Raw ANSI
// (not lipgloss) so the OSC 8 envelope survives — see renderMutedTrackerLink.
func renderSelectedTrackerLink(sess *pb.Session, title string) string {
	wrap := func(s string) string {
		if s == "" {
			return ""
		}
		return selectedTextOpen + s + selectedTextClose
	}
	if sess == nil || sess.TrackerId == nil || *sess.TrackerId == "" {
		return wrap(title)
	}
	target := "[" + *sess.TrackerId + "]"
	idx := strings.Index(title, target)
	if idx < 0 {
		return wrap(title)
	}
	styledTarget := selectedUnderlineOpen + target + selectedUnderlineClose
	linked := styledTarget
	if sess.TrackerUrl != nil && *sess.TrackerUrl != "" {
		linked = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", *sess.TrackerUrl, styledTarget)
	}
	return wrap(title[:idx]) + linked + wrap(title[idx+len(target):])
}

// renderSelectedPRLink returns the PR label (#N) in the selected row's bold
// blue + underline, wrapped in an OSC 8 hyperlink to the PR URL when present.
// Returns "" when the session has no PR. Raw ANSI mirrors renderMutedPRLink.
func renderSelectedPRLink(sess *pb.Session) string {
	if sess == nil || sess.PrNumber == nil {
		return ""
	}
	label := fmt.Sprintf("#%d", *sess.PrNumber)
	styled := selectedUnderlineOpen + label + selectedUnderlineClose
	if sess.PrUrl != nil && *sess.PrUrl != "" {
		return fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", *sess.PrUrl, styled)
	}
	return styled
}
