package attach

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/rpc"
)

func (m Model) fetchAttachState() tea.Cmd {
	return func() tea.Msg {
		sess, err := m.client.GetSession(m.ctx, m.sessionID, rpc.SessionReadOptions{})
		if err != nil {
			return attachErrMsg{err: err}
		}
		trustNote := repoTrustNote{}
		if repos, err := m.client.ListRepos(m.ctx); err == nil {
			for _, repo := range repos {
				if repo.GetId() == sess.GetRepoId() && !repo.GetIsTrusted() {
					trustNote = repoTrustNote{name: repo.GetDisplayName(), id: repo.GetId()}
					break
				}
			}
		}
		// Best-effort chat list — used only to gate first-attach prefill.
		// A failure here should not block attach; treat as "unknown count"
		// (i.e. assume non-empty so we don't accidentally prefill on a
		// resumed session).
		chats, _ := m.client.ListChats(m.ctx, m.sessionID)
		return attachReadyMsg{session: sess, chats: chats, trustNote: trustNote}
	}
}

// repoTrustNote keeps the repo decision while the authoritative chat agent is
// still being resolved, so its label always agrees with the launching view.
type repoTrustNote struct {
	name string
	id   string
}

func (n repoTrustNote) text(agentName string) string {
	if n.id == "" {
		return ""
	}
	return fmt.Sprintf(
		`%s is untrusted, so %s runs without its project configuration and may stop on a folder-trust prompt. To trust it: boss repo update %s --trusted, or "isTrusted": true in .boss-project.json.`,
		n.name, agentDisplayName(agentName), n.id)
}

// renderNote keeps attach status notes on one row within the content padding.
func (m Model) renderNote(note string) string {
	if avail := m.width - 2*ui.StatusLinePadding; m.width > 0 && avail > 0 && ansi.StringWidth(note) > avail {
		note = ansi.Truncate(note, avail, "…")
	}
	return lipgloss.NewStyle().Padding(0, 2).Render(ui.StyleStatusInfo.Render(note))
}
