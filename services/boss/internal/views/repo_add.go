package views

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
)

// --- Repo Add Wizard ---

// repoAddPhase tracks the current phase in the add-repo wizard.
type repoAddPhase int

const (
	repoAddPhaseSource          repoAddPhase = iota // Phase 1: table pick (open/clone)
	repoAddPhaseInput                               // Phase 2: path/URL input
	repoAddPhaseDetails                             // Phase 3: name + setup + confirm
	repoAddPhaseGitHubAppPrompt                     // Phase 4: confirm GitHub App install
	repoAddPhaseGitHubApp                           // Phase 5: GitHub App install follow-up
	repoAddPhaseDone                                // Terminal state
)

// sourceMode selects between open-project and clone flows.
const (
	sourceModeOpen  = 0
	sourceModeClone = 1
)

// repoRegisteredMsg carries the result of a RegisterRepo RPC call.
type repoRegisteredMsg struct {
	repo *pb.Repo
	err  error
}

// repoClonedMsg carries the result of a CloneAndRegisterRepo RPC call.
type repoClonedMsg struct {
	repo *pb.Repo
	err  error
}

// repoValidatedMsg carries the result of a ValidateRepoPath RPC call.
type repoValidatedMsg struct {
	resp *pb.ValidateRepoPathResponse
	err  error
}

type githubAppInstallURLMsg struct {
	url     string
	err     error
	attempt int
}

type githubAppBrowserOpenedMsg struct {
	url     string
	err     error
	attempt int
}

type githubAppReposMsg struct {
	repos   []*pb.GitHubAppRepoStatus
	err     error
	attempt int
}

type githubAppPollTickMsg struct {
	attempt int
}

// GitHubAppClient is the authenticated cloud subset used by the repo-add
// follow-up to start GitHub App setup and observe completion.
type GitHubAppClient interface {
	GetGitHubAppInstallURL(ctx context.Context, returnURL string) (string, error)
	ListGitHubAppRepos(ctx context.Context) ([]*pb.GitHubAppRepoStatus, error)
}

const githubAppInstallPollInterval = 3 * time.Second

// githubAppInstallMaxPolls bounds the automatic installation poll loop so a
// user who never completes the GitHub App install doesn't leave boss polling
// the cloud forever. At the default 3s interval this is ~5 minutes, after which
// the view pauses and offers to keep checking or skip.
const githubAppInstallMaxPolls = 100

var (
	openGitHubAppInstallURL              = openURLFunc
	githubAppInstallPollIntervalOverride time.Duration
)

func SetGitHubAppInstallPollIntervalOverride(interval time.Duration) {
	githubAppInstallPollIntervalOverride = interval
}

func githubAppPollIntervalValue() time.Duration {
	if githubAppInstallPollIntervalOverride > 0 {
		return githubAppInstallPollIntervalOverride
	}
	return githubAppInstallPollInterval
}

// sourceOptions defines the rows for the source-selection table.
var sourceOptions = []struct {
	label string
	desc  string
	mode  int
}{
	{"Open project", "Register an existing local repo", sourceModeOpen},
	{"Clone from URL", "Clone a repo and register it", sourceModeClone},
}

// repoAddFormData holds huh form-bound values on the heap so that Value()
// pointers remain valid across bubbletea value-receiver copies of RepoAddModel.
type repoAddFormData struct {
	gitURL    string
	clonePath string
	localPath string
	name      string
	setup     string
	confirm   bool
}

// RepoAddModel is the wizard for registering a new repository.
type RepoAddModel struct {
	client client.BossClient
	auth   *auth.Manager
	ctx    context.Context

	phase  repoAddPhase
	err    error
	done   bool
	cancel bool

	// Source selection table (phase 1)
	sourceTable table.Model

	sourceMode int

	// Form-bound values (heap-allocated for stable pointers)
	fd *repoAddFormData

	// Async state
	validating bool
	cloning    bool
	spinner    spinner.Model

	// Validation results
	isGithub           bool
	detectedBaseBranch string
	detectedOriginURL  string

	createdRepo *pb.Repo

	githubAppClient        GitHubAppClient
	githubAppAttempt       int
	githubAppInstallURL    string
	githubAppInstallErr    error
	githubAppInstallOpen   bool
	githubAppInstallNWO    string
	githubAppInstallReady  bool
	githubAppPollCount     int
	githubAppPollExhausted bool

	// Form (used for clone/open input fields + details phase)
	form *huh.Form

	// Layout
	width int
}

// NewRepoAddModel creates a RepoAddModel with sensible defaults.
func NewRepoAddModel(c client.BossClient, ctx context.Context) RepoAddModel {
	home, _ := os.UserHomeDir()

	m := RepoAddModel{
		client:             c,
		ctx:                ctx,
		phase:              repoAddPhaseSource,
		detectedBaseBranch: "main",
		spinner:            newStatusSpinner(),
		fd: &repoAddFormData{
			localPath: home + "/",
			name:      filepath.Base(home),
			confirm:   true,
		},
	}
	m.buildSourceTable()
	return m
}

func (m *RepoAddModel) SetGitHubAppInstall(authMgr *auth.Manager, c GitHubAppClient) {
	m.auth = authMgr
	m.githubAppClient = c
}

func (m *RepoAddModel) buildSourceTable() {
	cols := []table.Column{
		cursorColumn,
		{Title: "", Width: 14 + tableColumnSep},
		{Title: "", Width: 32 + tableColumnSep},
	}
	rows := make([]table.Row, len(sourceOptions))
	for i, opt := range sourceOptions {
		indicator := ""
		if i == 0 {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, opt.label, styleSubtle.Render(opt.desc)}
	}
	m.sourceTable = newBossTable(cols, rows, len(sourceOptions)+1)
	m.sourceTable.SetWidth(columnsWidth(cols))
}

func (m *RepoAddModel) buildInputForm() {
	if m.sourceMode == sourceModeClone {
		m.form = huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title("Git URL").
					Placeholder("https://github.com/user/repo.git").
					Value(&m.fd.gitURL).
					Validate(func(s string) error {
						if strings.TrimSpace(s) == "" {
							return fmt.Errorf("URL is required")
						}
						return nil
					}),
				huh.NewInput().
					Title("Clone path").
					Placeholder("Clone destination path").
					Value(&m.fd.clonePath).
					Validate(func(s string) error {
						if strings.TrimSpace(s) == "" {
							return fmt.Errorf("clone path is required")
						}
						return nil
					}),
			),
		).WithTheme(bossHuhTheme()).WithShowHelp(false).WithWidth(70)
	} else {
		m.form = huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title("Path").
					Placeholder("Repository path").
					Value(&m.fd.localPath).
					Validate(func(s string) error {
						if strings.TrimSpace(s) == "" {
							return fmt.Errorf("path is required")
						}
						return nil
					}),
			),
		).WithTheme(bossHuhTheme()).WithShowHelp(false).WithWidth(70)
	}
}

func (m *RepoAddModel) buildDetailsForm() {
	m.form = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Name").
				Placeholder("Display name").
				Value(&m.fd.name).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("name is required")
					}
					return nil
				}),
			huh.NewInput().
				Title("Setup command").
				Placeholder("Optional, e.g. make setup").
				Value(&m.fd.setup),
			huh.NewConfirm().
				Title("Add this repository?").
				Affirmative("Yes").
				Negative("No").
				Value(&m.fd.confirm),
		),
	).WithTheme(bossHuhTheme()).WithShowHelp(false).WithWidth(70)
}

func (m RepoAddModel) Init() tea.Cmd {
	if m.form != nil {
		return m.form.Init()
	}
	return nil
}

func (m RepoAddModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case repoRegisteredMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.createdRepo = msg.repo
		if m.shouldOfferGitHubAppInstall() {
			return m.promptGitHubAppInstall(), nil
		}
		m.done = true
		return m, nil

	case repoClonedMsg:
		m.cloning = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.createdRepo = msg.repo
		if m.shouldOfferGitHubAppInstall() {
			return m.promptGitHubAppInstall(), nil
		}
		m.done = true
		return m, nil

	case repoValidatedMsg:
		m.validating = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		if !msg.resp.IsValid {
			m.err = fmt.Errorf("%s", msg.resp.ErrorMessage)
			// Go back to input phase so user can fix path.
			m.phase = repoAddPhaseInput
			m.buildInputForm()
			return m, m.form.Init()
		}
		// Auto-populate fields from validation response.
		m.isGithub = msg.resp.IsGithub
		m.detectedOriginURL = msg.resp.OriginUrl
		if msg.resp.DefaultBranch != "" {
			m.detectedBaseBranch = msg.resp.DefaultBranch
		}
		if msg.resp.IsGithub {
			if nwo := vcs.GitHubNWO(msg.resp.OriginUrl); nwo != "" {
				m.fd.name = "@" + nwo
			} else {
				m.fd.name = filepath.Base(m.fd.localPath)
			}
		} else if n := parseRepoNameFromURL(msg.resp.OriginUrl); n != "" {
			m.fd.name = n
		} else {
			m.fd.name = filepath.Base(m.fd.localPath)
		}
		// Advance to details phase.
		m.phase = repoAddPhaseDetails
		m.buildDetailsForm()
		return m, m.form.Init()

	case githubAppInstallURLMsg:
		if msg.attempt != m.githubAppAttempt || m.phase != repoAddPhaseGitHubApp {
			return m, nil
		}
		if msg.err != nil {
			m.githubAppInstallErr = msg.err
			return m, nil
		}
		m.githubAppInstallURL = msg.url
		m.githubAppInstallOpen = true
		return m, tea.Batch(
			m.openGitHubAppInstall(msg.url, msg.attempt),
			m.githubAppPollTick(msg.attempt),
		)

	case githubAppBrowserOpenedMsg:
		if msg.attempt != m.githubAppAttempt || m.phase != repoAddPhaseGitHubApp {
			return m, nil
		}
		m.githubAppInstallURL = msg.url
		m.githubAppInstallErr = msg.err
		return m, nil

	case githubAppPollTickMsg:
		if msg.attempt != m.githubAppAttempt || m.phase != repoAddPhaseGitHubApp {
			return m, nil
		}
		return m, m.listGitHubAppRepos(msg.attempt)

	case githubAppReposMsg:
		if msg.attempt != m.githubAppAttempt || m.phase != repoAddPhaseGitHubApp {
			return m, nil
		}
		if msg.err != nil {
			m.githubAppInstallErr = msg.err
			cmd := m.nextGitHubAppPoll(msg.attempt)
			return m, cmd
		}
		if m.githubAppRepoInstalled(msg.repos) {
			m.githubAppInstallReady = true
			m.done = true
			return m, nil
		}
		cmd := m.nextGitHubAppPoll(msg.attempt)
		return m, cmd

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.phase == repoAddPhaseGitHubApp || m.cloning {
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		if m.phase == repoAddPhaseGitHubAppPrompt {
			switch msg.String() {
			case "esc":
				m.done = true
				return m, nil
			case "enter":
				return m.startGitHubAppInstall()
			}
			return m, nil
		}
		if m.phase == repoAddPhaseGitHubApp {
			switch msg.String() {
			case "esc":
				m.done = true
				return m, nil
			case "enter", "o":
				m.githubAppInstallErr = nil
				if m.githubAppPollExhausted {
					// Resume automatic polling and re-open the install page.
					m.githubAppPollExhausted = false
					m.githubAppPollCount = 0
					return m, tea.Batch(m.reopenGitHubAppInstallCmd(), m.githubAppPollTick(m.githubAppAttempt))
				}
				return m, m.reopenGitHubAppInstallCmd()
			}
			return m, nil
		}
		if msg.String() == "esc" {
			// If showing an error, go back to the input form with data preserved.
			if m.err != nil {
				m.err = nil
				m.phase = repoAddPhaseInput
				m.buildInputForm()
				return m, m.form.Init()
			}
			if m.phase == repoAddPhaseInput {
				// Go back to source selection.
				m.phase = repoAddPhaseSource
				m.form = nil
				return m, nil
			}
			if m.phase == repoAddPhaseDetails {
				m.phase = repoAddPhaseInput
				m.buildInputForm()
				return m, m.form.Init()
			}
			m.cancel = true
			return m, nil
		}
	}

	// Source phase: table navigation.
	if m.phase == repoAddPhaseSource {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			if msg.String() == "enter" {
				m.sourceMode = sourceOptions[m.sourceTable.Cursor()].mode
				m.phase = repoAddPhaseInput
				m.buildInputForm()
				return m, m.form.Init()
			}
			var cmd tea.Cmd
			m.sourceTable, cmd = m.sourceTable.Update(msg)
			updateCursorColumn(&m.sourceTable)
			return m, cmd
		}
		return m, nil
	}

	// Delegate to form when active (input + details phases).
	if m.form != nil && m.phase != repoAddPhaseDone {
		_, cmd := m.form.Update(msg)

		if m.form.State == huh.StateAborted {
			if m.phase == repoAddPhaseInput {
				m.phase = repoAddPhaseSource
				m.form = nil
				m.err = nil
				return m, nil
			}
			if m.phase == repoAddPhaseDetails {
				m.phase = repoAddPhaseInput
				m.buildInputForm()
				m.err = nil
				return m, m.form.Init()
			}
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

func (m RepoAddModel) shouldOfferGitHubAppInstall() bool {
	if m.auth == nil || m.githubAppClient == nil {
		return false
	}
	if status := m.auth.Status(); status == nil || !status.LoggedIn {
		return false
	}
	origin := m.githubAppRepoOrigin()
	return vcs.GitHubNWO(origin) != ""
}

func (m RepoAddModel) githubAppRepoOrigin() string {
	if m.sourceMode == sourceModeClone {
		return strings.TrimSpace(m.fd.gitURL)
	}
	if m.detectedOriginURL != "" {
		return strings.TrimSpace(m.detectedOriginURL)
	}
	if m.createdRepo != nil {
		return strings.TrimSpace(m.createdRepo.OriginUrl)
	}
	return ""
}

func (m RepoAddModel) promptGitHubAppInstall() RepoAddModel {
	m.phase = repoAddPhaseGitHubAppPrompt
	m.githubAppInstallErr = nil
	m.githubAppInstallNWO = vcs.GitHubNWO(m.githubAppRepoOrigin())
	return m
}

func (m RepoAddModel) startGitHubAppInstall() (RepoAddModel, tea.Cmd) {
	m.phase = repoAddPhaseGitHubApp
	m.githubAppAttempt++
	m.githubAppInstallURL = ""
	m.githubAppInstallErr = nil
	m.githubAppInstallOpen = false
	m.githubAppInstallReady = false
	m.githubAppPollCount = 0
	m.githubAppPollExhausted = false
	m.githubAppInstallNWO = vcs.GitHubNWO(m.githubAppRepoOrigin())
	return m, tea.Batch(m.spinner.Tick, m.requestGitHubAppInstallURL(m.githubAppAttempt))
}

// nextGitHubAppPoll schedules the next installation poll, or stops and marks
// the loop exhausted once githubAppInstallMaxPolls have elapsed without the app
// appearing. Pointer receiver so it updates the poll counter on the caller's
// model copy before that copy is returned from Update.
func (m *RepoAddModel) nextGitHubAppPoll(attempt int) tea.Cmd {
	m.githubAppPollCount++
	if m.githubAppPollCount >= githubAppInstallMaxPolls {
		m.githubAppPollExhausted = true
		return nil
	}
	return m.githubAppPollTick(attempt)
}

// reopenGitHubAppInstallCmd re-opens the install page when the URL is already
// known, otherwise re-requests it.
func (m RepoAddModel) reopenGitHubAppInstallCmd() tea.Cmd {
	if m.githubAppInstallURL != "" {
		return m.openGitHubAppInstall(m.githubAppInstallURL, m.githubAppAttempt)
	}
	return m.requestGitHubAppInstallURL(m.githubAppAttempt)
}

func (m RepoAddModel) requestGitHubAppInstallURL(attempt int) tea.Cmd {
	return func() tea.Msg {
		url, err := m.githubAppClient.GetGitHubAppInstallURL(m.ctx, "")
		if err == nil {
			url = enrichGitHubAppInstallURL(m.ctx, url, m.githubAppRepoOrigin())
		}
		return githubAppInstallURLMsg{url: url, err: err, attempt: attempt}
	}
}

func (m RepoAddModel) openGitHubAppInstall(rawURL string, attempt int) tea.Cmd {
	return func() tea.Msg {
		err := openGitHubAppInstallURL(rawURL)
		return githubAppBrowserOpenedMsg{url: rawURL, err: err, attempt: attempt}
	}
}

func (m RepoAddModel) githubAppPollTick(attempt int) tea.Cmd {
	return tea.Tick(githubAppPollIntervalValue(), func(time.Time) tea.Msg {
		return githubAppPollTickMsg{attempt: attempt}
	})
}

func (m RepoAddModel) listGitHubAppRepos(attempt int) tea.Cmd {
	return func() tea.Msg {
		repos, err := m.githubAppClient.ListGitHubAppRepos(m.ctx)
		return githubAppReposMsg{repos: repos, err: err, attempt: attempt}
	}
}

func (m RepoAddModel) githubAppRepoInstalled(repos []*pb.GitHubAppRepoStatus) bool {
	for _, repo := range repos {
		if repo.GetInstalled() && strings.EqualFold(repo.GetOwner()+"/"+repo.GetName(), m.githubAppInstallNWO) {
			return true
		}
	}
	return false
}

func (m *RepoAddModel) handleFormCompleted() (tea.Model, tea.Cmd) {
	switch m.phase {
	case repoAddPhaseInput:
		if m.sourceMode == sourceModeClone {
			// Derive defaults from URL.
			repoName := parseRepoNameFromURL(m.fd.gitURL)
			if repoName != "" {
				home, _ := os.UserHomeDir()
				if m.fd.clonePath == "" {
					m.fd.clonePath = filepath.Join(home, "Code", repoName)
				}
				if vcs.IsGitHubURL(m.fd.gitURL) {
					if nwo := vcs.GitHubNWO(m.fd.gitURL); nwo != "" {
						m.fd.name = "@" + nwo
					} else {
						m.fd.name = repoName
					}
				} else {
					m.fd.name = repoName
				}
			}
			// Go straight to details.
			m.phase = repoAddPhaseDetails
			m.buildDetailsForm()
			return *m, m.form.Init()
		}
		// Open mode — validate path first.
		m.validating = true
		m.err = nil
		localPath := m.fd.localPath
		return *m, func() tea.Msg {
			resp, err := m.client.ValidateRepoPath(m.ctx, localPath)
			return repoValidatedMsg{resp: resp, err: err}
		}

	case repoAddPhaseDetails:
		if !m.fd.confirm {
			m.cancel = true
			return *m, nil
		}
		return *m, m.submitRepo()

	case repoAddPhaseSource:
		// Source table selection — no form completion to handle.
	case repoAddPhaseGitHubAppPrompt:
		// GitHub App prompt is handled by key messages, not form completion.
	case repoAddPhaseGitHubApp:
		// GitHub App installation is driven by cloud poll messages, not form completion.
	case repoAddPhaseDone:
		// Nothing to do.
	}
	return *m, nil
}

func (m *RepoAddModel) submitRepo() tea.Cmd {
	cfg, _ := config.Load()

	if m.sourceMode == sourceModeClone {
		m.cloning = true
		req := &pb.CloneAndRegisterRepoRequest{
			CloneUrl:          m.fd.gitURL,
			LocalPath:         m.fd.clonePath,
			DisplayName:       m.fd.name,
			DefaultBaseBranch: "main",
			WorktreeBaseDir:   cfg.WorktreeBaseDir,
		}
		if s := m.fd.setup; s != "" {
			req.SetupScript = &s
		}
		return func() tea.Msg {
			repo, err := m.client.CloneAndRegisterRepo(m.ctx, req)
			return repoClonedMsg{repo: repo, err: err}
		}
	}

	req := &pb.RegisterRepoRequest{
		LocalPath:         m.fd.localPath,
		DisplayName:       m.fd.name,
		DefaultBaseBranch: m.detectedBaseBranch,
		WorktreeBaseDir:   cfg.WorktreeBaseDir,
	}
	if s := m.fd.setup; s != "" {
		req.SetupScript = &s
	}
	return func() tea.Msg {
		repo, err := m.client.RegisterRepo(m.ctx, req)
		return repoRegisteredMsg{repo: repo, err: err}
	}
}

// Cancelled returns true if the user cancelled.
func (m RepoAddModel) Cancelled() bool { return m.cancel }

// Done returns true if registration succeeded.
func (m RepoAddModel) Done() bool { return m.done }

func (m RepoAddModel) View() tea.View {
	if m.phase == repoAddPhaseGitHubAppPrompt {
		return tea.NewView(m.githubAppInstallPromptView())
	}

	if m.phase == repoAddPhaseGitHubApp {
		return tea.NewView(m.githubAppInstallView())
	}

	if m.validating {
		return tea.NewView(
			lipgloss.NewStyle().Padding(0, 2).Foreground(colorInfo).Render(
				fmt.Sprintf("Validating %s...", m.fd.localPath)),
		)
	}

	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				actionBar([]string{"[esc] back"}),
		)
	}

	if m.cloning {
		return tea.NewView(
			lipgloss.NewStyle().Padding(0, 2).Foreground(colorInfo).Render(
				fmt.Sprintf("Cloning %s...", m.fd.gitURL)),
		)
	}

	if m.done && m.createdRepo != nil {
		return tea.NewView(
			lipgloss.NewStyle().Padding(0, 2).Foreground(colorSuccess).Render("Repository registered!") + "\n" +
				lipgloss.NewStyle().Padding(0, 2).Render(
					fmt.Sprintf("  ID:   %s\n  Name: %s\n  Path: %s",
						m.createdRepo.Id, m.createdRepo.DisplayName, m.createdRepo.LocalPath)),
		)
	}

	if m.phase == repoAddPhaseSource {
		var b strings.Builder
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.sourceTable.View()))
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[enter] select"}, []string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	if m.form != nil {
		var b strings.Builder
		if m.sourceMode == sourceModeClone {
			b.WriteString(styleTitle.Render("Clone a repository from URL"))
		} else {
			b.WriteString(styleTitle.Render("Add a local repository"))
		}
		b.WriteString("\n\n")
		b.WriteString(lipgloss.NewStyle().PaddingLeft(2).Render(m.form.View()))
		return tea.NewView(b.String())
	}

	return tea.NewView("")
}

func (m RepoAddModel) githubAppInstallPromptView() string {
	repoLabel := m.githubAppInstallNWO
	if repoLabel == "" && m.createdRepo != nil {
		repoLabel = m.createdRepo.DisplayName
	}
	if repoLabel == "" {
		repoLabel = "this repository"
	}

	return githubAppInstallPromptContent(repoLabel)
}

func githubAppInstallPromptContent(repoLabel string) string {
	padding := lipgloss.NewStyle().Padding(0, 2)
	body := "Install the Bossanova Github App on " + repoLabel + "?\n\n" +
		"This enables automated realtime response to conflicts, failing tests and code reviews.\n\n" +
		"You can always set this up later if you don't want to decide yet."
	return padding.Render(body) + "\n" +
		styleActionBar.Render("[enter] install  [esc] skip")
}

func (m RepoAddModel) githubAppInstallView() string {
	padding := lipgloss.NewStyle().Padding(0, 2)
	repoLabel := m.githubAppInstallNWO
	if repoLabel == "" && m.createdRepo != nil {
		repoLabel = m.createdRepo.DisplayName
	}
	if repoLabel == "" {
		repoLabel = "this repository"
	}

	if m.githubAppPollExhausted {
		body := "Still waiting for the GitHub App installation on " + repoLabel + ".\n" +
			"Automatic checking has paused."
		if m.githubAppInstallURL != "" {
			body += "\nInstall URL: " + m.githubAppInstallURL
		}
		return padding.Render(body) + "\n" +
			styleActionBar.Render("[enter] keep checking  [esc] skip")
	}

	body := m.spinner.View() + "Waiting for GitHub App installation on " + repoLabel + "..."
	if !m.githubAppInstallOpen {
		body = m.spinner.View() + "Opening GitHub App installation page..."
	}
	if m.githubAppInstallErr != nil && m.githubAppInstallURL != "" {
		body += "\nOpen this GitHub App URL: " + m.githubAppInstallURL
	}
	if m.githubAppInstallErr != nil && m.githubAppInstallURL == "" {
		body = "Could not start GitHub App installation: " + m.githubAppInstallErr.Error()
	}

	return padding.Render(body) + "\n" +
		styleActionBar.Render("[enter] re-open install page  [esc] skip")
}
