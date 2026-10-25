package tui

import (
	"os/exec"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/home"
	"github.com/recurser/bossalib/contract/plugin/loginshell"
	"github.com/recurser/bossalib/kit/config"
)

var providerLookPath = exec.LookPath

// RunProviderStartupIfNeeded reconciles local provider CLIs with the plugin
// settings before the daemon is auto-started.
func RunProviderStartupIfNeeded() (home.ProviderStartupResult, error) {
	var result home.ProviderStartupResult
	settings, err := config.Load()
	if err != nil {
		return result, err
	}
	if next, changed := home.CaptureLoginShell(settings, loginshell.Detect); changed {
		result.LoginShellChanged = true
		result.SettingsChanged = true
		settings = next
		if err := config.Save(settings); err != nil {
			return result, err
		}
	}
	discovered := config.DiscoverPlugins()
	installed := home.DetectInstalledProviderPlugins(onboardingProviders, providerLookPath)
	plan := home.PlanProviderStartup(settings, installed, discovered, onboardingProviders)

	switch plan.Kind {
	case home.ProviderStartupContinue:
		if plan.Changed {
			result.SettingsChanged = true
			return result, config.Save(plan.Settings)
		}
		return result, nil
	case home.ProviderStartupBlock:
		if plan.Changed {
			if err := config.Save(plan.Settings); err != nil {
				return result, err
			}
			result.SettingsChanged = true
		}
		model := NewProviderInstallRequiredModel(onboardingProviders)
		model.SetShowBanner(true)
		p := tea.NewProgram(model)
		if _, err := p.Run(); err != nil {
			return result, err
		}
		return result, home.ErrPreflightCancelled
	case home.ProviderStartupPrompt:
		model := NewProviderSelectionModel(plan.Providers, plan.Preselected)
		model.SetShowBanner(true)
		p := tea.NewProgram(model)
		final, err := p.Run()
		if err != nil {
			return result, err
		}
		finalModel, ok := final.(OnboardingModel)
		if !ok || finalModel.Cancelled() {
			return result, home.ErrPreflightCancelled
		}
		selected := finalModel.SelectedPlugins()
		next, changed := home.ApplyProviderOnboarding(settings, installed, selected, finalModel.DangerousModeDefault(), discovered, onboardingProviders)
		if changed {
			result.SettingsChanged = true
			return result, config.Save(next)
		}
		return result, nil
	default:
		return result, nil
	}
}
