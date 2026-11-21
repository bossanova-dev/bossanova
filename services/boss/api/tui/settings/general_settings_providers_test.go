package settings

import (
	"context"
	"reflect"
	"testing"

	"github.com/recurser/bossalib/kit/config"
)

// An unloaded antigravity plugin, as on a client driving a daemon that has not
// started it, still gets its settings rows in provider order.
func TestGeneralSettings_RendersUnloadedAntigravityAfterClaudeAndCodex(t *testing.T) {
	withTempConfigHome(t)
	settings := config.DefaultSettings()
	settings.DefaultAgent = "claude"
	settings.KnownAgentProviders = []string{"antigravity", "claude", "codex"}
	settings.Plugins = []config.PluginConfig{
		{Name: "antigravity", Enabled: true},
		{Name: "claude", Enabled: true},
		{Name: "codex", Enabled: true},
	}
	if err := config.Save(settings); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m := NewGeneralSettingsModel(&settingsAgentStub{}, context.Background(), nil)

	var names []string
	for _, agent := range m.agents {
		names = append(names, agent.Name)
	}
	if want := []string{"claude", "codex", "antigravity"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("agents = %v, want %v", names, want)
	}
	var keys []string
	for _, setting := range m.agents[2].UserSettings {
		keys = append(keys, setting.Key)
	}
	if want := []string{"dangerously_skip_permissions", "mode", "model"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("antigravity settings = %v, want %v", keys, want)
	}
}
