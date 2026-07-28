package views

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
)

func TestGeneralSettings_RendersBuiltInRowsWithoutAgents(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	out := m.View().Content
	for _, want := range []string{
		"Worktree base directory",
		"Poll interval",
		"tracing",
		"Enable event tracing (for debugging problems)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("settings missing %q in:\n%s", want, out)
		}
	}
	for _, hidden := range []string{"PostHog project token", "PostHog host", "set when tracing is enabled"} {
		if strings.Contains(out, hidden) {
			t.Errorf("settings unexpectedly showed %q in:\n%s", hidden, out)
		}
	}
}

func TestGeneralSettings_NotificationsTogglePersistsAndRenders(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())

	idx := -1
	for i, row := range m.rows {
		if row.Label == "Enable desktop notifications for questions" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("desktop notifications row not found")
	}
	m.cursor = idx

	if !config.NotificationsEnabled(m.settings) {
		t.Fatal("precondition: desktop notifications should default to enabled")
	}
	if !strings.Contains(m.View().Content, "[x] Enable desktop notifications for questions") {
		t.Fatalf("desktop notifications should render checked by default. Got:\n%s", m.View().Content)
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(GeneralSettingsModel)
	if config.NotificationsEnabled(m.settings) {
		t.Error("space did not disable desktop notifications")
	}
	if !strings.Contains(m.View().Content, "[ ] Enable desktop notifications for questions") {
		t.Fatalf("desktop notifications should render unchecked after toggle. Got:\n%s", m.View().Content)
	}
	persisted, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if config.NotificationsEnabled(persisted) {
		t.Error("disabled desktop notifications were not persisted")
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)
	if !config.NotificationsEnabled(m.settings) {
		t.Error("enter did not re-enable desktop notifications")
	}
	if !strings.Contains(m.View().Content, "[x] Enable desktop notifications for questions") {
		t.Fatalf("desktop notifications should render checked after second toggle. Got:\n%s", m.View().Content)
	}
}

func TestGeneralSettings_SuccessfulOtherSaveClearsFailedNotificationSaveError(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())

	for i, row := range m.rows {
		if row.Kind == settingsRowKindNotifications {
			m.cursor = i
			break
		}
	}
	badPath := filepath.Join(t.TempDir(), "settings-dir")
	if err := os.Mkdir(badPath, 0o755); err != nil {
		t.Fatalf("os.Mkdir(%q): %v", badPath, err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", badPath)
	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(GeneralSettingsModel)
	if m.err == nil {
		t.Fatal("failed notification save did not retain an error")
	}
	if config.NotificationsEnabled(m.settings) {
		t.Fatal("failed notification save did not retain the requested toggle")
	}

	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(t.TempDir(), "settings.json"))
	for i, row := range m.rows {
		if row.Kind == settingsRowKindErrorTracking {
			m.cursor = i
			break
		}
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)
	if m.err != nil {
		t.Fatalf("successful non-notification save retained stale error: %v", m.err)
	}

	persisted, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if config.NotificationsEnabled(persisted) {
		t.Fatal("successful other save did not persist notification toggle")
	}
	if !persisted.ErrorTrackingEnabled {
		t.Fatal("successful other save did not persist its own setting")
	}
}

func TestGeneralSettings_RendersErrorTrackingRow(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	view := m.View().Content
	if !strings.Contains(view, "Enable error tracking") {
		t.Errorf("settings view missing error tracking row.\nGot:\n%s", view)
	}
}

func TestGeneralSettings_ErrorTrackingToggle(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	var idx = -1
	for i, r := range m.rows {
		if r.Kind == settingsRowKindErrorTracking {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("settingsRowKindErrorTracking row not found")
	}
	m.cursor = idx

	if m.settings.ErrorTrackingEnabled {
		t.Fatalf("precondition: ErrorTrackingEnabled should default to false")
	}

	newModel, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	sm := newModel.(GeneralSettingsModel)
	if !sm.settings.ErrorTrackingEnabled {
		t.Errorf("ErrorTrackingEnabled did not flip to true after Enter")
	}

	newModel, _ = sm.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	sm = newModel.(GeneralSettingsModel)
	if sm.settings.ErrorTrackingEnabled {
		t.Errorf("ErrorTrackingEnabled did not flip back to false")
	}
}

func TestGeneralSettings_RendersRotationRow(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	view := m.View().Content
	if !strings.Contains(view, "Enable automatic account rotation") {
		t.Errorf("settings view missing rotation row.\nGot:\n%s", view)
	}
	// Default is ON (nil Enabled), so the checkbox renders checked.
	if !strings.Contains(view, "[x] Enable automatic account rotation") {
		t.Errorf("rotation row should render checked by default.\nGot:\n%s", view)
	}
}

func TestGeneralSettings_RotationToggleFlipsRenderedValue(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	idx := -1
	for i, r := range m.rows {
		if r.Kind == settingsRowKindRotation {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("settingsRowKindRotation row not found")
	}
	m.cursor = idx

	if !m.settings.ManagedAccounts.ManagedAccountsEnabled() {
		t.Fatalf("precondition: rotation should default to enabled (nil)")
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	sm := updated.(GeneralSettingsModel)
	if sm.settings.ManagedAccounts.ManagedAccountsEnabled() {
		t.Errorf("rotation did not flip to disabled after Enter")
	}
	if !strings.Contains(sm.View().Content, "[ ] Enable automatic account rotation") {
		t.Errorf("rendered rotation row should be unchecked after toggle.\nGot:\n%s", sm.View().Content)
	}

	updated, _ = sm.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	sm = updated.(GeneralSettingsModel)
	if !sm.settings.ManagedAccounts.ManagedAccountsEnabled() {
		t.Errorf("rotation did not flip back to enabled")
	}
	if !strings.Contains(sm.View().Content, "[x] Enable automatic account rotation") {
		t.Errorf("rendered rotation row should be checked after second toggle.\nGot:\n%s", sm.View().Content)
	}
}

func TestGeneralSettings_EventTracingToggleSeedsDefaults(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())

	for i, row := range m.rows {
		if row.Kind == settingsRowKindEventTracing {
			m.cursor = i
			break
		}
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(GeneralSettingsModel)

	if !m.settings.EventTracingEnabled {
		t.Error("space did not enable event tracing")
	}
	if got := m.settings.PostHogProjectToken; got != telemetry.ProductionProjectToken {
		t.Errorf("PostHogProjectToken = %q, want %q", got, telemetry.ProductionProjectToken)
	}
	if got := m.settings.PostHogHost; got != telemetry.DefaultHost {
		t.Errorf("PostHogHost = %q, want %q", got, telemetry.DefaultHost)
	}
	out := m.View().Content
	for _, want := range []string{"PostHog project token", "PostHog host"} {
		if !strings.Contains(out, want) {
			t.Errorf("settings missing %q after enabling tracing in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "set when tracing is enabled") {
		t.Errorf("settings showed obsolete tracing placeholder in:\n%s", out)
	}
}

func TestGeneralSettings_RendersAgentSectionForEachAgent(t *testing.T) {
	withTempConfigHome(t)
	stub := &settingsAgentStub{
		stubClient: &stubClient{},
		agents: []client.AgentInfo{
			{
				Name:    "claude",
				Version: "v1",
				UserSettings: []client.UserSetting{
					{
						Key:   "dangerously_skip_permissions",
						Label: "Skip permissions",
						Type:  client.SettingTypeBool,
					},
				},
			},
			{
				Name:    "codex",
				Version: "v0.1",
				UserSettings: []client.UserSetting{
					{
						Key:           "model",
						Label:         "Model",
						Type:          client.SettingTypeEnum,
						AllowedValues: []string{"sonnet", "opus"},
					},
				},
			},
		},
	}

	m := NewGeneralSettingsModel(stub, context.Background())
	out := m.View().Content

	for _, want := range []string{
		"claude",
		"codex",
		"Skip permissions",
		"Model:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestGeneralSettings_RendersConfiguredAgentsWhenDaemonHasNoneLoaded(t *testing.T) {
	withTempConfigHome(t)
	settings := config.DefaultSettings()
	settings.DefaultAgent = "codex"
	settings.KnownAgentProviders = []string{"claude", "codex"}
	settings.Plugins = []config.PluginConfig{
		{Name: "claude", Enabled: false},
		{Name: "codex", Enabled: true},
	}
	if err := config.Save(settings); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	out := m.View().Content

	for _, want := range []string{
		"claude",
		"codex",
		"[ ] Enabled",
		"[x] Enabled",
		"Skip permission prompts",
		"Bypass approvals & sandbox",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("settings missing %q for configured unloaded agents:\n%s", want, out)
		}
	}
	if len(m.agents) != 2 {
		t.Fatalf("agents = %v, want claude and codex", m.agents)
	}
}

func TestGeneralSettings_MergeDiscoveredAgentPluginsPreservesEnabledState(t *testing.T) {
	settings := config.DefaultSettings()
	settings.KnownAgentProviders = []string{"claude", "codex"}
	settings.Plugins = []config.PluginConfig{
		{Name: "claude", Enabled: false},
	}

	got := mergeDiscoveredAgentPlugins(settings, []config.PluginConfig{
		{Name: "claude", Path: "/plugins/bossd-plugin-claude", Enabled: true},
		{Name: "codex", Path: "/plugins/bossd-plugin-codex", Enabled: true},
	})

	if len(got.Plugins) != 2 {
		t.Fatalf("plugins = %+v, want claude and codex", got.Plugins)
	}
	if got.Plugins[0].Name != "claude" || got.Plugins[0].Enabled || got.Plugins[0].Path == "" {
		t.Fatalf("claude plugin = %+v, want disabled with discovered path", got.Plugins[0])
	}
	if got.Plugins[1].Name != "codex" || got.Plugins[1].Enabled || got.Plugins[1].Path == "" {
		t.Fatalf("codex plugin = %+v, want disabled with discovered path", got.Plugins[1])
	}
}

func TestGeneralSettings_AgentEnabledToggleCannotDisableLastAgent(t *testing.T) {
	withTempConfigHome(t)
	settings := config.DefaultSettings()
	settings.DefaultAgent = "codex"
	settings.KnownAgentProviders = []string{"codex"}
	settings.Plugins = []config.PluginConfig{{Name: "codex", Enabled: true}}
	if err := config.Save(settings); err != nil {
		t.Fatalf("Save: %v", err)
	}
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	for i, row := range m.rows {
		if row.Kind == settingsRowKindAgentEnabled && row.Plugin == "codex" {
			m.cursor = i
			break
		}
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)

	if !pluginEnabled(m.settings, "codex") {
		t.Fatal("codex disabled; want last enabled agent preserved")
	}
	if m.err == nil || !strings.Contains(m.err.Error(), "select at least one agent") {
		t.Fatalf("err = %v, want select at least one agent", m.err)
	}
}

func TestGeneralSettings_AgentEnabledToggleEnablesConfiguredAgent(t *testing.T) {
	withTempConfigHome(t)
	settings := config.DefaultSettings()
	settings.DefaultAgent = "codex"
	settings.KnownAgentProviders = []string{"claude", "codex"}
	settings.Plugins = []config.PluginConfig{
		{Name: "claude", Enabled: false},
		{Name: "codex", Enabled: true},
	}
	if err := config.Save(settings); err != nil {
		t.Fatalf("Save: %v", err)
	}
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	for i, row := range m.rows {
		if row.Kind == settingsRowKindAgentEnabled && row.Plugin == "claude" {
			m.cursor = i
			break
		}
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)

	if !pluginEnabled(m.settings, "claude") {
		t.Fatal("claude enabled = false, want true")
	}
}

func TestGeneralSettings_BoolRowToggles(t *testing.T) {
	withTempConfigHome(t)
	stub := &settingsAgentStub{
		stubClient: &stubClient{},
		agents: []client.AgentInfo{
			{
				Name: "claude",
				UserSettings: []client.UserSetting{
					{Key: "dangerously_skip_permissions", Label: "Skip", Type: client.SettingTypeBool},
				},
			},
		},
	}
	m := NewGeneralSettingsModel(stub, context.Background())

	// Cursor should land on first non-header row. Walk to the bool row.
	for i, row := range m.rows {
		if row.Kind == settingsRowKindBool {
			m.cursor = i
			break
		}
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(GeneralSettingsModel)

	if !config.PluginConfigBool(&m.settings, "claude", "dangerously_skip_permissions") {
		t.Error("space did not toggle bool setting on")
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(GeneralSettingsModel)
	if config.PluginConfigBool(&m.settings, "claude", "dangerously_skip_permissions") {
		t.Error("second toggle did not clear bool setting")
	}
}

func TestGeneralSettings_EnumRowCycles(t *testing.T) {
	withTempConfigHome(t)
	stub := &settingsAgentStub{
		stubClient: &stubClient{},
		agents: []client.AgentInfo{
			{
				Name: "codex",
				UserSettings: []client.UserSetting{
					{Key: "model", Label: "Model", Type: client.SettingTypeEnum, AllowedValues: []string{"a", "b", "c"}},
				},
			},
		},
	}
	m := NewGeneralSettingsModel(stub, context.Background())

	for i, row := range m.rows {
		if row.Kind == settingsRowKindEnum {
			m.cursor = i
			break
		}
	}

	// First press cycles from "" → first allowed ("a") via nextEnumValue,
	// because empty string isn't in the list (treated as "not present").
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(GeneralSettingsModel)
	if got := config.PluginConfigString(&m.settings, "codex", "model"); got != "a" {
		t.Errorf("first cycle: got %q, want a", got)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(GeneralSettingsModel)
	if got := config.PluginConfigString(&m.settings, "codex", "model"); got != "b" {
		t.Errorf("second cycle: got %q, want b", got)
	}

	// Cycle past end wraps to start.
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(GeneralSettingsModel)
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(GeneralSettingsModel)
	if got := config.PluginConfigString(&m.settings, "codex", "model"); got != "a" {
		t.Errorf("wrap cycle: got %q, want a", got)
	}
}

func TestGeneralSettings_DefaultAgentRowAppearsForMultiAgent(t *testing.T) {
	withTempConfigHome(t)
	multi := &settingsAgentStub{
		stubClient: &stubClient{},
		agents: []client.AgentInfo{
			{Name: "claude", UserSettings: []client.UserSetting{{Key: "x", Label: "X", Type: client.SettingTypeBool}}},
			{Name: "codex", UserSettings: []client.UserSetting{{Key: "y", Label: "Y", Type: client.SettingTypeBool}}},
		},
	}
	m := NewGeneralSettingsModel(multi, context.Background())
	hasDefaultAgent := false
	for _, r := range m.rows {
		if r.Kind == settingsRowKindDefaultAgent {
			hasDefaultAgent = true
		}
	}
	if !hasDefaultAgent {
		t.Error("expected a Default agent row when >1 agent loaded")
	}

	single := &settingsAgentStub{
		stubClient: &stubClient{},
		agents: []client.AgentInfo{
			{Name: "claude", UserSettings: []client.UserSetting{{Key: "x", Label: "X", Type: client.SettingTypeBool}}},
		},
	}
	m2 := NewGeneralSettingsModel(single, context.Background())
	for _, r := range m2.rows {
		if r.Kind == settingsRowKindDefaultAgent {
			t.Error("Default agent row should not appear with a single agent")
		}
	}
}

func TestGeneralSettings_DefaultAgentAgentsThenTracingOrder(t *testing.T) {
	withTempConfigHome(t)
	stub := &settingsAgentStub{
		stubClient: &stubClient{},
		agents: []client.AgentInfo{
			{Name: "claude", UserSettings: []client.UserSetting{{Key: "x", Label: "X", Type: client.SettingTypeBool}}},
			{Name: "codex", UserSettings: []client.UserSetting{{Key: "y", Label: "Y", Type: client.SettingTypeBool}}},
		},
	}
	m := NewGeneralSettingsModel(stub, context.Background())

	indexOf := func(kind settingsRowKind, label string) int {
		for i, row := range m.rows {
			if row.Kind == kind && row.Label == label {
				return i
			}
		}
		return -1
	}

	defaultAgent := indexOf(settingsRowKindDefaultAgent, "Default agent")
	claude := indexOf(settingsRowKindAgentHeader, "claude")
	codex := indexOf(settingsRowKindAgentHeader, "codex")
	tracing := indexOf(settingsRowKindTracingHeader, "tracing")
	eventTracing := indexOf(settingsRowKindEventTracing, "Enable event tracing (for debugging problems)")
	if defaultAgent < 0 || claude <= defaultAgent || codex <= claude || tracing <= codex || eventTracing <= tracing {
		t.Fatalf("unexpected row order: default=%d claude=%d codex=%d tracing=%d event=%d rows=%v",
			defaultAgent, claude, codex, tracing, eventTracing, m.rows)
	}
}

func TestGeneralSettings_ErrorTrackingImmediatelyFollowsEventTracingWhenTracingEnabled(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())

	for i, row := range m.rows {
		if row.Kind == settingsRowKindEventTracing {
			m.cursor = i
			break
		}
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(GeneralSettingsModel)

	eventTracing := -1
	errorTracking := -1
	postHogToken := -1
	for i, row := range m.rows {
		if row.Kind == settingsRowKindEventTracing {
			eventTracing = i
		}
		if row.Kind == settingsRowKindErrorTracking {
			errorTracking = i
		}
		if row.Kind == settingsRowKindPostHogToken {
			postHogToken = i
		}
	}

	if eventTracing < 0 || errorTracking < 0 || postHogToken < 0 {
		t.Fatalf("missing expected tracing rows: event=%d error=%d token=%d rows=%v", eventTracing, errorTracking, postHogToken, m.rows)
	}
	if errorTracking != eventTracing+1 {
		t.Fatalf("error tracking row should immediately follow event tracing: event=%d error=%d rows=%v", eventTracing, errorTracking, m.rows)
	}
	if postHogToken <= errorTracking {
		t.Fatalf("PostHog rows should follow error tracking: error=%d token=%d rows=%v", errorTracking, postHogToken, m.rows)
	}
}

func TestGeneralSettings_NextEnumValueWraps(t *testing.T) {
	allowed := []string{"a", "b", "c"}
	cases := []struct {
		current string
		want    string
	}{
		{"", "a"},  // not present → first
		{"x", "a"}, // unknown → first
		{"a", "b"}, // wrap forward
		{"b", "c"}, // wrap forward
		{"c", "a"}, // wrap around end
	}
	for _, tc := range cases {
		if got := nextEnumValue(allowed, tc.current); got != tc.want {
			t.Errorf("nextEnumValue(%q) = %q, want %q", tc.current, got, tc.want)
		}
	}
}

func TestGeneralSettings_CursorSkipsHeaderRows(t *testing.T) {
	withTempConfigHome(t)
	stub := &settingsAgentStub{
		stubClient: &stubClient{},
		agents: []client.AgentInfo{
			{
				Name: "claude",
				UserSettings: []client.UserSetting{
					{Key: "x", Label: "X", Type: client.SettingTypeBool},
				},
			},
		},
	}
	m := NewGeneralSettingsModel(stub, context.Background())

	// Walk down through every row; cursor should never land on a header.
	for range m.rows {
		if m.cursor >= 0 && m.cursor < len(m.rows) {
			if m.rows[m.cursor].IsHeader {
				t.Errorf("cursor landed on header row at index %d", m.cursor)
			}
		}
		updated, _ := m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
		m = updated.(GeneralSettingsModel)
	}
}
