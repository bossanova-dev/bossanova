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
	sessionTypeQuickChat    sessionType = iota // Quick chat in base folder
	sessionTypeNewPR                           // Create a new PR
	sessionTypeExistingPR                      // Work on an existing PR
	sessionTypeExecutePlan                     // Execute a plan (placeholder)
	sessionTypeLinearTicket                    // Work on a Linear ticket
)

// newSessionPhase tracks the current phase of the wizard.
type newSessionPhase int

const (
	newSessionPhaseLoading     newSessionPhase = iota // Fetching repos
	newSessionPhaseRepoSelect                         // Table-based repo picker
	newSessionPhaseTypeSelect                         // Table-based session type picker
	newSessionPhasePRSelect                           // Table-based PR picker
	newSessionPhaseIssueSelect                        // Table-based issue picker (Linear)
	newSessionPhaseForm                               // Main huh form active
	newSessionPhaseCreating                           // Waiting for CreateSession RPC
	newSessionPhaseDone                               // Terminal
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

// issuesMsg carries the result of a ListTrackerIssues RPC call.
type issuesMsg struct {
	issues []*pb.TrackerIssue
	err    error
}

// createSessionStreamMsg carries the opened stream or error.
type createSessionStreamMsg struct {
	stream client.CreateSessionStream
	err    error
}

// setupScriptLineMsg carries a single line of setup script output.
type setupScriptLineMsg struct {
	text string
}

// streamSessionCreatedMsg carries the final session from the stream.
type streamSessionCreatedMsg struct {
	session *pb.Session
}

// streamErrorMsg carries an error from the stream.
type streamErrorMsg struct {
	err error
}

// sessionTypeOption defines a row in the session-type selection table.
type sessionTypeOption struct {
	label string
	desc  string
	typ   sessionType
}

// buildSessionTypeOptions returns available session types based on repo configuration.
func (m *NewSessionModel) buildSessionTypeOptions() []sessionTypeOption {
	opts := []sessionTypeOption{
		{"Create a new PR", "Start a fresh branch and pull request", sessionTypeNewPR},
		{"Work on an existing PR", "Attach to an open pull request", sessionTypeExistingPR},
		{"Quick chat", "Work directly in the repo's base folder", sessionTypeQuickChat},
	}

	// Add Linear ticket option if repo has Linear API key configured
	repo := m.selectedRepo()
	if repo != nil && repo.LinearApiKey != "" {
		// Insert before Quick chat
		opts = append(opts[:2], append([]sessionTypeOption{
			{"Work on a Linear ticket", "Pick a ticket from your Linear board", sessionTypeLinearTicket},
		}, opts[2:]...)...)
	}

	return opts
}

// formData holds huh form-bound values on the heap so that Value() pointers
// remain valid across bubbletea value-receiver copies of NewSessionModel.
type formData struct {
	title string
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

	// Linear issues
	trackerIssues   []*pb.TrackerIssue
	issueTable      table.Model
	issueTableReady bool
	issueErr        error
	selectedIssue   *pb.TrackerIssue

	// Form-bound values (heap-allocated for stable pointers)
	selectedRepoID string
	selectedType   sessionType
	fd             *formData

	// Async / conflict state
	createdSess         *pb.Session
	forceBranch         bool
	confirmingOverwrite bool

	// Streaming create session
	createStream client.CreateSessionStream
	setupLines   []string

	// Tables
	repoTable table.Model
	typeTable table.Model
	prTable   table.Model

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

func fetchIssues(c client.BossClient, ctx context.Context, repoID string) tea.Cmd {
	return func() tea.Msg {
		issues, err := c.ListTrackerIssues(ctx, repoID)
		return issuesMsg{issues: issues, err: err}
	}
}

func openCreateStream(c client.BossClient, ctx context.Context, req *pb.CreateSessionRequest) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.CreateSession(ctx, req)
		return createSessionStreamMsg{stream: stream, err: err}
	}
}

func readNextStreamMsg(stream client.CreateSessionStream) tea.Cmd {
	return func() tea.Msg {
		if !stream.Receive() {
			if err := stream.Err(); err != nil {
				return streamErrorMsg{err: err}
			}
			return streamErrorMsg{err: fmt.Errorf("stream ended unexpectedly")}
		}
		msg := stream.Msg()
		switch e := msg.Event.(type) {
		case *pb.CreateSessionResponse_SetupOutput:
			return setupScriptLineMsg{text: e.SetupOutput.GetText()}
		case *pb.CreateSessionResponse_SessionCreated:
			return streamSessionCreatedMsg{session: e.SessionCreated.GetSession()}
		default:
			return streamErrorMsg{err: fmt.Errorf("unexpected stream event")}
		}
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
	return clampedTableHeight(len(m.repos), m.height, bannerOverhead+6) // header + gaps + action bar
}

func (m *NewSessionModel) buildTypeTable() {
	cols := []table.Column{
		cursorColumn,
		{Title: "", Width: 24 + tableColumnSep},
		{Title: "", Width: 46 + tableColumnSep},
	}
	opts := m.buildSessionTypeOptions()
	rows := make([]table.Row, len(opts))
	for i, opt := range opts {
		indicator := ""
		if i == 0 {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, opt.label, styleSubtle.Render(opt.desc)}
	}
	m.typeTable = newBossTable(cols, rows, len(opts)+1)
	m.typeTable.SetWidth(columnsWidth(cols))
}

func (m *NewSessionModel) buildPRTable() {
	numbers := make([]string, len(m.prs))
	titles := make([]string, len(m.prs))
	branches := make([]string, len(m.prs))
	for i, pr := range m.prs {
		numbers[i] = fmt.Sprintf("#%d", pr.Number)
		titles[i] = pr.Title
		branches[i] = pr.HeadBranch
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "PR", Width: maxColWidth("PR", numbers, 10) + tableColumnSep},
		{Title: "TITLE", Width: maxColWidth("TITLE", titles, 50) + tableColumnSep},
		{Title: "BRANCH", Width: maxColWidth("BRANCH", branches, 30) + tableColumnSep},
	}

	rows := make([]table.Row, len(m.prs))
	for i := range m.prs {
		indicator := ""
		if i == 0 {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, numbers[i], titles[i], styleSubtle.Render(branches[i])}
	}

	m.prTable = newBossTable(cols, rows, m.prTableHeight())
	m.prTable.SetWidth(columnsWidth(cols))
}

// prTableHeight returns the height for the PR selection table.
func (m NewSessionModel) prTableHeight() int {
	return clampedTableHeight(len(m.prs), m.height, bannerOverhead+6) // header + gaps + action bar
}

func (m *NewSessionModel) buildIssueTable() {
	ids := make([]string, len(m.trackerIssues))
	titles := make([]string, len(m.trackerIssues))
	states := make([]string, len(m.trackerIssues))
	for i, issue := range m.trackerIssues {
		ids[i] = issue.ExternalId
		titles[i] = issue.Title
		states[i] = issue.State
	}

	cols := []table.Column{
		cursorColumn,
		{Title: "ID", Width: maxColWidth("ID", ids, 10) + tableColumnSep},
		{Title: "TITLE", Width: maxColWidth("TITLE", titles, 50) + tableColumnSep},
		{Title: "STATE", Width: maxColWidth("STATE", states, 15) + tableColumnSep},
	}

	rows := make([]table.Row, len(m.trackerIssues))
	for i := range m.trackerIssues {
		indicator := ""
		if i == 0 {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, ids[i], titles[i], styleSubtle.Render(states[i])}
	}

	m.issueTable = newBossTable(cols, rows, m.issueTableHeight())
	m.issueTable.SetWidth(columnsWidth(cols))
}

// issueTableHeight returns the height for the issue selection table.
func (m NewSessionModel) issueTableHeight() int {
	return clampedTableHeight(len(m.trackerIssues), m.height, bannerOverhead+6) // header + gaps + action bar
}

func (m *NewSessionModel) buildForm() {
	if m.fd == nil {
		m.fd = &formData{}
	}

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
		if m.phase == newSessionPhasePRSelect {
			m.prTable.SetHeight(m.prTableHeight())
			m.prTable.SetWidth(msg.Width)
		}
		if m.phase == newSessionPhaseIssueSelect {
			m.issueTable.SetHeight(m.issueTableHeight())
			m.issueTable.SetWidth(msg.Width)
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
		m.phase = newSessionPhasePRSelect
		m.buildPRTable()
		return m, nil

	case issuesMsg:
		m.trackerIssues = msg.issues
		if msg.err != nil {
			m.issueErr = msg.err
			m.err = msg.err
			return m, nil
		}
		if len(m.trackerIssues) == 0 {
			m.err = fmt.Errorf("no issues found in Linear")
			return m, nil
		}
		m.phase = newSessionPhaseIssueSelect
		m.buildIssueTable()
		m.issueTableReady = true
		return m, nil

	case createSessionStreamMsg:
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
		m.createStream = msg.stream
		return m, readNextStreamMsg(m.createStream)

	case setupScriptLineMsg:
		m.setupLines = append(m.setupLines, msg.text)
		return m, readNextStreamMsg(m.createStream)

	case streamSessionCreatedMsg:
		if m.createStream != nil {
			_ = m.createStream.Close()
		}
		m.createdSess = msg.session
		m.done = true
		return m, nil

	case streamErrorMsg:
		if m.createStream != nil {
			_ = m.createStream.Close()
		}
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

	case tea.KeyMsg:
		if m.confirmingOverwrite {
			return m.updateConfirmOverwrite(msg)
		}

		if m.phase == newSessionPhaseRepoSelect {
			switch msg.String() {
			case "esc":
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
				opts := m.buildSessionTypeOptions()
				m.selectedType = opts[idx].typ
				return m.advanceFromTypeSelect()
			}

			var cmd tea.Cmd
			m.typeTable, cmd = m.typeTable.Update(msg)
			updateCursorColumn(&m.typeTable)
			return m, cmd
		}

		if m.phase == newSessionPhasePRSelect {
			switch msg.String() {
			case "esc":
				m.phase = newSessionPhaseTypeSelect
				return m, nil
			case "enter":
				idx := m.prTable.Cursor()
				if idx >= 0 && idx < len(m.prs) {
					return m, m.startCreating()
				}
				return m, nil
			}

			var cmd tea.Cmd
			m.prTable, cmd = m.prTable.Update(msg)
			updateCursorColumn(&m.prTable)
			return m, cmd
		}

		if m.phase == newSessionPhaseIssueSelect {
			switch msg.String() {
			case "esc":
				m.phase = newSessionPhaseTypeSelect
				return m, nil
			case "enter":
				idx := m.issueTable.Cursor()
				if idx >= 0 && idx < len(m.trackerIssues) {
					m.selectedIssue = m.trackerIssues[idx]
					return m, m.startCreating()
				}
				return m, nil
			}

			var cmd tea.Cmd
			m.issueTable, cmd = m.issueTable.Update(msg)
			updateCursorColumn(&m.issueTable)
			return m, cmd
		}

		switch msg.String() {
		case "esc":
			if m.phase == newSessionPhaseForm {
				m.phase = newSessionPhaseTypeSelect
				m.form = nil
				m.err = nil
				m.fd = nil
				return m, nil
			}
			m.cancel = true
			return m, nil
		}
	}

	// Delegate to form.
	if m.form != nil && m.phase == newSessionPhaseForm && !m.confirmingOverwrite {
		_, cmd := m.form.Update(msg)

		if m.form.State == huh.StateAborted {
			m.phase = newSessionPhaseTypeSelect
			m.form = nil
			m.err = nil
			m.fd = nil
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
		// Fetch PRs, then show PR selector table.
		m.phase = newSessionPhaseLoading
		return *m, fetchPRs(m.client, m.ctx, m.selectedRepoID)
	case sessionTypeLinearTicket:
		// Fetch Linear issues, then show issue selector table.
		m.phase = newSessionPhaseLoading
		return *m, fetchIssues(m.client, m.ctx, m.selectedRepoID)
	case sessionTypeNewPR:
		m.phase = newSessionPhaseForm
		m.buildForm()
		return *m, m.form.Init()
	default:
		m.cancel = true
		return *m, nil
	}
}

func (m *NewSessionModel) handleFormCompleted() (tea.Model, tea.Cmd) {
	// Title input or plan input completed — proceed to create.
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
		m.err = nil
		m.phase = newSessionPhaseForm
		m.buildForm()
		return m, m.form.Init()
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
	m.setupLines = nil // Clear setup output from any previous attempt
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
		req.QuickChat = true
	case sessionTypeNewPR:
		req.Title = m.fd.title
	case sessionTypeExistingPR:
		idx := m.prTable.Cursor()
		if idx >= 0 && idx < len(m.prs) {
			pr := m.prs[idx]
			req.Title = pr.Title
			req.PrNumber = &pr.Number
		}
	case sessionTypeLinearTicket:
		if m.selectedIssue != nil {
			issue := m.selectedIssue
			req.Title = fmt.Sprintf("[%s] %s", issue.ExternalId, issue.Title)
			req.Plan = issue.Description
			if issue.PrNumber > 0 {
				// Existing PR - attach to it
				prNum := issue.PrNumber
				req.PrNumber = &prNum
			} else {
				// New branch using Linear's suggested name
				if issue.BranchName != "" {
					req.BranchName = &issue.BranchName
				}
			}
		}
	default:
		req.Title = "New session"
	}

	return openCreateStream(m.client, m.ctx, req)
}

// Cancelled returns true if the user cancelled the wizard.
func (m NewSessionModel) Cancelled() bool { return m.cancel }

// Done returns true if session creation succeeded.
func (m NewSessionModel) Done() bool { return m.done }

// CreatedSession returns the session created by the wizard, or nil.
func (m NewSessionModel) CreatedSession() *pb.Session { return m.createdSess }

func (m NewSessionModel) View() tea.View {
	if m.err != nil && !m.confirmingOverwrite {
		var b strings.Builder
		b.WriteString(renderError(fmt.Sprintf("Error: %v", m.err), m.width))
		if len(m.setupLines) > 0 {
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().PaddingLeft(2).Foreground(colorWarning).Render("Setup script output:"))
			b.WriteString("\n")
			for _, line := range m.setupLines {
				b.WriteString(lipgloss.NewStyle().PaddingLeft(4).Foreground(lipgloss.Color("8")).Render(line))
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[esc] back"}))
		return tea.NewView(b.String())
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
		b.WriteString(actionBar([]string{"[enter] select"}, []string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	if m.phase == newSessionPhaseTypeSelect {
		var b strings.Builder
		b.WriteString(m.headerView())
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.typeTable.View()))
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[enter] select"}, []string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	if m.phase == newSessionPhasePRSelect {
		var b strings.Builder
		b.WriteString(m.headerView())
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.prTable.View()))
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[enter] select"}, []string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	if m.phase == newSessionPhaseIssueSelect {
		var b strings.Builder
		b.WriteString(m.headerView())
		if m.issueErr != nil {
			b.WriteString(renderError(fmt.Sprintf("Error loading issues: %v", m.issueErr), m.width))
		} else if !m.issueTableReady {
			b.WriteString(lipgloss.NewStyle().Padding(1, 2).Render("Loading Linear issues..."))
		} else {
			b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.issueTable.View()))
			b.WriteString("\n")
			b.WriteString(actionBar([]string{"[enter] select"}, []string{"[esc] back"}))
		}
		return tea.NewView(b.String())
	}

	if m.phase == newSessionPhaseCreating {
		var b strings.Builder
		if len(m.setupLines) > 0 {
			b.WriteString(lipgloss.NewStyle().Padding(1, 2).Render("Running setup script..."))
			b.WriteString("\n")
			// Show last 10 lines of setup output.
			start := 0
			if len(m.setupLines) > 10 {
				start = len(m.setupLines) - 10
			}
			for _, line := range m.setupLines[start:] {
				b.WriteString(lipgloss.NewStyle().PaddingLeft(4).Foreground(lipgloss.Color("8")).Render(line))
				b.WriteString("\n")
			}
		} else {
			b.WriteString(lipgloss.NewStyle().Padding(1, 2).Render("Creating a new session..."))
		}
		return tea.NewView(b.String())
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
