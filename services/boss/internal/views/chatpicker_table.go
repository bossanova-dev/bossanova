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
		t := chat.Title
		if t == "" {
			t = "New chat"
		}
		titles[i] = t
		createds[i] = RelativeTime(chat.CreatedAt.AsTime())
		actives[i] = RelativeTime(m.chatLastActive(chat))
		agents[i] = m.chatAgentName(chat)
	}

	titleWidth := maxColWidth("CHAT", titles, 60)
	agentWidth := maxColWidth("AGENT", agents, 12)
	createdWidth := maxColWidth("CREATED", createds, 12)
	activeWidth := maxColWidth("ACTIVE", actives, 12)
	statusWidth := 12 // enough for spinner + "working"

	cols := []table.Column{
		cursorColumn,
		{Title: "CHAT", Width: titleWidth + tableColumnSep},
	}
	if showAgentColumn {
		cols = append(cols, table.Column{Title: "AGENT", Width: agentWidth + tableColumnSep})
	}
	cols = append(cols,
		table.Column{Title: "CREATED", Width: createdWidth + tableColumnSep},
		table.Column{Title: "ACTIVE", Width: activeWidth + tableColumnSep},
		table.Column{Title: "STATUS", Width: statusWidth + tableColumnSep},
	)

	cursor := m.table.Cursor()
	rows := make([]table.Row, len(m.chats))
	for i, chat := range m.chats {
		daemon := m.daemonStatuses[chat.AgentSessionId]
		statusStr := renderClaudeStatus(daemon, m.spinner)
		// A chat with start_error set never came up — the agent_chats
		// row was created but StartTmuxChat hit a failure (e.g.
		// SendPlan timeout from claude's broken --print regression
		// pre-#350). The row is preserved so the operator can see the
		// attempt; surface that explicitly here instead of letting the
		// row look like a fresh "stopped" chat.
		if chat.GetStartError() != "" {
			statusStr = renderChatStartFailed()
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
		row := table.Row{indicator, titles[i]}
		if showAgentColumn {
			row = append(row, agents[i])
		}
		row = append(row, createdStr, activeStr, statusStr)
		rows[i] = row
	}
	// Columns first: since BOS-532 the prose blocks tableHeight reserves wrap
	// at blockWrapWidth(), which reads the table's columns, so SetHeight must
	// see the new column set to reserve the right number of rows.
	m.table.SetColumns(cols)
	m.table.SetRows(rows)
	m.table.SetWidth(columnsWidth(cols))
	m.table.SetHeight(m.tableHeight())
	m.table.SetCursor(cursor)
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
	return "⚠ " + strings.Join(providers, ", ") + " usage-limited"
}

// tableHeight returns the height to pass to table.SetHeight.
func (m ChatPickerModel) tableHeight() int {
	// gap + actionbar padding + actionbar, plus the session warning block
	// (below the header, above the chat list). Reserving its lines shrinks the
	// chat table rather than letting the block push the table off-screen.
	overhead := bannerOverhead + 1 + actionBarPadY + 1 + m.warningBlockHeight() + m.limitedLineHeight() + m.httpLineHeight() + m.rotationHistoryHeight()
	return clampedTableHeight(len(m.chats), m.height, overhead)
}

// httpEndpointLine returns the muted "HTTP  :3000 · :5173" line for the
// session's verified machine-local HTTP endpoints (BOS-474), or "" when the
// session has none. Each ":port" is independently clickable; see
// renderSessionEndpoints for the OSC 8 / URL-validation rules.
//
// Styled with raw ANSI rather than lipgloss: Render on a string containing an
// OSC 8 envelope mangles the escape sequence (the same constraint the PR and
// tracker link renderers document). Only the *styling* needs the raw path
// though, so the inset is still built from chatPickerBlockPadding rather than
// hand-synced to it — chatPickerContentBlock's rule reaches this line too, just spelled
// as spaces.
//
// The result is capped at the terminal width. Nothing upstream bounds how many
// listeners a worktree exposes (sessionports emits one endpoint per listening
// port), so an unbounded line would soft-wrap onto a second terminal row while
// httpLineHeight reserved only one — pushing the last chat row or the action bar
// off screen, the exact regression the reservation exists to prevent. ansi.Truncate
// is OSC 8-aware, so a cut line still closes every hyperlink envelope it opened.
func (m ChatPickerModel) httpEndpointLine() string {
	links := renderSessionEndpoints(m.session)
	if links == "" {
		return ""
	}
	line := strings.Repeat(" ", chatPickerBlockPadding) + mutedTextOpen + "HTTP" + mutedTextClose + "  " + links
	if m.width > 0 && ansi.StringWidth(line) > m.width {
		line = ansi.Truncate(line, m.width, "…")
	}
	return line
}

// httpLineHeight returns the vertical lines the HTTP endpoint line occupies
// above the chat list — exactly one, with no blank line below it, so the links
// read as attached to the table — or 0 when the session has no endpoints.
// Reserving it in tableHeight keeps the last chat row on screen. httpEndpointLine
// truncates to the terminal width, so "exactly one" holds for any endpoint count.
func (m ChatPickerModel) httpLineHeight() int {
	if m.httpEndpointLine() == "" {
		return 0
	}
	return 1
}

// chatPickerBlockPadding is the horizontal inset shared by the chat picker's
// tables and the wrapped prose blocks that align to them. blockWrapWidth exists
// so the prose lines up with the table's content, and that alignment only holds
// while both are inset by the same amount, so they must move together. Apply it
// through chatPickerContentBlock rather than by hand.
//
// It is not the view's only inset: the status, prompt and notice lines sit at
// the wider Padding(0, 2) that Home uses, deliberately, because they align to
// nothing. Do not route those through chatPickerContentBlock.
//
// Unlike Home — where the padding is part of the same style that carries the
// wrap Width, so lipgloss counts it inside that width — the chat picker passes
// the wrap width *into* the block renderers (status.go applies
// style.Width(width) with a padding-less style) and only then insets the
// finished block. The padding is therefore *outside* the wrap width and adds
// to it on screen, which is why blockWrapWidth clamps against the room left
// after it rather than against the raw terminal width.
const chatPickerBlockPadding = 1

// chatPickerContentBlock insets a rendered block by chatPickerBlockPadding. It
// is the single place that padding is applied, so a fifth block inherits the
// rule instead of needing a follow-up commit — which is what the constant's
// first revision cost, when it shipped with the limited-provider hint still on
// a bare literal while the doc claimed everything in the column moved together.
//
// Name-prefixed like the constant: package views is shared by every view, and
// home, trash and repo_list render the same table-in-a-block shape at the wider
// statusLinePadding. An unprefixed helper would be reachable from those files
// and would silently apply the chat picker's inset.
//
// httpEndpointLine is the one caller that cannot use this: it emits OSC 8
// hyperlink escapes, which lipgloss mangles, so it inlines its own inset. See
// its doc comment.
func chatPickerContentBlock(s string) string {
	return lipgloss.NewStyle().Padding(0, chatPickerBlockPadding).Render(s)
}

// blockWrapWidth returns the width the chat picker's wrapped prose blocks —
// the session-warning block and the rotation-history block — wrap at. It
// mirrors HomeModel.statusWrapWidth (BOS-507/BOS-532): track the rendered
// table so those blocks line up with the content on screen instead of running
// to the terminal edge.
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
	// chatPickerContentBlock insets each block by chatPickerBlockPadding on top
	// of the width returned here, so a terminal with no columns left after that
	// inset cannot show a wrapped block at all. Report 0 — lipgloss treats
	// Width(0) as unconstrained — rather than a width that would only look like
	// it constrains. This also covers an unknown (0) or negative terminal width,
	// i.e. no tea.WindowSizeMsg yet.
	//
	// Same rule as Home's `width <= statusLinePadding*2` guard, just with this
	// view's smaller constant: Home bails at width <= 4, this at width <= 2. It
	// is not a claim that a 3-column terminal renders correctly — at avail == 1
	// lipgloss cannot break a word with no break point, so it emits the longest
	// unbreakable segment anyway.
	avail := m.width - chatPickerBlockPadding*2
	if avail <= 0 {
		return 0
	}
	w := columnsWidth(m.table.Columns())
	if w < minStatusWrapWidth {
		w = minStatusWrapWidth
	}
	// Clamp to the room the padding leaves, not to m.width: clamping to m.width
	// would render a block of m.width+2 columns and overhang the terminal by
	// two columns on a narrow window. The result is still never wider than
	// m.width, so the "clamped to the terminal width" rule holds.
	if w > avail {
		w = avail
	}
	return w
}

// rotationHistoryHeight returns the vertical lines the rotation-history block
// (BOS-176) occupies at the very bottom of the chat-picker View (BOS-432) — just
// the rendered block, since the single blank line above it is the preceding
// action-bar/prompt's own bottom-pad line (View writes only a "\n" to close it,
// adding no extra row) — or 0 when the session has no rotation history.
// Reserving these keeps the block from pushing a chat row off-screen.
func (m ChatPickerModel) rotationHistoryHeight() int {
	block := rotationHistoryBlock(m.session, m.blockWrapWidth(), time.Now())
	if block == "" {
		return 0
	}
	return lipgloss.Height(block)
}

// warningBlockHeight returns the number of vertical lines the session warning
// block occupies above the chat list (0 when the session has no
// finalize/repair hints), including the single blank line below it that View
// renders. There is no blank line above: the header banner already renders
// one below the worktree-path line.
func (m ChatPickerModel) warningBlockHeight() int {
	block := selectedSessionWarningBlock(m.session, m.chats, m.blockWrapWidth())
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
