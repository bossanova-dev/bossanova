package views

import (
	"os/exec"
	"reflect"
	"slices"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/loginshell"
)

type providerStartupKind int

const (
	providerStartupContinue providerStartupKind = iota
	providerStartupBlock
	providerStartupPrompt
)

type providerStartupPlan struct {
	kind        providerStartupKind
	providers   []onboardingProvider
	preselected map[string]bool
	settings    config.Settings
	changed     bool
}

type ProviderStartupResult struct {
	LoginShellChanged bool
	SettingsChanged   bool
}

var providerLookPath = exec.LookPath

// RunProviderStartupIfNeeded reconciles local provider CLIs with the plugin
// settings before the daemon is auto-started.
func RunProviderStartupIfNeeded() (ProviderStartupResult, error) {
	var result ProviderStartupResult
	settings, err := config.Load()
	if err != nil {
		return result, err
	}
	if next, changed := captureLoginShell(settings, loginshell.Detect); changed {
		result.LoginShellChanged = true
		result.SettingsChanged = true
		settings = next
		if err := config.Save(settings); err != nil {
			return result, err
		}
	}
	discovered := config.DiscoverPlugins()
	installed := detectInstalledProviderPlugins(onboardingProviders, providerLookPath)
	plan := planProviderStartup(settings, installed, discovered, onboardingProviders)

	switch plan.kind {
	case providerStartupContinue:
		if plan.changed {
			result.SettingsChanged = true
			return result, config.Save(plan.settings)
		}
		return result, nil
	case providerStartupBlock:
		if plan.changed {
			if err := config.Save(plan.settings); err != nil {
				return result, err
			}
			result.SettingsChanged = true
		}
		model := NewProviderInstallRequiredModel(onboardingProviders)
		model.showBanner = true
		p := tea.NewProgram(model)
		if _, err := p.Run(); err != nil {
			return result, err
		}
		return result, errPreflightCancelled
	case providerStartupPrompt:
		model := NewProviderSelectionModel(plan.providers, plan.preselected)
		model.showBanner = true
		p := tea.NewProgram(model)
		final, err := p.Run()
		if err != nil {
			return result, err
		}
		finalModel, ok := final.(OnboardingModel)
		if !ok || finalModel.Cancelled() {
			return result, errPreflightCancelled
		}
		selected := finalModel.SelectedPlugins()
		next, changed := applyProviderOnboarding(settings, installed, selected, finalModel.DangerousModeDefault(), discovered, onboardingProviders)
		if changed {
			result.SettingsChanged = true
			return result, config.Save(next)
		}
		return result, nil
	default:
		return result, nil
	}
}

// captureLoginShell records the user's interactive shell into settings so the
// daemon can launch agents through it. Pure for testability; detect supplies
// $SHELL. Returns the (possibly) updated settings and whether it changed.
func captureLoginShell(s config.Settings, detect func() string) (config.Settings, bool) {
	sh := detect()
	if sh == "" || s.LoginShell == sh {
		return s, false
	}
	s.LoginShell = sh
	return s, true
}

func detectInstalledProviderPlugins(providers []onboardingProvider, lookPath func(string) (string, error)) []string {
	installed := make([]string, 0, len(providers))
	for _, p := range providers {
		if _, err := lookPath(p.Command); err == nil {
			installed = append(installed, p.Plugin)
		}
	}
	return installed
}

func planProviderStartup(settings config.Settings, installedPlugins []string, discovered []config.PluginConfig, providers []onboardingProvider) providerStartupPlan {
	installed := normalizeProviderPlugins(installedPlugins, providers)
	if len(installed) == 0 {
		next, changed := applyProviderSelection(settings, nil, nil, discovered, providers)
		return providerStartupPlan{kind: providerStartupBlock, settings: next, changed: changed}
	}
	if len(installed) == 1 {
		next, changed := applyProviderSelection(settings, installed, installed, discovered, providers)
		return providerStartupPlan{kind: providerStartupContinue, settings: next, changed: changed}
	}

	known := normalizeProviderPlugins(settings.KnownAgentProviders, providers)
	enabled := enabledInstalledProviders(settings, installed, providers)
	if !settings.ProvidersAcknowledged || !slices.Equal(known, installed) || len(enabled) == 0 {
		return providerStartupPlan{
			kind:        providerStartupPrompt,
			providers:   providersForPlugins(installed, providers),
			preselected: preselectedProviders(settings, installed),
			settings:    settings,
		}
	}
	return providerStartupPlan{kind: providerStartupContinue, settings: settings}
}

func applyProviderOnboarding(settings config.Settings, installedPlugins, enabledPlugins []string, dangerousModeDefault bool, discovered []config.PluginConfig, providers []onboardingProvider) (config.Settings, bool) {
	next, _ := applyProviderSelection(settings, installedPlugins, enabledPlugins, discovered, providers)
	if dangerousModeDefault {
		config.SetPluginConfigBool(&next, "claude", "dangerously_skip_permissions", true)
		config.SetPluginConfigBool(&next, "codex", "dangerously_bypass_approvals_and_sandbox", true)
	}
	return next, !reflect.DeepEqual(settings, next)
}

func applyProviderSelection(settings config.Settings, installedPlugins, enabledPlugins []string, discovered []config.PluginConfig, providers []onboardingProvider) (config.Settings, bool) {
	next := settings
	installed := normalizeProviderPlugins(installedPlugins, providers)
	enabled := normalizeProviderPlugins(enabledPlugins, providers)
	enabledSet := stringSet(enabled)
	discoveredByName := pluginConfigByName(discovered)

	for _, p := range providers {
		idx := pluginIndex(next.Plugins, p.Plugin)
		discoveredCfg, discoveredOK := discoveredByName[p.Plugin]
		if idx == -1 && !discoveredOK {
			continue
		}
		if idx == -1 {
			next.Plugins = append(next.Plugins, config.PluginConfig{Name: p.Plugin})
			idx = len(next.Plugins) - 1
		}
		if discoveredOK && discoveredCfg.Path != "" {
			next.Plugins[idx].Path = discoveredCfg.Path
			next.Plugins[idx].Version = discoveredCfg.Version
		}
		next.Plugins[idx].Enabled = enabledSet[p.Plugin]
	}

	if len(enabled) > 0 && !enabledSet[next.DefaultAgent] {
		next.DefaultAgent = enabled[0]
	}
	next.KnownAgentProviders = installed
	if len(installed) > 0 {
		next.ProvidersAcknowledged = true
	}
	return next, !reflect.DeepEqual(settings, next)
}

func normalizeProviderPlugins(plugins []string, providers []onboardingProvider) []string {
	set := stringSet(plugins)
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		if set[p.Plugin] {
			out = append(out, p.Plugin)
		}
	}
	return out
}

func enabledInstalledProviders(settings config.Settings, installed []string, providers []onboardingProvider) []string {
	installedSet := stringSet(installed)
	enabledSet := map[string]bool{}
	for _, p := range settings.Plugins {
		if p.Enabled {
			enabledSet[p.Name] = true
		}
	}
	out := make([]string, 0, len(installed))
	for _, p := range providers {
		if installedSet[p.Plugin] && enabledSet[p.Plugin] {
			out = append(out, p.Plugin)
		}
	}
	return out
}

func providersForPlugins(plugins []string, providers []onboardingProvider) []onboardingProvider {
	set := stringSet(plugins)
	out := make([]onboardingProvider, 0, len(plugins))
	for _, p := range providers {
		if set[p.Plugin] {
			out = append(out, p)
		}
	}
	return out
}

func preselectedProviders(settings config.Settings, installed []string) map[string]bool {
	installedSet := stringSet(installed)
	out := make(map[string]bool)
	for _, p := range settings.Plugins {
		if p.Enabled && installedSet[p.Name] {
			out[p.Name] = true
		}
	}
	return out
}

func pluginConfigByName(plugins []config.PluginConfig) map[string]config.PluginConfig {
	out := make(map[string]config.PluginConfig, len(plugins))
	for _, p := range plugins {
		out[p.Name] = p
	}
	return out
}

func pluginIndex(plugins []config.PluginConfig, name string) int {
	for i, p := range plugins {
		if p.Name == name {
			return i
		}
	}
	return -1
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}
