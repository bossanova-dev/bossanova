package views

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Wizard steps for new session creation.
type wizardStep int

const (
	stepRepoSelect wizardStep = iota
	stepPRMode
	stepPRSelect
	stepPlanInput
	stepTitleInput
	stepConfirm
)

// prMode indicates whether to create a new session or attach to an existing PR.
type prMode int

const (
	prModeNew prMode = iota
	prModeExisting
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
	client *client.Client
	ctx    context.Context

	step   wizardStep
	err    error
	done   bool
	cancel bool

	// Step 1: Repo select
	repos      []*pb.Repo
	repoCursor int

	// Step 2: PR mode
	prModeChoice prMode

	// Step 3: PR select (existing PR)
	prs      []*pb.PRSummary
	prCursor int
	prsErr   error

	// Step 4: Plan input
	planInput textarea.Model

	// Step 5: Title input
	titleInput textinput.Model

	// Collected values
	selectedRepo *pb.Repo
	selectedPR   *pb.PRSummary
	createdSess  *pb.Session
}

// NewNewSessionModel creates a NewSessionModel wired to the daemon client.
func NewNewSessionModel(c *client.Client, ctx context.Context) NewSessionModel {
	ti := textinput.New()
	ti.Placeholder = "Session title"
	ti.SetWidth(50)

	ta := textarea.New()
	ta.Placeholder = "Describe what Claude should implement..."
	ta.SetWidth(60)
	ta.SetHeight(8)

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

func fetchRepos(c *client.Client, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		repos, err := c.ListRepos(ctx)
		return reposMsg{repos: repos, err: err}
	}
}

func fetchPRs(c *client.Client, ctx context.Context, repoID string) tea.Cmd {
	return func() tea.Msg {
		prs, err := c.ListRepoPRs(ctx, repoID)
		return prsMsg{prs: prs, err: err}
	}
}

func createSession(c *client.Client, ctx context.Context, req *pb.CreateSessionRequest) tea.Cmd {
	return func() tea.Msg {
		sess, err := c.CreateSession(ctx, req)
		return sessionCreatedMsg{session: sess, err: err}
	}
}

func (m NewSessionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case reposMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.repos = msg.repos
		if len(m.repos) == 1 {
			// Auto-select the only repo.
			m.selectedRepo = m.repos[0]
			m.step = stepPRMode
		}
		return m, nil

	case prsMsg:
		m.prs = msg.prs
		m.prsErr = msg.err
		return m, nil

	case sessionCreatedMsg:
		if msg.err != nil {
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
		case stepPRMode:
			return m.updatePRMode(msg)
		case stepPRSelect:
			return m.updatePRSelect(msg)
		case stepTitleInput:
			return m.updateTitleInput(msg)
		case stepPlanInput:
			return m.updatePlanInput(msg)
		case stepConfirm:
			return m.updateConfirm(msg)
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
			m.step = stepPRMode
		}
	}
	return m, nil
}

func (m NewSessionModel) updatePRMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k", "down", "j":
		if m.prModeChoice == prModeNew {
			m.prModeChoice = prModeExisting
		} else {
			m.prModeChoice = prModeNew
		}
	case "enter":
		if m.prModeChoice == prModeNew {
			m.step = stepTitleInput
			return m, m.titleInput.Focus()
		}
		m.step = stepPRSelect
		return m, fetchPRs(m.client, m.ctx, m.selectedRepo.Id)
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
			m.step = stepPlanInput
			return m, m.planInput.Focus()
		}
	}
	return m, nil
}

func (m NewSessionModel) updateTitleInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		if m.titleInput.Value() != "" {
			m.titleInput.Blur()
			m.step = stepPlanInput
			return m, m.planInput.Focus()
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
			m.step = stepConfirm
		}
		return m, nil
	}
	// Pass through to textarea.
	var cmd tea.Cmd
	m.planInput, cmd = m.planInput.Update(msg)
	return m, cmd
}

func (m NewSessionModel) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		req := &pb.CreateSessionRequest{
			RepoId:     m.selectedRepo.Id,
			Plan:       m.planInput.Value(),
			BaseBranch: m.selectedRepo.DefaultBaseBranch,
		}
		if m.selectedPR != nil {
			req.Title = m.selectedPR.Title
			req.PrNumber = &m.selectedPR.Number
		} else {
			req.Title = m.titleInput.Value()
		}
		return m, createSession(m.client, m.ctx, req)
	case "n":
		m.cancel = true
	}
	return m, nil
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
			styleError.Render(fmt.Sprintf("Error: %v", m.err)) + "\n" +
				styleActionBar.Render("[esc] back"),
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
	b.WriteString(styleTitle.Render("New Session"))
	b.WriteString("\n")

	switch m.step {
	case stepRepoSelect:
		m.viewRepoSelect(&b)
	case stepPRMode:
		m.viewPRMode(&b)
	case stepPRSelect:
		m.viewPRSelect(&b)
	case stepTitleInput:
		m.viewTitleInput(&b)
	case stepPlanInput:
		m.viewPlanInput(&b)
	case stepConfirm:
		m.viewConfirm(&b)
	}

	return tea.NewView(b.String())
}

func (m NewSessionModel) viewRepoSelect(b *strings.Builder) {
	if len(m.repos) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("No repositories registered."))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Add one with: boss repo add"))
		b.WriteString("\n\n")
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

	b.WriteString("\n")
	b.WriteString(styleActionBar.Render("[enter] select  [esc] cancel"))
}

func (m NewSessionModel) viewPRMode(b *strings.Builder) {
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
		fmt.Sprintf("Repo: %s", styleSelected.Render(m.selectedRepo.DisplayName))))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Session type:"))
	b.WriteString("\n\n")

	options := []string{"New session (create fresh PR)", "Existing PR (attach to open PR)"}
	for i, opt := range options {
		cursor := "  "
		if i == int(m.prModeChoice) {
			cursor = "> "
		}
		line := cursor + opt
		if i == int(m.prModeChoice) {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(styleActionBar.Render("[enter] select  [esc] cancel"))
}

func (m NewSessionModel) viewPRSelect(b *strings.Builder) {
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
		fmt.Sprintf("Repo: %s", styleSelected.Render(m.selectedRepo.DisplayName))))
	b.WriteString("\n\n")

	if m.prsErr != nil {
		b.WriteString(styleError.Render(fmt.Sprintf("Failed to load PRs: %v", m.prsErr)))
		b.WriteString("\n\n")
		b.WriteString(styleActionBar.Render("[esc] back"))
		return
	}

	if m.prs == nil {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Loading PRs..."))
		return
	}

	if len(m.prs) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("No open PRs found."))
		b.WriteString("\n\n")
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

	b.WriteString("\n")
	b.WriteString(styleActionBar.Render("[enter] select  [esc] cancel"))
}

func (m NewSessionModel) viewTitleInput(b *strings.Builder) {
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
		fmt.Sprintf("Repo: %s", styleSelected.Render(m.selectedRepo.DisplayName))))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Session title:"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(m.titleInput.View()))
	b.WriteString("\n\n")
	b.WriteString(styleActionBar.Render("[enter] next  [esc] cancel"))
}

func (m NewSessionModel) viewPlanInput(b *strings.Builder) {
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
		fmt.Sprintf("Repo: %s", styleSelected.Render(m.selectedRepo.DisplayName))))
	if m.selectedPR != nil {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			fmt.Sprintf("PR:   #%d %s", m.selectedPR.Number, m.selectedPR.Title)))
	} else {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			fmt.Sprintf("Title: %s", m.titleInput.Value())))
	}
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Plan (Ctrl+D when done):"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(m.planInput.View()))
	b.WriteString("\n\n")
	b.WriteString(styleActionBar.Render("[ctrl+d] next  [esc] cancel"))
}

func (m NewSessionModel) viewConfirm(b *strings.Builder) {
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Confirm session:"))
	b.WriteString("\n\n")

	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
		fmt.Sprintf("  Repo:   %s", m.selectedRepo.DisplayName)))
	b.WriteString("\n")

	if m.selectedPR != nil {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			fmt.Sprintf("  PR:     #%d %s", m.selectedPR.Number, m.selectedPR.Title)))
	} else {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			fmt.Sprintf("  Title:  %s", m.titleInput.Value())))
	}
	b.WriteString("\n")

	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
		fmt.Sprintf("  Base:   %s", m.selectedRepo.DefaultBaseBranch)))
	b.WriteString("\n")

	plan := m.planInput.Value()
	if len(plan) > 80 {
		plan = plan[:77] + "..."
	}
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
		fmt.Sprintf("  Plan:   %s", plan)))
	b.WriteString("\n\n")

	b.WriteString(styleActionBar.Render("[y/enter] create  [n/esc] cancel"))
}
