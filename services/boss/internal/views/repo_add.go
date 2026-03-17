package views

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- Repo Add Wizard ---

// repoAddStep tracks the current step in the add-repo wizard.
type repoAddStep int

const (
	repoAddStepSource    repoAddStep = iota // "Open project" vs "Clone from URL"
	repoAddStepURL                          // git URL input (clone mode only)
	repoAddStepClonePath                    // clone destination (clone mode only)
	repoAddStepPath                         // existing local path (open mode only)
	repoAddStepName
	repoAddStepBaseBranch
	repoAddStepWorktreeDir
	repoAddStepSetup
	repoAddStepConfirm
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

	step   repoAddStep
	err    error
	done   bool
	cancel bool

	// Source selection
	sourceMode   int
	sourceCursor int

	// Clone-specific inputs
	urlInput       textinput.Model
	clonePathInput textinput.Model
	cloning        bool

	// Shared inputs
	pathInput        textinput.Model
	nameInput        textinput.Model
	baseBranchInput  textinput.Model
	worktreeDirInput textinput.Model
	setupInput       textinput.Model

	// Validation
	validating bool
	isGithub   bool

	createdRepo *pb.Repo
}

// NewRepoAddModel creates a RepoAddModel with sensible defaults.
func NewRepoAddModel(c client.BossClient, ctx context.Context) RepoAddModel {
	cwd, _ := os.Getwd()

	pathIn := textinput.New()
	pathIn.Placeholder = "Repository path"
	pathIn.SetWidth(60)
	pathIn.SetValue(cwd)

	nameIn := textinput.New()
	nameIn.Placeholder = "Display name"
	nameIn.SetWidth(40)
	if cwd != "" {
		nameIn.SetValue(filepath.Base(cwd))
	}

	branchIn := textinput.New()
	branchIn.Placeholder = "Default base branch"
	branchIn.SetWidth(30)
	branchIn.SetValue("main")

	wtIn := textinput.New()
	wtIn.Placeholder = "Worktree base directory"
	wtIn.SetWidth(60)
	if cwd != "" {
		wtIn.SetValue(filepath.Join(filepath.Dir(cwd), ".worktrees"))
	}

	setupIn := textinput.New()
	setupIn.Placeholder = "Setup script (optional, e.g. ./setup.sh)"
	setupIn.SetWidth(60)

	urlIn := textinput.New()
	urlIn.Placeholder = "https://github.com/user/repo.git"
	urlIn.SetWidth(60)

	clonePathIn := textinput.New()
	clonePathIn.Placeholder = "Clone destination path"
	clonePathIn.SetWidth(60)

	return RepoAddModel{
		client:           c,
		ctx:              ctx,
		step:             repoAddStepSource,
		pathInput:        pathIn,
		nameInput:        nameIn,
		baseBranchInput:  branchIn,
		worktreeDirInput: wtIn,
		setupInput:       setupIn,
		urlInput:         urlIn,
		clonePathInput:   clonePathIn,
	}
}

func (m RepoAddModel) Init() tea.Cmd {
	return nil // Source step uses cursor, no textinput focus needed.
}

func (m RepoAddModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
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
			// Stay on path step so user can fix.
			m.step = repoAddStepPath
			return m, m.pathInput.Focus()
		}
		// Auto-populate fields from validation response.
		m.isGithub = msg.resp.IsGithub
		if msg.resp.DefaultBranch != "" {
			m.baseBranchInput.SetValue(msg.resp.DefaultBranch)
		}
		// Default the name to the basename of the entered path.
		m.nameInput.SetValue(filepath.Base(m.pathInput.Value()))
		m.step = repoAddStepName
		return m, m.nameInput.Focus()

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.cancel = true
			return m, nil
		}

		switch m.step {
		case repoAddStepSource:
			return m.updateSourceStep(msg)
		case repoAddStepURL:
			return m.updateURLStep(msg)
		case repoAddStepClonePath:
			return m.updateClonePathStep(msg)
		case repoAddStepPath:
			return m.updatePathStep(msg)
		case repoAddStepName:
			return m.updateNameStep(msg)
		case repoAddStepBaseBranch:
			return m.updateBaseBranchStep(msg)
		case repoAddStepWorktreeDir:
			return m.updateWorktreeDirStep(msg)
		case repoAddStepSetup:
			return m.updateSetupStep(msg)
		case repoAddStepConfirm:
			return m.updateConfirmStep(msg)
		}
	}

	// Pass through to focused inputs.
	return m.updateActiveInput(msg)
}

func (m RepoAddModel) updateSourceStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.sourceCursor > 0 {
			m.sourceCursor--
		}
	case "down", "j":
		if m.sourceCursor < 1 {
			m.sourceCursor++
		}
	case "enter":
		m.sourceMode = m.sourceCursor
		if m.sourceMode == sourceModeClone {
			m.step = repoAddStepURL
			return m, m.urlInput.Focus()
		}
		m.step = repoAddStepPath
		return m, m.pathInput.Focus()
	}
	return m, nil
}

func (m RepoAddModel) updateURLStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" && m.urlInput.Value() != "" {
		m.urlInput.Blur()

		// Derive defaults from URL.
		repoName := parseRepoNameFromURL(m.urlInput.Value())
		if repoName != "" {
			home, _ := os.UserHomeDir()
			defaultClonePath := filepath.Join(home, "Code", repoName)
			m.clonePathInput.SetValue(defaultClonePath)
			m.nameInput.SetValue(repoName)
			m.worktreeDirInput.SetValue(filepath.Join(home, "Code", ".worktrees"))
		}

		m.step = repoAddStepClonePath
		return m, m.clonePathInput.Focus()
	}
	var cmd tea.Cmd
	m.urlInput, cmd = m.urlInput.Update(msg)
	return m, cmd
}

func (m RepoAddModel) updateClonePathStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" && m.clonePathInput.Value() != "" {
		m.clonePathInput.Blur()
		m.step = repoAddStepName
		return m, m.nameInput.Focus()
	}
	var cmd tea.Cmd
	m.clonePathInput, cmd = m.clonePathInput.Update(msg)
	return m, cmd
}

func (m RepoAddModel) updatePathStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" && m.pathInput.Value() != "" {
		m.pathInput.Blur()
		m.err = nil
		m.validating = true
		localPath := m.pathInput.Value()
		return m, func() tea.Msg {
			resp, err := m.client.ValidateRepoPath(m.ctx, localPath)
			return repoValidatedMsg{resp: resp, err: err}
		}
	}
	var cmd tea.Cmd
	m.pathInput, cmd = m.pathInput.Update(msg)
	return m, cmd
}

func (m RepoAddModel) updateNameStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" && m.nameInput.Value() != "" {
		m.nameInput.Blur()
		m.step = repoAddStepBaseBranch
		return m, m.baseBranchInput.Focus()
	}
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

func (m RepoAddModel) updateBaseBranchStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" && m.baseBranchInput.Value() != "" {
		m.baseBranchInput.Blur()
		m.step = repoAddStepWorktreeDir
		return m, m.worktreeDirInput.Focus()
	}
	var cmd tea.Cmd
	m.baseBranchInput, cmd = m.baseBranchInput.Update(msg)
	return m, cmd
}

func (m RepoAddModel) updateWorktreeDirStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" && m.worktreeDirInput.Value() != "" {
		m.worktreeDirInput.Blur()
		m.step = repoAddStepSetup
		return m, m.setupInput.Focus()
	}
	var cmd tea.Cmd
	m.worktreeDirInput, cmd = m.worktreeDirInput.Update(msg)
	return m, cmd
}

func (m RepoAddModel) updateSetupStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" {
		m.setupInput.Blur()
		m.step = repoAddStepConfirm
		return m, nil
	}
	var cmd tea.Cmd
	m.setupInput, cmd = m.setupInput.Update(msg)
	return m, cmd
}

func (m RepoAddModel) updateConfirmStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		if m.sourceMode == sourceModeClone {
			m.cloning = true
			req := &pb.CloneAndRegisterRepoRequest{
				CloneUrl:          m.urlInput.Value(),
				LocalPath:         m.clonePathInput.Value(),
				DisplayName:       m.nameInput.Value(),
				DefaultBaseBranch: m.baseBranchInput.Value(),
				WorktreeBaseDir:   m.worktreeDirInput.Value(),
			}
			if s := m.setupInput.Value(); s != "" {
				req.SetupScript = &s
			}
			return m, func() tea.Msg {
				repo, err := m.client.CloneAndRegisterRepo(m.ctx, req)
				return repoClonedMsg{repo: repo, err: err}
			}
		}
		req := &pb.RegisterRepoRequest{
			LocalPath:         m.pathInput.Value(),
			DisplayName:       m.nameInput.Value(),
			DefaultBaseBranch: m.baseBranchInput.Value(),
			WorktreeBaseDir:   m.worktreeDirInput.Value(),
		}
		if s := m.setupInput.Value(); s != "" {
			req.SetupScript = &s
		}
		return m, func() tea.Msg {
			repo, err := m.client.RegisterRepo(m.ctx, req)
			return repoRegisteredMsg{repo: repo, err: err}
		}
	case "n":
		m.cancel = true
	}
	return m, nil
}

func (m RepoAddModel) updateActiveInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.step {
	case repoAddStepURL:
		m.urlInput, cmd = m.urlInput.Update(msg)
	case repoAddStepClonePath:
		m.clonePathInput, cmd = m.clonePathInput.Update(msg)
	case repoAddStepPath:
		m.pathInput, cmd = m.pathInput.Update(msg)
	case repoAddStepName:
		m.nameInput, cmd = m.nameInput.Update(msg)
	case repoAddStepBaseBranch:
		m.baseBranchInput, cmd = m.baseBranchInput.Update(msg)
	case repoAddStepWorktreeDir:
		m.worktreeDirInput, cmd = m.worktreeDirInput.Update(msg)
	case repoAddStepSetup:
		m.setupInput, cmd = m.setupInput.Update(msg)
	}
	return m, cmd
}

// Cancelled returns true if the user cancelled.
func (m RepoAddModel) Cancelled() bool { return m.cancel }

// Done returns true if registration succeeded.
func (m RepoAddModel) Done() bool { return m.done }

func (m RepoAddModel) View() tea.View {
	if m.validating {
		return tea.NewView(
			lipgloss.NewStyle().Padding(1, 2).Foreground(colorCyan).Render(
				fmt.Sprintf("Validating %s...", m.pathInput.Value())),
		)
	}

	if m.err != nil && m.step != repoAddStepPath {
		return tea.NewView(
			styleError.Render(fmt.Sprintf("Error: %v", m.err)) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.cloning {
		return tea.NewView(
			lipgloss.NewStyle().Padding(1, 2).Foreground(colorCyan).Render(
				fmt.Sprintf("Cloning %s...", m.urlInput.Value())),
		)
	}

	if m.done && m.createdRepo != nil {
		return tea.NewView(
			lipgloss.NewStyle().Padding(1, 2).Foreground(colorGreen).Render("Repository registered!") + "\n" +
				lipgloss.NewStyle().Padding(0, 2).Render(
					fmt.Sprintf("  ID:   %s\n  Name: %s\n  Path: %s",
						m.createdRepo.Id, m.createdRepo.DisplayName, m.createdRepo.LocalPath)),
		)
	}

	var b strings.Builder
	b.WriteString(styleTitle.Render("Add Repository"))
	b.WriteString("\n")

	if m.step == repoAddStepSource {
		return tea.NewView(m.viewSourceStep())
	}

	// Show source selection as completed.
	if m.sourceMode == sourceModeClone {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Source: Clone from URL"))
		b.WriteString("\n")
		return tea.NewView(b.String() + m.viewCloneFields())
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Source: Open project"))
	b.WriteString("\n")
	return tea.NewView(b.String() + m.viewOpenFields())
}

func (m RepoAddModel) viewSourceStep() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Add Repository"))
	b.WriteString("\n")

	options := []struct {
		label string
		desc  string
	}{
		{"Open project", "Register an existing local repo"},
		{"Clone from URL", "Clone a repo and register it"},
	}

	for i, opt := range options {
		cursor := "  "
		if i == m.sourceCursor {
			cursor = "> "
		}
		line := fmt.Sprintf("%s%-20s %s", cursor, opt.label, styleSubtle.Render(opt.desc))
		if i == m.sourceCursor {
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
		b.WriteString("\n")
	}

	b.WriteString(styleActionBar.Render("[enter] select  [esc] cancel"))

	return b.String()
}

func (m RepoAddModel) viewCloneFields() string {
	var b strings.Builder

	type field struct {
		label string
		value string
		step  repoAddStep
	}

	fields := []field{
		{"Git URL", m.urlInput.Value(), repoAddStepURL},
		{"Clone path", m.clonePathInput.Value(), repoAddStepClonePath},
		{"Name", m.nameInput.Value(), repoAddStepName},
		{"Base branch", m.baseBranchInput.Value(), repoAddStepBaseBranch},
		{"Worktree dir", m.worktreeDirInput.Value(), repoAddStepWorktreeDir},
		{"Setup script", m.setupInput.Value(), repoAddStepSetup},
	}

	inputForStep := func(step repoAddStep) textinput.Model {
		switch step {
		case repoAddStepURL:
			return m.urlInput
		case repoAddStepClonePath:
			return m.clonePathInput
		case repoAddStepName:
			return m.nameInput
		case repoAddStepBaseBranch:
			return m.baseBranchInput
		case repoAddStepWorktreeDir:
			return m.worktreeDirInput
		case repoAddStepSetup:
			return m.setupInput
		default:
			return m.nameInput
		}
	}

	for _, f := range fields {
		if m.step > f.step {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
				fmt.Sprintf("  %s: %s", f.label, f.value)))
			b.WriteString("\n")
		} else if m.step == f.step {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
				fmt.Sprintf("  %s:", f.label)))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(inputForStep(f.step).View()))
			b.WriteString("\n")
		}
	}

	if m.step == repoAddStepConfirm {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Clone and register this repository?"))
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		b.WriteString(styleActionBar.Render("[enter] next  [esc] cancel"))
	}

	return b.String()
}

func (m RepoAddModel) viewOpenFields() string {
	var b strings.Builder

	fields := []struct {
		label string
		input textinput.Model
		step  repoAddStep
	}{
		{"Path", m.pathInput, repoAddStepPath},
		{"Name", m.nameInput, repoAddStepName},
		{"Base branch", m.baseBranchInput, repoAddStepBaseBranch},
		{"Worktree dir", m.worktreeDirInput, repoAddStepWorktreeDir},
		{"Setup script", m.setupInput, repoAddStepSetup},
	}

	for _, f := range fields {
		if m.step > f.step {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
				fmt.Sprintf("  %s: %s", f.label, f.input.Value())))
			b.WriteString("\n")
		} else if m.step == f.step {
			if m.err != nil && f.step == repoAddStepPath {
				b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
					styleError.Render(fmt.Sprintf("  %v", m.err))))
				b.WriteString("\n")
			}
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
				fmt.Sprintf("  %s:", f.label)))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(f.input.View()))
			b.WriteString("\n")
		}
	}

	if m.step == repoAddStepConfirm {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Add this repository?"))
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		b.WriteString(styleActionBar.Render("[enter] next  [esc] cancel"))
	}

	return b.String()
}
