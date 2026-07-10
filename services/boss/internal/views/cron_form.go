package views

import (
	"context"
	"fmt"
	"regexp"
	"sort"
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

// cronFormAgentsMsg carries agents loaded for the Agent select field.
type cronFormAgentsMsg struct {
	agents []client.AgentInfo
	err    error
}

// --- Validation ---

var cronNameRe = regexp.MustCompile(`^[A-Za-z0-9 _-]+$`)

// cronSelectHeight caps how many options the Repo/Agent selects show at once.
// Past this the select scrolls internally instead of ballooning the form and
// pushing the Enabled toggle and action bar below the terminal fold.
const cronSelectHeight = 6

// cronFormChrome is the number of rendered lines that surround the huh form in
// the common (no-error) case, subtracted from terminal height when sizing the
// form so the action bar stays on screen. It covers the banner App.View prepends
// (bannerOverhead), the title + blank line, the live schedule preview line, and
// the action bar (top+bottom padding plus its single text line). Mirrors the
// overhead-constant style in cron_list.go / chatpicker.go.
const cronFormChrome = bannerOverhead + 2 /*title+blank*/ + 1 /*preview line*/ + (actionBarPadY*2 + 1) /*action bar*/

// --- Form data ---

// cronFormData holds huh-bound values on the heap so Value() pointers survive
// bubbletea value-receiver copies.
type cronFormData struct {
	name            string
	repoID          string
	prompt          string
	schedule        string
	timezone        string
	enabled         bool
	agentName       string
	model           string
	gateCommand     string
	runSetupCommand bool
	confirm         bool // true = save, false = cancel (mapped from the terminal Confirm field)
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

	// Loaded agents for the Agent select field. Agent loading is best-effort;
	// failures collapse to daemon-default agent behavior.
	agents      []client.AgentInfo
	agentsReady bool

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

	width  int
	height int
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
	return tea.Batch(m.fetchRepos(), m.fetchAgents())
}

func (m CronFormModel) fetchRepos() tea.Cmd {
	return func() tea.Msg {
		repos, err := m.client.ListRepos(m.ctx)
		return cronFormReposMsg{repos: repos, err: err}
	}
}

func (m CronFormModel) fetchAgents() tea.Cmd {
	return func() tea.Msg {
		agents, err := m.client.ListAgents(m.ctx)
		return cronFormAgentsMsg{agents: agents, err: err}
	}
}

// buildForm constructs the huh form once repos are available.
func (m *CronFormModel) buildForm() {
	if m.fd == nil {
		m.fd = &cronFormData{enabled: true, runSetupCommand: true, confirm: true}
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
		m.fd.agentName = cronDisplayAgentName(m.job.AgentName)
		m.fd.model = m.job.Model
		m.fd.gateCommand = m.job.GateCommand
		m.fd.runSetupCommand = m.job.RunSetupCommand
		m.fd.confirm = true
		m.fdPopulated = true
	}
	// Build repo select options, sorted alphabetically by display name.
	repoOpts := make([]huh.Option[string], len(m.repos))
	for i, r := range m.repos {
		repoOpts[i] = huh.NewOption(r.DisplayName, r.Id)
	}
	sort.SliceStable(repoOpts, func(i, j int) bool {
		return strings.ToLower(repoOpts[i].Key) < strings.ToLower(repoOpts[j].Key)
	})
	if len(repoOpts) == 0 {
		// Fallback: single blank option so the form doesn't panic.
		repoOpts = []huh.Option[string]{huh.NewOption("(no repos)", "")}
	}

	fields := []huh.Field{
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
			Height(cronSelectHeight).
			Value(&m.fd.repoID),
	}

	if agentOpts := m.agentOptions(); len(agentOpts) > 0 {
		fields = append(fields,
			huh.NewSelect[string]().
				Title("Agent").
				Options(agentOpts...).
				Height(cronSelectHeight).
				Value(&m.fd.agentName),
		)
	}

	fields = append(fields,
		huh.NewInput().
			Title("Model").
			Description("Agent model id (eg. claude-opus-4-8). blank = use the agent's default.").
			Suggestions(modelSuggestions(m.fd.agentName)).
			Value(&m.fd.model),
	)

	fields = append(fields,
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

		huh.NewInput().
			Title("Gate command").
			Description("Optional. Runs before each scheduled fire; a non-zero exit skips the run. Treated as a path if it starts with /, ./, or ../, otherwise run via the shell.").
			Value(&m.fd.gateCommand),

		huh.NewConfirm().
			Title("Run setup command").
			Description("Run the repo setup script before the agent. Turn off to keep light jobs fast. Default on.").
			Value(&m.fd.runSetupCommand),

		huh.NewConfirm().
			Title("Enabled").
			Description("Run this job on its schedule.").
			Value(&m.fd.enabled),

		huh.NewConfirm().
			Affirmative(saveLabel(m.job)).
			Negative("Cancel").
			Value(&m.fd.confirm),
	)

	m.form = huh.NewForm(
		huh.NewGroup(fields...),
	).WithTheme(bossHuhTheme()).WithShowHelp(false).WithWidth(70).WithHeight(m.formHeight())
}

// saveLabel returns the affirmative label for the terminal save Confirm based
// on whether this is a create or edit operation.
func saveLabel(job *pb.CronJob) string {
	if job == nil {
		return "Add Scheduled Job"
	}
	return "Update Scheduled Job"
}

// modelSuggestions returns convenience completions only. Any free-text value
// is accepted; the agent CLI validates the model at runtime, so the daemon and
// TUI never enumerate a closed set of valid models.
func modelSuggestions(agent string) []string {
	switch strings.TrimSpace(agent) {
	case "codex":
		return []string{"gpt-5-codex"}
	default: // claude (and daemon default)
		return []string{"opus", "sonnet", "haiku"}
	}
}

func (m CronFormModel) agentOptions() []huh.Option[string] {
	if len(m.agents) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(m.agents)+1)
	opts := make([]huh.Option[string], 0, len(m.agents)+1)
	if m.job == nil {
		seen[""] = true
		opts = append(opts, huh.NewOption("Daemon default", ""))
	}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		opts = append(opts, huh.NewOption(name, name))
	}
	for _, agent := range m.agents {
		add(agent.Name)
	}
	// In edit mode the saved agent may no longer be loaded (plugin removed, or a
	// legacy "claude" job on a non-claude install). Keep it selectable so an
	// unrelated edit doesn't silently rewrite the agent.
	if m.fd != nil {
		add(m.fd.agentName)
	}
	return opts
}

// formHeight returns the height to constrain the huh form to so the schedule
// preview and action bar below it stay on screen. It returns 0 when the
// terminal height is unknown (WithHeight(0) is a no-op in huh, leaving the form
// unconstrained). A submit error or schedule error adds a rendered line of
// chrome, so reserve for it to keep the action bar from being pushed off again.
func (m CronFormModel) formHeight() int {
	if m.height <= 0 {
		return 0
	}
	chrome := cronFormChrome
	if m.err != nil {
		chrome += 2 // submit error renders an extra block above the form
	}
	return max(m.height-chrome, 3)
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
		m.height = msg.Height
		if m.form != nil {
			m.form.WithHeight(m.formHeight())
		}
		return m, nil

	case cronFormReposMsg:
		if msg.err != nil {
			m.err = fmt.Errorf("load repos: %w", msg.err)
			return m, nil
		}
		m.repos = msg.repos
		m.reposReady = true
		if m.agentsReady {
			m.buildForm()
			return m, m.form.Init()
		}
		return m, nil

	case cronFormAgentsMsg:
		if msg.err == nil {
			m.agents = msg.agents
		}
		m.agentsReady = true
		if m.reposReady {
			m.buildForm()
			return m, m.form.Init()
		}
		return m, nil

	case cronFormSavedMsg:
		m.submitting = false
		if msg.err != nil {
			m.err = msg.err
			if m.form != nil {
				m.form.WithHeight(m.formHeight())
			}
			// Do NOT unwind the form — let the user correct and resubmit.
			return m, nil
		}
		m.done = true
		return m, func() tea.Msg { return cronFormDoneMsg{} }
	}

	// ESC before form is ready — cancel immediately.
	if !m.reposReady || !m.agentsReady {
		if km, ok := msg.(tea.KeyMsg); ok && km.String() == "esc" {
			m.cancelled = true
			return m, nil
		}
		return m, nil
	}

	// While submitting or after a successful save, ignore all input. The App
	// switches back to the cron list on cronFormDoneMsg, but a queued key can
	// arrive before that message is handled. The completed huh form would
	// otherwise submit the same job a second time.
	if m.submitting || m.done {
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
			if m.fd != nil && !m.fd.confirm {
				m.cancelled = true
				return m, nil
			}
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
				RepoId:          fd.repoID,
				Name:            strings.TrimSpace(fd.name),
				Prompt:          strings.TrimSpace(fd.prompt),
				Schedule:        strings.TrimSpace(fd.schedule),
				Timezone:        strings.TrimSpace(fd.timezone),
				Enabled:         fd.enabled,
				AgentName:       strings.TrimSpace(fd.agentName),
				Model:           strings.TrimSpace(fd.model),
				GateCommand:     strings.TrimSpace(fd.gateCommand),
				RunSetupCommand: &fd.runSetupCommand,
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
	agentName := strings.TrimSpace(fd.agentName)
	if cronDisplayAgentName(agentName) != cronDisplayAgentName(original.AgentName) {
		req.AgentName = &agentName
	}
	model := strings.TrimSpace(fd.model)
	if model != original.Model {
		req.Model = &model
	}
	gateCommand := strings.TrimSpace(fd.gateCommand)
	if gateCommand != original.GateCommand {
		req.GateCommand = &gateCommand
	}
	if fd.runSetupCommand != original.RunSetupCommand {
		rsc := fd.runSetupCommand
		req.RunSetupCommand = &rsc
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

	if !m.reposReady || !m.agentsReady {
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

	b.WriteString(actionBar([]string{"[tab] next field", "[enter] save", "[esc] cancel"}))

	return tea.NewView(b.String())
}
