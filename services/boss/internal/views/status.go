package views

import (
	"fmt"
	"net/url"
	"strconv"
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
	if hint := rotationRespawnCapHint(sess); hint != "" {
		hints = append(hints, hint)
	}
	if hint := setupErrorHint(sess); hint != "" {
		hints = append(hints, hint)
	}
	return hints
}

// setupErrorHint returns a short "setup script failed: <reason>" hint when
// the repo's configured setup script failed during worktree creation
// (Session.SetupError, populated server-side — see the field's proto
// comment). A setup-script failure is non-fatal, so the session still
// starts; this surfaces the degraded state in the same danger-styled
// warning block as repair/attention/rotation hints instead of leaving it
// visible only via `boss show`. The reason is truncated to its first line
// so a verbose setup-script failure (e.g. a full npm stack trace) can't
// blow out the warning block.
func setupErrorHint(sess *pb.Session) string {
	if sess == nil {
		return ""
	}
	se := sess.GetSetupError()
	if se == "" {
		return ""
	}
	return "setup script failed: " + firstLine(se)
}

// firstLine returns the portion of s before its first newline, or s
// unchanged if it contains none. Used to keep long, multi-line daemon
// error strings from blowing out single-line warning hints.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// chatStartErrorHint returns a short "chat failed to start: <reason>" hint
// for the first chat in chats whose StartError is non-empty — a tmux launch
// failure stamped by the daemon (ClaudeChat.StartError; see
// renderChatStartFailed for the per-row badge this mirrors at the session
// level). Empty when chats is nil/empty or no chat has a StartError. Mirrors
// setupErrorHint's firstLine truncation so a verbose tmux/daemon error
// string can't blow out the warning block. Deliberately NOT folded into
// sessionWarningHints: that function is shared with the home-list call
// sites, which have no chats slice to inspect — see selectedSessionWarningBlock.
func chatStartErrorHint(chats []*pb.ClaudeChat) string {
	for _, chat := range chats {
		if chat == nil {
			continue
		}
		if se := chat.GetStartError(); se != "" {
			return "chat failed to start: " + firstLine(se)
		}
	}
	return ""
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

// rotationRespawnCapHint surfaces a needs-attention warning when the session's
// newest rotation event is a respawn-cap-exhausted episode: the bound account
// keeps probing healthy while the pane stays auth-wedged, and we have hit the
// per-hour in-place respawn cap without clearing the wedge (BOS-482). Unlike an
// EXHAUSTED park (every account usage-limited) this is a plumbing wedge the
// operator likely needs to intervene on — hence a danger-styled warning rather
// than a muted history row. Empty unless the newest event is that outcome.
func rotationRespawnCapHint(sess *pb.Session) string {
	if sess == nil {
		return ""
	}
	evs := sess.GetRotationEvents()
	if len(evs) == 0 {
		return ""
	}
	if evs[0].GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED {
		return ""
	}
	return "auth wedge unresolved — respawn cap reached, may need /login"
}

// rotationHistoryBlock renders the session's recent rotation decisions
// (newest-first, capped) as a muted block, e.g.
// "15:02 yuki@kamik.ai switched to dave@kamik.ai — resumed". The per-row
// timestamp is date-aware relative to now (see rotationEventTime). Returns ""
// when the session has no rotation history so callers can skip rendering
// entirely (no layout shift).
func rotationHistoryBlock(sess *pb.Session, width int, now time.Time) string {
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
		// Assemble time + label + detail, tolerating an empty label: a
		// generic (UNSPECIFIED) event drops the label entirely and renders
		// "<time> <detail>" with no "— " join and no double space (BOS-432).
		label := rotationEventLabel(ev)
		detail := rotationEventDetail(ev)
		line := rotationEventTime(ev.GetCreatedAt().AsTime(), now)
		switch {
		case label != "" && detail != "":
			line += " " + label + " — " + detail
		case label != "":
			line += " " + label
		case detail != "":
			line += " " + detail
		}
		lines = append(lines, line)
	}
	return styleStatusMuted.Width(width).Render(strings.Join(lines, "\n"))
}

// rotationEventTime renders a rotation event's timestamp for a history row:
// time-only ("15:04") when the event falls on the same local calendar day as
// now, otherwise ISO date + time ("2006-01-02 15:04") so older events carry a
// date. Both sides are converted to local time before comparison (BOS-432).
func rotationEventTime(t, now time.Time) string {
	lt := t.Local()
	n := now.Local()
	if lt.Year() == n.Year() && lt.YearDay() == n.YearDay() {
		return lt.Format("15:04")
	}
	return lt.Format("2006-01-02 15:04")
}

// rotationEventDetail preserves outcome-specific context. The TrimPrefix is a
// LEGACY-ROW shim only: manual-switch audits written before the daemon split
// the pane notice from the audit Detail (switch_account.go step 6) stored the
// full "switched to <account> — resumed" sentence, which would duplicate the
// label line. New rows carry only the outcome suffix ("resumed" / "started
// fresh (…)"), so the prefix never matches and the detail passes through.
func rotationEventDetail(ev *pb.RotationEvent) string {
	detail := strings.TrimSpace(ev.GetDetail())
	if ev.GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_ROTATED {
		return detail
	}
	prefix := "switched to " + ev.GetToAccount() + " —"
	return strings.TrimSpace(strings.TrimPrefix(detail, prefix))
}

// rotationEventLabel renders a single rotation decision as a short,
// human-readable phrase keyed off its outcome (BOS-176).
func rotationEventLabel(ev *pb.RotationEvent) string {
	switch ev.GetOutcome() {
	case pb.RotationOutcome_ROTATION_OUTCOME_ROTATED:
		// Both sides render as account labels (emails). Legacy rows written
		// before the from-side was resolved to a label carry an empty from —
		// drop the leading space rather than render " switched to X".
		if ev.GetFromAccount() == "" {
			return fmt.Sprintf("switched to %s", ev.GetToAccount())
		}
		return fmt.Sprintf("%s switched to %s", ev.GetFromAccount(), ev.GetToAccount())
	case pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_DISABLED:
		return "rotation disabled — status only"
	case pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY:
		return "agent cannot rotate — status only"
	case pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT:
		return "no eligible account — status only"
	case pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED:
		return "all accounts limited"
	case pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT:
		// The bound account probed healthy but the pane stayed auth-wedged
		// (stale injected proxy token) — we respawned it in place on the same
		// account rather than rotate away (BOS-482).
		if ev.GetToAccount() == "" {
			return "refreshed auth in place"
		}
		return fmt.Sprintf("refreshed auth in place on %s", ev.GetToAccount())
	case pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED:
		return "auth-wedge respawn cap reached"
	case pb.RotationOutcome_ROTATION_OUTCOME_FAILED:
		return fmt.Sprintf("switch to %s failed", ev.GetToAccount())
	default:
		// A generic/unspecified outcome (e.g. a BOS-409 stale-port notice with
		// the whole message in Detail) carries no meaningful label. Return ""
		// so the row renders "<time> <detail>" with no filler prefix (BOS-432).
		// Callers that surface a label standalone (toast.go) fall back to the
		// detail so the notice is never empty.
		return ""
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
	// shift is statically in [0,16] here (attemptCount>0 ⇒ shift>=0, clamped to
	// <=16 above), and Go permits a signed non-negative shift count, so no
	// int→uint conversion is needed — dropping it removes the G115 finding.
	wait := base << shift
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
// chats supplies the session's chats so a per-chat StartError (tmux launch
// failure) can be surfaced alongside the session-level hints — the session
// proto has no chats slice of its own, and sessionWarningHints is shared
// with chats-less home-list callers, so that iteration lives here instead.
// Returns "" when the session has no hints, so callers can skip rendering
// entirely (no empty block, no layout shift).
func selectedSessionWarningBlock(sess *pb.Session, chats []*pb.ClaudeChat, width int) string {
	hints := sessionWarningHints(sess)
	if hint := chatStartErrorHint(chats); hint != "" {
		hints = append(hints, hint)
	}
	if len(hints) == 0 {
		return ""
	}
	// Dim a resolved session's residual warning so it no longer alarms; mirrors
	// the merged/closed hint fade in the home table (BOS-246).
	style := styleStatusDanger
	if sess != nil &&
		(sess.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_MERGED ||
			sess.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CLOSED) {
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

// osc8Link wraps an already-styled label in an OSC 8 hyperlink envelope
// targeting url, so a cmd/ctrl+click in a supporting terminal opens it.
//
// This is the single place the envelope's byte shape lives: ESC ] 8 ; ; <url>
// ESC \ <label> ESC ] 8 ; ; ESC \. Every link renderer below styles its label
// with RAW ANSI (never lipgloss — lipgloss.Render on a string containing OSC 8
// strips the leading ESC bytes and leaves the payload visible) and then calls
// this to make it clickable. Callers are responsible for validating url; see
// isHTTPEndpointURL for the endpoint path, where the URL is daemon-supplied
// rather than a well-known PR/tracker URL.
func osc8Link(url, styledLabel string) string {
	return "\x1b]8;;" + url + "\x1b\\" + styledLabel + "\x1b]8;;\x1b\\"
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
		return osc8Link(*sess.PrUrl, underlined)
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
		return osc8Link(*sess.PrUrl, styled)
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
		linked = osc8Link(*sess.TrackerUrl, underlined)
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
		linked = osc8Link(*sess.TrackerUrl, styledTarget)
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
		linked = osc8Link(*sess.TrackerUrl, styledTarget)
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
		return osc8Link(*sess.PrUrl, styled)
	}
	return styled
}

// --- Session HTTP endpoints (BOS-474) --------------------------------------

// SGR envelopes for the muted endpoint labels rendered beneath a session (Home)
// and above the chat table (ChatPicker). Raw ANSI rather than lipgloss so they
// can bracket an OSC 8 hyperlink without the Render path mangling the envelope
// — the same constraint the merged/selected link constants above document.
//
// 38;2;98;98;98 = muted gray fg (#626262). The underline variant pins the
// underline color with 58;2;98;98;98 so it matches the text instead of
// inheriting whatever SGR 58 the previous row left set, and adds 4 = underline
// to mark the label as clickable.
const (
	mutedTextOpen       = "\x1b[38;2;98;98;98m"
	mutedTextClose      = "\x1b[39m"
	mutedUnderlineOpen  = "\x1b[38;2;98;98;98;58;2;98;98;98;4m"
	mutedUnderlineClose = "\x1b[39;59;24m"
	// endpointSeparator joins multiple endpoint labels. Rendered muted so the
	// clickable ":port" labels remain the only emphasized tokens.
	endpointSeparator = " · "
)

// renderableEndpoints returns the session's HTTP endpoints that have something
// to show: non-nil with a non-zero port. The port is the label, so an endpoint
// without one cannot be rendered — and an endpoint with a port but a bad URL is
// still worth showing (just not as a hyperlink), so URL validity is NOT part of
// this filter. Returns nil when there is nothing to render, which is what every
// caller tests to decide whether an endpoint row/line exists at all.
func renderableEndpoints(sess *pb.Session) []*pb.HttpEndpoint {
	if sess == nil {
		return nil
	}
	var out []*pb.HttpEndpoint
	for _, ep := range sess.GetHttpEndpoints() {
		if ep == nil || ep.GetPort() == 0 {
			continue
		}
		out = append(out, ep)
	}
	return out
}

// sessionHasEndpointRow reports whether the Home session list must emit an
// endpoint auxiliary row for this session.
//
// This is the ONE predicate both the renderer (buildTableRows) and the
// accounting (sessionSubRowCount) ask. Deriving "is there a row?" separately
// from the rendered string and from the measured labels is exactly the desync
// that strands the cursor on an unselectable row, so neither caller is allowed
// to test emptiness of its own artifact.
func sessionHasEndpointRow(sess *pb.Session) bool {
	return len(renderableEndpoints(sess)) > 0
}

// sessionEndpointLabels returns the VISIBLE (unstyled) endpoint text, e.g.
// ":3000 · :5173", or "" when the session has no renderable endpoints. Used for
// column-width measurement, where styling bytes must not be counted — NOT for
// deciding whether the endpoint row exists (see sessionHasEndpointRow).
func sessionEndpointLabels(sess *pb.Session) string {
	eps := renderableEndpoints(sess)
	if len(eps) == 0 {
		return ""
	}
	labels := make([]string, 0, len(eps))
	for _, ep := range eps {
		labels = append(labels, endpointLabel(ep))
	}
	return strings.Join(labels, endpointSeparator)
}

// endpointLabel returns the compact ":<port>" label for one endpoint.
func endpointLabel(ep *pb.HttpEndpoint) string {
	return ":" + strconv.FormatUint(uint64(ep.GetPort()), 10)
}

// renderSessionEndpoints returns the muted, individually clickable endpoint
// labels for a session, joined by a muted " · ", or "" when there is nothing to
// render. Each label is underlined and wrapped in an OSC 8 hyperlink ONLY when
// its URL passes isHTTPEndpointURL; an endpoint whose URL is missing, non-HTTP,
// or otherwise unsafe still shows its port, plainly muted and not underlined,
// so the operator sees the port without the TUI emitting an escape envelope
// around an untrusted target.
func renderSessionEndpoints(sess *pb.Session) string {
	eps := renderableEndpoints(sess)
	if len(eps) == 0 {
		return ""
	}
	var b strings.Builder
	for i, ep := range eps {
		if i > 0 {
			b.WriteString(mutedTextOpen + endpointSeparator + mutedTextClose)
		}
		label := endpointLabel(ep)
		if url := ep.GetUrl(); isHTTPEndpointURL(url) {
			b.WriteString(osc8Link(url, mutedUnderlineOpen+label+mutedUnderlineClose))
			continue
		}
		b.WriteString(mutedTextOpen + label + mutedTextClose)
	}
	return b.String()
}

// isHTTPEndpointURL reports whether raw is safe to place inside an OSC 8
// hyperlink target: a parseable absolute URL with an http/https scheme, a
// non-empty host, and no control or space bytes.
//
// The control-byte check is the load-bearing one. The OSC 8 envelope is
// delimited by ESC bytes, so a URL carrying ESC/BEL/newline could terminate the
// introducer early and inject arbitrary escape sequences into the operator's
// terminal. url.Parse already rejects ASCII control characters, but this is
// asserted explicitly rather than relying on that as an implementation detail.
// A raw space (0x20) is rejected on the same pass: url.Parse accepts it, but a
// URI containing one is invalid per RFC 3986, so the terminal would render a
// dead link rather than a working one.
func isHTTPEndpointURL(raw string) bool {
	if raw == "" {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 || raw[i] == 0x7f {
			return false
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return false
	}
	return u.Host != ""
}

// sessionSubRowCount returns how many NON-SELECTABLE auxiliary rows the Home
// session list renders directly beneath a session's primary row: the endpoint
// row (0 or 1) followed by one row per warning hint.
//
// This is the single source of truth for auxiliary-row accounting. Row
// construction, table height, cursor↔session mapping, and cursor normalization
// all route through it — if any of them counted independently they could
// disagree and strand the cursor on an unselectable row (BOS-474).
func sessionSubRowCount(sess *pb.Session) int {
	n := len(sessionWarningHints(sess))
	if sessionHasEndpointRow(sess) {
		n++
	}
	return n
}
