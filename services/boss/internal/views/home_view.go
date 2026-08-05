// Home's render helpers: the four View branch renderers and the shared status
// line/width helpers. Split out of home.go (BOS-526); the declarations are
// unchanged.

package views

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/recurser/boss/internal/auth"
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
func renderError(msg string, width int) string {
	s := styleError
	// Mirror statusWrapWidth's floor. lipgloss wraps at width minus the
	// horizontal padding, so a width at or below the padding leaves no columns
	// to wrap into and silently renders exactly like Width(0). This guard is
	// therefore behaviour-neutral — nothing can make a block fit a terminal
	// narrower than the padding itself — and exists so the fallback is stated
	// rather than stumbled into, the same reason statusWrapWidth spells its
	// floor out. Ask the style for its padding so it cannot drift from theme.go.
	if width > styleError.GetHorizontalPadding() {
		s = s.Width(width)
	}
	return s.Render(msg)
}

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
func (h HomeModel) statusWrapWidth() int {
	// A terminal too narrow to hold the Padding(0, 2) block leaves lipgloss no
	// columns to wrap into: it computes wrapAt = width - leftPad - rightPad and
	// silently skips wrapping when that is <= 0, so any width here that is not
	// > statusLinePadding*2 renders exactly like Width(0). Say so explicitly
	// rather than returning a width that only looks like it constrains.
	if h.width <= statusLinePadding*2 {
		return 0
	}
	w := 0
	if len(h.sessions) > 0 {
		w = columnsWidth(h.table.Columns())
	}
	if w < minStatusWrapWidth {
		w = minStatusWrapWidth
	}
	if w > h.width {
		w = h.width
	}
	return w
}

// statusLine renders one Home status/warning line: the shared Padding(0, 2)
// block, an optional foreground colour, and the wrap width from
// statusWrapWidth. Every Home *status* line goes through here so the wrap rule
// cannot be missed at one call site (and so the next status line added
// inherits it). Pass a nil colour for an unstyled line.
func (h HomeModel) statusLine(fg color.Color, text string) string {
	s := lipgloss.NewStyle().Padding(0, statusLinePadding).Width(h.statusWrapWidth())
	if fg != nil {
		s = s.Foreground(fg)
	}
	return s.Render(text)
}

// loginAction returns the login/logout action label for the action bar,
// or "" if auth is not configured.
func (h HomeModel) loginAction() string {
	if h.authMgr == nil {
		return ""
	}
	if h.loggedIn {
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
func (h HomeModel) authReloginLine() string {
	if !h.needsRelogin {
		return ""
	}
	// ReloginReasonDescription always returns one of its two sentences, so
	// this sentence-cases a known-non-empty string. It is the same helper
	// `boss auth-status` renders, which keeps the two surfaces in step.
	reason := auth.ReloginReasonDescription(h.reloginReason)
	return h.statusLine(colorWarning, strings.Join([]string{
		"Sign in required: cloud authentication needs [l]ogin.",
		strings.ToUpper(reason[:1]) + reason[1:] + ".",
		"Local sessions are still available.",
	}, "\n"))
}

// guestCloudOfferVisible wraps the package-level cloudGuestOfferVisible with
// Home's own suppression. A retained re-login state reports LoggedIn=false,
// which is exactly what the promo keys on, so without this "[l]ogin to try
// Bossanova Cloud for free" renders directly above "Sign in required: cloud
// authentication needs [l]ogin". The user is not a guest — they are a
// signed-out account holder, and offering them a free trial in that moment is
// wrong. Named distinctly from the package function it calls so the call
// below does not read as recursion.
func (h HomeModel) guestCloudOfferVisible() bool {
	if h.needsRelogin {
		return false
	}
	return cloudGuestOfferVisible(h.settings, h.currentTime(), h.startedAt, h.loggedIn, h.authMgr != nil)
}

// logoutErrorLine renders a failed logout, or "" when the last one succeeded.
// The populated-session footer reserves this block whenever it is visible, so
// its second line cannot be clipped below a full table.
func (h HomeModel) logoutErrorLine() string {
	if h.logoutError == "" {
		return ""
	}
	message := "Logout: " + h.logoutError
	if h.loggedIn && h.loggedInEmail != "" {
		message += "\nStill signed in as " + h.loggedInEmail + "."
	}
	return renderError(message, h.statusWrapWidth())
}

func (h HomeModel) renderDaemonError() string {
	remediation := h.daemonRemediation
	if remediation == "" {
		remediation = daemonDownRemediation()
	}
	return renderError(fmt.Sprintf("Cannot connect to daemon (%v)", h.err), h.statusWrapWidth()) +
		"\n\n" +
		lipgloss.NewStyle().Padding(0, 2).Render(remediation) +
		"\n" +
		styleActionBar.Render("Press q to quit.")
}

func (h HomeModel) renderLoading() string {
	return lipgloss.NewStyle().Padding(0, 2).Render("Loading sessions...")
}

// renderEmptyState draws the no-sessions screen: either the first-run repo
// prompt or the "no active sessions" guidance, followed by the shared
// discovery / upgrade / cloud trailer.
func (h HomeModel) renderEmptyState() string {
	var content string
	discovery := ""
	if h.guestCloudOfferVisible() {
		discovery = cloudDiscoveryLine(h.loggedIn, h.authMgr != nil)
	}
	if h.repoCount == 0 {
		content = h.renderNoReposEmptyState()
	} else {
		content = h.renderNoSessionsEmptyState()
	}
	if discovery != "" {
		content += discovery
	}
	if !h.confirm.active {
		if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
			separator := "\n"
			if h.upgrading || h.restarting {
				separator = "\n\n"
			}
			content += separator + upgradeStatus
		}
	}
	if relogin := h.authReloginLine(); relogin != "" && !h.confirm.active {
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
	if gate := h.cloudGateLine(); gate != "" {
		content += "\n" + gate
	}
	if checkoutStatus := h.cloudCheckoutStatusLine(); checkoutStatus != "" {
		content += "\n" + checkoutStatus
	}
	if h.confirm.active {
		content += "\n" + h.confirm.footer(h.width)
		if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
			content += "\n" + upgradeStatus
		}
	}
	return content
}

func (h HomeModel) renderNoReposEmptyState() string {
	// No repos configured - guide the new user straight to adding their
	// first repository (the primary action), rather than sending them to
	// Settings. The [s]ettings key still works as a hidden escape hatch.
	nav := []string{"[enter] add repository"}
	// Suppress the [l]ogout action while the logout confirmation is
	// showing so the empty state doesn't display the control and its
	// confirmation prompt at once (mirrors the session-list view).
	if la := h.loginAction(); la != "" && !h.confirm.active {
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
	if !h.upgrading && !h.restarting {
		content += "\n" + actionBarWidth(h.width, nav, []string{"[q]uit"})
	}
	return content
}

func (h HomeModel) renderNoSessionsEmptyState() string {
	// Repos exist but no sessions - show simplified guidance
	left := []string{"[n]ew session"}
	nav := []string{"[s]ettings"}
	// Suppress the [l]ogout action while the logout confirmation is
	// showing so the empty state doesn't display the control and its
	// confirmation prompt at once (mirrors the session-list view).
	if la := h.loginAction(); la != "" && !h.confirm.active {
		nav = append(nav, la)
	}
	if ca := h.cloudAction(); ca != "" {
		nav = append(nav, ca)
	}
	content := lipgloss.NewStyle().Padding(0, 2).Render(
		"You have no active sessions.\n\n" +
			lipgloss.NewStyle().Bold(true).Render("Press 'n' to create a new session."),
	)
	if !h.upgrading && !h.restarting {
		content += "\n" + actionBarWidth(h.width, left, nav, []string{"[q]uit"})
	}
	return content
}

// renderSessionTable draws the populated session list plus whichever footer the
// current mode calls for: the logout confirmation, the in-progress upgrade
// status, or the action bar with its cloud/upgrade lines.
func (h HomeModel) renderSessionTable() string {
	var b strings.Builder

	if len(h.sessions) > 0 {
		// homeTableBlockPadding, not a literal 1: tableAvailWidth subtracts this
		// same number from the terminal width to get the fit budget, so a
		// literal here could be widened without the fit following and every
		// narrow board would overhang by the difference (BOS-572).
		b.WriteString(lipgloss.NewStyle().Padding(0, homeTableBlockPadding).Render(h.table.View()))
		b.WriteString("\n")
	}
	if h.status != "" {
		b.WriteString(h.statusLine(colorSuccess, h.status))
		b.WriteString("\n")
	}

	if h.confirm.active {
		b.WriteString("\n")
		b.WriteString(h.confirm.footer(h.width))
	} else if h.upgrading || h.restarting {
		if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
			b.WriteString("\n")
			b.WriteString(upgradeStatus)
		}
	} else if !h.upgrading && !h.restarting {
		b.WriteString(h.renderSessionTableFooter())
	}
	if h.confirm.active {
		if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
			b.WriteString("\n")
			b.WriteString(upgradeStatus)
		}
	}

	return b.String()
}

func (h HomeModel) renderSessionTableFooter() string {
	var b strings.Builder
	left, nav, quit := h.sessionTableFooterActions()
	b.WriteString(actionBarWidth(h.width, left, nav, quit))
	if h.guestCloudOfferVisible() {
		if discovery := cloudDiscoveryLine(h.loggedIn, h.authMgr != nil); discovery != "" {
			b.WriteString(discovery)
		}
	}
	if upgradeStatus := h.upgradeStatusView(); upgradeStatus != "" {
		b.WriteString("\n")
		b.WriteString(upgradeStatus)
	}
	if relogin := h.authReloginLine(); relogin != "" {
		b.WriteString("\n")
		b.WriteString(relogin)
	}
	if logoutErr := h.logoutErrorLine(); logoutErr != "" {
		b.WriteString("\n")
		b.WriteString(logoutErr)
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

func (h HomeModel) sessionTableFooterActions() (left, nav, quit []string) {
	left = []string{"[n]ew session", "[enter] select"}
	nav = []string{"[s]ettings"}
	if la := h.loginAction(); la != "" {
		nav = append(nav, la)
	}
	if ca := h.cloudAction(); ca != "" {
		nav = append(nav, ca)
	}
	return left, nav, []string{"[q]uit"}
}

func (h HomeModel) sessionTableFooterLineCount() int {
	left, nav, quit := h.sessionTableFooterActions()
	lines := actionBarLineCount(h.width, left, nav, quit)
	if relogin := h.authReloginLine(); relogin != "" {
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
	return lines
}
