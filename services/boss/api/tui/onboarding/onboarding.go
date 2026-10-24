package onboarding

import (
	"fmt"
	"strings"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type Provider = ui.Provider

var Providers = ui.Providers

type onboardingStep int

const (
	onboardingStepProviders onboardingStep = iota
	onboardingStepDangerousMode
)

// Model is the startup provider screen. In selection mode it lets
// the user choose from installed provider CLIs. In install-required mode it
// blocks startup with installation instructions when no provider CLI exists.
type Model struct {
	providers       []Provider
	step            onboardingStep
	cursor          int
	selected        map[int]bool
	dangerousMode   bool
	installRequired bool
	done            bool
	cancel          bool
	err             error
	width           int
	showBanner      bool
}

// NewModel keeps the old constructor available for tests and any
// direct view routing. Startup uses the explicit constructors below.
func NewModel() Model {
	return NewProviderSelectionModel(Providers, nil)
}

// NewProviderSelectionModel builds the provider picker with only installed
// providers. preselected is keyed by plugin name.
func NewProviderSelectionModel(providers []Provider, preselected map[string]bool) Model {
	selected := make(map[int]bool)
	for i, p := range providers {
		selected[i] = true
		if preselected != nil {
			if enabled, ok := preselected[p.Plugin]; ok {
				selected[i] = enabled
			}
		}
	}
	return Model{
		providers: providers,
		selected:  selected,
	}
}

// NewProviderInstallRequiredModel builds the blocking install-instructions
// screen shown when no supported provider CLI is available on PATH.
func NewProviderInstallRequiredModel(providers []Provider) Model {
	return Model{
		providers:       providers,
		selected:        make(map[int]bool),
		installRequired: true,
	}
}

// Done reports whether the user finished onboarding (pressed enter).
func (m Model) Done() bool { return m.done }

// Cancelled reports whether the user dismissed onboarding (pressed esc).
func (m Model) Cancelled() bool { return m.cancel }

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.installRequired {
		switch msg.String() {
		case "esc", "q", "ctrl+c":
			m.cancel = true
			return m, tea.Quit
		}
		return m, nil
	}
	if m.step == onboardingStepDangerousMode {
		return m.handleDangerousModeKey(msg)
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
		m.step = onboardingStepDangerousMode
		m.err = nil
		return m, nil
	}
	return m, nil
}

func (m Model) handleDangerousModeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		// Skip dangerous mode: finish onboarding with it left disabled. This
		// preserves the provider selection already made rather than quitting
		// and discarding it, matching the "[esc] skip" action hint.
		m.dangerousMode = false
		m.done = true
		return m, tea.Quit
	case "q", "ctrl+c":
		m.cancel = true
		return m, tea.Quit
	case " ", "space", "x":
		m.dangerousMode = !m.dangerousMode
	case "enter":
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

// SelectedPlugins returns the selected provider plugin names in rendered order.
func (m Model) SelectedPlugins() []string {
	out := make([]string, 0, len(m.selected))
	for i, p := range m.providers {
		if m.selected[i] {
			out = append(out, p.Plugin)
		}
	}
	return out
}

// DangerousModeDefault reports whether onboarding should enable each agent's
// unsafe no-prompts mode by default.
func (m Model) DangerousModeDefault() bool { return m.dangerousMode }

func (m Model) View() tea.View {
	var b strings.Builder

	if m.installRequired {
		b.WriteString(ui.StyleTitle.Render("Install an agent provider"))
		b.WriteString("\n\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(ui.ColorMuted).Render(
			"Bossanova needs at least one supported agent CLI on your PATH. Install one, then restart boss."))
		b.WriteString("\n\n")
		for _, p := range m.providers {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(p.Name))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).PaddingLeft(8).Render("install: " + p.InstallCmd))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).PaddingLeft(8).Foreground(ui.ColorMuted).Render(p.Description))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).PaddingLeft(8).Foreground(ui.ColorMuted).Render(p.InstallURL))
			b.WriteString("\n\n")
		}
		b.WriteString(ui.StyleActionBar.Render("Press q to quit."))
		return m.withStartupBanner(tea.NewView(b.String()))
	}

	if m.step == onboardingStepDangerousMode {
		return m.dangerousModeView()
	}

	b.WriteString(ui.StyleTitle.Render("Choose providers to enable"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(ui.ColorMuted).Render(
		"Choose the installed agents you want Bossanova to use. You can change this later."))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(ui.RenderError(ui.RPCErrorMessage(m.err), m.width))
		b.WriteString("\n")
	}

	for i, p := range m.providers {
		cursor := "  "
		if i == m.cursor {
			cursor = ui.CursorChevron + " "
		}
		header := cursor + ui.RenderCheckboxLabel(m.selected[i], p.Name)
		if i == m.cursor {
			header = ui.StyleSelected.Render(header)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(header))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).PaddingLeft(8).Foreground(ui.ColorMuted).Render(p.Description))
		b.WriteString("\n")
		if i < len(m.providers)-1 {
			b.WriteString("\n")
		}
	}

	b.WriteString(ui.ActionBarWidth(m.width,
		[]string{"[space] toggle", "[enter] continue"},
		[]string{"[esc] quit"},
	))

	return m.withStartupBanner(tea.NewView(b.String()))
}

func (m Model) dangerousModeView() tea.View {
	var b strings.Builder

	b.WriteString(ui.StyleTitle.Render("Dangerous mode"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(ui.ColorMuted).Render(
		"Do you want to skip permissions prompts and use 'dangerous mode' by default?"))
	b.WriteString("\n\n")

	line := ui.RenderCheckboxLabel(m.dangerousMode, "Use 'dangerous mode' by default")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(ui.StyleSelected.Render(line)))
	b.WriteString("\n")
	b.WriteString(ui.ActionBarWidth(m.width,
		[]string{"[space] toggle", "[enter] next"},
		[]string{"[esc] skip"},
	))

	return m.withStartupBanner(tea.NewView(b.String()))
}

func (m Model) withStartupBanner(v tea.View) tea.View {
	if !m.showBanner || v.Content == "" {
		return v
	}
	v.Content = ui.RenderBanner(ui.ViewOnboarding, ui.BannerOpts{Line1: "Welcome to Bossanova"}) + "\n" + v.Content
	return v
}

func (m *Model) SetWidth(width int)      { m.width = width }
func (m *Model) SetShowBanner(show bool) { m.showBanner = show }
