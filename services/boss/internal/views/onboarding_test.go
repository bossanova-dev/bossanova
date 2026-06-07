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
		"Choose the installed agents you want Bossanova to use.",
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
	if strings.Contains(out, "OpenAI's Codex CLI\n\n  [space]") {
		t.Errorf("provider selection should leave only one newline before action bar:\n%s", out)
	}
}

func TestStandaloneOnboardingShowsBannerOnProviderSelection(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, nil)
	m.showBanner = true
	out := m.View().Content

	for _, want := range []string{"Welcome to Bossanova", "Choose providers to enable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("standalone onboarding missing %q in:\n%s", want, out)
		}
	}
}

func TestStandaloneOnboardingShowsBannerOnDangerousMode(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, nil)
	m.showBanner = true
	m.step = onboardingStepDangerousMode
	out := m.View().Content

	for _, want := range []string{"Welcome to Bossanova", "Dangerous mode"} {
		if !strings.Contains(out, want) {
			t.Fatalf("standalone dangerous-mode onboarding missing %q in:\n%s", want, out)
		}
	}
}

func TestOnboardingDefaultsAvailableProvidersOn(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, map[string]bool{"claude": true})
	out := m.View().Content

	for _, want := range []string{
		"[x] Claude",
		"[x] Codex",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default provider selection missing %q in:\n%s", want, out)
		}
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

	// Cursor down to row 1 (Codex), then press space to turn it off.
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	m = updated.(OnboardingModel)
	updated, _ = m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(OnboardingModel)

	if m.selected[1] {
		t.Errorf("expected Codex (index 1) to be deselected; selected map = %v", m.selected)
	}

	out := m.View().Content
	if !strings.Contains(out, "[ ] Codex") {
		t.Errorf("Codex not toggled off in view:\n%s", out)
	}
}

func TestOnboardingDoesNotAdvanceWithNoSelectedProviders(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, map[string]bool{
		"claude": false,
		"codex":  false,
	})

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(OnboardingModel)

	if m.Done() || m.step == onboardingStepDangerousMode {
		t.Fatalf("onboarding advanced with no selected providers: %+v", m)
	}
	out := m.View().Content
	if !strings.Contains(out, "select at least one provider") {
		t.Fatalf("missing no-provider validation error in:\n%s", out)
	}
}

func TestOnboardingDoneReturnsSelectedPlugins(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, nil)
	// Providers default on; advance to dangerous-mode prompt, then press enter on Next.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(OnboardingModel)
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(OnboardingModel)

	if !m.Done() {
		t.Fatalf("expected Done() = true after enter; m = %+v", m)
	}
	got := m.SelectedPlugins()
	if len(got) != 2 || got[0] != "claude" || got[1] != "codex" {
		t.Fatalf("SelectedPlugins() = %v, want [claude codex]", got)
	}
}

func TestOnboardingProviderSelectionAdvancesToDangerousModePrompt(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, nil)
	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(OnboardingModel)

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(OnboardingModel)

	if m.Done() {
		t.Fatal("expected onboarding to wait for dangerous-mode prompt before Done()")
	}
	out := m.View().Content
	for _, want := range []string{
		"Do you want to skip permissions prompts and use 'dangerous mode' by default?",
		"[ ] Use 'dangerous mode' by default",
		"[enter] next",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dangerous-mode prompt missing %q in:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"[ Next ]", cursorChevron + " [ ] Use 'dangerous mode' by default"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("dangerous-mode prompt contains %q in:\n%s", unwanted, out)
		}
	}
}

func TestOnboardingDangerousModePromptTogglesDefault(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, nil)
	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(OnboardingModel)
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(OnboardingModel)

	updated, _ = m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(OnboardingModel)

	if !m.DangerousModeDefault() {
		t.Fatal("DangerousModeDefault() = false, want true")
	}
	out := m.View().Content
	if !strings.Contains(out, "[x] Use 'dangerous mode' by default") {
		t.Fatalf("checked dangerous-mode checkbox not rendered:\n%s", out)
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

func TestOnboardingProviderSelectionActionBarLabelsEscAsQuit(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, nil)
	out := m.View().Content
	if !strings.Contains(out, "[esc] quit") {
		t.Errorf("provider selection should label esc as quit (it discards and exits):\n%s", out)
	}
}

func TestOnboardingDangerousModeEscSkipsWithoutCancel(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, nil)
	// Advance from provider selection to the dangerous-mode prompt.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(OnboardingModel)
	if m.step != onboardingStepDangerousMode {
		t.Fatalf("expected dangerous-mode step, got %v", m.step)
	}
	// The action bar advertises "[esc] skip"; esc must skip forward (finish
	// onboarding with dangerous mode off) rather than cancel and discard the
	// provider selection just made.
	if !strings.Contains(m.View().Content, "[esc] skip") {
		t.Fatalf("dangerous-mode prompt should label esc as skip:\n%s", m.View().Content)
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc, Text: "esc"})
	m = updated.(OnboardingModel)

	if m.Cancelled() {
		t.Error("esc on dangerous-mode step should not cancel onboarding")
	}
	if !m.Done() {
		t.Error("esc on dangerous-mode step should finish onboarding")
	}
	if m.DangerousModeDefault() {
		t.Error("skipping dangerous mode should leave it disabled")
	}
	if got := m.SelectedPlugins(); len(got) != 2 {
		t.Errorf("provider selection should be preserved after skip, got %v", got)
	}
}

func TestOnboardingDangerousModeQuitCancels(t *testing.T) {
	m := NewProviderSelectionModel(onboardingProviders, nil)
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(OnboardingModel)
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	m = updated.(OnboardingModel)

	if !m.Cancelled() {
		t.Error("q on dangerous-mode step should cancel onboarding")
	}
	if m.Done() {
		t.Error("q on dangerous-mode step should not mark onboarding done")
	}
}
