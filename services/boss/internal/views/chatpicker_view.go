package views

// Per-mode renderers for the chat picker (BOS-527). ChatPickerModel.View is
// now just the mode selector; each branch it used to inline lives here as a
// named renderer returning the body string it contributed. wakeFreshFallbackStatus
// rides along as the status-text formatter handleWakeResult reads.

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// isArchiving includes the local archive RPC and a daemon archive waiting for
// chats to idle or already running. Only the local RPC blocks action keys.
func (m ChatPickerModel) isArchiving() bool {
	return m.archiving || m.archiveAwaited()
}

func (m ChatPickerModel) archiveAwaited() bool {
	return m.optimisticArchiveLatch || m.session.GetArchivePending()
}

func (m ChatPickerModel) archiveStatusLine() string {
	return lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(
		m.fitProseLine(m.spinner.View() + "Archiving session once every chat is idle…"))
}

// renderErrState renders the chat picker once chat listing has failed. The
// session itself may still have loaded, so the archive affordance stays
// reachable from here.
func (m ChatPickerModel) renderErrState() string {
	// A --host tunnel that drops mid-session fails every chat RPC with the
	// forwarded socket's dial error, which is neither actionable nor true of
	// the remote machine — rpcErrorMessage shows the reconnecting affordance
	// instead (BOS-724).
	body := renderError(rpcErrorMessage(m.err), m.width) + "\n"
	switch {
	case m.archiving:
		body += lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(
			m.spinner.View() + "Archiving session…")
	case m.confirm == confirmArchive:
		body += "\n" + lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render("Archive this session?") + "\n" +
			styleActionBar.Render("[y/enter] confirm  [n/esc] cancel")
	default:
		// Each status line takes a blank line above it, as in the chat list's
		// footer. A failed archive (stored in statusMsg) shows here so the user
		// isn't dropped back to the bare error screen with no feedback.
		var lines []string
		if m.archiveAwaited() {
			lines = append(lines, m.archiveStatusLine())
		}
		if m.statusMsg != "" {
			lines = append(lines, lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(m.statusMsg))
		}
		for _, line := range lines {
			body += "\n" + line + "\n"
		}
		if m.sessionID != "" {
			body += actionBarWidth(m.width, []string{"[a]rchive"}, []string{"[esc] back"})
		} else {
			body += styleActionBar.Render("[esc] back")
		}
	}
	return body
}

// renderLoading renders the placeholder shown until the first chat list lands.
func (m ChatPickerModel) renderLoading() string {
	title := m.sessionID
	if m.session != nil {
		title = m.session.Title
	}
	return lipgloss.NewStyle().Padding(0, 2).Render(
		fmt.Sprintf("Loading chats for %s…", title))
}

// renderAgentSelect renders the agent-select overlay.
func (m ChatPickerModel) renderAgentSelect() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render(
		"Pick an agent for this new chat."))
	b.WriteString("\n\n")
	b.WriteString(chatPickerContentBlock(m.agentTable.View()))
	b.WriteString("\n")
	b.WriteString(actionBarWidth(m.width, []string{"[enter] select"}, []string{"[esc] cancel"}))
	return b.String()
}

// renderSwitchAccounts renders the switch-account overlay.
func (m ChatPickerModel) renderSwitchAccounts() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render(
		"Switch this chat to which account?"))
	b.WriteString("\n\n")
	b.WriteString(chatPickerContentBlock(m.switchAccountTable.View()))
	b.WriteString("\n")
	if m.statusMsg != "" {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(m.statusMsg))
		b.WriteString("\n")
	}
	b.WriteString(actionBarWidth(m.width, []string{"[enter] select"}, []string{"[esc] cancel"}))
	return b.String()
}

// renderChatList renders the picker's default mode: the shared chrome (warning
// block, needed-from-you answer, usage-limited hint, HTTP endpoints, chat
// table, session timeline) with either a transient confirm/in-flight prompt or
// the action bar between the table and the timeline.
func (m ChatPickerModel) renderChatList() string {
	var b strings.Builder

	// Surface the session's full finalize/repair error below the header and
	// above the chat list. The header banner already renders one blank line
	// below the worktree-path line, so we add none above here; the trailing
	// "\n\n" yields the single blank line below, before the chat list.
	if block := m.legacyWarningBlock(); block != "" {
		b.WriteString(chatPickerProseBlock(block))
		b.WriteString("\n\n")
	}

	if block := m.neededBlock(); block != "" {
		b.WriteString(chatPickerProseBlock(block))
		b.WriteString("\n\n")
	}

	// Name the usage-limited provider(s) (BOS-167) above the chat list so the
	// operator can see at a glance which agent hit its cap without scanning the
	// per-chat STATUS column. Only rendered when at least one chat is limited.
	if line := m.limitedProviderLine(); line != "" {
		b.WriteString(chatPickerProseBlock(styleStatusWarning.Render(line)))
		b.WriteString("\n\n")
	}

	// The session's machine-local HTTP endpoints (BOS-474), one blank line
	// above the chat table. Written raw (not via lipgloss) to keep the OSC 8
	// envelopes intact; httpLineHeight reserves both rows in tableHeight.
	if line := m.httpEndpointLine(); line != "" {
		b.WriteString(line)
		b.WriteString("\n\n")
	}

	b.WriteString(chatPickerContentBlock(m.table.View()))
	b.WriteString("\n")
	b.WriteString(m.renderFooter())

	// The session timeline, account rotations included, sits at the very bottom
	// of the view where the rotation history used to (BOS-432): below the
	// action bar, or during a transient dialog harmlessly below that prompt.
	// The preceding block ends in a styleActionBar Padding(actionBarPadY, 2)
	// bottom-pad blank line with no trailing newline, so a single leading "\n"
	// closes that pad line and leaves exactly one blank line above the block.
	if block := m.timelineBlock(); block != "" {
		b.WriteString("\n")
		b.WriteString(chatPickerProseBlock(block))
	}

	return b.String()
}

// renderFooter renders what sits between the chat table and the timeline: the
// rename prompt, a confirmation or in-flight line, or the steady status lines
// and action bar. chatListOverhead reserves its height as drawn.
func (m ChatPickerModel) renderFooter() string {
	if prompt, handled := m.renderRenamePrompt(); handled {
		return prompt
	}
	if prompt, handled := m.renderConfirm(); handled {
		return prompt
	}
	return m.renderActionBar()
}

// renderRenamePrompt renders the inline [r]ename editor that stands in for the
// action bar while the prompt is open (BOS-836): a blank line, the prompt line
// with any "cannot be empty" complaint appended, then the key hints. The bool
// reports ownership, on the same contract as renderConfirm.
func (m ChatPickerModel) renderRenamePrompt() (string, bool) {
	if !m.renaming {
		return "", false
	}
	line := m.renameInput.View()
	if m.renameErr != "" {
		line += lipgloss.NewStyle().Foreground(colorDanger).Render("  " + m.renameErr)
	}
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
	b.WriteString("\n")
	b.WriteString(styleActionBar.Render("[enter] save  [esc] cancel"))
	return b.String(), true
}

// renderConfirm replaces the action bar for a local in-flight RPC or y/n
// prompt. Daemon-side archives leave keys live and fall through to the bar.
//
// The bool reports whether this chain owned the state, not whether it produced
// output: a confirmDelete with no selected chat is owned here and deliberately
// renders nothing, so an empty string alone cannot be distinguished from "fall
// through to the action bar" — which would put the action bar back on screen
// mid-confirm and change behaviour.
func (m ChatPickerModel) renderConfirm() (string, bool) {
	if m.attentionAction.active() {
		return m.attentionAction.footer(m.width, m.spinner.View()), true
	}
	var b strings.Builder
	if m.merging {
		label := "Merging PR…"
		if n := m.session.GetPrNumber(); n != 0 {
			label = fmt.Sprintf("Merging PR #%d…", n)
		}
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(
			m.spinner.View() + label))
	} else if m.archiving {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(
			m.spinner.View() + "Archiving session…"))
	} else if m.switching {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(
			m.spinner.View() + "Switching account…"))
	} else if m.confirm == confirmSwitch {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(
			"This chat is mid-turn. Switch account and interrupt it?"))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else if m.confirm == confirmDelete {
		chat := m.selectedChat()
		if chat != nil {
			chatTitle := chat.Title
			if chatTitle == "" {
				chatTitle = "New chat"
			}
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
				fmt.Sprintf("Delete %q?", chatTitle)))
			b.WriteString("\n")
			b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
		}
	} else if m.confirm == confirmMerge {
		prompt := "Merge PR?"
		if n := m.session.GetPrNumber(); n != 0 {
			prompt = fmt.Sprintf("Merge PR #%d?", n)
		}
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(prompt))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else if m.confirm == confirmArchive {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render("Archive this session?"))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		return "", false
	}
	return b.String(), true
}

// statusFooterLines are the one-line statuses the steady-state footer stacks
// above the action bar, in display order: the daemon archive wait, the transient
// failure line, an unanswered merge's waiting line, the last attention result,
// the switch-account notice and the merged label.
func (m ChatPickerModel) statusFooterLines() []string {
	var lines []string
	if m.archiveAwaited() && !m.archiving {
		lines = append(lines, m.archiveStatusLine())
	}
	if m.statusMsg != "" {
		lines = append(lines, lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(m.statusMsg))
	}
	for _, note := range []string{m.attentionAction.waiting, m.statusNote} {
		if note != "" {
			lines = append(lines, lipgloss.NewStyle().Padding(0, 2).Render(styleStatusMuted.Render(note)))
		}
	}
	// Passive system/notice line for the switch-account outcome (BOS-171):
	// the daemon's "switched to <label> — resumed/started fresh" text, or its
	// human-readable error. Rendered muted, not injected into any input.
	if m.switchNotice != "" {
		lines = append(lines, lipgloss.NewStyle().Padding(0, 2).Render(styleStatusMuted.Render(m.switchNotice)))
	}
	if m.merged {
		mergedLabel := "✓ merged"
		if n := m.session.GetPrNumber(); n != 0 {
			mergedLabel = fmt.Sprintf("✓ PR #%d merged", n)
		}
		lines = append(lines, lipgloss.NewStyle().Padding(0, 2).Render(styleStatusMuted.Render(mergedLabel)))
	}
	return lines
}

// renderActionBar renders the steady-state footer: the status lines, each with
// a blank line above it (the first one separates it from the chat table, which
// ends its last row with a newline), then the action bar, whose own top padding
// is the blank line above it.
func (m ChatPickerModel) renderActionBar() string {
	var b strings.Builder
	for _, line := range m.statusFooterLines() {
		b.WriteString("\n")
		b.WriteString(line)
		b.WriteString("\n")
	}
	left, middle, back := m.chatListActionGroups()
	if len(left) > 0 {
		b.WriteString(actionBarWidth(m.width, left, middle, back))
	} else {
		b.WriteString(actionBarWidth(m.width, middle, back))
	}
	return b.String()
}

func (m ChatPickerModel) chatListActionGroups() (left, middle, back []string) {
	middle = []string{"[n]ew chat", "[s]ettings"}
	if !m.attentionAction.mergeUnresolved() {
		middle = append(middle, attentionFooter(m.session, true)...)
	}
	if m.newTabSupported {
		middle = append(middle, "[t]erminal")
	}
	if _, ok := attentionBinding(m.session, "g", true); !ok && m.canOpenGitHub() {
		middle = append(middle, "[g]ithub")
	}
	if m.canOpenTracker() {
		middle = append(middle, "[l]inear")
	}
	if _, ok := attentionBinding(m.session, "m", true); !ok && m.canMerge() {
		middle = append(middle, "[m]erge")
	}
	if m.sessionID != "" {
		middle = append(middle, "[a]rchive")
	}
	if chat := m.selectedChat(); chat != nil {
		left = []string{"[enter] select", "[d]elete"}
		if m.session.GetAttention() != nil {
			left[0] = "[enter] open chat"
		}
		// [r]ename and swit[c]h account are deliberately unadvertised
		// (BOS-1063): both keys still work, but the bar stays short enough to
		// read at a glance. Their availability predicates live with the key
		// handlers — see canRename and keySwitchAccount.
		//
		// Only advertise [w]ake when the highlighted chat is actually
		// stopped — for any other status the keypress is a no-op, so
		// dangling the action in the bar would mislead users.
		if m.daemonStatuses[chat.AgentSessionId] == statusStopped {
			left = append(left, "[w]ake")
		}
	}
	return left, middle, []string{"[esc] back"}
}

func wakeFreshFallbackStatus(reason string) string {
	switch reason {
	case "transcript_missing":
		return "Started fresh: transcript missing"
	case "provider_id_missing":
		return "Started fresh: provider session was not discovered yet"
	case "provider_id_discovery_timeout":
		return "Started fresh: provider session is still being discovered"
	case "legacy_provider_id_discovery_ambiguous":
		return "Started fresh: the earlier conversation could not be identified"
	case "provider_id_discovery_ambiguous":
		return "Started fresh: provider session discovery matched multiple candidates"
	default:
		return "Started fresh"
	}
}
