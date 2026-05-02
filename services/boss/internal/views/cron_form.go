package views

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/cronutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Messages ---

// cronFormDoneMsg is emitted by CronFormModel after a successful submit.
// The App handles it by switching back to ViewCron and refreshing the list.
type cronFormDoneMsg struct{}

// cronFormSavedMsg carries the result of the Create/Update RPC.
type cronFormSavedMsg struct {
	job *pb.CronJob
	err error
}

// cronFormReposMsg carries repos loaded for the repo select field.
type cronFormReposMsg struct {
	repos []*pb.Repo
	err   error
}

// --- Validation ---

var cronNameRe = regexp.MustCompile(`^[A-Za-z0-9 _-]+$`)

// --- Form data ---

// cronFormData holds huh-bound values on the heap so Value() pointers survive
// bubbletea value-receiver copies.
type cronFormData struct {
	name     string
	repoID   string
	prompt   string
	schedule string
	timezone string
	enabled  bool
}

// --- Model ---

// CronFormModel is the create/edit form for a scheduled cron job.
type CronFormModel struct {
	client client.BossClient
	ctx    context.Context
	job    *pb.CronJob // nil = create mode; non-nil = edit mode

	// Loaded repos for the Repo select field.
	repos      []*pb.Repo
	reposReady bool

	// huh form and bound data.
	form *huh.Form
	fd   *cronFormData

	// Live schedule preview rendered below the form.
	schedulePreview string // empty if invalid or blank
	scheduleErr     string // error text if invalid, empty if valid

	// fdPopulated is set to true after the first pre-populate from m.job so
	// that a subsequent buildForm call (e.g. after a submit error) does not
	// overwrite user edits.
	fdPopulated bool

	// Submit state.
	submitting bool
	err        error // RPC error after submit
	cancelled  bool
	done       bool

	width int
}

// NewCronFormModel creates a CronFormModel wired to the daemon client.
// job is nil for create mode, non-nil for edit mode.
func NewCronFormModel(c client.BossClient, ctx context.Context) CronFormModel {
	return CronFormModel{
		client: c,
		ctx:    ctx,
	}
}

// Cancelled reports whether the user dismissed the cron form.
func (m CronFormModel) Cancelled() bool { return m.cancelled }

// Done reports whether the form was successfully submitted.
func (m CronFormModel) Done() bool { return m.done }

func (m CronFormModel) Init() tea.Cmd {
	return m.fetchRepos()
}

func (m CronFormModel) fetchRepos() tea.Cmd {
	return func() tea.Msg {
		repos, err := m.client.ListRepos(m.ctx)
		return cronFormReposMsg{repos: repos, err: err}
	}
}

// buildForm constructs the huh form once repos are available.
func (m *CronFormModel) buildForm() {
	if m.fd == nil {
		m.fd = &cronFormData{enabled: true}
	}

	// Pre-populate fields from existing job in edit mode.
	// Guard with fdPopulated so a second buildForm call (e.g. after a submit
	// error) does not overwrite values the user may have already edited.
	if m.job != nil && !m.fdPopulated {
		m.fd.name = m.job.Name
		m.fd.repoID = m.job.RepoId
		m.fd.prompt = m.job.Prompt
		m.fd.schedule = m.job.Schedule
		m.fd.timezone = m.job.Timezone
		m.fd.enabled = m.job.Enabled
		m.fdPopulated = true
	}

	// Build repo select options.
	repoOpts := make([]huh.Option[string], len(m.repos))
	for i, r := range m.repos {
		repoOpts[i] = huh.NewOption(r.DisplayName, r.Id)
	}
	if len(repoOpts) == 0 {
		// Fallback: single blank option so the form doesn't panic.
		repoOpts = []huh.Option[string]{huh.NewOption("(no repos)", "")}
	}

	m.form = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Name").
				Placeholder("Daily dependency update").
				Value(&m.fd.name).
				Validate(func(s string) error {
					s = strings.TrimSpace(s)
					if s == "" {
						return fmt.Errorf("name is required")
					}
					if len(s) > 80 {
						return fmt.Errorf("name must be 80 characters or fewer")
					}
					if !cronNameRe.MatchString(s) {
						return fmt.Errorf("name may only contain letters, digits, spaces, hyphens, and underscores")
					}
					return nil
				}),

			huh.NewSelect[string]().
				Title("Repo").
				Options(repoOpts...).
				Value(&m.fd.repoID),

			huh.NewText().
				Title("Prompt").
				Description("Single-turn prompt. Cron sessions only listen for the main agent's Stop hook — subagents are ignored. Keep it self-contained.").
				Value(&m.fd.prompt).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("prompt is required")
					}
					return nil
				}),

			huh.NewInput().
				Title("Schedule").
				Placeholder("0 9 * * 1-5").
				Description("5-field cron expression or @daily/@hourly/@weekly/@monthly").
				Value(&m.fd.schedule).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("schedule is required")
					}
					_, err := cronutil.Parse(strings.TrimSpace(s))
					if err != nil {
						return err
					}
					return nil
				}),

			huh.NewInput().
				Title("Timezone").
				Placeholder("America/New_York").
				Description("Optional IANA timezone name. Empty = daemon local.").
				Value(&m.fd.timezone).
				Validate(func(s string) error {
					if s == "" {
						return nil
					}
					_, err := cronutil.ResolveTimezone(s)
					return err
				}),

			huh.NewConfirm().
				Title("Enabled").
				Value(&m.fd.enabled),
		),
	).WithTheme(bossHuhTheme()).WithShowHelp(false).WithWidth(70)
}

// recomputePreview refreshes m.schedulePreview and m.scheduleErr based on
// current fd values. Called after each Update so the footer stays live.
func (m *CronFormModel) recomputePreview() {
	if m.fd == nil {
		m.schedulePreview = ""
		m.scheduleErr = ""
		return
	}
	spec := strings.TrimSpace(m.fd.schedule)
	if spec == "" {
		m.schedulePreview = ""
		m.scheduleErr = ""
		return
	}

	sched, err := cronutil.Parse(spec)
	if err != nil {
		m.schedulePreview = ""
		m.scheduleErr = err.Error()
		return
	}

	tzName := strings.TrimSpace(m.fd.timezone)
	loc, err := cronutil.ResolveTimezone(tzName)
	if err != nil {
		// Timezone is invalid — don't show a preview yet.
		m.schedulePreview = ""
		m.scheduleErr = ""
		return
	}

	next := cronutil.NextAt(sched, time.Now(), loc)
	label := next.In(loc).Format("Mon 2006-01-02 15:04:05 MST")
	m.schedulePreview = "Next fire: " + label
	m.scheduleErr = ""
}

// Update handles messages.
func (m CronFormModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case cronFormReposMsg:
		if msg.err != nil {
			m.err = fmt.Errorf("load repos: %w", msg.err)
			return m, nil
		}
		m.repos = msg.repos
		m.reposReady = true
		m.buildForm()
		return m, m.form.Init()

	case cronFormSavedMsg:
		m.submitting = false
		if msg.err != nil {
			m.err = msg.err
			// Do NOT unwind the form — let the user correct and resubmit.
			return m, nil
		}
		m.done = true
		return m, func() tea.Msg { return cronFormDoneMsg{} }
	}

	// ESC before form is ready — cancel immediately.
	if !m.reposReady {
		if km, ok := msg.(tea.KeyMsg); ok && km.String() == "esc" {
			m.cancelled = true
			return m, nil
		}
		return m, nil
	}

	// While submitting, ignore all input.
	if m.submitting {
		return m, nil
	}

	// ESC before the huh form handles it: cancel (return to list).
	if km, ok := msg.(tea.KeyMsg); ok && km.String() == "esc" {
		if m.form != nil && m.form.State == huh.StateNormal {
			m.cancelled = true
			return m, nil
		}
	}

	// Delegate to huh form.
	if m.form != nil {
		_, cmd := m.form.Update(msg)

		if m.form.State == huh.StateAborted {
			m.cancelled = true
			return m, nil
		}

		if m.form.State == huh.StateCompleted {
			return m.handleSubmit()
		}

		// Recompute live preview after every form update.
		m.recomputePreview()
		return m, cmd
	}

	return m, nil
}

// handleSubmit fires the Create or Update RPC.
func (m CronFormModel) handleSubmit() (tea.Model, tea.Cmd) {
	m.submitting = true
	m.err = nil

	fd := m.fd
	c := m.client
	ctx := m.ctx

	if m.job == nil {
		// Create mode.
		return m, func() tea.Msg {
			job, err := c.CreateCronJob(ctx, &pb.CreateCronJobRequest{
				RepoId:   fd.repoID,
				Name:     strings.TrimSpace(fd.name),
				Prompt:   strings.TrimSpace(fd.prompt),
				Schedule: strings.TrimSpace(fd.schedule),
				Timezone: strings.TrimSpace(fd.timezone),
				Enabled:  fd.enabled,
			})
			return cronFormSavedMsg{job: job, err: err}
		}
	}

	// Edit mode: only send changed fields.
	original := m.job
	req := &pb.UpdateCronJobRequest{Id: original.Id}

	name := strings.TrimSpace(fd.name)
	if name != original.Name {
		req.Name = &name
	}
	prompt := strings.TrimSpace(fd.prompt)
	if prompt != original.Prompt {
		req.Prompt = &prompt
	}
	schedule := strings.TrimSpace(fd.schedule)
	if schedule != original.Schedule {
		req.Schedule = &schedule
	}
	timezone := strings.TrimSpace(fd.timezone)
	if timezone != original.Timezone {
		req.Timezone = &timezone
	}
	if fd.enabled != original.Enabled {
		enabled := fd.enabled
		req.Enabled = &enabled
	}

	return m, func() tea.Msg {
		job, err := c.UpdateCronJob(ctx, req)
		return cronFormSavedMsg{job: job, err: err}
	}
}

// View renders the form.
func (m CronFormModel) View() tea.View {
	var b strings.Builder

	// Header.
	title := "New Scheduled Job"
	if m.job != nil {
		title = fmt.Sprintf("Edit Scheduled Job: %s", m.job.Name)
	}
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Bold(true).Render(title))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(renderError(fmt.Sprintf("Error: %v", m.err), m.width))
		b.WriteString("\n")
	}

	if !m.reposReady {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render("Loading…"))
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[esc] cancel"}))
		return tea.NewView(b.String())
	}

	if m.submitting {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Saving…"))
		b.WriteString("\n")
		return tea.NewView(b.String())
	}

	if m.form != nil {
		b.WriteString(lipgloss.NewStyle().PaddingLeft(1).Render(m.form.View()))
		b.WriteString("\n")
	}

	// Live schedule preview / error (rendered outside huh, below the form).
	if m.scheduleErr != "" {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(m.scheduleErr))
		b.WriteString("\n")
	} else if m.schedulePreview != "" {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorSuccess).Render(m.schedulePreview))
		b.WriteString("\n")
	}

	b.WriteString(actionBar([]string{"[tab/enter] next field", "[esc] cancel"}))

	return tea.NewView(b.String())
}
