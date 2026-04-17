package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Autopilot TUI View ---

// autopilotListMsg carries the result of listing autopilot workflows.
type autopilotListMsg struct {
	workflows []*pb.AutopilotWorkflow
	err       error
}

// autopilotActionMsg carries the result of a pause/resume/cancel action.
type autopilotActionMsg struct {
	err error
}

// AutopilotModel displays autopilot workflows with pause/resume/cancel controls.
type AutopilotModel struct {
	client  client.BossClient
	ctx     context.Context
	spinner spinner.Model

	workflows []*pb.AutopilotWorkflow
	table     table.Model
	err       error
	actionErr error // separate from err so polling doesn't clear action failures
	cancel    bool
	loading   bool

	// Confirmation state
	confirming bool

	width  int
	height int
}

// NewAutopilotModel creates an AutopilotModel.
func NewAutopilotModel(c client.BossClient, ctx context.Context) AutopilotModel {
	return AutopilotModel{
		client:  c,
		ctx:     ctx,
		spinner: newStatusSpinner(),
		loading: true,
		table:   newBossTable(nil, nil, 0),
	}
}

func (m AutopilotModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.fetchWorkflows(), tickCmd())
}

func (m AutopilotModel) fetchWorkflows() tea.Cmd {
	return func() tea.Msg {
		workflows, err := m.client.ListAutopilotWorkflows(m.ctx, &pb.ListAutopilotWorkflowsRequest{
			IncludeAll: true,
		})
		if err != nil {
			return autopilotListMsg{err: err}
		}
		return autopilotListMsg{workflows: workflows}
	}
}

func (m *AutopilotModel) buildTable() {
	if len(m.workflows) == 0 {
		m.table.SetRows(nil)
		return
	}

	ids := make([]string, len(m.workflows))
	statuses := make([]string, len(m.workflows))
	steps := make([]string, len(m.workflows))
	legs := make([]string, len(m.workflows))
	plans := make([]string, len(m.workflows))
	durations := make([]string, len(m.workflows))

	for i, w := range m.workflows {
		id := w.Id
		if len(id) > 8 {
			id = id[:8]
		}
		ids[i] = id
		statuses[i] = WorkflowStatusLabel(w.Status)
		steps[i] = WorkflowStepLabel(w.CurrentStep)

		legs[i] = FormatFlightLeg(w)

		plan := w.PlanPath
		runes := []rune(plan)
		if len(runes) > 30 {
			plan = "..." + string(runes[len(runes)-27:])
		}
		plans[i] = plan

		if w.StartedAt != nil {
			durations[i] = workflowElapsed(w).String()
		} else {
			durations[i] = "-"
		}
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "ID", Width: maxColWidth("ID", ids, 8) + tableColumnSep},
		{Title: "STATUS", Width: maxColWidth("STATUS", statuses, 12) + tableColumnSep},
		{Title: "STEP", Width: maxColWidth("STEP", steps, 12) + tableColumnSep},
		{Title: "LEG", Width: maxColWidth("LEG", legs, 16) + tableColumnSep},
		{Title: "PLAN", Width: maxColWidth("PLAN", plans, 30) + tableColumnSep},
		{Title: "ELAPSED", Width: maxColWidth("ELAPSED", durations, 12) + tableColumnSep},
	}

	cursor := m.table.Cursor()
	rows := make([]table.Row, len(m.workflows))
	for i := range m.workflows {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, ids[i], statuses[i], steps[i], legs[i], plans[i], durations[i]}
	}

	m.table.SetColumns(cols)
	m.table.SetRows(rows)
	m.table.SetWidth(columnsWidth(cols))
	m.table.SetHeight(m.tableHeight())
	m.table.SetCursor(cursor)
}

// selectedWorkflow returns the workflow at the current cursor, or nil.
func (m AutopilotModel) selectedWorkflow() *pb.AutopilotWorkflow {
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.workflows) {
		return nil
	}
	return m.workflows[idx]
}

func (m AutopilotModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.table.SetHeight(m.tableHeight())
		m.table.SetWidth(msg.Width)
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if len(m.workflows) > 0 {
			m.buildTable()
		}
		return m, cmd

	case autopilotListMsg:
		m.loading = false
		m.workflows = msg.workflows
		m.err = msg.err
		m.buildTable()
		if m.table.Cursor() >= len(m.workflows) && len(m.workflows) > 0 {
			m.table.SetCursor(len(m.workflows) - 1)
		}
		return m, nil

	case autopilotActionMsg:
		if msg.err != nil {
			m.actionErr = msg.err
			return m, nil
		}
		m.actionErr = nil
		// Refresh after action.
		return m, m.fetchWorkflows()

	case tickMsg:
		return m, tea.Batch(m.fetchWorkflows(), tickCmd())

	case tea.KeyMsg:
		if m.confirming {
			return m.updateConfirm(msg)
		}

		switch msg.String() {
		case "esc":
			m.cancel = true
			return m, nil
		case "p":
			if w := m.selectedWorkflow(); w != nil && (w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING || w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_PENDING) {
				return m, m.pauseWorkflow(w.Id)
			}
			return m, nil
		case "r":
			if w := m.selectedWorkflow(); w != nil && w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED {
				return m, m.resumeWorkflow(w.Id)
			}
			return m, nil
		case "c":
			if w := m.selectedWorkflow(); w != nil {
				active := w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING ||
					w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED ||
					w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_PENDING
				if active {
					m.confirming = true
				}
			}
			return m, nil
		}

		// Forward navigation keys.
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		updateCursorColumn(&m.table)
		return m, cmd
	}

	return m, nil
}

func (m AutopilotModel) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		m.confirming = false
		if w := m.selectedWorkflow(); w != nil {
			return m, m.cancelWorkflow(w.Id)
		}
	case "n", "esc":
		m.confirming = false
	}
	return m, nil
}

func (m AutopilotModel) pauseWorkflow(id string) tea.Cmd {
	return func() tea.Msg {
		_, err := m.client.PauseAutopilot(m.ctx, id)
		return autopilotActionMsg{err: err}
	}
}

func (m AutopilotModel) resumeWorkflow(id string) tea.Cmd {
	return func() tea.Msg {
		_, err := m.client.ResumeAutopilot(m.ctx, id)
		return autopilotActionMsg{err: err}
	}
}

func (m AutopilotModel) cancelWorkflow(id string) tea.Cmd {
	return func() tea.Msg {
		_, err := m.client.CancelAutopilot(m.ctx, id)
		return autopilotActionMsg{err: err}
	}
}

// Cancelled returns true if the user exited the autopilot view.
func (m AutopilotModel) Cancelled() bool { return m.cancel }

// tableHeight returns the height to pass to table.SetHeight.
func (m AutopilotModel) tableHeight() int {
	return clampedTableHeight(len(m.workflows), m.height, bannerOverhead+1+actionBarPadY+1) // gap + actionbar padding + actionbar
}

func (m AutopilotModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.actionErr != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.actionErr), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.loading {
		return tea.NewView(lipgloss.NewStyle().Padding(0, 2).Render("Loading autopilot workflows..."))
	}

	var b strings.Builder

	if len(m.workflows) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("No workflows."))
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
	b.WriteString("\n")

	if m.confirming {
		if w := m.selectedWorkflow(); w != nil {
			id := w.Id
			if len(id) > 8 {
				id = id[:8]
			}
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
				fmt.Sprintf("Cancel workflow %s?", id)))
			b.WriteString("\n")
			b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
		}
	} else {
		// Context-sensitive action bar based on selected workflow status.
		w := m.selectedWorkflow()
		var itemActions []string
		if w != nil {
			switch w.Status {
			case pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING, pb.WorkflowStatus_WORKFLOW_STATUS_PENDING:
				itemActions = []string{"[p]ause", "[c]ancel"}
			case pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED:
				itemActions = []string{"[r]esume", "[c]ancel"}
			case pb.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED,
				pb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED,
				pb.WorkflowStatus_WORKFLOW_STATUS_FAILED,
				pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED:
				// No actions for terminal/unspecified states.
			}
		}
		if len(itemActions) > 0 {
			b.WriteString(actionBar(itemActions, []string{"[esc] back"}))
		} else {
			b.WriteString(actionBar([]string{"[esc] back"}))
		}
	}

	return tea.NewView(b.String())
}

// --- Shared Label Helpers ---

// WorkflowStatusLabel returns a human-readable label for a workflow status.
func WorkflowStatusLabel(s pb.WorkflowStatus) string {
	switch s {
	case pb.WorkflowStatus_WORKFLOW_STATUS_PENDING:
		return "pending"
	case pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING:
		return "running"
	case pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED:
		return "paused"
	case pb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED:
		return "completed"
	case pb.WorkflowStatus_WORKFLOW_STATUS_FAILED:
		return "failed"
	case pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED:
		return "cancelled"
	default:
		return "unknown"
	}
}

// WorkflowStepLabel returns a human-readable label for a workflow step.
func WorkflowStepLabel(s pb.WorkflowStep) string {
	switch s {
	case pb.WorkflowStep_WORKFLOW_STEP_PLAN:
		return "plan"
	case pb.WorkflowStep_WORKFLOW_STEP_IMPLEMENT:
		return "implement"
	case pb.WorkflowStep_WORKFLOW_STEP_HANDOFF:
		return "handoff"
	case pb.WorkflowStep_WORKFLOW_STEP_RESUME:
		return "resume"
	case pb.WorkflowStep_WORKFLOW_STEP_VERIFY:
		return "verify"
	case pb.WorkflowStep_WORKFLOW_STEP_LAND:
		return "land"
	default:
		return "-"
	}
}

// WorkflowElapsed returns the elapsed duration for a workflow. For terminal
// statuses (completed, failed, cancelled) it uses updated_at as the end time
// so the elapsed value stops growing. For active workflows it uses time.Now().
func WorkflowElapsed(w *pb.AutopilotWorkflow) time.Duration {
	return workflowElapsed(w)
}

func workflowElapsed(w *pb.AutopilotWorkflow) time.Duration {
	if w.StartedAt == nil {
		return 0
	}
	start := w.StartedAt.AsTime()
	end := time.Now()
	if isTerminalWorkflowStatus(w.Status) && w.UpdatedAt != nil {
		end = w.UpdatedAt.AsTime()
	}
	return end.Sub(start).Truncate(time.Second)
}

// FormatFlightLeg returns a display string for a workflow's flight leg progress.
// For completed workflows where FlightLeg was never updated from its default
// of 0, it displays MaxLegs/MaxLegs since the workflow ran to completion.
func FormatFlightLeg(w *pb.AutopilotWorkflow) string {
	leg := w.FlightLeg
	if w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED && leg == 0 {
		leg = w.MaxLegs
	}
	legStr := fmt.Sprintf("%d/%d", leg, w.MaxLegs)
	if w.Status == pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED && w.FlightLeg < w.MaxLegs {
		legStr += " (incomplete)"
	}
	return legStr
}

func isTerminalWorkflowStatus(s pb.WorkflowStatus) bool {
	return s == pb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED ||
		s == pb.WorkflowStatus_WORKFLOW_STATUS_FAILED ||
		s == pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED
}
