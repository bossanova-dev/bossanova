package views

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// sessionType identifies the kind of session to create.
type sessionType int

const (
	sessionTypeQuickChat   sessionType = iota // Quick chat in base folder
	sessionTypeNewPR                          // Create a new PR
	sessionTypeExistingPR                     // Work on an existing PR
	sessionTypePlanFeature                    // Plan a feature
	sessionTypeExecutePlan                    // Execute a plan (placeholder)
)

// newSessionPhase tracks the current phase of the wizard.
type newSessionPhase int

const (
	newSessionPhaseLoading    newSessionPhase = iota // Fetching repos
	newSessionPhaseRepoSelect                        // Table-based repo picker
	newSessionPhaseTypeSelect                        // Table-based session type picker
	newSessionPhaseForm                              // Main huh form active
	newSessionPhaseCreating                          // Waiting for CreateSession RPC
	newSessionPhaseDone                              // Terminal
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

// sessionTypeOption defines a row in the session-type selection table.
var sessionTypeOptions = []struct {
	label string
	desc  string
	typ   sessionType
}{
	{"Quick chat", "Work directly in the repo's base folder", sessionTypeQuickChat},
	{"Create a new PR", "Start a fresh branch and pull request", sessionTypeNewPR},
	{"Work on an existing PR", "Attach to an open pull request", sessionTypeExistingPR},
	{"Plan a feature", "Describe what to build, then launch Claude", sessionTypePlanFeature},
}

// formData holds huh form-bound values on the heap so that Value() pointers
// remain valid across bubbletea value-receiver copies of NewSessionModel.
type formData struct {
	selectedPRIdx int
	title         string
	plan          string
}

// NewSessionModel is the multi-step wizard for creating a new coding session.
type NewSessionModel struct {
	client client.BossClient
	ctx    context.Context

	phase  newSessionPhase
	err    error
	done   bool
	cancel bool

	// Data
	repos []*pb.Repo
	prs   []*pb.PRSummary

	// Form-bound values (heap-allocated for stable pointers)
	selectedRepoID string
	selectedType   sessionType
	fd             *formData

	// Async / conflict state
	createdSess         *pb.Session
	forceBranch         bool
	confirmingOverwrite bool

	// Tables
	repoTable table.Model
	typeTable table.Model

	// Form
	form *huh.Form

	// Layout
	width  int
	height int
}

// NewNewSessionModel creates a NewSessionModel wired to the daemon client.
func NewNewSessionModel(c client.BossClient, ctx context.Context) NewSessionModel {
	return NewSessionModel{
		client: c,
		ctx:    ctx,
		phase:  newSessionPhaseLoading,
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

func (m *NewSessionModel) buildRepoTable() {
	names := make([]string, len(m.repos))
	paths := make([]string, len(m.repos))
	for i, r := range m.repos {
		names[i] = r.DisplayName
		paths[i] = r.LocalPath
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "NAME", Width: maxColWidth("NAME", names, 30) + tableColumnSep},
		{Title: "PATH", Width: maxColWidth("PATH", paths, 60) + tableColumnSep},
	}

	rows := make([]table.Row, len(m.repos))
	for i := range m.repos {
		indicator := ""
		if i == 0 {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, names[i], paths[i]}
	}

	m.repoTable = newBossTable(cols, rows, m.repoTableHeight())
	m.repoTable.SetWidth(columnsWidth(cols))
}

// repoTableHeight returns the height for the repo selection table.
func (m NewSessionModel) repoTableHeight() int {
	return clampedTableHeight(len(m.repos), m.height, 6) // header + gaps + action bar
}

func (m *NewSessionModel) buildTypeTable() {
	cols := []table.Column{
		cursorColumn,
		{Title: "", Width: 24 + tableColumnSep},
		{Title: "", Width: 46 + tableColumnSep},
	}
	rows := make([]table.Row, len(sessionTypeOptions))
	for i, opt := range sessionTypeOptions {
		indicator := ""
		if i == 0 {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, opt.label, styleSubtle.Render(opt.desc)}
	}
	m.typeTable = newBossTable(cols, rows, len(sessionTypeOptions)+1)
	m.typeTable.SetWidth(columnsWidth(cols))
}

func (m *NewSessionModel) buildForm() {
	m.fd = &formData{}

	switch m.selectedType {
	case sessionTypeNewPR:
		m.form = huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title("Session title").
					Placeholder("What are you working on?").
					Value(&m.fd.title).
					Validate(func(s string) error {
						if strings.TrimSpace(s) == "" {
							return fmt.Errorf("title is required")
						}
						return nil
					}),
			),
		).WithTheme(bossHuhTheme()).WithShowHelp(false).WithWidth(70)

	case sessionTypePlanFeature:
		m.form = huh.NewForm(
			huh.NewGroup(
				huh.NewText().
					Title("What would you like to work on?").
					Placeholder("Describe what Claude should implement...").
					Lines(8).
					Value(&m.fd.plan).
					ExternalEditor(false).
					Validate(func(s string) error {
						if strings.TrimSpace(s) == "" {
							return fmt.Errorf("plan is required")
						}
						return nil
					}),
			),
		).WithTheme(bossHuhTheme()).WithShowHelp(false).WithWidth(70)

	case sessionTypeQuickChat, sessionTypeExistingPR, sessionTypeExecutePlan:
		// No form needed for these types.
	}
}

func (m NewSessionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.phase == newSessionPhaseRepoSelect {
			m.repoTable.SetHeight(m.repoTableHeight())
			m.repoTable.SetWidth(msg.Width)
		}
		return m, nil

	case reposMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.repos = msg.repos
		slices.SortFunc(m.repos, func(a, b *pb.Repo) int {
			return strings.Compare(strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName))
		})
		if len(m.repos) == 1 {
			m.selectedRepoID = m.repos[0].Id
			m.phase = newSessionPhaseTypeSelect
			m.buildTypeTable()
			return m, nil
		}
		m.phase = newSessionPhaseRepoSelect
		m.buildRepoTable()
		return m, nil

	case prsMsg:
		m.prs = msg.prs
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		if len(m.prs) == 0 {
			m.err = fmt.Errorf("no open PRs found")
			return m, nil
		}
		// Build a selection form for PRs.
		prOpts := make([]huh.Option[int], len(m.prs))
		for i, pr := range m.prs {
			prOpts[i] = huh.NewOption(
				fmt.Sprintf("#%d  %s  %s", pr.Number, pr.Title, styleSubtle.Render(pr.HeadBranch)),
				i,
			)
		}
		if m.fd == nil {
			m.fd = &formData{}
		}
		m.fd.selectedPRIdx = 0
		m.form = huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[int]().
					Title("Select a PR").
					Options(prOpts...).
					Value(&m.fd.selectedPRIdx),
			),
		).WithTheme(bossHuhTheme()).WithShowHelp(false).WithWidth(70)
		m.phase = newSessionPhaseForm
		return m, m.form.Init()

	case sessionCreatedMsg:
		if msg.err != nil {
			var connectErr *connect.Error
			if errors.As(msg.err, &connectErr) && connectErr.Code() == connect.CodeAlreadyExists {
				m.confirmingOverwrite = true
				m.phase = newSessionPhaseForm
				m.err = nil
				return m, nil
			}
			m.err = msg.err
			m.phase = newSessionPhaseForm
			return m, nil
		}
		m.createdSess = msg.session
		m.done = true
		return m, nil

	case tea.KeyMsg:
		if m.confirmingOverwrite {
			return m.updateConfirmOverwrite(msg)
		}

		if m.phase == newSessionPhaseRepoSelect {
			switch msg.String() {
			case "esc", "q":
				m.cancel = true
				return m, nil
			case "enter":
				idx := m.repoTable.Cursor()
				if idx >= 0 && idx < len(m.repos) {
					m.selectedRepoID = m.repos[idx].Id
					m.phase = newSessionPhaseTypeSelect
					m.buildTypeTable()
				}
				return m, nil
			}

			var cmd tea.Cmd
			m.repoTable, cmd = m.repoTable.Update(msg)
			updateCursorColumn(&m.repoTable)
			return m, cmd
		}

		if m.phase == newSessionPhaseTypeSelect {
			switch msg.String() {
			case "esc":
				// Go back to repo select if multiple repos, otherwise cancel.
				if len(m.repos) > 1 {
					m.phase = newSessionPhaseRepoSelect
					return m, nil
				}
				m.cancel = true
				return m, nil
			case "enter":
				idx := m.typeTable.Cursor()
				m.selectedType = sessionTypeOptions[idx].typ
				return m.advanceFromTypeSelect()
			}

			var cmd tea.Cmd
			m.typeTable, cmd = m.typeTable.Update(msg)
			updateCursorColumn(&m.typeTable)
			return m, cmd
		}

		switch msg.String() {
		case "esc":
			m.cancel = true
			return m, nil
		}
	}

	// Delegate to form.
	if m.form != nil && m.phase == newSessionPhaseForm && !m.confirmingOverwrite {
		_, cmd := m.form.Update(msg)

		if m.form.State == huh.StateAborted {
			m.cancel = true
			return m, nil
		}

		if m.form.State == huh.StateCompleted {
			return m.handleFormCompleted()
		}

		return m, cmd
	}

	return m, nil
}

func (m *NewSessionModel) advanceFromTypeSelect() (tea.Model, tea.Cmd) {
	switch m.selectedType {
	case sessionTypeQuickChat:
		// No form needed — create session directly.
		return *m, m.startCreating()
	case sessionTypeExistingPR:
		// Fetch PRs, then show PR selector form.
		m.phase = newSessionPhaseLoading
		return *m, fetchPRs(m.client, m.ctx, m.selectedRepoID)
	case sessionTypeNewPR, sessionTypePlanFeature:
		m.phase = newSessionPhaseForm
		m.buildForm()
		return *m, m.form.Init()
	default:
		m.cancel = true
		return *m, nil
	}
}

func (m *NewSessionModel) handleFormCompleted() (tea.Model, tea.Cmd) {
	// PR selection, title input, or plan input completed — proceed to create.
	return *m, m.startCreating()
}

func (m NewSessionModel) updateConfirmOverwrite(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		m.confirmingOverwrite = false
		m.forceBranch = true
		return m, m.startCreating()
	case "n", "N", "esc":
		m.confirmingOverwrite = false
		m.cancel = true
		return m, nil
	}
	return m, nil
}

func (m *NewSessionModel) selectedRepo() *pb.Repo {
	for _, r := range m.repos {
		if r.Id == m.selectedRepoID {
			return r
		}
	}
	return nil
}

// startCreating builds a CreateSessionRequest and fires the RPC.
func (m *NewSessionModel) startCreating() tea.Cmd {
	m.phase = newSessionPhaseCreating
	repo := m.selectedRepo()
	if repo == nil {
		m.err = fmt.Errorf("no repository selected")
		return nil
	}

	req := &pb.CreateSessionRequest{
		RepoId:      repo.Id,
		BaseBranch:  repo.DefaultBaseBranch,
		ForceBranch: m.forceBranch,
	}

	switch m.selectedType {
	case sessionTypeQuickChat:
		req.Title = "Quick chat"
	case sessionTypeNewPR:
		req.Title = m.fd.title
	case sessionTypeExistingPR:
		if m.fd.selectedPRIdx >= 0 && m.fd.selectedPRIdx < len(m.prs) {
			pr := m.prs[m.fd.selectedPRIdx]
			req.Title = pr.Title
			req.PrNumber = &pr.Number
		}
	case sessionTypePlanFeature:
		plan := m.fd.plan
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
	if m.err != nil && !m.confirmingOverwrite {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.phase == newSessionPhaseLoading {
		return tea.NewView(
			lipgloss.NewStyle().Padding(1, 2).Render("Loading..."),
		)
	}

	if m.phase == newSessionPhaseRepoSelect {
		var b strings.Builder
		b.WriteString(styleTitle.Render("Select a repository"))
		b.WriteString("\n\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.repoTable.View()))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[enter] select  [esc] back"))
		return tea.NewView(b.String())
	}

	if m.phase == newSessionPhaseTypeSelect {
		var b strings.Builder
		b.WriteString(m.headerView())
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.typeTable.View()))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[enter] select  [esc] back"))
		return tea.NewView(b.String())
	}

	if m.phase == newSessionPhaseCreating {
		return tea.NewView(
			lipgloss.NewStyle().Padding(1, 2).Render("Creating a new session..."),
		)
	}

	if m.done && m.createdSess != nil {
		return tea.NewView(
			lipgloss.NewStyle().Padding(1, 2).Foreground(colorSuccess).Render("Session created!") + "\n" +
				lipgloss.NewStyle().Padding(0, 2).Render(
					fmt.Sprintf("  ID:     %s\n  Title:  %s\n  Branch: %s",
						m.createdSess.Id, m.createdSess.Title, m.createdSess.BranchName)),
		)
	}

	if m.confirmingOverwrite {
		var b strings.Builder
		b.WriteString(m.headerView())
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(
			"A branch with this name already exists."))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			"Remove the old branch and create a new session?"))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
		return tea.NewView(b.String())
	}

	if m.form != nil {
		var b strings.Builder
		b.WriteString(m.headerView())
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().PaddingLeft(2).Render(m.form.View()))
		return tea.NewView(b.String())
	}

	return tea.NewView("")
}

func (m NewSessionModel) headerView() string {
	repo := m.selectedRepo()
	if repo == nil {
		return ""
	}
	bold := lipgloss.NewStyle().Bold(true)
	return lipgloss.NewStyle().Padding(0, 2).Render(
		"Starting a "+bold.Render("new session")+" for "+bold.Render(repo.DisplayName)) + "\n"
}
