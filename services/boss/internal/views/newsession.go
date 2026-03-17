package views

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Wizard steps for new session creation.
type wizardStep int

const (
	stepRepoSelect       wizardStep = iota
	stepSessionType                 // replaces stepPRMode
	stepPRSelect                    // existing PR only
	stepTitleInput                  // new PR only
	stepPlanInput                   // plan feature only
	stepConfirmOverwrite            // branch exists, confirm force
	stepCreating                    // waiting for CreateSession RPC
)

// sessionType identifies the kind of session to create.
type sessionType int

const (
	sessionTypeNewPR       sessionType = iota // Create a new PR
	sessionTypeExistingPR                     // Work on an existing PR
	sessionTypePlanFeature                    // Plan a feature
	sessionTypeExecutePlan                    // Execute a plan (placeholder)
)

// reposMsg carries the result of a ListRepos RPC call.
type reposMsg struct {
	repos []*pb.Repo
	err   error
}

// prsMsg carries the result of a ListRepoPRs RPC call.
type prsMsg struct {
	prs []*pb.PRSummary
	err error
}

// sessionCreatedMsg carries the result of a CreateSession RPC call.
type sessionCreatedMsg struct {
	session *pb.Session
	err     error
}

// NewSessionModel is the multi-step wizard for creating a new coding session.
type NewSessionModel struct {
	client client.BossClient
	ctx    context.Context

	step   wizardStep
	err    error
	done   bool
	cancel bool

	// Step 1: Repo select
	repos      []*pb.Repo
	repoCursor int

	// Step 2: Session type
	sessionTypeCursor int

	// Step 3: PR select (existing PR)
	prs      []*pb.PRSummary
	prCursor int
	prsErr   error

	// Plan input (plan feature only)
	planInput textarea.Model

	// Title input (new PR only)
	titleInput textinput.Model

	// Collected values
	selectedRepo *pb.Repo
	selectedPR   *pb.PRSummary
	createdSess  *pb.Session
	forceBranch  bool // retry with force after branch conflict

	// Layout
	width int
}

// NewNewSessionModel creates a NewSessionModel wired to the daemon client.
func NewNewSessionModel(c client.BossClient, ctx context.Context) NewSessionModel {
	ti := textinput.New()
	ti.Placeholder = "Session title"
	ti.SetWidth(50)

	ta := textarea.New()
	ta.Placeholder = "Describe what Claude should implement..."
	ta.SetWidth(60)
	ta.SetHeight(8)
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	taStyles := ta.Styles()
	taStyles.Focused.Base = lipgloss.NewStyle().
		BorderTop(false).BorderBottom(true).
		BorderLeft(false).BorderRight(false).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("8"))
	ta.SetStyles(taStyles)

	return NewSessionModel{
		client:     c,
		ctx:        ctx,
		step:       stepRepoSelect,
		titleInput: ti,
		planInput:  ta,
	}
}

func (m NewSessionModel) Init() tea.Cmd {
	return fetchRepos(m.client, m.ctx)
}

func fetchRepos(c client.BossClient, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		repos, err := c.ListRepos(ctx)
		return reposMsg{repos: repos, err: err}
	}
}

func fetchPRs(c client.BossClient, ctx context.Context, repoID string) tea.Cmd {
	return func() tea.Msg {
		prs, err := c.ListRepoPRs(ctx, repoID)
		return prsMsg{prs: prs, err: err}
	}
}

func createSession(c client.BossClient, ctx context.Context, req *pb.CreateSessionRequest) tea.Cmd {
	return func() tea.Msg {
		sess, err := c.CreateSession(ctx, req)
		return sessionCreatedMsg{session: sess, err: err}
	}
}

func (m NewSessionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case reposMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.repos = msg.repos
		if len(m.repos) == 1 {
			// Auto-select the only repo.
			m.selectedRepo = m.repos[0]
			m.step = stepSessionType
		}
		return m, nil

	case prsMsg:
		m.prs = msg.prs
		m.prsErr = msg.err
		return m, nil

	case sessionCreatedMsg:
		if msg.err != nil {
			// Check if the error is a branch-already-exists conflict.
			var connectErr *connect.Error
			if errors.As(msg.err, &connectErr) && connectErr.Code() == connect.CodeAlreadyExists {
				m.step = stepConfirmOverwrite
				m.err = nil
				return m, nil
			}
			m.err = msg.err
			return m, nil
		}
		m.createdSess = msg.session
		m.done = true
		return m, nil

	case tea.KeyMsg:
		// Global keys
		switch msg.String() {
		case "esc":
			m.cancel = true
			return m, nil
		}

		switch m.step {
		case stepRepoSelect:
			return m.updateRepoSelect(msg)
		case stepSessionType:
			return m.updateSessionType(msg)
		case stepPRSelect:
			return m.updatePRSelect(msg)
		case stepTitleInput:
			return m.updateTitleInput(msg)
		case stepPlanInput:
			return m.updatePlanInput(msg)
		case stepConfirmOverwrite:
			return m.updateConfirmOverwrite(msg)
		default:
		}
	}

	// Pass through to focused inputs.
	switch m.step {
	case stepTitleInput:
		var cmd tea.Cmd
		m.titleInput, cmd = m.titleInput.Update(msg)
		return m, cmd
	case stepPlanInput:
		var cmd tea.Cmd
		m.planInput, cmd = m.planInput.Update(msg)
		return m, cmd
	default:
	}

	return m, nil
}

func (m NewSessionModel) updateRepoSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.repoCursor > 0 {
			m.repoCursor--
		}
	case "down", "j":
		if m.repoCursor < len(m.repos)-1 {
			m.repoCursor++
		}
	case "enter":
		if len(m.repos) > 0 {
			m.selectedRepo = m.repos[m.repoCursor]
			m.step = stepSessionType
		}
	}
	return m, nil
}

func (m NewSessionModel) selectedSessionType() sessionType {
	return sessionType(m.sessionTypeCursor)
}

func (m NewSessionModel) updateSessionType(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	const optionCount = 4
	switch msg.String() {
	case "up", "k":
		if m.sessionTypeCursor > 0 {
			m.sessionTypeCursor--
		}
	case "down", "j":
		if m.sessionTypeCursor < optionCount-1 {
			m.sessionTypeCursor++
		}
	case "enter":
		switch m.selectedSessionType() {
		case sessionTypeNewPR:
			m.step = stepTitleInput
			return m, m.titleInput.Focus()
		case sessionTypeExistingPR:
			m.step = stepPRSelect
			return m, fetchPRs(m.client, m.ctx, m.selectedRepo.Id)
		case sessionTypePlanFeature:
			m.step = stepPlanInput
			return m, m.planInput.Focus()
		case sessionTypeExecutePlan:
			// Placeholder — no action.
		}
	}
	return m, nil
}

func (m NewSessionModel) updatePRSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.prCursor > 0 {
			m.prCursor--
		}
	case "down", "j":
		if m.prCursor < len(m.prs)-1 {
			m.prCursor++
		}
	case "enter":
		if len(m.prs) > 0 {
			m.selectedPR = m.prs[m.prCursor]
			return m, m.startCreating()
		}
	}
	return m, nil
}

func (m NewSessionModel) updateTitleInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		if m.titleInput.Value() != "" {
			m.titleInput.Blur()
			return m, m.startCreating()
		}
	}
	// Pass through to textinput.
	var cmd tea.Cmd
	m.titleInput, cmd = m.titleInput.Update(msg)
	return m, cmd
}

func (m NewSessionModel) updatePlanInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+d":
		if m.planInput.Value() != "" {
			m.planInput.Blur()
			return m, m.startCreating()
		}
		return m, nil
	}
	// Pass through to textarea.
	var cmd tea.Cmd
	m.planInput, cmd = m.planInput.Update(msg)
	return m, cmd
}

func (m NewSessionModel) updateConfirmOverwrite(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		m.forceBranch = true
		return m, m.startCreating()
	case "n", "N":
		// Go back to the title input step.
		m.step = stepTitleInput
		return m, m.titleInput.Focus()
	}
	return m, nil
}

// startCreating builds a CreateSessionRequest and fires the RPC.
func (m *NewSessionModel) startCreating() tea.Cmd {
	m.step = stepCreating
	req := &pb.CreateSessionRequest{
		RepoId:      m.selectedRepo.Id,
		BaseBranch:  m.selectedRepo.DefaultBaseBranch,
		ForceBranch: m.forceBranch,
	}

	switch m.selectedSessionType() {
	case sessionTypeNewPR:
		req.Title = m.titleInput.Value()
	case sessionTypeExistingPR:
		req.Title = m.selectedPR.Title
		req.PrNumber = &m.selectedPR.Number
	case sessionTypePlanFeature:
		// Use the first line of the plan as the title, or a default.
		plan := m.planInput.Value()
		req.Plan = plan
		firstLine := strings.SplitN(plan, "\n", 2)[0]
		if len(firstLine) > 72 {
			firstLine = firstLine[:69] + "..."
		}
		req.Title = firstLine
	default:
		req.Title = "New session"
	}

	return createSession(m.client, m.ctx, req)
}

// Cancelled returns true if the user cancelled the wizard.
func (m NewSessionModel) Cancelled() bool { return m.cancel }

// Done returns true if session creation succeeded.
func (m NewSessionModel) Done() bool { return m.done }

// CreatedSession returns the session created by the wizard, or nil.
func (m NewSessionModel) CreatedSession() *pb.Session { return m.createdSess }

func (m NewSessionModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.step == stepCreating {
		return tea.NewView(
			lipgloss.NewStyle().Padding(1, 2).Render("Creating session..."),
		)
	}

	if m.done && m.createdSess != nil {
		return tea.NewView(
			lipgloss.NewStyle().Padding(1, 2).Foreground(colorGreen).Render("Session created!") + "\n" +
				lipgloss.NewStyle().Padding(0, 2).Render(
					fmt.Sprintf("  ID:     %s\n  Title:  %s\n  Branch: %s",
						m.createdSess.Id, m.createdSess.Title, m.createdSess.BranchName)),
		)
	}

	var b strings.Builder
	bold := lipgloss.NewStyle().Bold(true)
	if m.step == stepRepoSelect {
		b.WriteString(styleTitle.Render("New Session"))
	} else {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			"Starting a " + bold.Render("new session") + " for " + bold.Render(m.selectedRepo.DisplayName)))
	}
	b.WriteString("\n")

	switch m.step {
	case stepRepoSelect:
		m.viewRepoSelect(&b)
	case stepSessionType:
		m.viewSessionType(&b)
	case stepPRSelect:
		m.viewPRSelect(&b)
	case stepTitleInput:
		m.viewTitleInput(&b)
	case stepPlanInput:
		m.viewPlanInput(&b)
	case stepConfirmOverwrite:
		m.viewConfirmOverwrite(&b)
	default:
	}

	return tea.NewView(b.String())
}

func (m NewSessionModel) viewRepoSelect(b *strings.Builder) {
	if len(m.repos) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("No repositories registered."))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Add one with: boss repo add"))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[esc] back"))
		return
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Select a repository:"))
	b.WriteString("\n\n")

	for i, repo := range m.repos {
		cursor := "  "
		if i == m.repoCursor {
			cursor = "> "
		}
		line := fmt.Sprintf("%s%s  %s", cursor, repo.DisplayName, styleSubtle.Render(repo.LocalPath))
		if i == m.repoCursor {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	b.WriteString(styleActionBar.Render("[enter] select  [esc] cancel"))
}

func (m NewSessionModel) viewSessionType(b *strings.Builder) {
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Session type:"))
	b.WriteString("\n\n")

	type option struct {
		label string
		desc  string
	}
	options := []option{
		{"Create a new PR", "Start a fresh branch and pull request"},
		{"Work on an existing PR", "Attach to an open pull request"},
		{"Plan a feature", "Describe what to build, then launch Claude"},
		{"Execute a plan", "Coming soon"},
	}
	for i, opt := range options {
		cursor := "  "
		if i == m.sessionTypeCursor {
			cursor = "> "
		}
		line := cursor + opt.label
		if i == len(options)-1 {
			// Dim the placeholder option.
			line = cursor + styleSubtle.Render(opt.label+" — "+opt.desc)
		} else if i == m.sessionTypeCursor {
			line = styleSelected.Render(line) + "  " + styleSubtle.Render(opt.desc)
		} else {
			line = line + "  " + styleSubtle.Render(opt.desc)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	b.WriteString(styleActionBar.Render("[enter] select  [esc] cancel"))
}

func (m NewSessionModel) viewPRSelect(b *strings.Builder) {
	if m.prsErr != nil {
		b.WriteString(renderError(fmt.Sprintf("Failed to load PRs: %v", m.prsErr), m.width))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[esc] back"))
		return
	}

	if m.prs == nil {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Loading PRs..."))
		return
	}

	if len(m.prs) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("No open PRs found."))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[esc] back"))
		return
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Select a PR:"))
	b.WriteString("\n\n")

	for i, pr := range m.prs {
		cursor := "  "
		if i == m.prCursor {
			cursor = "> "
		}
		line := fmt.Sprintf("%s#%d  %s  %s", cursor, pr.Number, pr.Title, styleSubtle.Render(pr.HeadBranch))
		if i == m.prCursor {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	b.WriteString(styleActionBar.Render("[enter] select  [esc] cancel"))
}

func (m NewSessionModel) viewTitleInput(b *strings.Builder) {
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Session title:"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(m.titleInput.View()))
	b.WriteString("\n")
	b.WriteString(styleActionBar.Render("[enter] next  [esc] cancel"))
}

func (m NewSessionModel) viewPlanInput(b *strings.Builder) {
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("What would you like to work on?"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(m.planInput.View()))
	b.WriteString("\n")
	b.WriteString(styleActionBar.Render("[ctrl+d] next  [esc] cancel"))
}

func (m NewSessionModel) viewConfirmOverwrite(b *strings.Builder) {
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorYellow).Render(
		"A branch with this name already exists."))
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
		"Remove the old branch and create a new session?"))
	b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
}
