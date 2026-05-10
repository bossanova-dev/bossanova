package views

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestOnboardingShowsProviderListWithInstallInstructions(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, nil)
	out := m.View().Content
	for _, want := range []string{
		"Choose providers",
		"Choose the installed agent runners you want Bossanova to use.",
		"Claude",
		"Codex",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("onboarding view missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "install:") {
		t.Errorf("provider selection should not show install commands:\n%s", out)
	}
	if strings.Contains(out, "Pick the agent runners you want to install") {
		t.Errorf("onboarding view implies it installs providers automatically:\n%s", out)
	}
}

func TestOnboardingInstallRequiredShowsInstallInstructions(t *testing.T) {
	m := NewProviderInstallRequiredModel(onboardingProviders)
	out := m.View().Content

	for _, want := range []string{
		"Install an agent provider",
		"Bossanova needs at least one supported agent CLI on your PATH.",
		"install: npm install -g @anthropic-ai/claude-code",
		"https://docs.claude.com/en/docs/claude-code/overview",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("install-required view missing %q in:\n%s", want, out)
		}
	}
}

func TestOnboardingTogglingProviderUpdatesSelection(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, nil)

	// Cursor down to row 1 (Codex), then press space.
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	m = updated.(OnboardingModel)
	updated, _ = m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(OnboardingModel)

	if !m.selected[1] {
		t.Errorf("expected Codex (index 1) to be selected; selected map = %v", m.selected)
	}

	out := m.View().Content
	if !strings.Contains(out, "[x] Codex") {
		t.Errorf("Codex not toggled on in view:\n%s", out)
	}
}

func TestOnboardingDoneReturnsSelectedPlugins(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, nil)
	// Select claude (cursor starts at 0) for realism, then press enter.
	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(OnboardingModel)
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(OnboardingModel)

	if !m.Done() {
		t.Fatalf("expected Done() = true after enter; m = %+v", m)
	}
	got := m.SelectedPlugins()
	if len(got) != 1 || got[0] != "claude" {
		t.Fatalf("SelectedPlugins() = %v, want [claude]", got)
	}
}

func TestOnboardingEscMarksCancel(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, nil)
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc, Text: "esc"})
	m = updated.(OnboardingModel)
	if !m.Cancelled() {
		t.Error("expected Cancelled() = true after esc")
	}
}
