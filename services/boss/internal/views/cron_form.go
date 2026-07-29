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
// jobID carries the id of the saved (created or edited) job so the recreated
// list can highlight that row; it is empty when the saved job is unknown
// (falls back to the top-row default).
type cronFormDoneMsg struct{ jobID string }

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

// cronFormChrome is the number of rendered lines that surround the huh form in
// the common (no-error) case, subtracted from terminal height when sizing the
// form so the action bar stays on screen. It covers the banner App.View prepends
// (bannerOverhead), the title + blank line, the live schedule preview line, and
// the action bar (top+bottom padding plus its actual text lines). Mirrors the
// overhead-constant style in cron_list.go / chatpicker.go.
func cronFormChrome(width int) int {
	return bannerOverhead + 2 /*title+blank*/ + 1 /*preview line*/ + actionBarPadY*2 +
		formActionBarLineCount(width, []string{"[enter] save"}, []string{"[esc] cancel"})
}

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
	zeroOutput      bool
	confirm         bool // true = save, false = cancel (mapped from the terminal Confirm field)
}

// cronZeroOutputHelp is the help text for the "Zero output" confirm. The web
// cron form (services/web/src/components/CronJobForm.tsx) carries the same
// string so the two surfaces read as one product; a web-side parity test reads
// this file and asserts they stay byte-identical.
const cronZeroOutputHelp = "Run with no worktree, branch, or PR — for jobs that report elsewhere and change nothing in this repo. The agent runs in the repository checkout. Default off."

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
	// formFields is the ordered field slice handed to huh.NewGroup, retained
	// because huh.Form exposes no enumeration; it backs click-to-focus (BOS-512).
	// This is the tallest form in the package, so it is the one that actually
	// exercises the scroll-offset path.
	formFields formFields
	fd         *cronFormData
	// promptField is the Prompt Text field, retained so Update can resize it to
	// its content after every keystroke (resizePrompt). Like fd it is a pointer,
	// so it survives bubbletea's value-receiver copies of this model.
	promptField *huh.Text

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
		m.fd.enabled = m.job.IsEnabled
		m.fd.agentName = cronDisplayAgentName(m.job.AgentName)
		m.fd.model = m.job.Model
		m.fd.gateCommand = m.job.GateCommand
		m.fd.runSetupCommand = m.job.ShouldRunSetupCommand
		m.fd.zeroOutput = m.job.IsZeroOutput
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

		bossSelect[string](len(repoOpts), 1).
			Title("Repo").
			Options(repoOpts...).
			Value(&m.fd.repoID),
	}

	if agentOpts := m.agentOptions(); len(agentOpts) > 0 {
		fields = append(fields,
			bossSelect[string](len(agentOpts), 1).
				Title("Agent").
				Options(agentOpts...).
				Value(&m.fd.agentName),
		)
	}

	fields = append(fields,
		huh.NewInput().
			Title("Model").
			Description("Agent model id (eg. claude-opus-5). blank = use the agent's default.").
			Suggestions(modelSuggestions(m.fd.agentName)).
			Value(&m.fd.model),
	)

	// Seeded at build time as well as resized on every Update, so an edit-mode
	// prefill opens at the height its prompt needs instead of snapping to it on
	// the first keypress.
	m.promptField = bossText(m.fd.prompt).
		Title("Prompt").
		Description("Single-turn prompt. Cron sessions only listen for the main agent's Stop hook — subagents are ignored. Keep it self-contained.").
		Value(&m.fd.prompt).
		Validate(func(s string) error {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("prompt is required")
			}
			return nil
		})

	fields = append(fields,
		m.promptField,

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

		bossConfirm().
			Title("Run setup command").
			Description("Run the repo setup script before the agent. Turn off to keep light jobs fast. Default on.").
			Value(&m.fd.runSetupCommand),

		bossConfirm().
			Title("Zero output").
			Description(cronZeroOutputHelp).
			Value(&m.fd.zeroOutput),

		bossConfirm().
			Title("Enabled").
			Description("Run this job on its schedule.").
			Value(&m.fd.enabled),

		bossConfirm().
			Affirmative(saveLabel(m.job)).
			Negative("Cancel").
			Value(&m.fd.confirm),
	)

	m.form = newBossForm(fields...).WithHeight(m.formHeight())
	m.formFields = newFormFields(fields...)
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
	chrome := cronFormChrome(m.width)
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

// resizePrompt re-sizes the Prompt field to its current content. Called after
// each Update so the box tracks what the user has typed. huh's Text.Lines is a
// plain textarea.SetHeight, which clamps itself, so calling it every update is
// cheap and safe.
func (m *CronFormModel) resizePrompt() {
	m.resizePromptFor("")
}

// resizePromptFor re-sizes the Prompt field for its content plus pending — the
// text a message is about to insert. Call it with the pending insert *before*
// handing the message to huh and with "" after; bossTextPendingInsert explains
// why the second call alone leaves the box scrolled off its own first line.
func (m *CronFormModel) resizePromptFor(pending string) {
	if m.promptField == nil || m.fd == nil {
		return
	}
	m.promptField.Lines(bossTextLines(m.fd.prompt+pending, bossFormWrapWidth()))
}

// Update handles messages.
func (m CronFormModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.form != nil {
			// Re-size the prompt before the form, not after: huh's
			// Group.WithHeight only ever shrinks an over-tall field, so a
			// terminal that shrank and then grew again would otherwise leave
			// the textarea clipped at its shrunk height until the next
			// keystroke. Seeding it back to its content height first lets
			// resizeForm re-clamp only if the new height still demands it.
			//
			// The Prompt is the only field re-seeded because it is the only one
			// this view holds a pointer to, and the only one whose height is
			// content-derived rather than fixed at build time. The selects
			// carry the same shrink-only asymmetry — that predates BOS-567 and
			// costs at most one hidden option — so it is left alone here.
			m.resizePrompt()
			return m, resizeForm(m.form, m.formHeight(), msg)
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
			// Deliberately a bare WithHeight, not resizeForm: this message can
			// only arrive from handleSubmit, which is only reached with the
			// form in huh.StateCompleted, and huh's Form.Update early-returns
			// for any non-StateNormal state — so nothing here can rebuild the
			// group's viewport, and there is no scroll offset to resync because
			// a completed form renders nothing at all (Form.View returns "" once
			// quitting). The call is inert; it is kept only so the height stays
			// consistent if the form is ever revived.
			if m.form != nil {
				m.form.WithHeight(m.formHeight())
			}
			return m, nil
		}
		m.done = true
		jobID := ""
		if msg.job != nil {
			jobID = msg.job.Id
		}
		return m, func() tea.Msg { return cronFormDoneMsg{jobID: jobID} }
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

	// Click-to-focus, ahead of huh: huh v2 has no mouse support, so every
	// mouse-shaped message is consumed here rather than forwarded (BOS-512).
	if m.form != nil {
		if cmd, handled := m.formFields.handleMouse(msg, m.form, linesBefore(m.formPrefix())); handled {
			return m, cmd
		}
	}

	// Delegate to huh form.
	if m.form != nil {
		// Grow the Prompt for what this message is about to type before huh
		// sees it, so the textarea never scrolls to find its own cursor.
		m.resizePromptFor(bossTextPendingInsert(msg))
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

		// Recompute live preview and re-fit the Prompt box after every form
		// update.
		m.recomputePreview()
		m.resizePrompt()
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
				RepoId:                fd.repoID,
				Name:                  strings.TrimSpace(fd.name),
				Prompt:                strings.TrimSpace(fd.prompt),
				Schedule:              strings.TrimSpace(fd.schedule),
				Timezone:              strings.TrimSpace(fd.timezone),
				IsEnabled:             fd.enabled,
				AgentName:             strings.TrimSpace(fd.agentName),
				Model:                 strings.TrimSpace(fd.model),
				GateCommand:           strings.TrimSpace(fd.gateCommand),
				ShouldRunSetupCommand: &fd.runSetupCommand,
				IsZeroOutput:          &fd.zeroOutput,
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
	if fd.enabled != original.IsEnabled {
		enabled := fd.enabled
		req.IsEnabled = &enabled
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
	if fd.runSetupCommand != original.ShouldRunSetupCommand {
		rsc := fd.runSetupCommand
		req.ShouldRunSetupCommand = &rsc
	}
	if fd.zeroOutput != original.IsZeroOutput {
		zo := fd.zeroOutput
		req.IsZeroOutput = &zo
	}

	return m, func() tea.Msg {
		job, err := c.UpdateCronJob(ctx, req)
		return cronFormSavedMsg{job: job, err: err}
	}
}

// formPrefix is everything View renders above the huh form: the header, the
// blank line under it, and the submit-error block when one is showing. The
// click hit test measures this same string, so a line added here moves both the
// render and the hit test together (BOS-512).
func (m CronFormModel) formPrefix() string {
	var b strings.Builder
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
	return b.String()
}

// View renders the form.
func (m CronFormModel) View() tea.View {
	var b strings.Builder

	b.WriteString(m.formPrefix())

	if !m.reposReady || !m.agentsReady {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render("Loading…"))
		b.WriteString("\n")
		b.WriteString(actionBarWidth(m.width, []string{"[esc] cancel"}))
		return tea.NewView(b.String())
	}

	if m.submitting {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Saving…"))
		b.WriteString("\n")
		return tea.NewView(b.String())
	}

	// huh returns "" from View once the form is completed or aborted, which this
	// view stays on after a failed submit — so "m.form != nil" is not the same
	// question as "a form is on screen", and only the latter may turn mouse
	// reporting on, or advertise [click], below.
	onScreen := formOnScreen(m.form)
	if onScreen {
		// PaddingLeft(2) matches this view's title, schedule preview, error and
		// action bar (all Padding(_, 2)) and the other three form hosts, so the
		// focused field's gutter lines up with everything else on screen.
		b.WriteString(lipgloss.NewStyle().PaddingLeft(2).Render(m.form.View()))
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

	// The field-navigation bar belongs only on a screen that has fields to
	// navigate; without a form the plain bar carries the verbs alone.
	if !onScreen {
		b.WriteString(actionBarWidth(m.width, []string{"[enter] save"}, []string{"[esc] cancel"}))
		return tea.NewView(b.String())
	}
	b.WriteString(formActionBarWidth(m.width, []string{"[enter] save"}, []string{"[esc] cancel"}))

	v := tea.NewView(b.String())
	// Mouse reporting is scoped to screens that actually render a form
	// (BOS-512): the loading and saving branches above return before here, and
	// after a failed submit the completed form renders nothing — there is no
	// field to click, so the mode must stay off.
	v.MouseMode = tea.MouseModeCellMotion
	return v
}
