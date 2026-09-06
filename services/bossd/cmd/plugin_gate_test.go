package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/recurser/bossalib/config"
)

func pluginEnabled(t *testing.T, cfgs []config.PluginConfig, name string) bool {
	t.Helper()
	for _, c := range cfgs {
		if c.Name == name {
			return c.Enabled
		}
	}
	t.Fatalf("plugin %q missing from resolved list: %+v", name, cfgs)
	return false
}

// TestApplyExperimentalPluginGate_OverridesPersistedEnabled is the BOS-1145
// installed-base repair: those users' settings.json already carries
// {"name":"opencode","enabled":true} from a daemon that auto-enabled it, so the
// discovery default alone can never reach them. The gate must override the
// PERSISTED flag, not merely the default.
func TestApplyExperimentalPluginGate_OverridesPersistedEnabled(t *testing.T) {
	cfgs := []config.PluginConfig{
		{Name: "claude", Enabled: true},
		{Name: "opencode", Enabled: true},
	}
	settings := config.Settings{}

	got := applyExperimentalPluginGate(cfgs, &settings, false)

	if pluginEnabled(t, got, "opencode") {
		t.Error("opencode enabled = true, want false with an empty experimental_plugins")
	}
	if !pluginEnabled(t, got, "claude") {
		t.Error("claude enabled = false, want the non-member left untouched")
	}
	if cfgs[1].Enabled != true {
		t.Error("the caller's pre-gate slice was mutated; bossd still holds it for persistence decisions")
	}
}

func TestApplyExperimentalPluginGate_OptInKeepsPluginEnabled(t *testing.T) {
	cfgs := []config.PluginConfig{
		{Name: "claude", Enabled: true},
		{Name: "opencode", Enabled: true},
	}
	settings := config.Settings{ExperimentalPlugins: []string{"opencode"}}

	got := applyExperimentalPluginGate(cfgs, &settings, false)

	if !pluginEnabled(t, got, "opencode") {
		t.Error("opencode enabled = false, want true when experimental_plugins lists it")
	}
	if !pluginEnabled(t, got, "claude") {
		t.Error("claude enabled = false, want true")
	}
}

// TestApplyExperimentalPluginGate_SkipsExplicitOverride pins the E2E carve-out:
// the --plugins override loads an explicit list (including the NO_DISTRIBUTE
// stub-runner) and bypasses the filter chain, exactly as
// FilterNonDiscoverablePlugins is bypassed there.
func TestApplyExperimentalPluginGate_SkipsExplicitOverride(t *testing.T) {
	cfgs := []config.PluginConfig{{Name: "opencode", Enabled: true}}
	settings := config.Settings{}

	got := applyExperimentalPluginGate(cfgs, &settings, true)

	if !pluginEnabled(t, got, "opencode") {
		t.Error("opencode enabled = false, want the explicit --plugins list returned unchanged")
	}
}

func TestApplyExperimentalPluginGate_EmptyList(t *testing.T) {
	settings := config.Settings{}

	if got := applyExperimentalPluginGate(nil, &settings, false); len(got) != 0 {
		t.Errorf("got %d entries, want 0", len(got))
	}
}

// TestApplyExperimentalPluginGate_ClearsStaleDefaultAgent covers the risk the
// plan measured: resolveDefaultAgentName returns the configured default
// verbatim whenever more than one runner is loaded, and
// validateExplicitAgentName then rejects it as "agent %q is not loaded" — so a
// user who set default_agent: "opencode" while it was auto-enabled would lose
// session creation entirely, not just a preference. Clearing the stale value
// lets the existing backfill take over.
func TestApplyExperimentalPluginGate_ClearsStaleDefaultAgent(t *testing.T) {
	cases := []struct {
		name        string
		cfgs        []config.PluginConfig
		settings    config.Settings
		explicit    bool
		wantDefault string
	}{
		{
			name:        "gated-off plugin named as the default is cleared",
			cfgs:        []config.PluginConfig{{Name: "opencode", Enabled: true}},
			settings:    config.Settings{DefaultAgent: "opencode"},
			wantDefault: "",
		},
		{
			name: "an opted-in plugin stays the default",
			cfgs: []config.PluginConfig{{Name: "opencode", Enabled: true}},
			settings: config.Settings{
				DefaultAgent:        "opencode",
				ExperimentalPlugins: []string{"opencode"},
			},
			wantDefault: "opencode",
		},
		{
			name:        "a non-member default is never cleared",
			cfgs:        []config.PluginConfig{{Name: "claude", Enabled: true}, {Name: "opencode", Enabled: true}},
			settings:    config.Settings{DefaultAgent: "claude"},
			wantDefault: "claude",
		},
		{
			name:        "an already-disabled member named as the default is still cleared",
			cfgs:        []config.PluginConfig{{Name: "opencode", Enabled: false}},
			settings:    config.Settings{DefaultAgent: "opencode"},
			wantDefault: "",
		},
		{
			name:        "the explicit --plugins path leaves the default alone",
			cfgs:        []config.PluginConfig{{Name: "opencode", Enabled: true}},
			settings:    config.Settings{DefaultAgent: "opencode"},
			explicit:    true,
			wantDefault: "opencode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The gate persists a cleared default_agent, so every case has to be
			// pointed at a temp settings file or it would write the developer's own.
			t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(t.TempDir(), "settings.json"))
			settings := tc.settings
			applyExperimentalPluginGate(tc.cfgs, &settings, tc.explicit)
			if settings.DefaultAgent != tc.wantDefault {
				t.Errorf("DefaultAgent = %q, want %q", settings.DefaultAgent, tc.wantDefault)
			}
		})
	}
}

// TestApplyExperimentalPluginGate_PersistsClearedDefaultAgent is the assertion
// the in-memory one above cannot make. Server.resolveAgentName re-reads
// settings.json through config.Load on every create, so a clear that lives only
// in the daemon's struct leaves session creation resolving the gated-off agent
// exactly as before. Reading the value back through config.Load is what proves
// the readers that actually decide will see it; asserting settings.DefaultAgent
// would pass against that broken behaviour.
func TestApplyExperimentalPluginGate_PersistsClearedDefaultAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)

	settings := config.Settings{
		DefaultAgent: "opencode",
		Plugins:      []config.PluginConfig{{Name: "opencode", Enabled: true}},
	}
	if err := config.Save(settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	applyExperimentalPluginGate(settings.Plugins, &settings, false)

	reloaded, err := config.Load()
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	// config.Load backfills an empty default_agent to "claude"; either that or a
	// literal empty value proves the stale "opencode" is gone from disk. What
	// must not survive is the gated-off plugin's own name.
	if reloaded.DefaultAgent == "opencode" {
		t.Errorf("default_agent on disk = %q, want the stale gated-off agent cleared", reloaded.DefaultAgent)
	}
}

// TestApplyExperimentalPluginGate_DoesNotPersistGatedEnabledFlags pins the other
// half of the persistence rule: the default_agent write above must not smuggle
// the gate's forced plugins[].enabled values onto disk. The no-persist decision
// says an experimental plugin's stored flag is inert, not rewritten.
func TestApplyExperimentalPluginGate_DoesNotPersistGatedEnabledFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)

	settings := config.Settings{
		DefaultAgent: "opencode",
		Plugins:      []config.PluginConfig{{Name: "opencode", Enabled: true}},
	}
	if err := config.Save(settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	gated := applyExperimentalPluginGate(settings.Plugins, &settings, false)
	if pluginEnabled(t, gated, "opencode") {
		t.Fatalf("gate returned opencode enabled: %+v", gated)
	}

	reloaded, err := config.Load()
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if !pluginEnabled(t, reloaded.Plugins, "opencode") {
		t.Errorf("opencode's stored enabled flag was rewritten on disk; the gate must leave plugins[].enabled alone")
	}
}

// TestFinalizePluginConfigsOrder pins WHERE the gate runs. Ordering here has no
// runtime shape of its own — with real steps every order yields the same final
// slice — and the enclosing daemon boot is far too heavy to drive, so the
// sequence is asserted through the pipeline seam instead of by a human
// re-reading main.go. Dedup must run first so the gate sees one entry per
// plugin; verification must run last so it vets the list that will load.
func TestFinalizePluginConfigsOrder(t *testing.T) {
	var order []string
	record := func(step string) func([]config.PluginConfig, *config.Settings, bool) []config.PluginConfig {
		return func(cfgs []config.PluginConfig, _ *config.Settings, _ bool) []config.PluginConfig {
			order = append(order, step)
			return cfgs
		}
	}
	settings := config.Settings{}

	finalizePluginConfigs(nil, &settings, false, pluginPipelineDeps{
		dedup: record("dedup"),
		gate:  record("gate"),
		verify: func(cfgs []config.PluginConfig, _ bool) []config.PluginConfig {
			order = append(order, "verify")
			return cfgs
		},
	})

	if want := []string{"dedup", "gate", "verify"}; !slices.Equal(order, want) {
		t.Errorf("pipeline order = %v, want %v", order, want)
	}
}

// TestFinalizePluginConfigsVerifiesTheGatedList is the half the order alone
// cannot state: verification must receive the list the gate PRODUCED, so a
// plugin the gate turned off is what gets vetted and handed to the plugin host.
// A pipeline that verified the pre-gate slice would keep this ordering and
// still be wrong.
func TestFinalizePluginConfigsVerifiesTheGatedList(t *testing.T) {
	var seenByVerify []config.PluginConfig
	settings := config.Settings{}
	cfgs := []config.PluginConfig{{Name: "claude", Enabled: true}, {Name: "opencode", Enabled: true}}

	got := finalizePluginConfigs(cfgs, &settings, false, pluginPipelineDeps{
		dedup: dedupPluginConfigs,
		gate:  applyExperimentalPluginGate,
		verify: func(in []config.PluginConfig, _ bool) []config.PluginConfig {
			seenByVerify = in
			return in
		},
	})

	if pluginEnabled(t, seenByVerify, "opencode") {
		t.Error("verification saw opencode enabled; it must run on the GATED list")
	}
	if pluginEnabled(t, got, "opencode") {
		t.Error("opencode enabled = true in the final list")
	}
	if !pluginEnabled(t, got, "claude") {
		t.Error("claude enabled = false in the final list")
	}
}
