package home

import (
	"reflect"
	"slices"

	"github.com/recurser/boss/api/tui/ui"

	"github.com/recurser/bossalib/kit/config"
)

type providerStartupKind int

const (
	ProviderStartupContinue providerStartupKind = iota
	ProviderStartupBlock
	ProviderStartupPrompt
)

type providerStartupPlan struct {
	Kind        providerStartupKind
	Providers   []ui.Provider
	Preselected map[string]bool
	Settings    config.Settings
	Changed     bool
}

type ProviderStartupResult struct {
	LoginShellChanged bool
	SettingsChanged   bool
}

// CaptureLoginShell records the user's interactive shell into Settings so the
// daemon can launch agents through it. Pure for testability; detect supplies
// $SHELL. Returns the (possibly) updated Settings and whether it Changed.
func CaptureLoginShell(s config.Settings, detect func() string) (config.Settings, bool) {
	sh := detect()
	if sh == "" || s.LoginShell == sh {
		return s, false
	}
	s.LoginShell = sh
	return s, true
}

func DetectInstalledProviderPlugins(providers []ui.Provider, lookPath func(string) (string, error)) []string {
	installed := make([]string, 0, len(providers))
	for _, p := range providers {
		if _, err := lookPath(p.Command); err == nil {
			installed = append(installed, p.Plugin)
		}
	}
	return installed
}

func PlanProviderStartup(settings config.Settings, installedPlugins []string, discovered []config.PluginConfig, providers []ui.Provider) providerStartupPlan {
	installed := normalizeProviderPlugins(installedPlugins, providers)
	if len(installed) == 0 {
		next, changed := applyProviderSelection(settings, nil, nil, discovered, providers)
		return providerStartupPlan{Kind: ProviderStartupBlock, Settings: next, Changed: changed}
	}
	if len(installed) == 1 {
		next, changed := applyProviderSelection(settings, installed, installed, discovered, providers)
		return providerStartupPlan{Kind: ProviderStartupContinue, Settings: next, Changed: changed}
	}

	known := normalizeProviderPlugins(settings.KnownAgentProviders, providers)
	enabled := enabledInstalledProviders(settings, installed, providers)
	if !settings.ProvidersAcknowledged || !slices.Equal(known, installed) || len(enabled) == 0 {
		return providerStartupPlan{
			Kind:        ProviderStartupPrompt,
			Providers:   providersForPlugins(installed, providers),
			Preselected: preselectedProviders(settings, installed),
			Settings:    settings,
		}
	}
	return providerStartupPlan{Kind: ProviderStartupContinue, Settings: settings}
}

func ApplyProviderOnboarding(settings config.Settings, installedPlugins, enabledPlugins []string, dangerousModeDefault bool, discovered []config.PluginConfig, providers []ui.Provider) (config.Settings, bool) {
	next, _ := applyProviderSelection(settings, installedPlugins, enabledPlugins, discovered, providers)
	if dangerousModeDefault {
		config.SetPluginConfigBool(&next, "claude", "dangerously_skip_permissions", true)
		config.SetPluginConfigBool(&next, "codex", "dangerously_bypass_approvals_and_sandbox", true)
	}
	return next, !reflect.DeepEqual(settings, next)
}

func applyProviderSelection(settings config.Settings, installedPlugins, enabledPlugins []string, discovered []config.PluginConfig, providers []ui.Provider) (config.Settings, bool) {
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

func normalizeProviderPlugins(plugins []string, providers []ui.Provider) []string {
	set := stringSet(plugins)
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		if set[p.Plugin] {
			out = append(out, p.Plugin)
		}
	}
	return out
}

func enabledInstalledProviders(settings config.Settings, installed []string, providers []ui.Provider) []string {
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

func providersForPlugins(plugins []string, providers []ui.Provider) []ui.Provider {
	set := stringSet(plugins)
	out := make([]ui.Provider, 0, len(plugins))
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
