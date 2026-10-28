package cron

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/boss/internal/domain/rpc"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/display"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/telemetry"
)

// --- Messages ---

// TickMsg signals a polling refresh for the cron list.
type TickMsg struct{}

// JobsLoadedMsg carries the result of ListCronJobs.
type JobsLoadedMsg struct {
	Jobs []*pb.CronJob
	Err  error
}

// ReposLoadedMsg carries the result of ListRepos for cron display names.
type ReposLoadedMsg struct {
	Repos []*pb.Repo
	Err   error
}

// JobDeletedMsg carries the result of DeleteCronJob.
type JobDeletedMsg struct {
	ID  string
	Err error
}

// JobUpdatedMsg carries the result of UpdateCronJob (toggle enabled).
type JobUpdatedMsg struct {
	Job *pb.CronJob
	Err error
}

// RunNowMsg carries the result of RunCronJobNow.
type RunNowMsg struct {
	ID            string
	Name          string
	SkippedReason string
	Err           error
}

// FormOpenMsg is emitted by ListModel to ask the parent App to open
// the cron form in create or edit mode.
type FormOpenMsg struct {
	Job *pb.CronJob // nil = create mode; non-nil = edit mode
}

// --- Cron List Model ---

const cronPollInterval = 2 * time.Second

// ListModel displays all scheduled cron jobs with CRUD keybindings.
type ListModel struct {
	client    Client
	ctx       context.Context
	telemetry telemetry.Client

	jobs  []*pb.CronJob
	repos map[string]*pb.Repo // repoID → Repo (for display name)

	table  ui.Table
	err    error
	cancel bool

	returnView ui.View

	// highlightJobID, when non-empty, requests that the row for this job id be
	// selected (caret placed on it) the next time jobs load. It is set by the
	// parent App after a save so the just-edited/created job stays highlighted,
	// and cleared after the first load attempt. Mirrors highlightRepoID in
	// repo_list.go.
	highlightJobID string

	confirm ui.ConfirmPrompt

	// running tracks job IDs whose RunCronJobNow RPC is in flight, so the
	// row can show a "Running…" spinner like home.go does for Archiving.
	// The bit is set on 'r' press and cleared when RunNowMsg arrives.
	// Mirrors the duration of the RPC call (worktree creation), not the
	// full session lifetime.
	running  map[string]bool
	deleting map[string]bool
	spinner  spinner.Model

	// transient status message (toast)
	status       string
	statusErr    bool
	statusAt     time.Time
	statusFiring bool // true while the toast should render an animated spinner

	// layout
	width  int
	height int
}

// NewListModel creates a ListModel wired to the daemon client.
func NewListModel(c Client, ctx context.Context) ListModel {
	return ListModel{
		client:   c,
		ctx:      ctx,
		repos:    make(map[string]*pb.Repo),
		running:  make(map[string]bool),
		deleting: make(map[string]bool),
		spinner:  ui.NewStatusSpinner(),
		table:    ui.NewBossTable(nil, nil, 0),
	}
}

// SetTelemetry installs a telemetry client for completed cron actions.
func (m *ListModel) SetTelemetry(client telemetry.Client) {
	m.telemetry = client
}

// Cancelled reports whether the user dismissed the cron list view.
func (m ListModel) Cancelled() bool { return m.cancel }

func (m ListModel) Init() tea.Cmd {
	return tea.Batch(m.fetchRepos(), m.fetchJobs(), m.tickCmd(), m.spinner.Tick)
}

// --- Commands ---

func (m ListModel) tickCmd() tea.Cmd {
	return tea.Tick(cronPollInterval, func(time.Time) tea.Msg {
		return TickMsg{}
	})
}

func (m ListModel) fetchJobs() tea.Cmd {
	return func() tea.Msg {
		jobs, err := m.client.ListCronJobs(m.ctx, "")
		return JobsLoadedMsg{Jobs: jobs, Err: err}
	}
}

func (m ListModel) fetchRepos() tea.Cmd {
	return func() tea.Msg {
		repos, err := m.client.ListRepos(m.ctx)
		return ReposLoadedMsg{Repos: repos, Err: err}
	}
}

// --- Update ---

func (m ListModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case JobsLoadedMsg:
		if msg.Err != nil {
			m.err = msg.Err
		} else {
			m.err = nil
			m.jobs = msg.Jobs
			m.rebuildTable()
			if m.highlightJobID != "" {
				for i, job := range m.jobs {
					if job.Id == m.highlightJobID {
						m.table.SetCursor(i)
						break
					}
				}
				m.highlightJobID = ""
			}
		}
		return m, nil

	case ReposLoadedMsg:
		if msg.Err == nil {
			for _, r := range msg.Repos {
				m.repos[r.Id] = r
			}
			m.rebuildTable()
		}
		return m, nil

	case JobDeletedMsg:
		delete(m.deleting, msg.ID)
		// cron_job_deleted is captured at the app root
		// (App.handleCronJobDeletedResult); see the note there.
		if msg.Err != nil {
			m.setStatus(ui.RPCStatusMessage("Delete failed", msg.Err), true)
		} else {
			m.setStatus("Deleted.", false)
		}
		m.rebuildTable()
		return m, m.fetchJobs()

	case JobUpdatedMsg:
		// cron_job_updated is captured at the app root
		// (App.handleCronJobUpdatedResult); see the note there.
		if msg.Err != nil {
			m.setStatus(ui.RPCStatusMessage("Update failed", msg.Err), true)
		} else {
			m.jobs = replaceJob(m.jobs, msg.Job)
			m.rebuildTable()
		}
		return m, nil

	case RunNowMsg:
		// RPC done — clear the per-row spinner regardless of outcome.
		delete(m.running, msg.ID)
		// cron_job_run_now is captured at the app root
		// (App.handleCronRunNowResult), including the gate-skip reasoning.
		if msg.Err != nil {
			m.setStatus(ui.RPCStatusMessage("Run failed", msg.Err), true)
		} else if msg.SkippedReason != "" {
			m.setStatus(fmt.Sprintf("Skipped: %s", display.RunNowSkipLabel(msg.SkippedReason)), true)
		} else {
			m.setFiringStatus(fmt.Sprintf("Firing %q…", msg.Name))
		}
		m.rebuildTable()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		// Re-render so per-row pending frames animate while local RPCs or
		// server-derived cron runs remain active.
		if len(m.running) > 0 || len(m.deleting) > 0 || hasRunningStatus(m.jobs) || hasGatingStatus(m.jobs) {
			m.rebuildTable()
		}
		return m, cmd

	case TickMsg:
		// Suspend polling while confirm overlay is active.
		if m.confirm.Active {
			return m, nil
		}
		return m, tea.Batch(m.fetchJobs(), m.tickCmd())

	case tea.KeyMsg:
		if m.confirm.Active {
			confirmed := msg.String() == "y" || msg.String() == "enter"
			deleteID := ""
			if confirmed {
				if job := m.selectedJob(); job != nil {
					deleteID = job.Id
				}
			}
			var cmd tea.Cmd
			m.confirm, cmd = m.confirm.Update(msg)
			if cmd != nil && deleteID != "" {
				m.deleting[deleteID] = true
				m.rebuildTable()
			}
			return m, cmd
		}
		return m.updateNormal(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.rebuildTable()
		return m, nil
	}

	// Forward non-key messages (mouse, resize, focus, …) to the table; keyboard
	// navigation is handled in updateNormal and never reaches here.
	updated, cmd := m.table.Update(msg)
	m.table = updated
	return m, cmd
}

func (m ListModel) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "n":
		// Open form in create mode.
		return m, func() tea.Msg { return FormOpenMsg{Job: nil} }

	case "e", "enter":
		// Open form in edit mode for highlighted row.
		if job := m.selectedJob(); job != nil {
			return m, func() tea.Msg { return FormOpenMsg{Job: job} }
		}
		return m, nil

	case "d":
		if job := m.selectedJob(); job != nil {
			c := m.client
			ctx := m.ctx
			id := job.Id
			name := job.Name
			m.confirm = ui.NewConfirmPrompt(fmt.Sprintf("Delete %q?", name), func() tea.Msg {
				err := c.DeleteCronJob(ctx, id)
				return JobDeletedMsg{ID: id, Err: err}
			})
		}
		return m, nil

	case "space":
		job := m.selectedJob()
		if job == nil {
			return m, nil
		}
		newEnabled := !job.IsEnabled
		return m, func() tea.Msg {
			updated, err := m.client.UpdateCronJob(m.ctx, &pb.UpdateCronJobRequest{
				Id:        job.Id,
				IsEnabled: &newEnabled,
			})
			return JobUpdatedMsg{Job: updated, Err: err}
		}

	case "r":
		job := m.selectedJob()
		if job == nil {
			return m, nil
		}
		name := job.Name
		id := job.Id
		// Mark the row as running so the STATUS cell shows the spinner
		// while the daemon spawns the worktree. Cleared in RunNowMsg;
		// after that, server-derived LastRunStatus drives the cell.
		m.running[id] = true
		m.rebuildTable()
		return m, func() tea.Msg {
			resp, err := m.client.RunCronJobNow(m.ctx, id)
			if err != nil {
				return RunNowMsg{ID: id, Name: name, Err: err}
			}
			return RunNowMsg{
				ID:            id,
				Name:          name,
				SkippedReason: resp.SkippedReason,
			}
		}

	case "esc":
		m.cancel = true
		return m, nil
	}

	// Forward to table for navigation.
	updated, cmd := m.table.Update(msg)
	m.table = updated
	return m, cmd
}

// --- Helpers ---

func (m *ListModel) setStatus(s string, isErr bool) {
	m.status = s
	m.statusErr = isErr
	m.statusAt = time.Now()
	m.statusFiring = false
}

// setFiringStatus is the variant for the post-RunCronJobNow toast: the
// daemon kicked off a session in the background, and the toast advertises
// that fact with an animated spinner so the user knows work is happening.
// The row's STATUS cell will read "idle" between polls (or "Running" once
// the next poll picks up the server-derived state), so the toast carries
// the visual cue that the fire actually landed.
func (m *ListModel) setFiringStatus(s string) {
	m.status = s
	m.statusErr = false
	m.statusAt = time.Now()
	m.statusFiring = true
}

func (m ListModel) selectedJob() *pb.CronJob {
	i := m.table.Cursor()
	if i < 0 || i >= len(m.jobs) {
		return nil
	}
	return m.jobs[i]
}

func (m *ListModel) rebuildTable() {
	if len(m.jobs) == 0 {
		m.table.SetRows(nil)
		m.table.SetColumns(nil)
		return
	}

	n := len(m.jobs)
	schedules := make([]string, n)
	names := make([]string, n)
	repoNames := make([]string, n)
	agents := make([]string, n)
	enableds := make([]string, n)
	lastRuns := make([]string, n)
	nextRuns := make([]string, n)
	statuses := make([]string, n)

	for i, job := range m.jobs {
		schedules[i] = job.Schedule
		names[i] = job.Name

		if r, ok := m.repos[job.RepoId]; ok {
			repoNames[i] = r.DisplayName
		} else {
			repoNames[i] = job.RepoId
		}
		agents[i] = cronDisplayAgentName(job.AgentName)

		if job.IsEnabled {
			enableds[i] = "yes"
		} else {
			enableds[i] = "no"
		}

		if job.LastRunAt != nil && !job.LastRunAt.AsTime().IsZero() {
			lastRuns[i] = relTimeAgo(job.LastRunAt.AsTime())
		} else {
			lastRuns[i] = "—"
		}

		if job.NextRunAt != nil && !job.NextRunAt.AsTime().IsZero() {
			nextRuns[i] = relTimeFuture(job.NextRunAt.AsTime())
		} else {
			nextRuns[i] = "—"
		}

		// STATUS reflects the live state of the last fire:
		//   - Running: local m.running bridge (snappy 'r'-press feedback)
		//     OR server-derived LastRunStatus == RUNNING (after polls catch up).
		//   - Failed:  server-derived LastRunStatus == FAILED.
		//   - Idle:    everything else (UNSPECIFIED / IDLE / never run).
		// spinner.View() already emits a trailing space — see
		// renderArchivingStatus in status.go, where it's appended directly
		// to "archiving".
		switch {
		case m.deleting[job.Id]:
			statuses[i] = ui.RenderRowPendingStatus(m.spinner, "deleting")
		case m.running[job.Id] || job.LastRunStatus == pb.CronJobStatus_CRON_JOB_STATUS_RUNNING:
			statuses[i] = m.spinner.View() + "Running"
		case job.LastRunStatus == pb.CronJobStatus_CRON_JOB_STATUS_GATING:
			statuses[i] = m.spinner.View() + "gating"
		case job.LastRunStatus == pb.CronJobStatus_CRON_JOB_STATUS_FAILED:
			statuses[i] = cronStatusLabel(job.IsEnabled, ui.StyleStatusDanger, "failed")
		case job.LastRunStatus == pb.CronJobStatus_CRON_JOB_STATUS_GATED:
			statuses[i] = cronStatusLabel(job.IsEnabled, ui.StyleStatusWarning, "gated")
		default:
			statuses[i] = ui.StyleSubtle.Render("idle")
		}
	}

	// Preserve the job name and enabled state first; derived scheduling details
	// and usually-shared repository metadata make room on narrow terminals.
	rcols := []ui.ResponsiveColumn{
		{Col: ui.CursorColumn, Priority: 0, MinWidth: 1},
		{Col: table.Column{Title: "CRON", Width: display.MaxColWidth("CRON", schedules, 20) + ui.TableColumnSep}, Priority: 2, MinWidth: 1},
		{Col: table.Column{Title: "NAME", Width: display.MaxColWidth("NAME", names, 30) + ui.TableColumnSep}, Priority: 0, MinWidth: 1},
		{Col: table.Column{Title: "REPO", Width: display.MaxColWidth("REPO", repoNames, 25) + ui.TableColumnSep}, Priority: 5, MinWidth: 1},
		{Col: table.Column{Title: "AGENT", Width: display.MaxColWidth("AGENT", agents, 16) + ui.TableColumnSep}, Priority: 5, MinWidth: 1},
		{Col: table.Column{Title: "ENABLED", Width: display.MaxColWidth("ENABLED", enableds, 8) + ui.TableColumnSep}, Priority: 1, MinWidth: 1},
		{Col: table.Column{Title: "LAST RUN", Width: display.MaxColWidth("LAST RUN", lastRuns, 12) + ui.TableColumnSep}, Priority: 4, MinWidth: 1},
		{Col: table.Column{Title: "NEXT RUN", Width: display.MaxColWidth("NEXT RUN", nextRuns, 12) + ui.TableColumnSep}, Priority: 4, MinWidth: 1},
		{Col: table.Column{Title: "STATUS", Width: display.MaxColWidth("STATUS", statuses, 12) + ui.TableColumnSep}, Priority: 3, MinWidth: 1},
	}
	fitted := ui.FitColumnsIndexed(rcols, ui.FitAvailWidth(m.width, 1))
	cols := ui.FittedColumns(fitted)

	muted := lipgloss.NewStyle().Foreground(ui.ColorMuted)

	cursor := m.table.Cursor()
	rows := make([]table.Row, n)
	for i, job := range m.jobs {
		indicator := ""
		if i == cursor {
			indicator = ui.CursorChevron
		}
		schedule, name, repo, agent, enabled := schedules[i], names[i], repoNames[i], agents[i], enableds[i]
		lastRun, nextRun, status := lastRuns[i], nextRuns[i], statuses[i]
		if !job.IsEnabled {
			schedule = muted.Render(schedule)
			name = muted.Render(name)
			repo = muted.Render(repo)
			agent = muted.Render(agent)
			enabled = muted.Render(enabled)
			lastRun = muted.Render(lastRun)
			nextRun = muted.Render(nextRun)
			status = muted.Render(status)
		}
		rows[i] = ui.ProjectRow(fitted, table.Row{
			indicator,
			schedule,
			name,
			repo,
			agent,
			enabled,
			lastRun,
			nextRun,
			status,
		})
	}

	ui.SetTableContent(&m.table, cols, rows)
	m.table.SetWidth(ui.ColumnsWidth(cols))
	m.table.SetHeight(m.tableHeight())
	// Restore (or initialize) the cursor. SetRows clamps the cursor down
	// when rows shrink, but does not raise it from –1 (which SetRows(nil)
	// leaves behind when the list was previously empty). Clamp to [0, n-1].
	newCursor := cursor
	if newCursor < 0 {
		newCursor = 0
	} else if newCursor >= n {
		newCursor = n - 1
	}
	m.table.SetCursor(newCursor)
}

// cronStatusLabel renders a terminal STATUS label with its status color only
// when the job is enabled. For a disabled job it returns the plain label so the
// row-level muting applied later (muted.Render in rebuildTable) is the only
// foreground, keeping the whole row uniformly grey. Lipgloss does not strip an
// inner foreground, so a pre-colored label would otherwise keep its status color
// under the outer muted style (BOS-313).
func cronStatusLabel(enabled bool, style lipgloss.Style, label string) string {
	if enabled {
		return style.Render(label)
	}
	return label
}

func cronDisplayAgentName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "claude"
	}
	return name
}

func (m ListModel) tableHeight() int {
	actionLines := ui.ActionBarLineCount(m.width,
		[]string{"[n]ew", "[e/enter]dit", "[d]elete", "[space] toggle", "[r]un now"},
		[]string{"[esc] back"},
	)
	return ui.ClampedTableHeight(len(m.jobs), m.height, ui.BannerOverhead+1+ui.ActionBarPadY+actionLines)
}

// hasRunningStatus reports whether any job's server-derived LastRunStatus is
// RUNNING. Used by the spinner Tick handler to keep the per-row animation
// frame-stepping across polls (when the local m.running bridge has cleared
// but the daemon still reports the session as in-flight).
func hasRunningStatus(jobs []*pb.CronJob) bool {
	for _, j := range jobs {
		if j.LastRunStatus == pb.CronJobStatus_CRON_JOB_STATUS_RUNNING {
			return true
		}
	}
	return false
}

// hasGatingStatus reports whether any job's server-derived LastRunStatus is
// GATING. Used by the spinner Tick handler alongside hasRunningStatus to keep
// the per-row animation frame-stepping while a gate command is in progress.
func hasGatingStatus(jobs []*pb.CronJob) bool {
	for _, j := range jobs {
		if j.LastRunStatus == pb.CronJobStatus_CRON_JOB_STATUS_GATING {
			return true
		}
	}
	return false
}

// replaceJob swaps out the job with the matching ID in the slice.
func replaceJob(jobs []*pb.CronJob, updated *pb.CronJob) []*pb.CronJob {
	if updated == nil {
		return jobs
	}
	result := make([]*pb.CronJob, len(jobs))
	copy(result, jobs)
	for i, j := range result {
		if j.Id == updated.Id {
			result[i] = updated
			break
		}
	}
	return result
}

func relTimeAgo(t time.Time) string          { return ui.RelTimeAgo(t) }
func relTimeFuture(t time.Time) string       { return ui.RelTimeFuture(t) }
func formatRelAgo(d time.Duration) string    { return ui.FormatRelAgo(d) }
func formatRelFuture(d time.Duration) string { return ui.FormatRelFuture(d) }

// --- View ---

func (m ListModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			ui.RenderError(ui.RPCErrorMessage(m.err), m.width) + "\n" +
				ui.StyleActionBar.Render("[esc] back"),
		)
	}

	var b strings.Builder

	if len(m.jobs) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			"No cron jobs. Press [n] to create one.",
		))
		b.WriteString("\n")
		b.WriteString(ui.ActionBarWidth(m.width,
			[]string{"[n]ew"},
			[]string{"[esc] back"},
		))
		return tea.NewView(b.String())
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
	b.WriteString("\n")

	if m.confirm.Active {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(ui.ColorDanger).Render(
			m.confirm.Prompt,
		))
		b.WriteString("\n")
		b.WriteString(ui.StyleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		// Status toast (shown for ~5 seconds). For the "Firing …" toast
		// emitted after a successful RunCronJobNow, prepend the spinner so
		// the user has an animated cue that work was actually kicked off
		// (the row's STATUS cell will read "idle" until the next poll picks
		// up the server-derived RUNNING state).
		if m.status != "" && time.Since(m.statusAt) < 5*time.Second {
			style := ui.StyleStatusInfo
			if m.statusErr {
				style = ui.StyleStatusDanger
			}
			text := m.status
			if m.statusFiring {
				text = m.spinner.View() + text
			}
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(style.Render(text)))
			b.WriteString("\n")
		}
		b.WriteString(ui.ActionBarWidth(m.width,
			[]string{"[n]ew", "[e/enter]dit", "[d]elete", "[space] toggle", "[r]un now"},
			[]string{"[esc] back"},
		))
	}

	return tea.NewView(b.String())
}

// Client is the daemon capability consumed by this screen.
type Client interface {
	ListRepos(ctx context.Context) ([]*pb.Repo, error)
	ListAgents(ctx context.Context) ([]rpc.AgentInfo, error)
	ListCronJobs(ctx context.Context, repoID string) ([]*pb.CronJob, error)
	CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error)
	UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error)
	DeleteCronJob(ctx context.Context, id string) error
	RunCronJobNow(ctx context.Context, id string) (*pb.RunCronJobNowResponse, error)
}

func (m *ListModel) SetSize(width, height int)  { m.width, m.height = width, height }
func (m *ListModel) SetReturnView(view ui.View) { m.returnView = view }
func (m ListModel) ReturnView() ui.View         { return m.returnView }
func (m *ListModel) HighlightJob(id string)     { m.highlightJobID = id }
func (m *ListModel) Table() *ui.Table           { return &m.table }
func (m ListModel) TableHeight() int            { return m.tableHeight() }
func (m *FormModel) SetSize(width, height int)  { m.width, m.height = width, height }
func (m *FormModel) SetJob(job *pb.CronJob)     { m.job = job }
func (m FormModel) Submitting() bool            { return m.submitting }
func (m FormModel) TextEntryActive() bool       { return m.textEntryActive() }

func (m ListModel) Width() int            { return m.width }
func (m ListModel) Height() int           { return m.height }
func (m *ListModel) SetHeight(height int) { m.height = height }

func (m FormModel) Width() int            { return m.width }
func (m FormModel) Height() int           { return m.height }
func (m *FormModel) SetHeight(height int) { m.height = height }

func (m ListModel) HasTelemetry() bool { return m.telemetry != nil }
func (m FormModel) HasTelemetry() bool { return m.telemetry != nil }
func (m FormModel) Job() *pb.CronJob   { return m.job }

func (m FormModel) Form() *huh.Form           { return m.form }
func (m ListModel) Status() string            { return m.status }
func (m ListModel) IsRunning(id string) bool  { return m.running[id] }
func (m ListModel) IsDeleting(id string) bool { return m.deleting[id] }

func (m FormModel) FormFields() ui.FormFields { return m.formFields }
func (m FormModel) FormPrefix() string        { return m.formPrefix() }
func (m FormModel) FormHeight() int           { return m.formHeight() }
