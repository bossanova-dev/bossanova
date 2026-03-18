package views

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Trash View ---

// trashListMsg carries archived sessions for the trash view.
type trashListMsg struct {
	sessions []*pb.Session
	err      error
}

// sessionRestoredMsg carries the result of restoring an archived session.
type sessionRestoredMsg struct {
	id  string
	err error
}

// sessionDeletedMsg carries the result of permanently deleting a session.
type sessionDeletedMsg struct {
	id  string
	err error
}

// TrashModel displays archived sessions with restore/delete functionality.
type TrashModel struct {
	client  client.BossClient
	ctx     context.Context
	spinner spinner.Model

	sessions []*pb.Session
	cursor   int
	err      error
	cancel   bool
	loading  bool

	// Delete confirmation / in-progress states
	confirming bool
	deleting   bool
	restoring  bool

	// Layout
	width int
}

// NewTrashModel creates a TrashModel.
func NewTrashModel(c client.BossClient, ctx context.Context) TrashModel {
	return TrashModel{
		client:  c,
		ctx:     ctx,
		spinner: newStatusSpinner(),
		loading: true,
	}
}

func (m TrashModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.fetchArchived())
}

func (m TrashModel) fetchArchived() tea.Cmd {
	return func() tea.Msg {
		sessions, err := m.client.ListSessions(m.ctx, &pb.ListSessionsRequest{IncludeArchived: true})
		if err != nil {
			return trashListMsg{err: err}
		}
		// Filter to only archived sessions.
		var archived []*pb.Session
		for _, s := range sessions {
			if s.ArchivedAt != nil {
				archived = append(archived, s)
			}
		}
		return trashListMsg{sessions: archived}
	}
}

func (m TrashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case trashListMsg:
		m.loading = false
		m.sessions = msg.sessions
		m.err = msg.err
		return m, nil

	case sessionRestoredMsg:
		m.restoring = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		// Remove from list and adjust cursor.
		for i, s := range m.sessions {
			if s.Id == msg.id {
				m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
				break
			}
		}
		if m.cursor >= len(m.sessions) && len(m.sessions) > 0 {
			m.cursor = len(m.sessions) - 1
		}
		return m, nil

	case sessionDeletedMsg:
		m.confirming = false
		m.deleting = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		// Remove from list and adjust cursor.
		for i, s := range m.sessions {
			if s.Id == msg.id {
				m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
				break
			}
		}
		if m.cursor >= len(m.sessions) && len(m.sessions) > 0 {
			m.cursor = len(m.sessions) - 1
		}
		return m, nil

	case tea.KeyMsg:
		if m.confirming {
			return m.updateDeleteConfirm(msg)
		}

		switch msg.String() {
		case "esc", "q":
			m.cancel = true
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
			}
		case "r":
			if len(m.sessions) > 0 {
				m.restoring = true
				sess := m.sessions[m.cursor]
				return m, func() tea.Msg {
					_, err := m.client.ResurrectSession(m.ctx, sess.Id)
					return sessionRestoredMsg{id: sess.Id, err: err}
				}
			}
		case "d":
			if len(m.sessions) > 0 {
				m.confirming = true
			}
		}
	}

	return m, nil
}

func (m TrashModel) updateDeleteConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		m.confirming = false
		m.deleting = true
		sess := m.sessions[m.cursor]
		return m, func() tea.Msg {
			err := m.client.RemoveSession(m.ctx, sess.Id)
			return sessionDeletedMsg{id: sess.Id, err: err}
		}
	case "n", "esc":
		m.confirming = false
	}
	return m, nil
}

// Cancelled returns true if the user exited the trash view.
func (m TrashModel) Cancelled() bool { return m.cancel }

func (m TrashModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.loading {
		return tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render("Loading archived sessions..."))
	}

	var b strings.Builder
	b.WriteString(styleTitle.Render("Trash"))
	b.WriteString("\n\n")

	if len(m.sessions) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Trash is empty."))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[esc] back"))
		return tea.NewView(b.String())
	}

	// Compute column widths from data.
	maxRepo := len("REPO")
	maxBranch := len("BRANCH")
	for _, sess := range m.sessions {
		if rl := len(sess.RepoDisplayName); rl > maxRepo {
			maxRepo = rl
		}
		if bl := len(strings.TrimPrefix(sess.BranchName, "boss/")); bl > maxBranch {
			maxBranch = bl
		}
	}
	if maxRepo > 20 {
		maxRepo = 20
	}
	if maxBranch > 60 {
		maxBranch = 60
	}

	// Table header.
	header := fmt.Sprintf("  %-*s"+colSep+"%-*s"+colSep+"%-*s"+colSep+"%s",
		shortIDLen, "ID", maxRepo, "REPO", maxBranch, "BRANCH", "ARCHIVED")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Faint(true).Render(header))
	b.WriteString("\n")

	for i, sess := range m.sessions {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}

		shortID := sess.Id
		if len(shortID) > shortIDLen {
			shortID = shortID[:shortIDLen]
		}

		repoName := truncate(sess.RepoDisplayName, maxRepo)
		branchDisplay := strings.TrimPrefix(sess.BranchName, "boss/")
		branch := truncate(branchDisplay, maxBranch)

		archived := "-"
		if sess.ArchivedAt != nil {
			archived = relativeTime(sess.ArchivedAt.AsTime())
		}

		row := fmt.Sprintf("%s%-*s"+colSep+"%-*s"+colSep+"%-*s"+colSep+"%s",
			cursor, shortIDLen, shortID,
			maxRepo, repoName,
			maxBranch, branch,
			archived)
		if i == m.cursor {
			row = styleSelected.Render(row)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(row))
		b.WriteString("\n")
	}

	if m.deleting {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorRed).Render(
			m.spinner.View() + "Deleting..."))
	} else if m.restoring {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorRed).Render(
			m.spinner.View() + "Restoring..."))
	} else if m.confirming {
		b.WriteString("\n")
		sess := m.sessions[m.cursor]
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorRed).Render(
			fmt.Sprintf("Permanently delete %q?", sess.Title)))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		b.WriteString(styleActionBar.Render("[d]elete  [r]estore  [esc] back"))
	}

	return tea.NewView(b.String())
}
