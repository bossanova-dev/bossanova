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
	repoAddStepPath repoAddStep = iota
	repoAddStepName
	repoAddStepBaseBranch
	repoAddStepWorktreeDir
	repoAddStepSetup
	repoAddStepConfirm
)

// repoRegisteredMsg carries the result of a RegisterRepo RPC call.
type repoRegisteredMsg struct {
	repo *pb.Repo
	err  error
}

// RepoAddModel is the wizard for registering a new repository.
type RepoAddModel struct {
	client *client.Client
	ctx    context.Context

	step   repoAddStep
	err    error
	done   bool
	cancel bool

	pathInput        textinput.Model
	nameInput        textinput.Model
	baseBranchInput  textinput.Model
	worktreeDirInput textinput.Model
	setupInput       textinput.Model

	createdRepo *pb.Repo
}

// NewRepoAddModel creates a RepoAddModel with sensible defaults.
func NewRepoAddModel(c *client.Client, ctx context.Context) RepoAddModel {
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

	return RepoAddModel{
		client:           c,
		ctx:              ctx,
		step:             repoAddStepPath,
		pathInput:        pathIn,
		nameInput:        nameIn,
		baseBranchInput:  branchIn,
		worktreeDirInput: wtIn,
		setupInput:       setupIn,
	}
}

func (m RepoAddModel) Init() tea.Cmd {
	return m.pathInput.Focus()
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

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.cancel = true
			return m, nil
		}

		switch m.step {
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

func (m RepoAddModel) updatePathStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" {
		m.pathInput.Blur()
		m.step = repoAddStepName
		return m, m.nameInput.Focus()
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
	if m.err != nil {
		return tea.NewView(
			styleError.Render(fmt.Sprintf("Error: %v", m.err)) + "\n" +
				styleActionBar.Render("[esc] back"),
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
			// Completed field: show value.
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
				fmt.Sprintf("  %s: %s", f.label, f.input.Value())))
			b.WriteString("\n")
		} else if m.step == f.step {
			// Active field: show input.
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
				fmt.Sprintf("  %s:", f.label)))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(f.input.View()))
			b.WriteString("\n")
		}
	}

	if m.step == repoAddStepConfirm {
		for _, f := range fields {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
				fmt.Sprintf("  %s: %s", f.label, f.input.Value())))
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] register  [n/esc] cancel"))
	} else {
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[enter] next  [esc] cancel"))
	}

	return tea.NewView(b.String())
}

// --- Repo List View ---

// repoListLoadedMsg carries repos for the list view.
type repoListLoadedMsg struct {
	repos []*pb.Repo
	err   error
}

// repoRemovedMsg carries the result of removing a repo.
type repoRemovedMsg struct {
	err error
}

// RepoListModel displays registered repos with remove functionality.
type RepoListModel struct {
	client *client.Client
	ctx    context.Context

	repos   []*pb.Repo
	cursor  int
	err     error
	cancel  bool
	loading bool

	// Remove confirmation
	confirming bool
}

// NewRepoListModel creates a RepoListModel.
func NewRepoListModel(c *client.Client, ctx context.Context) RepoListModel {
	return RepoListModel{
		client:  c,
		ctx:     ctx,
		loading: true,
	}
}

func (m RepoListModel) Init() tea.Cmd {
	return func() tea.Msg {
		repos, err := m.client.ListRepos(m.ctx)
		return repoListLoadedMsg{repos: repos, err: err}
	}
}

func (m RepoListModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case repoListLoadedMsg:
		m.loading = false
		m.repos = msg.repos
		m.err = msg.err
		return m, nil

	case repoRemovedMsg:
		m.confirming = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		// Refresh list.
		m.loading = true
		return m, m.Init()

	case tea.KeyMsg:
		if m.confirming {
			return m.updateConfirm(msg)
		}

		switch msg.String() {
		case "esc", "q":
			m.cancel = true
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.repos)-1 {
				m.cursor++
			}
		case "d":
			if len(m.repos) > 0 {
				m.confirming = true
			}
		}
	}

	return m, nil
}

func (m RepoListModel) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		repo := m.repos[m.cursor]
		return m, func() tea.Msg {
			err := m.client.RemoveRepo(m.ctx, repo.Id)
			return repoRemovedMsg{err: err}
		}
	case "n", "esc":
		m.confirming = false
	}
	return m, nil
}

// Cancelled returns true if the user exited the list.
func (m RepoListModel) Cancelled() bool { return m.cancel }

func (m RepoListModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			styleError.Render(fmt.Sprintf("Error: %v", m.err)) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.loading {
		return tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render("Loading repositories..."))
	}

	var b strings.Builder
	b.WriteString(styleTitle.Render("Repositories"))
	b.WriteString("\n")

	if len(m.repos) == 0 {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("No repositories registered."))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Add one with: boss repo add"))
		b.WriteString("\n\n")
		b.WriteString(styleActionBar.Render("[esc] back"))
		return tea.NewView(b.String())
	}

	// Table header.
	header := fmt.Sprintf("  %-10s %-20s %-30s %-12s %-10s",
		"ID", "NAME", "PATH", "BRANCH", "SETUP")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Faint(true).Render(header))
	b.WriteString("\n")

	for i, repo := range m.repos {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		id := truncate(repo.Id, 8)
		name := truncate(repo.DisplayName, 20)
		path := truncate(repo.LocalPath, 30)
		branch := truncate(repo.DefaultBaseBranch, 12)
		setup := "-"
		if repo.SetupScript != nil {
			setup = truncate(*repo.SetupScript, 10)
		}

		row := fmt.Sprintf("%s%-10s %-20s %-30s %-12s %-10s",
			cursor, id, name, path, branch, setup)
		if i == m.cursor {
			row = styleSelected.Render(row)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(row))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if m.confirming {
		repo := m.repos[m.cursor]
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorRed).Render(
			fmt.Sprintf("Remove %q? [y/n]", repo.DisplayName)))
	} else {
		b.WriteString(styleActionBar.Render("[d] remove  [esc] back"))
	}

	return tea.NewView(b.String())
}
