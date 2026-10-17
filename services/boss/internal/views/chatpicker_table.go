package views

// Chat-table construction and the chat picker's layout arithmetic — the row
// builder plus every *Height reservation the View's blocks claim. Split out of
// chatpicker.go (BOS-527); the declarations are unchanged.

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// buildTableRows rebuilds the table rows from m.chats.
func (m *ChatPickerModel) buildTableRows() {
	if len(m.chats) == 0 {
		m.table.SetRows(nil)
		return
	}

	titles := make([]string, len(m.chats))
	createds := make([]string, len(m.chats))
	actives := make([]string, len(m.chats))
	agents := make([]string, len(m.chats))
	showAgentColumn := len(m.agents) > 1
	for i, chat := range m.chats {
		titles[i] = chatDisplayTitle(chat)
		createds[i] = RelativeTime(chat.CreatedAt.AsTime())
		actives[i] = RelativeTime(m.chatLastActive(chat))
		agents[i] = m.chatAgentName(chat)
	}

	titleWidth := maxColWidth("CHAT", titles, 60)
	agentWidth := maxColWidth("AGENT", agents, 12)
	createdWidth := maxColWidth("CREATED", createds, 12)
	activeWidth := maxColWidth("ACTIVE", actives, 12)
	statusWidth := 12 // preserve the default width for unphased chats
	for _, chat := range m.chats {
		label := m.detailChatStatus(chat)
		if label == "" {
			label = m.statusDetail(chat)
		}
		if label == "" {
			continue
		}
		w := lipgloss.Width(m.spinner.View() + label)
		if m.session.GetAttention() == nil && m.daemonStatuses[chat.GetAgentSessionId()] == statusWorking {
			w = min(w, 2+displaystatus.MaxPhaseRunes)
		}
		statusWidth = max(statusWidth, w)
	}

	rcols := []responsiveColumn{
		{col: cursorColumn, priority: 0, minWidth: 1},
		{col: table.Column{Title: "CHAT", Width: titleWidth + tableColumnSep}, priority: 0, minWidth: 16},
	}
	if showAgentColumn {
		rcols = append(rcols, responsiveColumn{col: table.Column{Title: "AGENT", Width: agentWidth + tableColumnSep}, priority: 3, minWidth: 4})
	}
	rcols = append(rcols,
		responsiveColumn{col: table.Column{Title: "CREATED", Width: createdWidth + tableColumnSep}, priority: 4, minWidth: 4},
		responsiveColumn{col: table.Column{Title: "ACTIVE", Width: activeWidth + tableColumnSep}, priority: 2, minWidth: 4},
		responsiveColumn{col: table.Column{Title: "STATUS", Width: statusWidth + tableColumnSep}, priority: 1, minWidth: 8},
	)
	fitted := fitColumnsIndexed(rcols, m.chatPickerTableAvailWidth())
	cols := fittedColumns(fitted)

	cursor := m.table.Cursor()
	rows := make([]table.Row, len(m.chats))
	for i, chat := range m.chats {
		daemon := m.daemonStatuses[chat.AgentSessionId]
		statusStr := renderClaudeStatus(daemon, m.spinner)
		wait := m.chatWait(chat.GetAgentSessionId())
		if label := m.detailChatStatus(chat); m.session.GetAttention() != nil && label != "" {
			statusStr = styleStatusMuted.Render(ansi.Truncate(label, statusWidth, "…"))
		} else if wait == waitDone {
			statusStr = styleStatusMuted.Render(chatDoneLabel)
		} else if wait == waitStandby {
			statusStr = renderStandbyStatus()
		} else if label := m.statusDetail(chat); label != "" {
			style := styleStatusSuccess
			if daemon == statusWaiting {
				style = styleStatusInfo
			}
			statusStr = style.Render(ansi.Truncate(m.spinner.View()+label, statusWidth, "…"))
		}
		// A chat with start_error set never came up — the agent_chats
		// row was created but StartTmuxChat hit a failure (e.g.
		// SendPlan timeout from claude's broken --print regression
		// pre-#350). The row is preserved so the operator can see the
		// attempt; surface that explicitly here instead of letting the
		// row look like a fresh "stopped" chat.
		if chat.GetStartError() != "" {
			statusStr = renderChatStartFailed()
		} else if chat.GetReclaimedAt() != nil {
			statusStr = renderChatReplaced()
		}
		if chat.AgentSessionId != "" && chat.AgentSessionId == m.deletingAgentSessionID {
			statusStr = renderRowPendingStatus(m.spinner, "deleting")
		}
		createdStr := styleSubtle.Render(createds[i])
		activeStr := styleSubtle.Render(actives[i])
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		full := table.Row{indicator, titles[i], agents[i], createdStr, activeStr, statusStr}
		if !showAgentColumn {
			full = append(full[:2:2], full[3:]...)
		}
		rows[i] = projectRow(fitted, full)
	}
	// Columns first: since BOS-532 the prose blocks tableHeight reserves wrap
	// at blockWrapWidth(), which reads the table's columns, so SetHeight must
	// see the new column set to reserve the right number of rows.
	setTableContent(&m.table, cols, rows)
	m.table.SetWidth(columnsWidth(cols))
	m.table.SetHeight(m.tableHeight())
	m.table.SetCursor(cursor)
}

// chatDoneLabel is the STATUS of a chat whose every wait left is the session's
// own PR merging or closing (waitDone): muted and without a spinner, because
// the chat has nothing left to do until then.
const chatDoneLabel = "done"

// renderChatReplaced is the STATUS badge for a chat automation deliberately
// replaced (repair reclaiming an idle agent-owned chat). It ran and ended on
// purpose, so it reads as a neutral terminal state, not the danger-styled
// "× failed" reserved for a chat whose agent never came up.
func renderChatReplaced() string {
	return styleSubtle.Render("↺ replaced")
}

// statusDetail is the label that replaces a chat's bare STATUS word: the
// skill-reported phase of a working chat, or the armed trigger of a chat
// waiting on the PR's progress (BOS-1517). "" keeps the plain
// renderClaudeStatus badge. Standby and done waits have no detail:
// buildTableRows renders their own muted badges.
//
// A wait on a PR other than the session's own names that PR, because nothing
// else on the screen says which PR the chat is parked on.
func (m ChatPickerModel) statusDetail(chat *pb.ClaudeChat) string {
	id := chat.GetAgentSessionId()
	switch m.daemonStatuses[id] {
	case statusWorking:
		return m.daemonPhases[id]
	case statusWaiting:
		if m.chatWait(id) != waitProgress {
			return ""
		}
		cb := m.statusCallback(chat)
		label := callbackWaitLabel(cb.GetTrigger())
		if label != "" && !m.isOwnPRReason(callbackReason(cb)) {
			label += " " + m.prRef(cb)
		}
		return label
	}
	return ""
}

// chatPickerTableAvailWidth returns the rendered columns the chat picker can
// draw inside chatPickerContentBlock's horizontal inset. The resize handler
// continues to set the table viewport width independently; this is only the
// content budget used while rebuilding its fitted columns.
func (m ChatPickerModel) chatPickerTableAvailWidth() int {
	return fitAvailWidth(m.width, chatPickerBlockPadding)
}

// limitedProviderLine returns a concise warning line naming the provider(s)
// whose chats are currently usage-limited, e.g. "⚠ claude usage-limited". It
// returns "" when no chat in the session is limited, so callers can skip
// rendering entirely (no empty line, no layout shift). Provider names are
// de-duplicated in first-seen order, so a session with two limited claude
// chats reads "⚠ claude usage-limited" rather than naming claude twice.
// Answers the BOS-167 acceptance criterion that session detail names which
// provider/agent (Claude/Codex) is limited.
func (m ChatPickerModel) limitedProviderLine() string {
	var providers []string
	seen := make(map[string]bool)
	for _, chat := range m.chats {
		if m.daemonStatuses[chat.AgentSessionId] != statusLimited {
			continue
		}
		name := m.chatAgentName(chat)
		if name == "" || name == "-" || seen[name] {
			continue
		}
		seen[name] = true
		providers = append(providers, name)
	}
	if len(providers) == 0 {
		return ""
	}
	return m.fitProseLine("⚠ " + strings.Join(providers, ", ") + " usage-limited")
}

// chatPickerProseAvailWidth returns the rendered columns a prose line may
// occupy inside chatPickerProseBlock's inset — the prose counterpart to
// chatPickerTableAvailWidth above. 0 means unbounded: no tea.WindowSizeMsg yet,
// or a terminal the inset alone consumes.
func (m ChatPickerModel) chatPickerProseAvailWidth() int {
	return fitAvailWidth(m.width, chatPickerProsePadding)
}

// fitProseLine truncates a single-line prose hint to that budget. It is the
// one-row half of the reservation contract tableHeight documents: a hint
// claims a fixed row count while carrying unbounded text, and httpEndpointLine
// already guards its own line the same way for the same reason.
//
// Truncates against the terminal rather than blockWrapWidth: these are hints,
// not blocks, and clamping them to the table's content width would shorten them
// on wide terminals where they fit perfectly well.
func (m ChatPickerModel) fitProseLine(s string) string {
	avail := m.chatPickerProseAvailWidth()
	if avail <= 0 || ansi.StringWidth(s) <= avail {
		return s
	}
	return ansi.Truncate(s, avail, "…")
}

// chatListOverhead reserves all chrome outside the attention details and table.
//
// It owns the reservation sum, and with it the invariant every *Height function
// below depends on: each block must actually occupy the rows it claims here. A
// line wider than the terminal soft-wraps onto a row nobody reserved and pushes
// the last chat row or the action bar off screen, so every block is width-bounded
// at its source — the wrapped blocks by blockWrapWidth, the HTTP line by its own
// ansi.Truncate, the single-line hints by fitProseLine. Those sites cite this
// one rather than restating the reasoning.
func (m ChatPickerModel) chatListOverhead() int {
	// gap + actionbar padding + actionbar, plus the session warning block
	// (below the header, above the chat list). Reserving its lines shrinks the
	// chat table rather than letting the block push the table off-screen.
	left, middle, back := m.chatListActionGroups()
	footerLines := actionBarLineCount(m.width, middle, back)
	if len(left) > 0 {
		footerLines = actionBarLineCount(m.width, left, middle, back)
	}
	if m.attentionAction.active() {
		footerLines = m.attentionAction.lineCount(m.width)
	}
	return bannerOverhead + 1 + actionBarPadY + footerLines + m.warningBlockHeight() + m.limitedLineHeight() + m.statusFooterHeight() + m.httpLineHeight()
}

func (m ChatPickerModel) tableHeight() int {
	return clampedTableHeight(len(m.chats), m.height, m.chatListOverhead()+m.attentionDetailsHeight())
}

// httpEndpointLine returns the muted "HTTP  :3000 :5173" line for the
// session's verified machine-local HTTP endpoints (BOS-474), or "" when the
// session has none. Each ":port" is independently clickable; see
// renderSessionEndpoints for the OSC 8 / URL-validation rules.
//
// It sits one blank line above the chat table, indented to the table's CHAT
// column (chatPickerFirstColumnInset), so the ports read as part of the table
// block rather than as another prose line.
//
// Styled with raw ANSI rather than lipgloss: Render on a string containing an
// OSC 8 envelope mangles the escape sequence (the same constraint the PR and
// tracker link renderers document), so the inset is written as spaces.
//
// The result is capped at the terminal width. Nothing upstream bounds how many
// listeners a worktree exposes (sessionports emits one endpoint per listening
// port), so an unbounded line would soft-wrap onto a terminal row
// httpLineHeight did not reserve, pushing the last chat row or the action bar
// off screen. ansi.Truncate is OSC 8-aware, so a cut line still closes every
// hyperlink envelope it opened.
func (m ChatPickerModel) httpEndpointLine() string {
	if !m.showOpenPorts {
		return ""
	}
	links := renderSessionEndpoints(m.session)
	if links == "" {
		return ""
	}
	line := strings.Repeat(" ", chatPickerFirstColumnInset) + mutedTextOpen + "HTTP" + mutedTextClose + " " + links
	if m.width > 0 && ansi.StringWidth(line) > m.width {
		line = ansi.Truncate(line, m.width, "…")
	}
	return line
}

// chatPickerFirstColumnInset is the screen column the chat table's first data
// column (CHAT) starts at: the table block's inset, then the cursor column,
// each cell carrying bossTableStyles' one-column left pad.
var chatPickerFirstColumnInset = chatPickerBlockPadding + 1 + cursorColumn.Width + 1

// httpLineHeight returns the rows the HTTP endpoint line occupies above the
// chat table (the line plus the blank line below it), or 0 when the session
// has no endpoints. httpEndpointLine truncates to the terminal width, so the
// line is one row for any endpoint count.
func (m ChatPickerModel) httpLineHeight() int {
	if m.httpEndpointLine() == "" {
		return 0
	}
	return 2
}

// chatPickerBlockPadding is the horizontal inset on the chat picker's *tables*
// — the agent-select, switch-account and chat tables — applied through
// chatPickerContentBlock rather than by hand.
//
// It is deliberately one column narrower than the prose inset beside it
// (chatPickerProsePadding), and the two no longer move together (BOS-718).
// bossTableStyles gives every table cell its own Padding(0, 0, 0, 1), so a
// table drawn inside this 1-column frame lands its ❯ caret on display column 2
// — the column styleActionBar, renderBanner and styleToast all sit at. Prose
// carries no such cell pad, so routing prose through this constant drew it at
// column 1, one short of every other piece of chrome in the view. The table's
// cell pad supplies the missing column; the prose has to be given it. Bumping
// this constant instead would move the caret to column 3 and break an alignment
// that is already correct — see TestChatPicker_ResizedTableLeavesRoomForItsInset
// and the chatPickerBlockPadding assertions in chatpicker_switch_test.go and
// app_fallthrough_test.go, which pin the table side un-edited.
//
// Unlike Home — where the padding is part of the same style that carries the
// wrap Width, so lipgloss counts it inside that width — the chat picker passes
// the wrap width *into* the block renderers (status.go applies
// style.Width(width) with a padding-less style) and only then insets the
// finished block. The padding is therefore *outside* the wrap width and adds
// to it on screen, which is why blockWrapWidth clamps against the room left
// after the (wider) prose inset rather than against the raw terminal width.
const chatPickerBlockPadding = 1

// chatPickerProsePadding is the horizontal inset on every prose/status line the
// chat picker draws around its tables: the finalize/repair warning block, the
// usage-limited hint and the rotation-history block. Apply it through
// chatPickerProseBlock. The HTTP endpoint line is not prose: it sits in the
// table's CHAT column (chatPickerFirstColumnInset).
//
// Defined as statusLinePadding rather than a bare 2 because column 2 is one
// global chrome rule, not a chat-picker fact: Home's status lines
// (HomeModel.statusLine), styleActionBar, styleTitle, styleError and styleToast
// all sit there. Two constants holding 2 for the same reason would eventually
// disagree. Home already runs this exact split — homeTableBlockPadding = 1 for
// its table, statusLinePadding = 2 for its prose — and BOS-718 brings the chat
// picker into line with it.
const chatPickerProsePadding = statusLinePadding

// chatPickerContentBlock insets a rendered table by chatPickerBlockPadding. It
// is the single place that padding is applied, so a fourth table inherits the
// rule instead of needing a follow-up commit — which is what the constant's
// first revision cost, when it shipped a block on a bare literal while the doc
// claimed everything in the column moved together. (That straggler was the
// limited-provider hint, which BOS-718 has since moved to the prose side.)
//
// Name-prefixed like the constant: package views is shared by every view, and
// home, trash and repo_list render the same table-in-a-block shape at the wider
// statusLinePadding. An unprefixed helper would be reachable from those files
// and would silently apply the chat picker's inset.
//
// Its only callers are the three tables. Prose goes through chatPickerProseBlock
// instead (BOS-718): the tables' cell padding supplies the column that prose
// does not have, so sharing one helper drew the prose one column short.
func chatPickerContentBlock(s string) string {
	return lipgloss.NewStyle().Padding(0, chatPickerBlockPadding).Render(s)
}

// chatPickerProseBlock insets a rendered prose block by chatPickerProsePadding,
// the chat picker's counterpart to Home's Padding(0, statusLinePadding) status
// style. The view's prose sites go through it (the warning block, the
// usage-limited hint and the rotation-history block), so it is the single
// place their inset is applied and a new one inherits the rule rather than
// needing a follow-up commit.
//
// It is not the view's only route to column 2, and does not claim to be: the
// confirm prompts, the transient statusMsg line, the switch-account notice, the
// merged label and the two overlay headers in chatpicker_view.go each build
// their own lipgloss.NewStyle().Padding(0, 2) because they carry a foreground
// colour or wrap another style, and none of them is width-clamped by
// blockWrapWidth. They land in the same column by holding the same number, not
// by sharing this helper. Routing them through it is a fine follow-up; it is
// not a prerequisite for the blocks below, which is why BOS-718 left them.
//
// Name-prefixed for the same reason chatPickerContentBlock is: package views is
// shared by every view, and an unprefixed proseBlock would be reachable from
// home, trash and repo_list, which own their own status styling.
func chatPickerProseBlock(s string) string {
	return lipgloss.NewStyle().Padding(0, chatPickerProsePadding).Render(s)
}

// blockWrapWidth returns the width the chat picker's wrapped prose blocks —
// the session-warning block and the needed-from-you answer — wrap at. It
// mirrors HomeModel.statusWrapWidth (BOS-507/BOS-532): track the rendered
// table so those blocks span roughly the table's content instead of running to
// the terminal edge.
//
// "Track", not "align to": since BOS-718 the blocks start one column right of
// where the table's frame starts (prose inset 2 vs table inset 1) while wrapping
// at the table's content width, so only their LEFT edges line up — with the
// caret and the rest of the chrome, which is the point. Their right edge now
// falls within a column of the table's rather than on it. That is invisible
// unless text reaches the right margin, and the left edge is the alignment the
// view is judged on.
//
// Derived from the columns rather than m.table.Width() because the table's
// width is set from two places with different meanings (buildTableRows uses
// columnsWidth, the resize handler uses the room the inset leaves in the
// terminal), so Width() is whichever ran last — which is exactly the defect
// BOS-532 fixes: before the first tea.WindowSizeMsg these blocks wrapped at the
// content width and after it at the terminal width, visibly reflowing on the
// user's first resize. SetWidth sizes only the table's viewport, a different
// concern from how wide the prose below it should wrap, so neither SetWidth
// feeds this width.
//
// Unlike Home there is no len(chats) > 0 gate: the chat picker always draws its
// table, so the columns are always on screen. Before the first chatsListedMsg
// there are no columns at all and columnsWidth returns 0, which the floor picks
// up.
func (m ChatPickerModel) blockWrapWidth() int {
	// The prose budget, not the table's: since BOS-718 these blocks sit at
	// chatPickerProsePadding while the tables stay at the narrower
	// chatPickerBlockPadding, and clamping against the narrower one would let a
	// block render two columns wider than the terminal — the BOS-530/BOS-532
	// overhang defect tableHeight's doc describes. Same rule as Home's
	// `width <= statusLinePadding*2` guard, with the same constant: both bail at
	// width <= 4, reporting 0 because lipgloss reads Width(0) as unconstrained,
	// which is honest about a terminal too narrow to constrain rather than a
	// bound that only looks like one.
	//
	// Bailing is not a claim that a 5-column terminal renders correctly: at
	// avail == 1 lipgloss has no break point to use and emits the longest
	// unbreakable segment regardless. What the clamp bounds is the width this
	// function reports, never the rendered block.
	avail := m.chatPickerProseAvailWidth()
	if avail <= 0 {
		return 0
	}
	w := columnsWidth(m.table.Columns())
	if w < minStatusWrapWidth {
		w = minStatusWrapWidth
	}
	// Clamp to the room the padding leaves, not to m.width: clamping to m.width
	// would render a block of m.width+chatPickerProsePadding*2 columns and
	// overhang the terminal by that much on a narrow window. The result is still
	// never wider than m.width, so the "clamped to the terminal width" rule holds.
	if w > avail {
		w = avail
	}
	return w
}

// warningBlockHeight returns the number of vertical lines the session warning
// block occupies above the chat list (0 when the session has no
// finalize/repair hints), including the single blank line below it that View
// renders. There is no blank line above: the header banner already renders
// one below the worktree-path line.
func (m ChatPickerModel) warningBlockHeight() int {
	block := m.legacyWarningBlock()
	if block == "" {
		return 0
	}
	return lipgloss.Height(block) + 1
}

// limitedLineHeight returns the vertical lines the usage-limited provider hint
// (BOS-167) occupies above the chat list — the hint line plus the single blank
// line View renders below it — or 0 when no chat is limited. Reserving these in
// tableHeight keeps the hint from pushing a chat row off-screen.
func (m ChatPickerModel) limitedLineHeight() int {
	if m.limitedProviderLine() == "" {
		return 0
	}
	return 2
}

// chatLastActive returns the most recent activity time for a chat.
// Prefers daemon-reported output time, then created_at.
func (m ChatPickerModel) chatLastActive(chat *pb.ClaudeChat) time.Time {
	if t, ok := m.daemonLastOutput[chat.AgentSessionId]; ok && !t.IsZero() {
		return t
	}
	return chat.CreatedAt.AsTime()
}

// RelativeTime formats a time as a human-readable relative string.
func RelativeTime(t time.Time) string {
	d := time.Since(t)

	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%dh ago", h)
	case d < 14*24*time.Hour:
		days := int(d.Hours() / 24)
		return fmt.Sprintf("%dd ago", days)
	default:
		weeks := int(d.Hours() / 24 / 7)
		return fmt.Sprintf("%dw ago", weeks)
	}
}

func (m ChatPickerModel) legacyWarningBlock() string {
	if m.session.GetAttention() != nil {
		return ""
	}
	return selectedSessionWarningBlock(m.session, m.chats, m.blockWrapWidth())
}

func (m ChatPickerModel) detailChatStatus(chat *pb.ClaudeChat) string {
	a := m.session.GetAttention()
	if a == nil {
		return ""
	}
	id := chat.GetAgentSessionId()
	parked := a.GetKind() == pb.AttentionKind_ATTENTION_KIND_REVIEW && (id == a.GetTargetChatId() || (a.GetTargetChatId() == "" && m.daemonStatuses[id] == statusWaiting))
	switch wait := m.chatWait(id); {
	case wait == waitStandby:
		return "done · standby: wakes if CI turns red"
	case parked:
		return "parked for you"
	case wait == waitDone:
		return chatDoneLabel
	}
	return ""
}
