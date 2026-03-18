package views

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Repo Add Wizard ---

// repoAddPhase tracks the current phase in the add-repo wizard.
type repoAddPhase int

const (
	repoAddPhaseSource  repoAddPhase = iota // Phase 1: source + path/URL
	repoAddPhaseDetails                     // Phase 2: name + setup + confirm
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

// RepoAddModel is the wizard for registering a new repository.
type RepoAddModel struct {
	client client.BossClient
	ctx    context.Context

	phase  repoAddPhase
	err    error
	done   bool
	cancel bool

	// Form-bound values
	sourceMode int
	gitURL     string
	clonePath  string
	localPath  string
	name       string
	setup      string
	confirm    bool

	// Async state
	validating bool
	cloning    bool

	// Validation results
	isGithub           bool
	detectedBaseBranch string

	createdRepo *pb.Repo

	// Form
	form *huh.Form

	// Layout
	width int
}

// NewRepoAddModel creates a RepoAddModel with sensible defaults.
func NewRepoAddModel(c client.BossClient, ctx context.Context) RepoAddModel {
	cwd, _ := os.Getwd()

	m := RepoAddModel{
		client:             c,
		ctx:                ctx,
		phase:              repoAddPhaseSource,
		localPath:          cwd,
		name:               filepath.Base(cwd),
		detectedBaseBranch: "main",
		confirm:            true,
	}
	m.buildSourceForm()
	return m
}

func (m *RepoAddModel) buildSourceForm() {
	m.form = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[int]().
				Title("Add Repository").
				Options(
					huh.NewOption("Open project — Register an existing local repo", sourceModeOpen),
					huh.NewOption("Clone from URL — Clone a repo and register it", sourceModeClone),
				).
				Value(&m.sourceMode),
		),
		// Clone fields — shown only in clone mode.
		huh.NewGroup(
			huh.NewInput().
				Title("Git URL").
				Placeholder("https://github.com/user/repo.git").
				Value(&m.gitURL).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("URL is required")
					}
					return nil
				}),
			huh.NewInput().
				Title("Clone path").
				Placeholder("Clone destination path").
				Value(&m.clonePath).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("clone path is required")
					}
					return nil
				}),
		).WithHideFunc(func() bool { return m.sourceMode != sourceModeClone }),
		// Open fields — shown only in open mode.
		huh.NewGroup(
			huh.NewInput().
				Title("Path").
				Placeholder("Repository path").
				Value(&m.localPath).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("path is required")
					}
					return nil
				}),
		).WithHideFunc(func() bool { return m.sourceMode != sourceModeOpen }),
	).WithTheme(bossHuhTheme()).WithShowHelp(false).WithWidth(70)
}

func (m *RepoAddModel) buildDetailsForm() {
	m.form = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Name").
				Placeholder("Display name").
				Value(&m.name).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("name is required")
					}
					return nil
				}),
			huh.NewInput().
				Title("Setup script").
				Placeholder("Optional, e.g. ./setup.sh").
				Value(&m.setup),
			huh.NewConfirm().
				Title("Add this repository?").
				Affirmative("Yes").
				Negative("No").
				Value(&m.confirm),
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
			// Rebuild source form so user can fix path.
			m.buildSourceForm()
			return m, m.form.Init()
		}
		// Auto-populate fields from validation response.
		m.isGithub = msg.resp.IsGithub
		if msg.resp.DefaultBranch != "" {
			m.detectedBaseBranch = msg.resp.DefaultBranch
		}
		m.name = filepath.Base(m.localPath)
		// Advance to details phase.
		m.phase = repoAddPhaseDetails
		m.buildDetailsForm()
		return m, m.form.Init()

	case tea.KeyMsg:
		if msg.String() == "esc" {
			m.cancel = true
			return m, nil
		}
	}

	// Delegate to form when active.
	if m.form != nil && m.phase != repoAddPhaseDone {
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

func (m *RepoAddModel) handleFormCompleted() (tea.Model, tea.Cmd) {
	switch m.phase {
	case repoAddPhaseSource:
		if m.sourceMode == sourceModeClone {
			// Derive defaults from URL.
			repoName := parseRepoNameFromURL(m.gitURL)
			if repoName != "" {
				home, _ := os.UserHomeDir()
				if m.clonePath == "" {
					m.clonePath = filepath.Join(home, "Code", repoName)
				}
				m.name = repoName
			}
			// Go straight to details.
			m.phase = repoAddPhaseDetails
			m.buildDetailsForm()
			return m, m.form.Init()
		}
		// Open mode — validate path first.
		m.validating = true
		m.err = nil
		localPath := m.localPath
		return m, func() tea.Msg {
			resp, err := m.client.ValidateRepoPath(m.ctx, localPath)
			return repoValidatedMsg{resp: resp, err: err}
		}

	case repoAddPhaseDetails:
		if !m.confirm {
			m.cancel = true
			return m, nil
		}
		return m, m.submitRepo()

	case repoAddPhaseDone:
		// Nothing to do.
	}
	return m, nil
}

func (m *RepoAddModel) submitRepo() tea.Cmd {
	cfg, _ := config.Load()

	if m.sourceMode == sourceModeClone {
		m.cloning = true
		req := &pb.CloneAndRegisterRepoRequest{
			CloneUrl:          m.gitURL,
			LocalPath:         m.clonePath,
			DisplayName:       m.name,
			DefaultBaseBranch: "main",
			WorktreeBaseDir:   cfg.WorktreeBaseDir,
		}
		if s := m.setup; s != "" {
			req.SetupScript = &s
		}
		return func() tea.Msg {
			repo, err := m.client.CloneAndRegisterRepo(m.ctx, req)
			return repoClonedMsg{repo: repo, err: err}
		}
	}

	req := &pb.RegisterRepoRequest{
		LocalPath:         m.localPath,
		DisplayName:       m.name,
		DefaultBaseBranch: m.detectedBaseBranch,
		WorktreeBaseDir:   cfg.WorktreeBaseDir,
	}
	if s := m.setup; s != "" {
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
				fmt.Sprintf("Validating %s...", m.localPath)),
		)
	}

	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.cloning {
		return tea.NewView(
			lipgloss.NewStyle().Padding(1, 2).Foreground(colorInfo).Render(
				fmt.Sprintf("Cloning %s...", m.gitURL)),
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

	if m.form != nil {
		var b strings.Builder
		if m.phase == repoAddPhaseDetails {
			b.WriteString(styleTitle.Render("Add Repository"))
			b.WriteString("\n")
			if m.sourceMode == sourceModeClone {
				b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Source: Clone from URL"))
			} else {
				b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Source: Open project"))
			}
			b.WriteString("\n")
		}
		b.WriteString(m.form.View())
		return tea.NewView(b.String())
	}

	return tea.NewView("")
}
