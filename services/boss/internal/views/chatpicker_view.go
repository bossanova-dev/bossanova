package views

// Per-mode renderers for the chat picker (BOS-527). ChatPickerModel.View is
// now just the mode selector; each branch it used to inline lives here as a
// named renderer returning the body string it contributed. wakeFreshFallbackStatus
// rides along as the status-text formatter handleWakeResult reads.

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// isArchiving reports whether the detail view should render "Archiving…". True
// when a manual archive is in flight (m.archiving), when the daemon reports an
// archive actually running for this session (Session.archive_pending, hydrated
// from the DisplayTracker), or while the short-lived optimistic merge latch is
// set right after a TUI-initiated merge. The daemon signal replaced the old
// inferred MERGED + repo-flag heuristic that never cleared for a resurrected
// merged session (BOS-425).
func (m ChatPickerModel) isArchiving() bool {
	return m.archiving || m.optimisticArchiveLatch || m.session.GetArchivePending()
}

// renderErrState renders the chat picker once chat listing has failed. The
// session itself may still have loaded, so the archive affordance stays
// reachable from here.
func (m ChatPickerModel) renderErrState() string {
	body := renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n"
	switch {
	case m.isArchiving():
		body += lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(
			m.spinner.View() + "Archiving session...")
	case m.confirm == confirmArchive:
		body += lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render("Archive this session?") + "\n" +
			styleActionBar.Render("[y/enter] confirm  [n/esc] cancel")
	default:
		// Surface a failed archive (stored in statusMsg) so the user isn't
		// dropped back to the bare error screen with no feedback.
		if m.statusMsg != "" {
			body += lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(m.statusMsg) + "\n"
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
		fmt.Sprintf("Loading chats for %s...", title))
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
// block, usage-limited hint, HTTP endpoints, chat table, rotation history) with
// either a transient confirm/in-flight prompt or the action bar between them.
func (m ChatPickerModel) renderChatList() string {
	var b strings.Builder

	// Surface the session's full finalize/repair error below the header and
	// above the chat list. The header banner already renders one blank line
	// below the worktree-path line, so we add none above here; the trailing
	// "\n\n" yields the single blank line below, before the chat list.
	if block := selectedSessionWarningBlock(m.session, m.chats, m.blockWrapWidth()); block != "" {
		b.WriteString(chatPickerContentBlock(block))
		b.WriteString("\n\n")
	}

	// Name the usage-limited provider(s) (BOS-167) above the chat list so the
	// operator can see at a glance which agent hit its cap without scanning the
	// per-chat STATUS column. Only rendered when at least one chat is limited.
	if line := m.limitedProviderLine(); line != "" {
		b.WriteString(chatPickerContentBlock(styleStatusWarning.Render(line)))
		b.WriteString("\n\n")
	}

	// The session's machine-local HTTP endpoints (BOS-474), directly above the
	// chat table with no blank line between, so the ports read as belonging to
	// this session. Written raw (not via lipgloss) to keep the OSC 8 envelopes
	// intact; httpLineHeight reserves the single line in tableHeight.
	if line := m.httpEndpointLine(); line != "" {
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString(chatPickerContentBlock(m.table.View()))
	b.WriteString("\n")

	if prompt, handled := m.renderConfirm(); handled {
		b.WriteString(prompt)
	} else {
		b.WriteString(m.renderActionBar())
	}

	// Recent automatic-rotation decisions for this session (BOS-176), moved to
	// the very bottom of the view (BOS-432) — below the action bar (or, during a
	// transient confirm/merge/archive dialog, harmlessly below that prompt), with
	// one blank line above. The preceding action-bar/prompt block ends in a
	// styleActionBar Padding(actionBarPadY, 2) bottom-pad blank line (no trailing
	// newline), so a single leading "\n" closes that pad line and leaves exactly
	// one blank line above the block. Empty (skipped) when the session has no
	// rotation history, so sessions that never rotated see nothing.
	if hist := rotationHistoryBlock(m.session, m.blockWrapWidth(), time.Now()); hist != "" {
		b.WriteString("\n")
		b.WriteString(chatPickerContentBlock(hist))
	}

	return b.String()
}

// renderConfirm renders the transient in-flight spinner or y/n prompt that
// stands in for the action bar.
//
// The bool reports whether this chain owned the state, not whether it produced
// output: a confirmDelete with no selected chat is owned here and deliberately
// renders nothing, so an empty string alone cannot be distinguished from "fall
// through to the action bar" — which would put the action bar back on screen
// mid-confirm and change behaviour.
func (m ChatPickerModel) renderConfirm() (string, bool) {
	var b strings.Builder
	if m.merging {
		label := "Merging PR..."
		if n := m.session.GetPrNumber(); n != 0 {
			label = fmt.Sprintf("Merging PR #%d...", n)
		}
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(
			m.spinner.View() + label))
	} else if m.isArchiving() {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(
			m.spinner.View() + "Archiving session..."))
	} else if m.switching {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(
			m.spinner.View() + "Switching account..."))
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

// renderActionBar renders the steady-state footer: the transient status line,
// the switch-account notice, the merged label, and the action bar itself.
func (m ChatPickerModel) renderActionBar() string {
	var b strings.Builder
	if m.statusMsg != "" {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(m.statusMsg))
		b.WriteString("\n")
	}
	// Passive system/notice line for the switch-account outcome (BOS-171):
	// the daemon's "switched to <label> — resumed/started fresh" text, or its
	// human-readable error. Rendered muted, not injected into any input.
	if m.switchNotice != "" {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(styleStatusMuted.Render(m.switchNotice)))
		b.WriteString("\n")
	}
	if m.merged {
		mergedLabel := "✓ merged"
		if n := m.session.GetPrNumber(); n != 0 {
			mergedLabel = fmt.Sprintf("✓ PR #%d merged", n)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(styleStatusMuted.Render(mergedLabel)))
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
	if m.newTabSupported {
		middle = append(middle, "[t]erminal")
	}
	if m.canOpenGitHub() {
		middle = append(middle, "[g]ithub")
	}
	if m.canOpenTracker() {
		middle = append(middle, "[l]inear")
	}
	if m.canMerge() {
		middle = append(middle, "[m]erge")
	}
	if m.sessionID != "" {
		middle = append(middle, "[a]rchive")
	}
	if chat := m.selectedChat(); chat != nil {
		left = []string{"[enter] select", "[d]elete"}
		// Only advertise [w]ake when the highlighted chat is actually
		// stopped — for any other status the keypress is a no-op, so
		// dangling the action in the bar would mislead users.
		if m.daemonStatuses[chat.AgentSessionId] == statusStopped {
			left = append(left, "[w]ake")
		}
		left = append(left, "swit[c]h account")
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
		return "Started fresh: legacy backfill matched multiple provider sessions"
	case "provider_id_discovery_ambiguous":
		return "Started fresh: provider session discovery matched multiple candidates"
	default:
		return "Started fresh"
	}
}
