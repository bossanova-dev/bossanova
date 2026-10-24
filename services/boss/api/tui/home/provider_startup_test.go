package home

import (
	"os"
	"reflect"
	"testing"

	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/bossalib/config"
)

func testProvider(name, plugin, command string) ui.Provider {
	return ui.Provider{Name: name, Plugin: plugin, Command: command}
}

func TestCaptureLoginShell_WritesEnvShell(t *testing.T) {
	t.Setenv("SHELL", "/opt/homebrew/bin/fish")
	s := config.Settings{}
	got, changed := CaptureLoginShell(s, func() string { return os.Getenv("SHELL") })
	if !changed || got.LoginShell != "/opt/homebrew/bin/fish" {
		t.Fatalf("expected login_shell captured, got %q Changed=%v", got.LoginShell, changed)
	}
	// idempotent
	if _, changed2 := CaptureLoginShell(got, func() string { return os.Getenv("SHELL") }); changed2 {
		t.Fatalf("second capture should be a no-op")
	}
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

	plan := PlanProviderStartup(settings, nil, nil, []ui.Provider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	})

	if plan.Kind != ProviderStartupBlock {
		t.Fatalf("Kind = %v, want ProviderStartupBlock", plan.Kind)
	}
	assertPluginEnabled(t, plan.Settings, "claude", false)
	assertPluginEnabled(t, plan.Settings, "codex", false)
	assertPluginEnabled(t, plan.Settings, "linear", true)
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

	plan := PlanProviderStartup(settings, []string{"claude"}, discovered, []ui.Provider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	})

	if plan.Kind != ProviderStartupContinue {
		t.Fatalf("Kind = %v, want ProviderStartupContinue", plan.Kind)
	}
	if !plan.Changed {
		t.Fatal("Changed = false, want true")
	}
	if plan.Settings.DefaultAgent != "claude" {
		t.Fatalf("DefaultAgent = %q, want claude", plan.Settings.DefaultAgent)
	}
	if !reflect.DeepEqual(plan.Settings.KnownAgentProviders, []string{"claude"}) {
		t.Fatalf("KnownAgentProviders = %v, want [claude]", plan.Settings.KnownAgentProviders)
	}
	if !plan.Settings.ProvidersAcknowledged {
		t.Fatal("ProvidersAcknowledged = false, want true")
	}
	assertPluginEnabled(t, plan.Settings, "claude", true)
	assertPluginPath(t, plan.Settings, "claude", "/plugins/bossd-plugin-claude")
	assertPluginEnabled(t, plan.Settings, "codex", false)
}

func TestPlanProviderStartupAddsDiscoveredProviderPluginEntries(t *testing.T) {
	settings := config.Settings{DefaultAgent: "claude"}
	discovered := []config.PluginConfig{
		{Name: "claude", Path: "/plugins/bossd-plugin-claude", Version: "1.2.3"},
		{Name: "codex", Path: "/plugins/bossd-plugin-codex", Version: "1.2.3"},
	}

	plan := PlanProviderStartup(settings, []string{"claude"}, discovered, []ui.Provider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	})

	if plan.Kind != ProviderStartupContinue {
		t.Fatalf("Kind = %v, want ProviderStartupContinue", plan.Kind)
	}
	assertPluginEnabled(t, plan.Settings, "claude", true)
	assertPluginPath(t, plan.Settings, "claude", "/plugins/bossd-plugin-claude")
	assertPluginEnabled(t, plan.Settings, "codex", false)
	assertPluginPath(t, plan.Settings, "codex", "/plugins/bossd-plugin-codex")
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

	plan := PlanProviderStartup(settings, []string{"claude", "codex"}, nil, []ui.Provider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	})

	if plan.Kind != ProviderStartupPrompt {
		t.Fatalf("Kind = %v, want ProviderStartupPrompt", plan.Kind)
	}
	if len(plan.Providers) != 2 {
		t.Fatalf("Providers = %d, want 2", len(plan.Providers))
	}
	if !plan.Preselected["claude"] {
		t.Fatalf("claude should be Preselected; got %v", plan.Preselected)
	}
	if plan.Preselected["codex"] {
		t.Fatalf("codex should not be Preselected; got %v", plan.Preselected)
	}
}

func TestPlanProviderStartupPromptsWhenProvidersNotAcknowledged(t *testing.T) {
	settings := config.Settings{
		DefaultAgent:          "claude",
		KnownAgentProviders:   []string{"claude", "codex"},
		ProvidersAcknowledged: false,
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "codex", Enabled: true},
		},
	}

	plan := PlanProviderStartup(settings, []string{"claude", "codex"}, nil, []ui.Provider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	})

	if plan.Kind != ProviderStartupPrompt {
		t.Fatalf("Kind = %v, want ProviderStartupPrompt", plan.Kind)
	}
	if !plan.Preselected["claude"] || !plan.Preselected["codex"] {
		t.Fatalf("all enabled Providers should be Preselected; got %v", plan.Preselected)
	}
}

func TestPlanProviderStartupContinuesWhenMultipleInstalledProvidersAreUnchanged(t *testing.T) {
	settings := config.Settings{
		DefaultAgent:          "claude",
		KnownAgentProviders:   []string{"claude", "codex"},
		ProvidersAcknowledged: true,
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "codex", Enabled: false},
		},
	}

	plan := PlanProviderStartup(settings, []string{"codex", "claude"}, nil, []ui.Provider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	})

	if plan.Kind != ProviderStartupContinue {
		t.Fatalf("Kind = %v, want ProviderStartupContinue", plan.Kind)
	}
	if plan.Changed {
		t.Fatal("Changed = true, want false")
	}
}

func TestApplyProviderOnboardingAcknowledgesDefaultSelection(t *testing.T) {
	settings := config.Settings{
		DefaultAgent:          "claude",
		KnownAgentProviders:   []string{"claude", "codex"},
		ProvidersAcknowledged: false,
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "codex", Enabled: true},
		},
	}
	providers := []ui.Provider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	}

	got, changed := ApplyProviderOnboarding(settings, []string{"claude", "codex"}, []string{"claude", "codex"}, false, nil, providers)

	if !changed {
		t.Fatal("Changed = false, want true")
	}
	if !got.ProvidersAcknowledged {
		t.Fatal("ProvidersAcknowledged = false, want true")
	}
}

func TestApplyProviderOnboardingDangerousModeEnablesClaudeAndCodex(t *testing.T) {
	settings := config.Settings{DefaultAgent: "claude"}
	discovered := []config.PluginConfig{
		{Name: "claude", Path: "/plugins/bossd-plugin-claude"},
		{Name: "codex", Path: "/plugins/bossd-plugin-codex"},
	}
	providers := []ui.Provider{
		testProvider("Claude", "claude", "claude"),
		testProvider("Codex", "codex", "codex"),
	}

	got, changed := ApplyProviderOnboarding(settings, []string{"claude", "codex"}, []string{"claude"}, true, discovered, providers)

	if !changed {
		t.Fatal("Changed = false, want true")
	}
	if !config.PluginConfigBool(&got, "claude", "dangerously_skip_permissions") {
		t.Fatalf("claude dangerously_skip_permissions not enabled: %+v", got.Plugins)
	}
	if !config.PluginConfigBool(&got, "codex", "dangerously_bypass_approvals_and_sandbox") {
		t.Fatalf("codex dangerously_bypass_approvals_and_sandbox not enabled: %+v", got.Plugins)
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
