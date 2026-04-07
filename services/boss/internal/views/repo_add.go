package views

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
)

// --- Repo Add Wizard ---

// repoAddPhase tracks the current phase in the add-repo wizard.
type repoAddPhase int

const (
	repoAddPhaseSource  repoAddPhase = iota // Phase 1: table pick (open/clone)
	repoAddPhaseInput                       // Phase 2: path/URL input
	repoAddPhaseDetails                     // Phase 3: name + setup + confirm
	repoAddPhaseDone                        // Terminal state
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

	// Validation results
	isGithub           bool
	detectedBaseBranch string

	createdRepo *pb.Repo

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
		fd: &repoAddFormData{
			localPath: home + "/",
			name:      filepath.Base(home),
			confirm:   true,
		},
	}
	m.buildSourceTable()
	return m
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
		m.done = true
		return m, nil

	case repoClonedMsg:
		m.cloning = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.createdRepo = msg.repo
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

	case tea.KeyMsg:
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
	if m.validating {
		return tea.NewView(
			lipgloss.NewStyle().Padding(1, 2).Foreground(colorInfo).Render(
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
			lipgloss.NewStyle().Padding(1, 2).Foreground(colorInfo).Render(
				fmt.Sprintf("Cloning %s...", m.fd.gitURL)),
		)
	}

	if m.done && m.createdRepo != nil {
		return tea.NewView(
			lipgloss.NewStyle().Padding(1, 2).Foreground(colorSuccess).Render("Repository registered!") + "\n" +
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
