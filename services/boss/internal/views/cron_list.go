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

// --- Messages ---

// cronTickMsg signals a polling refresh for the cron list.
type cronTickMsg struct{}

// cronJobsLoadedMsg carries the result of ListCronJobs.
type cronJobsLoadedMsg struct {
	jobs []*pb.CronJob
	err  error
}

// cronReposLoadedMsg carries the result of ListRepos for cron display names.
type cronReposLoadedMsg struct {
	repos []*pb.Repo
	err   error
}

// cronJobDeletedMsg carries the result of DeleteCronJob.
type cronJobDeletedMsg struct {
	err error
}

// cronJobUpdatedMsg carries the result of UpdateCronJob (toggle enabled).
type cronJobUpdatedMsg struct {
	job *pb.CronJob
	err error
}

// cronRunNowMsg carries the result of RunCronJobNow.
type cronRunNowMsg struct {
	id            string
	name          string
	skippedReason string
	err           error
}

// cronFormOpenMsg is emitted by CronListModel to ask the parent App to open
// the cron form in create or edit mode.
type cronFormOpenMsg struct {
	job *pb.CronJob // nil = create mode; non-nil = edit mode
}

// --- Cron List Model ---

const cronPollInterval = 2 * time.Second

// CronListModel displays all scheduled cron jobs with CRUD keybindings.
type CronListModel struct {
	client client.BossClient
	ctx    context.Context

	jobs  []*pb.CronJob
	repos map[string]*pb.Repo // repoID → Repo (for display name)

	table  table.Model
	err    error
	cancel bool

	// confirm overlay state (nil = inactive)
	confirming bool

	// running tracks job IDs whose RunCronJobNow RPC is in flight, so the
	// row can show a "Running…" spinner like home.go does for Archiving.
	// The bit is set on 'r' press and cleared when cronRunNowMsg arrives.
	// Mirrors the duration of the RPC call (worktree creation), not the
	// full session lifetime.
	running map[string]bool
	spinner spinner.Model

	// transient status message (toast)
	status       string
	statusErr    bool
	statusAt     time.Time
	statusFiring bool // true while the toast should render an animated spinner

	// layout
	width  int
	height int
}

// NewCronListModel creates a CronListModel wired to the daemon client.
func NewCronListModel(c client.BossClient, ctx context.Context) CronListModel {
	return CronListModel{
		client:  c,
		ctx:     ctx,
		repos:   make(map[string]*pb.Repo),
		running: make(map[string]bool),
		spinner: newStatusSpinner(),
		table:   newBossTable(nil, nil, 0),
	}
}

// Cancelled reports whether the user dismissed the cron list view.
func (m CronListModel) Cancelled() bool { return m.cancel }

func (m CronListModel) Init() tea.Cmd {
	return tea.Batch(m.fetchRepos(), m.fetchJobs(), m.tickCmd(), m.spinner.Tick)
}

// --- Commands ---

func (m CronListModel) tickCmd() tea.Cmd {
	return tea.Tick(cronPollInterval, func(time.Time) tea.Msg {
		return cronTickMsg{}
	})
}

func (m CronListModel) fetchJobs() tea.Cmd {
	return func() tea.Msg {
		jobs, err := m.client.ListCronJobs(m.ctx)
		return cronJobsLoadedMsg{jobs: jobs, err: err}
	}
}

func (m CronListModel) fetchRepos() tea.Cmd {
	return func() tea.Msg {
		repos, err := m.client.ListRepos(m.ctx)
		return cronReposLoadedMsg{repos: repos, err: err}
	}
}

// --- Update ---

func (m CronListModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case cronJobsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.err = nil
			m.jobs = msg.jobs
			m.rebuildTable()
		}
		return m, nil

	case cronReposLoadedMsg:
		if msg.err == nil {
			for _, r := range msg.repos {
				m.repos[r.Id] = r
			}
			m.rebuildTable()
		}
		return m, nil

	case cronJobDeletedMsg:
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("Delete failed: %v", msg.err), true)
		} else {
			m.setStatus("Deleted.", false)
		}
		return m, m.fetchJobs()

	case cronJobUpdatedMsg:
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("Update failed: %v", msg.err), true)
		} else {
			m.jobs = replaceJob(m.jobs, msg.job)
			m.rebuildTable()
		}
		return m, nil

	case cronRunNowMsg:
		// RPC done — clear the per-row spinner regardless of outcome.
		delete(m.running, msg.id)
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("Run failed: %v", msg.err), true)
		} else if msg.skippedReason != "" {
			m.setStatus(fmt.Sprintf("Skipped: %s", msg.skippedReason), true)
		} else {
			m.setFiringStatus(fmt.Sprintf("Firing %q…", msg.name))
		}
		m.rebuildTable()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		// Re-render so the per-row Running… frames animate. Skipping the
		// rebuild when nothing is running keeps idle screens cheap. We also
		// keep animating when any visible row's server-derived status is
		// RUNNING, so the spinner stays alive across polls even when the
		// local m.running bridge has cleared.
		if len(m.running) > 0 || hasRunningStatus(m.jobs) {
			m.rebuildTable()
		}
		return m, cmd

	case cronTickMsg:
		// Suspend polling while confirm overlay is active.
		if m.confirming {
			return m, nil
		}
		return m, tea.Batch(m.fetchJobs(), m.tickCmd())

	case tea.KeyMsg:
		if m.confirming {
			return m.updateConfirm(msg)
		}
		return m.updateNormal(msg)
	}

	// Forward to table for j/k/up/down navigation.
	updated, cmd := m.table.Update(msg)
	m.table = updated
	return m, cmd
}

func (m CronListModel) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "n":
		// Open form in create mode.
		return m, func() tea.Msg { return cronFormOpenMsg{job: nil} }

	case "e":
		// Open form in edit mode for highlighted row.
		if job := m.selectedJob(); job != nil {
			return m, func() tea.Msg { return cronFormOpenMsg{job: job} }
		}
		return m, nil

	case "d":
		if m.selectedJob() != nil {
			m.confirming = true
		}
		return m, nil

	case "space":
		job := m.selectedJob()
		if job == nil {
			return m, nil
		}
		newEnabled := !job.Enabled
		return m, func() tea.Msg {
			updated, err := m.client.UpdateCronJob(m.ctx, &pb.UpdateCronJobRequest{
				Id:      job.Id,
				Enabled: &newEnabled,
			})
			return cronJobUpdatedMsg{job: updated, err: err}
		}

	case "r":
		job := m.selectedJob()
		if job == nil {
			return m, nil
		}
		name := job.Name
		id := job.Id
		// Mark the row as running so the STATUS cell shows the spinner
		// while the daemon spawns the worktree. Cleared in cronRunNowMsg;
		// after that, server-derived LastRunStatus drives the cell.
		m.running[id] = true
		m.rebuildTable()
		return m, func() tea.Msg {
			resp, err := m.client.RunCronJobNow(m.ctx, id)
			if err != nil {
				return cronRunNowMsg{id: id, name: name, err: err}
			}
			return cronRunNowMsg{
				id:            id,
				name:          name,
				skippedReason: resp.SkippedReason,
			}
		}

	case "enter":
		// no-op v1; reserved for future detail view.
		return m, nil

	case "esc":
		m.cancel = true
		return m, nil
	}

	// Forward to table for navigation.
	updated, cmd := m.table.Update(msg)
	m.table = updated
	return m, cmd
}

func (m CronListModel) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		job := m.selectedJob()
		m.confirming = false
		if job == nil {
			return m, nil
		}
		id := job.Id
		return m, func() tea.Msg {
			err := m.client.DeleteCronJob(m.ctx, id)
			return cronJobDeletedMsg{err: err}
		}
	case "n", "esc":
		m.confirming = false
	}
	return m, nil
}

// --- Helpers ---

func (m *CronListModel) setStatus(s string, isErr bool) {
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
func (m *CronListModel) setFiringStatus(s string) {
	m.status = s
	m.statusErr = false
	m.statusAt = time.Now()
	m.statusFiring = true
}

func (m CronListModel) selectedJob() *pb.CronJob {
	i := m.table.Cursor()
	if i < 0 || i >= len(m.jobs) {
		return nil
	}
	return m.jobs[i]
}

func (m *CronListModel) rebuildTable() {
	if len(m.jobs) == 0 {
		m.table.SetColumns(nil)
		m.table.SetRows(nil)
		return
	}

	n := len(m.jobs)
	schedules := make([]string, n)
	names := make([]string, n)
	repoNames := make([]string, n)
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

		if job.Enabled {
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
		// spinner.View() already emits a trailing space — see home.go:570
		// where it's appended directly to "Archiving...".
		switch {
		case m.running[job.Id] || job.LastRunStatus == pb.CronJobStatus_CRON_JOB_STATUS_RUNNING:
			statuses[i] = m.spinner.View() + "Running"
		case job.LastRunStatus == pb.CronJobStatus_CRON_JOB_STATUS_FAILED:
			statuses[i] = styleStatusDanger.Render("failed")
		default:
			statuses[i] = styleSubtle.Render("idle")
		}
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "CRON", Width: maxColWidth("CRON", schedules, 20) + tableColumnSep},
		{Title: "NAME", Width: maxColWidth("NAME", names, 30) + tableColumnSep},
		{Title: "REPO", Width: maxColWidth("REPO", repoNames, 25) + tableColumnSep},
		{Title: "ENABLED", Width: maxColWidth("ENABLED", enableds, 8) + tableColumnSep},
		{Title: "LAST RUN", Width: maxColWidth("LAST RUN", lastRuns, 12) + tableColumnSep},
		{Title: "NEXT RUN", Width: maxColWidth("NEXT RUN", nextRuns, 12) + tableColumnSep},
		{Title: "STATUS", Width: maxColWidth("STATUS", statuses, 12) + tableColumnSep},
	}

	muted := lipgloss.NewStyle().Foreground(colorMuted)

	cursor := m.table.Cursor()
	rows := make([]table.Row, n)
	for i, job := range m.jobs {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		schedule, name, repo, enabled := schedules[i], names[i], repoNames[i], enableds[i]
		lastRun, nextRun, status := lastRuns[i], nextRuns[i], statuses[i]
		if !job.Enabled {
			schedule = muted.Render(schedule)
			name = muted.Render(name)
			repo = muted.Render(repo)
			enabled = muted.Render(enabled)
			lastRun = muted.Render(lastRun)
			nextRun = muted.Render(nextRun)
			status = muted.Render(status)
		}
		rows[i] = table.Row{
			indicator,
			schedule,
			name,
			repo,
			enabled,
			lastRun,
			nextRun,
			status,
		}
	}

	m.table.SetColumns(cols)
	m.table.SetRows(rows)
	m.table.SetWidth(columnsWidth(cols))
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

func (m CronListModel) tableHeight() int {
	return clampedTableHeight(len(m.jobs), m.height, bannerOverhead+1+actionBarPadY+1)
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

// --- Relative time helpers ---

// relTimeAgo formats a past time as "Xm ago", "Xh ago", etc.
func relTimeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// relTimeFuture formats a future time as "in Xm", "in Xh", etc.
func relTimeFuture(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "now"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("in %ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	default:
		return fmt.Sprintf("in %dd", int(d.Hours()/24))
	}
}

// --- View ---

func (m CronListModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	var b strings.Builder

	if len(m.jobs) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			"No cron jobs. Press [n] to create one.",
		))
		b.WriteString("\n")
		b.WriteString(actionBar(
			[]string{"[n]ew"},
			[]string{"[esc] back"},
		))
		return tea.NewView(b.String())
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
	b.WriteString("\n")

	if m.confirming {
		job := m.selectedJob()
		label := "this job"
		if job != nil {
			label = fmt.Sprintf("%q", job.Name)
		}
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
			fmt.Sprintf("Delete %s?", label),
		))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		// Status toast (shown for ~5 seconds). For the "Firing …" toast
		// emitted after a successful RunCronJobNow, prepend the spinner so
		// the user has an animated cue that work was actually kicked off
		// (the row's STATUS cell will read "idle" until the next poll picks
		// up the server-derived RUNNING state).
		if m.status != "" && time.Since(m.statusAt) < 5*time.Second {
			style := styleStatusInfo
			if m.statusErr {
				style = styleStatusDanger
			}
			text := m.status
			if m.statusFiring {
				text = m.spinner.View() + text
			}
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(style.Render(text)))
			b.WriteString("\n")
		}
		b.WriteString(actionBar(
			[]string{"[n]ew", "[e]dit", "[d]elete", "[space] toggle", "[r]un now"},
			[]string{"[esc] back"},
		))
	}

	return tea.NewView(b.String())
}
