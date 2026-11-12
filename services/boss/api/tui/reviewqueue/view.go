package reviewqueue

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	queue "github.com/recurser/bossalib/reviewqueue"
)

const tableBlockPadding = 1

func (m Model) orderLabel() string {
	switch m.order {
	case pb.ReviewQueueSort_REVIEW_QUEUE_SORT_PRIORITY:
		return "priority"
	case pb.ReviewQueueSort_REVIEW_QUEUE_SORT_LEAST_CONFIDENT:
		return "least confident"
	case pb.ReviewQueueSort_REVIEW_QUEUE_SORT_SMALLEST:
		return "smallest"
	case pb.ReviewQueueSort_REVIEW_QUEUE_SORT_OLDEST:
		return "oldest"
	default:
		return "most confident"
	}
}
func (m Model) header() string {
	return fmt.Sprintf("merging now %d · in repair %d · need you %d · deep review %d · order: %s", m.counts[pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH], m.counts[pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED], m.counts[pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE], m.counts[pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW], m.orderLabel())
}
func (m Model) footer() string {
	if m.action.Active() {
		return m.action.Footer(m.width, "")
	}
	hints := []string{"[o]rder"}
	if r := m.selected(); r != nil {
		hints = []string{"[enter] details"}
		if webURL(queue.Park(r.GetSession()).GetProofUrl()) {
			hints = append(hints, "[p]roof")
		}
		if _, ok := ui.AttentionBinding(r.GetSession(), "m", false); ok && !r.GetIsAuditSample() && !m.action.MergeUnresolved() {
			hints = append(hints, "[m]erge")
		}
		if r.GetConfidenceLevel() == pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED {
			hints = append(hints, "[a]ccept")
		}
		hints = append(hints, "[o]rder")
	}
	return ui.ActionBarWidth(m.width, hints, []string{"[esc] back"})
}
func (m Model) healthLine() string {
	if len(m.health) == 0 {
		return ""
	}
	w := m.table.Width()
	if w <= 0 {
		w = m.width
	}
	return lipgloss.NewStyle().Foreground(ui.ColorWarning).Render(ansi.Truncate("⚠ "+strings.Join(m.health, " · "), max(1, w), "…"))
}
func (m Model) tableHeight() int {
	headerHeight := lipgloss.Height(ansi.Wrap(m.header(), max(1, m.width-4), "")) + 2
	overhead := headerHeight + 1 + lipgloss.Height(m.footer())
	for _, text := range m.repoErrs {
		overhead += 1 + lipgloss.Height(ui.RenderError(text, m.width))
	}
	if len(m.health) > 0 {
		overhead++
	}
	if m.status != "" {
		overhead += 1 + lipgloss.Height(ui.RenderError(m.status, m.width))
	}
	return ui.ClampedTableHeight(len(m.rows), m.height, overhead)
}
func (m *Model) buildTable() {
	cols := []ui.ResponsiveColumn{
		{Col: ui.CursorColumn, MinWidth: 1},
		{Col: table.Column{Title: "LEVEL", Width: 36}, MinWidth: 10},
		{Col: table.Column{Title: "PR", Width: 8}, MinWidth: 4},
		{Col: table.Column{Title: "TITLE", Width: 42}, MinWidth: 1, Priority: 1},
		{Col: table.Column{Title: "REPO", Width: 22}, MinWidth: 1, Priority: 2},
		{Col: table.Column{Title: "SIZE", Width: 24}, MinWidth: 1, Priority: 3},
		{Col: table.Column{Title: "WAITING", Width: 12}, MinWidth: 1, Priority: 2},
	}
	fitted := ui.FitColumnsIndexed(cols, ui.FitAvailWidth(m.width, tableBlockPadding))
	rows := make([]table.Row, len(m.rows))
	for i, r := range m.rows {
		s := r.GetSession()
		park := queue.Park(s)
		level := ui.ConfidenceCell(r.GetConfidenceLevel(), park.GetAsks(), park.GetLoweredBy())
		since := park.GetSince()
		if r.GetIsAuditSample() {
			level += " sample"
			since = s.GetStateEnteredAt()
		}
		wait := "—"
		if since != nil {
			wait = ui.RelTimeAgo(since.AsTime())
		}
		size := "—"
		if s.GetPrChangedFileCount() > 0 {
			size = fmt.Sprintf("+%d −%d · %d files", s.GetPrAddedLineCount(), s.GetPrDeletedLineCount(), s.GetPrChangedFileCount())
		}
		rows[i] = ui.ProjectRow(fitted, table.Row{"", level, fmt.Sprintf("#%d", s.GetPrNumber()), s.GetTitle(), s.GetRepoDisplayName(), size, wait})
	}
	ui.SetTableContent(&m.table, ui.FittedColumns(fitted), rows)
	m.table.SetWidth(ui.ColumnsWidth(ui.FittedColumns(fitted)))
	m.table.SetHeight(m.tableHeight())
}
func (m Model) View() tea.View {
	if m.accept.Active() {
		return tea.NewView(m.accept.View(m.width, m.height, ""))
	}
	body := lipgloss.NewStyle().Padding(0, 2).Render(ansi.Wrap(m.header(), max(1, m.width-4), "")) + "\n\n"
	if line := m.healthLine(); line != "" {
		body += line + "\n"
	}
	switch {
	case m.err != nil:
		body += ui.RenderError(m.err.Error(), m.width)
	case m.loading:
		body += "  Loading review queue…"
	case len(m.rows) == 0 && len(m.repoErrs) == 0:
		body += "  No PRs are waiting for you."
	case len(m.rows) > 0:
		body += lipgloss.NewStyle().Padding(0, tableBlockPadding).Render(m.table.View())
	}
	for _, err := range m.repoErrs {
		body += "\n" + ui.RenderError(err, m.width)
	}
	if m.status != "" {
		body += "\n" + ui.RenderError(m.status, m.width)
	}
	return tea.NewView(body + "\n" + m.footer())
}
