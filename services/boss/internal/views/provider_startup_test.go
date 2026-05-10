package views

import (
	"reflect"
	"testing"

	"github.com/recurser/bossalib/config"
)

func testProvider(name, plugin, command string) onboardingProvider {
	return onboardingProvider{Name: name, Plugin: plugin, Command: command}
}

func TestPlanProviderStartupBlocksWhenNoProviderCommandsAreInstalled(t *testing.T) {
	settings := config.Settings{
		DefaultAgent: "claude",
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "codex", Enabled: true},
			{Name: "linear", Enabled: true},
		},
	}

	plan := planProviderStartup(settings, nil, nil, []onboardingProvider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	})

	if plan.kind != providerStartupBlock {
		t.Fatalf("kind = %v, want providerStartupBlock", plan.kind)
	}
	assertPluginEnabled(t, plan.settings, "claude", false)
	assertPluginEnabled(t, plan.settings, "codex", false)
	assertPluginEnabled(t, plan.settings, "linear", true)
}

func TestPlanProviderStartupAutoEnablesSingleInstalledProvider(t *testing.T) {
	settings := config.Settings{
		DefaultAgent: "codex",
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: false},
			{Name: "codex", Enabled: true},
		},
	}
	discovered := []config.PluginConfig{
		{Name: "claude", Path: "/plugins/bossd-plugin-claude"},
		{Name: "codex", Path: "/plugins/bossd-plugin-codex"},
	}

	plan := planProviderStartup(settings, []string{"claude"}, discovered, []onboardingProvider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	})

	if plan.kind != providerStartupContinue {
		t.Fatalf("kind = %v, want providerStartupContinue", plan.kind)
	}
	if !plan.changed {
		t.Fatal("changed = false, want true")
	}
	if plan.settings.DefaultAgent != "claude" {
		t.Fatalf("DefaultAgent = %q, want claude", plan.settings.DefaultAgent)
	}
	if !reflect.DeepEqual(plan.settings.KnownAgentProviders, []string{"claude"}) {
		t.Fatalf("KnownAgentProviders = %v, want [claude]", plan.settings.KnownAgentProviders)
	}
	assertPluginEnabled(t, plan.settings, "claude", true)
	assertPluginPath(t, plan.settings, "claude", "/plugins/bossd-plugin-claude")
	assertPluginEnabled(t, plan.settings, "codex", false)
}

func TestPlanProviderStartupAddsDiscoveredProviderPluginEntries(t *testing.T) {
	settings := config.Settings{DefaultAgent: "claude"}
	discovered := []config.PluginConfig{
		{Name: "claude", Path: "/plugins/bossd-plugin-claude", Version: "1.2.3"},
		{Name: "codex", Path: "/plugins/bossd-plugin-codex", Version: "1.2.3"},
	}

	plan := planProviderStartup(settings, []string{"claude"}, discovered, []onboardingProvider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	})

	if plan.kind != providerStartupContinue {
		t.Fatalf("kind = %v, want providerStartupContinue", plan.kind)
	}
	assertPluginEnabled(t, plan.settings, "claude", true)
	assertPluginPath(t, plan.settings, "claude", "/plugins/bossd-plugin-claude")
	assertPluginEnabled(t, plan.settings, "codex", false)
	assertPluginPath(t, plan.settings, "codex", "/plugins/bossd-plugin-codex")
}

func TestPlanProviderStartupPromptsWhenInstalledProviderSetChanges(t *testing.T) {
	settings := config.Settings{
		DefaultAgent:          "claude",
		KnownAgentProviders:   []string{"claude"},
		ProvidersAcknowledged: true,
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "codex", Enabled: false},
		},
	}

	plan := planProviderStartup(settings, []string{"claude", "codex"}, nil, []onboardingProvider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	})

	if plan.kind != providerStartupPrompt {
		t.Fatalf("kind = %v, want providerStartupPrompt", plan.kind)
	}
	if len(plan.providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(plan.providers))
	}
	if !plan.preselected["claude"] {
		t.Fatalf("claude should be preselected; got %v", plan.preselected)
	}
	if plan.preselected["codex"] {
		t.Fatalf("codex should not be preselected; got %v", plan.preselected)
	}
}

func TestPlanProviderStartupContinuesWhenMultipleInstalledProvidersAreUnchanged(t *testing.T) {
	settings := config.Settings{
		DefaultAgent:        "claude",
		KnownAgentProviders: []string{"claude", "codex"},
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "codex", Enabled: false},
		},
	}

	plan := planProviderStartup(settings, []string{"codex", "claude"}, nil, []onboardingProvider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	})

	if plan.kind != providerStartupContinue {
		t.Fatalf("kind = %v, want providerStartupContinue", plan.kind)
	}
	if plan.changed {
		t.Fatal("changed = true, want false")
	}
}

func assertPluginEnabled(t *testing.T, settings config.Settings, name string, want bool) {
	t.Helper()
	for _, p := range settings.Plugins {
		if p.Name == name {
			if p.Enabled != want {
				t.Fatalf("%s Enabled = %v, want %v; plugins=%+v", name, p.Enabled, want, settings.Plugins)
			}
			return
		}
	}
	t.Fatalf("plugin %q not found in %+v", name, settings.Plugins)
}

func assertPluginPath(t *testing.T, settings config.Settings, name, want string) {
	t.Helper()
	for _, p := range settings.Plugins {
		if p.Name == name {
			if p.Path != want {
				t.Fatalf("%s Path = %q, want %q; plugins=%+v", name, p.Path, want, settings.Plugins)
			}
			return
		}
	}
	t.Fatalf("plugin %q not found in %+v", name, settings.Plugins)
}
