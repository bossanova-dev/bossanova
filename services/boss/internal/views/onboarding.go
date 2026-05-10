package views

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// onboardingProvider describes a coding-agent provider shown on the startup
// provider screen.
type onboardingProvider struct {
	Name        string // display label, e.g. "Claude"
	Plugin      string // matching plugin name, e.g. "claude"
	Command     string // executable expected on PATH
	InstallCmd  string // shell command to install the underlying CLI
	InstallURL  string // documentation URL for the provider
	Description string // one-liner shown under the row
}

// onboardingProviders is the static list rendered by the onboarding screen.
// Order matches the install order recommended in docs (Claude first, Codex
// second). New providers should be appended here as plugins are added.
var onboardingProviders = []onboardingProvider{
	{
		Name:        "Claude",
		Plugin:      "claude",
		Command:     "claude",
		InstallCmd:  "npm install -g @anthropic-ai/claude-code",
		InstallURL:  "https://docs.claude.com/en/docs/claude-code/overview",
		Description: "Anthropic's Claude Code CLI",
	},
	{
		Name:        "Codex",
		Plugin:      "codex",
		Command:     "codex",
		InstallCmd:  "npm install -g @openai/codex",
		InstallURL:  "https://github.com/openai/codex",
		Description: "OpenAI's Codex CLI",
	},
}

// OnboardingModel is the startup provider screen. In selection mode it lets
// the user choose from installed provider CLIs. In install-required mode it
// blocks startup with installation instructions when no provider CLI exists.
type OnboardingModel struct {
	providers       []onboardingProvider
	cursor          int
	selected        map[int]bool
	installRequired bool
	done            bool
	cancel          bool
	err             error
	width           int
}

// NewOnboardingModel keeps the old constructor available for tests and any
// direct view routing. Startup uses the explicit constructors below.
func NewOnboardingModel() OnboardingModel {
	return NewProviderSelectionModel(onboardingProviders, nil)
}

// NewProviderSelectionModel builds the provider picker with only installed
// providers. preselected is keyed by plugin name.
func NewProviderSelectionModel(providers []onboardingProvider, preselected map[string]bool) OnboardingModel {
	selected := make(map[int]bool)
	for i, p := range providers {
		if preselected[p.Plugin] {
			selected[i] = true
		}
	}
	return OnboardingModel{
		providers: providers,
		selected:  selected,
	}
}

// NewProviderInstallRequiredModel builds the blocking install-instructions
// screen shown when no supported provider CLI is available on PATH.
func NewProviderInstallRequiredModel(providers []onboardingProvider) OnboardingModel {
	return OnboardingModel{
		providers:       providers,
		selected:        make(map[int]bool),
		installRequired: true,
	}
}

// Done reports whether the user finished onboarding (pressed enter).
func (m OnboardingModel) Done() bool { return m.done }

// Cancelled reports whether the user dismissed onboarding (pressed esc).
func (m OnboardingModel) Cancelled() bool { return m.cancel }

func (m OnboardingModel) Init() tea.Cmd { return nil }

func (m OnboardingModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.KeyMsg:
		// Older bubbletea key wrapper; treat the same as KeyPressMsg.
		if k, ok := msg.(tea.KeyPressMsg); ok {
			return m.handleKey(k)
		}
	}
	return m, nil
}

func (m OnboardingModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.installRequired {
		switch msg.String() {
		case "esc", "q", "ctrl+c":
			m.cancel = true
			return m, tea.Quit
		}
		return m, nil
	}

	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.cancel = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.providers)-1 {
			m.cursor++
		}
	case " ", "space", "x":
		if m.selected == nil {
			m.selected = make(map[int]bool)
		}
		m.selected[m.cursor] = !m.selected[m.cursor]
	case "enter":
		if len(m.SelectedPlugins()) == 0 {
			m.err = fmt.Errorf("select at least one provider")
			return m, nil
		}
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

// SelectedPlugins returns the selected provider plugin names in rendered order.
func (m OnboardingModel) SelectedPlugins() []string {
	out := make([]string, 0, len(m.selected))
	for i, p := range m.providers {
		if m.selected[i] {
			out = append(out, p.Plugin)
		}
	}
	return out
}

func (m OnboardingModel) View() tea.View {
	var b strings.Builder

	if m.installRequired {
		b.WriteString(styleTitle.Render("Install an agent provider"))
		b.WriteString("\n\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render(
			"Bossanova needs at least one supported agent CLI on your PATH. Install one, then restart boss."))
		b.WriteString("\n\n")
		for _, p := range m.providers {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(p.Name))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).PaddingLeft(8).Render("install: " + p.InstallCmd))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).PaddingLeft(8).Foreground(colorMuted).Render(p.Description))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).PaddingLeft(8).Foreground(colorMuted).Render(p.InstallURL))
			b.WriteString("\n\n")
		}
		b.WriteString(styleActionBar.Render("Press q to quit."))
		return tea.NewView(b.String())
	}

	b.WriteString(styleTitle.Render("Choose providers to enable"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render(
		"Choose the installed agent runners you want Bossanova to use. You can change this later."))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(renderError(fmt.Sprintf("Error: %v", m.err), m.width))
		b.WriteString("\n")
	}

	for i, p := range m.providers {
		check := " "
		if m.selected[i] {
			check = "x"
		}
		cursor := "  "
		if i == m.cursor {
			cursor = cursorChevron + " "
		}
		header := fmt.Sprintf("%s[%s] %s", cursor, check, p.Name)
		if i == m.cursor {
			header = styleSelected.Render(header)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(header))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).PaddingLeft(8).Foreground(colorMuted).Render(p.Description))
		b.WriteString("\n")
		b.WriteString("\n")
	}

	b.WriteString(actionBar(
		[]string{"[space] toggle", "[enter] continue"},
		[]string{"[esc] skip"},
	))

	return tea.NewView(b.String())
}
