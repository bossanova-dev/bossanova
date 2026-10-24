// Home's render helpers: the four View branch renderers and the shared status
// line/width helpers. Split out of home.go (BOS-526); the declarations are
// unchanged.

package home

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"

	"github.com/recurser/boss/internal/auth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// renderError renders an error message that wraps to the given terminal width.
// A width too narrow to wrap into (including 0, meaning unknown) falls back to
// no width constraint.
//
// lipgloss .Width() sets the TOTAL block width with padding included, so the
// terminal width is passed through as-is; subtracting the padding would render
// the block 4 columns narrower than the terminal (BOS-531, under the BOS-507
// epic). For the same reason callers must pass the RESULT through bare:
// re-wrapping it in another padding style adds 2 columns each side and overflows
// the terminal by 4.
func renderError(msg string, width int) string { return ui.RenderError(msg, width) }

// statusWrapWidth returns the width Home's status and warning lines wrap at.
// It tracks the rendered table so status text lines up with the TUI content
// block instead of running to the terminal edge (BOS-507).
//
// Returns 0 when the terminal width is unknown (no tea.WindowSizeMsg yet) or
// too narrow to wrap into at all; lipgloss treats Width(0) as unconstrained,
// preserving the previous single-line behaviour.
//
// Derived from the columns rather than h.table.Width() because the table's
// width is set from two places with different meanings (buildTableRows uses
// columnsWidth, the resize handler uses the terminal width), so Width() is
// whichever ran last. The len(h.sessions) > 0 gate mirrors the condition
// View() uses to draw the table at all, so an empty state never wraps at the
// width of a table that is no longer on screen.
//
// Since BOS-572 the columns this reads are the WIDTH-FITTED set —
// buildTableRows runs them through fitColumnsIndexed, so on a narrow board they
// are already the dropped/squeezed columns actually on screen. The rule has
// always been "track whatever the table drew", and that needs no change here.
//
// It is NOT, however, free, and the honest summary is a mechanism rather than a
// taxonomy — three review rounds each produced a confidently-wrong enumeration
// of "the cases", so this comment deliberately no longer claims to have them
// all. What is certain:
//
//   - "Tracks the table" is bounded on both sides. The minStatusWrapWidth floor
//     overrides a narrower table from above; the clamp to h.width overrides
//     from below. Between them the status block can be WIDER than the table it
//     sits under, and BOS-572 makes that easier to reach, because the table now
//     narrows with the terminal instead of holding its declared width. Whether
//     any given board's overhang is new or pre-existing depends on that board's
//     declared width — a board that already fits sees a no-op fit and its
//     overhang is BOS-507's pre-existing floor behaviour, while a board that
//     does not fit can reach the floor through drop-overshoot and its overhang
//     is new. Do not read either case as the general one.
//   - The status block cannot overhang the SCREEN; that is exactly what the
//     clamp below guarantees. The TABLE still can — fitColumnsIndexed rule 6
//     accepts overflow rather than emit a non-positive width, so a terminal
//     narrower than the priority-0 floors renders a table wider than itself.
//     The clamp protects this block only.
//
// Measured examples, so the above is not just prose: with the shared
// responsiveHomeSessions fixture the block never exceeds the table at any width
// from 20 to 200. With a one-short-session board (declared columnsWidth 39,
// rendering as 41 inside renderSessionTable's Padding(0, 1)) the 60-column
// floor overhangs by 19 at a 60-column terminal, and at a 30-column terminal
// the same board draws an 11-column table — every droppable column gone — under
// a 30-column status line set by the clamp.
func (h Model) statusWrapWidth() int {
	// A terminal too narrow to hold the Padding(0, 2) block leaves lipgloss no
	// columns to wrap into: it computes wrapAt = width - leftPad - rightPad and
	// silently skips wrapping when that is <= 0, so any width here that is not
	// > statusLinePadding*2 renders exactly like Width(0). Say so explicitly
	// rather than returning a width that only looks like it constrains.
	if h.Width <= ui.StatusLinePadding*2 {
		return 0
	}
	w := 0
	if len(h.Sessions) > 0 {
		w = ui.ColumnsWidth(h.Table.Columns())
	}
	if w < ui.MinStatusWrapWidth {
		w = ui.MinStatusWrapWidth
	}
	if w > h.Width {
		w = h.Width
	}
	return w
}

// statusLine renders one Home status/warning line: the shared Padding(0, 2)
// block, an optional foreground colour, and the wrap width from
// statusWrapWidth. Every Home *status* line goes through here so the wrap rule
// cannot be missed at one call site (and so the next status line added
// inherits it). Pass a nil colour for an unstyled line.
func (h Model) statusLine(fg color.Color, text string) string {
	s := lipgloss.NewStyle().Padding(0, ui.StatusLinePadding).Width(h.statusWrapWidth())
	if fg != nil {
		s = s.Foreground(fg)
	}
	return s.Render(text)
}

// statusColor is the foreground for the status line: danger when the status
// reports a failure, success otherwise. A single accessor rather than a
// conditional at the render site, so the next status producer cannot render an
// error in the success colour by forgetting the branch.
func (h Model) statusColor() color.Color {
	if h.StatusErr() {
		return ui.ColorDanger
	}
	return ui.ColorSuccess
}

// statusHeight is the rendered height of the status line and the blank line
// above it, or 0 when there is none. tableHeight reserves it: statusLine WRAPS
// at statusWrapWidth, and a session title is operator-supplied text of no
// bounded length, so a renamed-to message can easily cost more than the one row
// an assumed literal would reserve. Measured the same way sessionTableFooterLineCount measures the
// re-login warning, and for the same reason — an unreserved rendered row pushes
// the board past the terminal.
//
// The blank line is the separator renderSessionTable writes above the status,
// the same gap the confirmation prompts leave under the table.
func (h Model) statusHeight() int {
	if h.Status == "" {
		return 0
	}
	return 1 + lipgloss.Height(h.statusLine(h.statusColor(), h.Status))
}

// loginAction returns the login/logout action label for the action bar,
// or "" if auth is not configured.
func (h Model) loginAction() string {
	if h.authMgr == nil {
		return ""
	}
	if h.LoggedIn {
		return "[l]ogout"
	}
	return "[l]ogin"
}

// authReloginLine renders Home's retained-re-login warning, or "" when the
// stored credentials are fine (or absent — never having logged in is not a
// warning, it is the normal signed-out state).
//
// The copy is deliberately split across three short source lines rather than
// left to wrap: statusLine wraps at statusWrapWidth minus the block padding,
// and each line here fits inside the minStatusWrapWidth (60) floor's budget,
// so the reason phrase stays contiguous at every width at or above the floor.
// Below 60 columns statusWrapWidth clamps to h.width and the phrase can wrap
// mid-sentence — accepted, and the reason the width sweep in
// TestHomeReloginWarningSurvivesWrappingAtEveryWidth starts at the floor.
// The BOS-659 proof scenario asserts "Sign in required", the reason
// description, and "[l]ogin" as substrings of the captured frame, and a
// mid-phrase wrap would break it. Keep the lines short if you edit them.
//
// This block renders THREE rows at the floor, so it is deliberately outside
// the <= 2-row bound TestHomeFixedStatusCopyStaysReadableAtFloor puts on
// single-sentence status copy: the plan requires the state, the reason, and
// the local-sessions reassurance all be shown. Its own bound is asserted by
// TestHomeReloginLineStaysWithinThreeRowsAtFloor.
func (h Model) AuthReloginLine() string {
	if !h.NeedsRelogin {
		return ""
	}
	// ReloginReasonDescription always returns one of its two sentences, so
	// this sentence-cases a known-non-empty string. It is the same helper
	// `boss auth-status` renders, which keeps the two surfaces in step.
	reason := auth.ReloginReasonDescription(h.ReloginReason)
	return h.statusLine(ui.ColorWarning, strings.Join([]string{
		"Sign in required: cloud authentication needs [l]ogin.",
		strings.ToUpper(reason[:1]) + reason[1:] + ".",
		"Local sessions are still available.",
	}, "\n"))
}

// guestCloudOfferVisible wraps the package-level ui.CloudGuestOfferVisible with
// Home's own suppression. A retained re-login state reports LoggedIn=false,
// which is exactly what the promo keys on, so without this "[l]ogin to try
// Bossanova Cloud for free" renders directly above "Sign in required: cloud
// authentication needs [l]ogin". The user is not a guest — they are a
// signed-out account holder, and offering them a free trial in that moment is
// wrong. Named distinctly from the package function it calls so the call
// below does not read as recursion.
func (h Model) GuestCloudOfferVisible() bool {
	if h.NeedsRelogin {
		return false
	}
	return ui.CloudGuestOfferVisible(h.Settings, h.currentTime(), h.StartedAt, h.LoggedIn, h.authMgr != nil)
}

// guestCloudOfferOnScreen reports whether View would actually draw the guest
// offer right now: the offer gate passes AND View is on one of the two screens
// that render it, in a state where that screen actually reaches the offer.
// Only the empty state and the session-table footer draw it; the daemon-error
// and loading screens render neither, so an impression counted during them
// would be an impression the user never saw. The empty state appends the offer
// unconditionally, but the footer itself is conditional: the populated board
// reaches renderSessionTableFooter — its only offer site — solely in the arm
// where no rename editor, confirm prompt or upgrade status has taken the
// footer's place.
//
// It exists for App.Update's once-per-session capture, and it mirrors View's
// leading guards rather than being called FROM View — a capture on a render
// path fires on every frame, which is the whole failure this instrumentation
// is written to avoid (BOS-1260). Keep it in step with View above.
func (h Model) GuestCloudOfferOnScreen() bool {
	if h.Err != nil || h.Loading {
		return false
	}
	// Without this arm a modal open at the moment the gate first passes burns
	// the permanent one-per-session latch on a frame the offer was never on,
	// and the impression the user does see is never counted.
	if len(h.Sessions) > 0 && (h.Rename.Active() || h.Confirm.Active || h.Upgrading || h.Restarting) {
		return false
	}
	return h.GuestCloudOfferVisible()
}

// logoutErrorLine renders a failed logout, or "" when the last one succeeded.
// The populated-session footer reserves this block whenever it is visible, so
// its second line cannot be clipped below a full table.
func (h Model) logoutErrorLine() string {
	if h.LogoutError == "" {
		return ""
	}
	message := "Logout: " + h.LogoutError
	if h.LoggedIn && h.LoggedInEmail != "" {
		message += "\nStill signed in as " + h.LoggedInEmail + "."
	}
	return renderError(message, h.statusWrapWidth())
}

// sessionReadFailureName is the display label for one failed organization: its
// name, or its id when the server sent no name. Mirrors the web's own
// readFailureName (services/web/src/pages/Sessions.tsx) so the two surfaces
// label the same failure the same way — an unlabelled failure would tell the
// user the list is short without telling them what is missing from it.
func sessionReadFailureName(f *pb.OrganizationSessionReadFailure) string {
	if name := f.GetOrganizationName(); name != "" {
		return name
	}
	return f.GetOrganizationId()
}

// sessionReadFailureLine renders Home's partial-read notice, or "" when the
// last poll read every organization. A cloud session read fans out across every
// organization the caller belongs to and serves what it could get, so without
// this the missing organization's sessions simply are not there — which reads
// as "that organization has no sessions", in the very view whose point is
// seeing across them.
//
// The server's reason is passed through VERBATIM: the proto guarantees it is
// display-safe and carries no credentials, and re-wording it here would put
// this view's guess in front of the user instead of what actually happened.
// Copy mirrors the web's SessionsPartialFailureNotice.
func (h Model) sessionReadFailureLine() string {
	if len(h.SessionReadFailures) == 0 {
		return ""
	}
	var b strings.Builder
	if len(h.SessionReadFailures) == 1 {
		failure := h.SessionReadFailures[0]
		b.WriteString("Sessions from " + sessionReadFailureName(failure) + " could not be loaded, so this list is incomplete.")
		if reason := failure.GetReason(); reason != "" {
			b.WriteString("\n" + reason)
		}
	} else {
		fmt.Fprintf(&b, "Sessions from %d organizations could not be loaded, so this list is incomplete.", len(h.SessionReadFailures))
		for _, failure := range h.SessionReadFailures {
			b.WriteString("\n" + sessionReadFailureName(failure))
			if reason := failure.GetReason(); reason != "" {
				b.WriteString(": " + reason)
			}
		}
	}
	return h.statusLine(ui.ColorWarning, b.String())
}

func (h Model) renderDaemonError() string {
	remediation := h.daemonRemediation
	if remediation == "" {
		remediation = daemonDownRemediation()
	}
	summary := fmt.Sprintf("Cannot connect to daemon (%v)", h.Err)
	if affordance, ok := ui.HostDialAffordance(h.Err); ok {
		// The dial error names the forwarded socket on THIS machine, which says
		// nothing about the host that stopped answering — so the summary names
		// the destination instead, and the affordance carries the reason.
		summary = "Cannot reach the daemon on " + ui.RemoteHostDestination()
		remediation = affordance
	}
	return renderError(summary, h.statusWrapWidth()) +
		"\n\n" +
		lipgloss.NewStyle().Padding(0, 2).Render(remediation) +
		"\n" +
		ui.ActionBarWidth(h.Width, nil, []string{"[q]uit"})
}

func (h Model) renderLoading() string {
	return lipgloss.NewStyle().Padding(0, 2).Render("Loading sessions…")
}

// renderEmptyState draws the no-sessions screen: either the first-run repo
// prompt or the "no active sessions" guidance, followed by the shared
// discovery / upgrade / cloud trailer.
func (h Model) RenderEmptyState() string {
	var content string
	discovery := ""
	if h.GuestCloudOfferVisible() {
		discovery = ui.CloudDiscoveryLine(h.LoggedIn, h.authMgr != nil)
	}
	if h.RepoCount == 0 {
		content = h.renderNoReposEmptyState()
	} else {
		content = h.renderNoSessionsEmptyState()
	}
	if discovery != "" {
		content += discovery
	}
	if !h.Confirm.Active {
		if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
			separator := "\n"
			if h.Upgrading || h.Restarting {
				separator = "\n\n"
			}
			content += separator + upgradeStatus
		}
	}
	if relogin := h.AuthReloginLine(); relogin != "" && !h.Confirm.Active {
		// Hidden behind a confirm prompt, as the session table hides it (a
		// confirm replaces the whole footer there) and as the upgrade status
		// above is hidden: a modal question should not compete with a
		// three-row advisory.
		//
		// DEFENSIVE today: Home's only confirmPrompt is the logout prompt,
		// raised behind `if h.loggedIn`, and a flagged record reports
		// LoggedIn=false — so the two states cannot currently coexist. The
		// clause keeps the parity rule true if Home ever grows a second
		// confirm.
		content += "\n" + relogin
	}
	if logoutErr := h.logoutErrorLine(); logoutErr != "" {
		content += "\n" + logoutErr
	}
	if readFailures := h.sessionReadFailureLine(); readFailures != "" {
		// Reachable on the empty state too: every organization that WAS read can
		// legitimately have no sessions, and then the only thing on screen is
		// "no active sessions" — which is a lie about the organization that
		// failed unless the notice comes with it.
		content += "\n" + readFailures
	}
	if notice := h.ticketNoticeLine(); notice != "" {
		content += "\n" + notice
	}
	if supervision := h.DaemonSupervisionLine(); supervision != "" && !h.Confirm.Active {
		// Hidden behind a confirm prompt like the re-login warning above.
		content += "\n" + supervision
	}
	if gate := h.cloudGateLine(); gate != "" {
		content += "\n" + gate
	}
	if checkoutStatus := h.cloudCheckoutStatusLine(); checkoutStatus != "" {
		content += "\n" + checkoutStatus
	}
	if h.Confirm.Active {
		content += "\n" + h.Confirm.Footer(h.Width)
		if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
			content += "\n" + upgradeStatus
		}
	}
	return content
}

func (h Model) renderNoReposEmptyState() string {
	// No repos configured - guide the new user straight to adding their
	// first repository (the primary action), rather than sending them to
	// Settings. The [s]ettings key still works as a hidden escape hatch.
	nav := []string{"[enter] add repository"}
	// Suppress the [l]ogout action while the logout confirmation is
	// showing so the empty state doesn't display the control and its
	// confirmation prompt at once (mirrors the session-list view).
	if la := h.loginAction(); la != "" && !h.Confirm.Active {
		nav = append(nav, la)
	}
	if ca := h.cloudAction(); ca != "" {
		nav = append(nav, ca)
	}
	content := lipgloss.NewStyle().Padding(0, 2).Render(
		"Welcome to Bossanova!\n\n" +
			"To get started, you need to add a repository for us to work on together.\n\n" +
			lipgloss.NewStyle().Bold(true).Render("Press Enter to add your first repository"),
	)
	if !h.Upgrading && !h.Restarting {
		content += "\n" + ui.ActionBarWidth(h.Width, nav, []string{"[q]uit"})
	}
	return content
}

func (h Model) renderNoSessionsEmptyState() string {
	// Repos exist but no sessions - show simplified guidance
	left := []string{"[n]ew session"}
	nav := []string{"[s]ettings"}
	// Suppress the [l]ogout action while the logout confirmation is
	// showing so the empty state doesn't display the control and its
	// confirmation prompt at once (mirrors the session-list view).
	if la := h.loginAction(); la != "" && !h.Confirm.Active {
		nav = append(nav, la)
	}
	if ca := h.cloudAction(); ca != "" {
		nav = append(nav, ca)
	}
	content := lipgloss.NewStyle().Padding(0, 2).Render(
		"You have no active sessions.\n\n" +
			lipgloss.NewStyle().Bold(true).Render("Press 'n' to create a new session."),
	)
	if !h.Upgrading && !h.Restarting {
		content += "\n" + ui.ActionBarWidth(h.Width, left, nav, []string{"[q]uit"})
	}
	return content
}

// renderSessionTable draws the populated session list plus whichever footer the
// current mode calls for: the logout confirmation, the in-progress upgrade
// status, or the action bar with its cloud/upgrade lines.
func (h Model) RenderSessionTable() string {
	var b strings.Builder

	if h.Filter.Engaged() && len(h.BoardSessions()) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("No sessions match search."))
		b.WriteString("\n")
	} else if len(h.Sessions) > 0 {
		// homeTableBlockPadding, not a literal 1: tableAvailWidth subtracts this
		// same number from the terminal width to get the fit budget, so a
		// literal here could be widened without the fit following and every
		// narrow board would overhang by the difference (BOS-572).
		b.WriteString(lipgloss.NewStyle().Padding(0, homeTableBlockPadding).Render(h.boardView()))
		b.WriteString("\n")
	}
	if h.Filter.Engaged() {
		b.WriteString(h.Filter.View())
		b.WriteString("\n")
	}
	if h.Status != "" {
		b.WriteString("\n")
		b.WriteString(h.statusLine(h.statusColor(), h.Status))
		b.WriteString("\n")
	}

	// The rename editor takes precedence over every other footer: it is the only
	// one that is capturing keystrokes, so it has to be the one on screen.
	//
	// No separator newline before it, unlike the confirm prompt below: the
	// editor stands in for the action bar, which is written bare because
	// styleActionBar's own top padding is the gap. Adding one here would make
	// the editor two lines taller than the bar it replaces where tableHeight
	// only gives a row back for one.
	if h.AttentionAction.Active() {
		b.WriteString(h.AttentionAction.Footer(h.Width, h.Spinner.View()))
	} else if h.Rename.Active() {
		b.WriteString(h.Rename.Footer(h.Width))
	} else if h.Confirm.Active {
		b.WriteString("\n")
		b.WriteString(h.Confirm.Footer(h.Width))
	} else if h.Upgrading || h.Restarting {
		if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
			b.WriteString("\n")
			b.WriteString(upgradeStatus)
		}
	} else if !h.Upgrading && !h.Restarting {
		b.WriteString(h.RenderSessionTableFooter())
	}
	if h.Confirm.Active {
		if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
			b.WriteString("\n")
			b.WriteString(upgradeStatus)
		}
	}

	return b.String()
}

func (h Model) RenderSessionTableFooter() string {
	var b strings.Builder
	left, nav, quit := h.SessionTableFooterActions()
	b.WriteString(ui.ActionBarWidth(h.Width, left, nav, quit))
	if h.GuestCloudOfferVisible() {
		if discovery := ui.CloudDiscoveryLine(h.LoggedIn, h.authMgr != nil); discovery != "" {
			b.WriteString(discovery)
		}
	}
	if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
		b.WriteString("\n")
		b.WriteString(upgradeStatus)
	}
	if relogin := h.AuthReloginLine(); relogin != "" {
		b.WriteString("\n")
		b.WriteString(relogin)
	}
	if logoutErr := h.logoutErrorLine(); logoutErr != "" {
		b.WriteString("\n")
		b.WriteString(logoutErr)
	}
	if readFailures := h.sessionReadFailureLine(); readFailures != "" {
		b.WriteString("\n")
		b.WriteString(readFailures)
	}
	if notice := h.ticketNoticeLine(); notice != "" {
		b.WriteString("\n")
		b.WriteString(notice)
	}
	if supervision := h.DaemonSupervisionLine(); supervision != "" {
		b.WriteString("\n")
		b.WriteString(supervision)
	}
	if gate := h.cloudGateLine(); gate != "" {
		b.WriteString("\n")
		b.WriteString(gate)
	}
	if checkoutStatus := h.cloudCheckoutStatusLine(); checkoutStatus != "" {
		b.WriteString("\n")
		b.WriteString(checkoutStatus)
	}
	return b.String()
}

func (h Model) SessionTableFooterActions() (left, nav, quit []string) {
	if h.Filter.Active() {
		return h.Filter.ActionBar(), nil, nil
	}
	sel := h.SelectedSession()
	var actions []string
	if !h.isMerging(sel.GetId()) {
		actions = ui.AttentionFooter(sel, false)
	}
	left = make([]string, 0, len(actions)+2)
	left = append(left, "[n]ew session")
	if sel != nil {
		left = append(left, "[enter] select")
	}
	left = append(left, actions...)
	if h.Filter.Applied() {
		left = append(left, "[/] edit search", "[esc] clear")
	} else if len(h.BoardSessions()) > 0 {
		left = append(left, "[/] search")
	}
	nav = []string{"[s]ettings"}
	if la := h.loginAction(); la != "" {
		nav = append(nav, la)
	}
	if ca := h.cloudAction(); ca != "" {
		nav = append(nav, ca)
	}
	return left, nav, []string{"[q]uit"}
}

func (h Model) SessionTableFooterLineCount() int {
	left, nav, quit := h.SessionTableFooterActions()
	lines := ui.ActionBarLineCount(h.Width, left, nav, quit)
	if relogin := h.AuthReloginLine(); relogin != "" {
		// renderSessionTableFooter puts a newline between the action bar and
		// this multi-line warning, so reserve its rendered height and that
		// separator too.
		lines += 1 + lipgloss.Height(relogin)
	}
	if logoutErr := h.logoutErrorLine(); logoutErr != "" {
		// The logout error follows the action bar (and any re-login warning),
		// with a separator that is also a rendered row.
		lines += 1 + strings.Count(logoutErr, "\n") + 1
	}
	if readFailures := h.sessionReadFailureLine(); readFailures != "" {
		// Reserved like the re-login warning above, and for the same reason: the
		// notice wraps at statusWrapWidth and carries a server-supplied reason of
		// no bounded length, so an assumed single row would push the table past
		// the terminal exactly when the user most needs to read both.
		lines += 1 + lipgloss.Height(readFailures)
	}
	if notice := h.ticketNoticeLine(); notice != "" {
		lines += 1 + lipgloss.Height(notice)
	}
	if supervision := h.DaemonSupervisionLine(); supervision != "" {
		// The unsupervised-daemon line follows with its own separator row.
		lines += 1 + lipgloss.Height(supervision)
	}
	return lines
}
