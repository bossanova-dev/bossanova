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
	if !config.NotificationsEnabled(m.settings) {
		t.Fatal("failed notification save did not roll back the requested toggle")
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
	if !config.NotificationsEnabled(persisted) {
		t.Fatal("successful other save persisted a rolled-back notification toggle")
	}
	if !persisted.ErrorTrackingEnabled {
		t.Fatal("successful other save did not persist its own setting")
	}
}

func rowIndexByKind(t *testing.T, m GeneralSettingsModel, kind settingsRowKind) int {
	t.Helper()
	for i, row := range m.rows {
		if row.Kind == kind {
			return i
		}
	}
	t.Fatalf("row kind %v not found", kind)
	return -1
}

func rowIndexByKindAndPlugin(t *testing.T, m GeneralSettingsModel, kind settingsRowKind, plugin, key string) int {
	t.Helper()
	for i, row := range m.rows {
		if row.Kind == kind && row.Plugin == plugin && row.Key == key {
			return i
		}
	}
	t.Fatalf("row kind %v for %s.%s not found", kind, plugin, key)
	return -1
}

func makeSettingsSaveFail(t *testing.T) {
	t.Helper()
	badPath := filepath.Join(t.TempDir(), "settings-dir")
	if err := os.Mkdir(badPath, 0o755); err != nil {
		t.Fatalf("os.Mkdir(%q): %v", badPath, err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", badPath)
}

func updateGeneralSettings(t *testing.T, m GeneralSettingsModel, msg tea.Msg) GeneralSettingsModel {
	t.Helper()
	updated, _ := m.Update(msg)
	next, ok := updated.(GeneralSettingsModel)
	if !ok {
		t.Fatalf("updated model = %T, want GeneralSettingsModel", updated)
	}
	return next
}

func TestGeneralSettings_FailedSaveRollsBackEditedRows(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(t *testing.T) GeneralSettingsModel
		rowIndex   func(t *testing.T, m GeneralSettingsModel) int
		setInput   func(m *GeneralSettingsModel)
		assertKept func(t *testing.T, m GeneralSettingsModel)
	}{
		{
			name: "plugin string",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.Plugins = []config.PluginConfig{{Name: "codex", Enabled: true, Config: map[string]string{"model": "old-model"}}}
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{
					stubClient: &stubClient{},
					agents: []client.AgentInfo{{
						Name: "codex",
						UserSettings: []client.UserSetting{{
							Key:   "model",
							Label: "Model",
							Type:  client.SettingTypeString,
						}},
					}},
				}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKindAndPlugin(t, m, settingsRowKindString, "codex", "model")
			},
			setInput: func(m *GeneralSettingsModel) {
				m.stringInput.SetValue("new-model")
			},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := config.PluginConfigString(&m.settings, "codex", "model"); got != "old-model" {
					t.Fatalf("plugin string = %q, want old-model", got)
				}
				if got := m.stringInput.Value(); got != "new-model" {
					t.Fatalf("editor value = %q, want candidate new-model", got)
				}
			},
		},
		{
			name: "PostHog token",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.EventTracingEnabled = true
				settings.PostHogProjectToken = "ph-old"
				settings.PostHogHost = "https://old.example"
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindPostHogToken)
			},
			setInput: func(m *GeneralSettingsModel) {
				m.stringInput.SetValue(" ph-new ")
			},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := m.settings.PostHogProjectToken; got != "ph-old" {
					t.Fatalf("PostHogProjectToken = %q, want ph-old", got)
				}
			},
		},
		{
			name: "PostHog host",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.EventTracingEnabled = true
				settings.PostHogProjectToken = "ph-token"
				settings.PostHogHost = "https://old.example"
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindPostHogHost)
			},
			setInput: func(m *GeneralSettingsModel) {
				m.stringInput.SetValue(" https://new.example ")
			},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := m.settings.PostHogHost; got != "https://old.example" {
					t.Fatalf("PostHogHost = %q, want https://old.example", got)
				}
			},
		},
		{
			name: "daemon name",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.DaemonName = "studio-mini"
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindDaemonName)
			},
			setInput: func(m *GeneralSettingsModel) {
				m.stringInput.SetValue("studio-scratch")
			},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := m.settings.DaemonName; got != "studio-mini" {
					t.Fatalf("DaemonName = %q, want studio-mini", got)
				}
			},
		},
		{
			name: "worktree",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.WorktreeBaseDir = "/tmp/old-worktrees"
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindWorktree)
			},
			setInput: func(m *GeneralSettingsModel) {
				m.worktreeDirInput.SetValue("/tmp/new-worktrees")
			},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := m.settings.WorktreeBaseDir; got != "/tmp/old-worktrees" {
					t.Fatalf("WorktreeBaseDir = %q, want /tmp/old-worktrees", got)
				}
			},
		},
		{
			name: "poll interval",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.PollIntervalSeconds = 15
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindPollInterval)
			},
			setInput: func(m *GeneralSettingsModel) {
				m.pollIntervalInput.SetValue("45")
			},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := m.settings.PollIntervalSeconds; got != 15 {
					t.Fatalf("PollIntervalSeconds = %d, want 15", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempConfigHome(t)
			m := tc.setup(t)
			m.cursor = tc.rowIndex(t, m)
			m = updateGeneralSettings(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			if m.editingRow < 0 {
				t.Fatal("enter did not open editor")
			}
			tc.setInput(&m)
			makeSettingsSaveFail(t)
			m = updateGeneralSettings(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

			if m.err == nil {
				t.Fatal("failed save did not surface an error")
			}
			tc.assertKept(t, m)
		})
	}
}

func TestGeneralSettings_CancelRestoresEditedRowsFromSettings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(t *testing.T) GeneralSettingsModel
		rowIndex   func(t *testing.T, m GeneralSettingsModel) int
		setInput   func(m *GeneralSettingsModel)
		assertKept func(t *testing.T, m GeneralSettingsModel)
	}{
		{
			name: "plugin string",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.Plugins = []config.PluginConfig{{Name: "codex", Enabled: true, Config: map[string]string{"model": "old-model"}}}
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{
					stubClient: &stubClient{},
					agents: []client.AgentInfo{{
						Name: "codex",
						UserSettings: []client.UserSetting{{
							Key:   "model",
							Label: "Model",
							Type:  client.SettingTypeString,
						}},
					}},
				}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKindAndPlugin(t, m, settingsRowKindString, "codex", "model")
			},
			setInput: func(m *GeneralSettingsModel) {
				m.stringInput.SetValue("new-model")
			},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := m.stringInput.Value(); got != "old-model" {
					t.Fatalf("string input = %q, want old-model", got)
				}
			},
		},
		{
			name: "PostHog token",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.EventTracingEnabled = true
				settings.PostHogProjectToken = "ph-old"
				settings.PostHogHost = "https://old.example"
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindPostHogToken)
			},
			setInput: func(m *GeneralSettingsModel) {
				m.stringInput.SetValue("ph-new")
			},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := m.stringInput.Value(); got != "ph-old" {
					t.Fatalf("string input = %q, want ph-old", got)
				}
			},
		},
		{
			name: "PostHog host",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.EventTracingEnabled = true
				settings.PostHogProjectToken = "ph-token"
				settings.PostHogHost = "https://old.example"
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindPostHogHost)
			},
			setInput: func(m *GeneralSettingsModel) {
				m.stringInput.SetValue("https://new.example")
			},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := m.stringInput.Value(); got != "https://old.example" {
					t.Fatalf("string input = %q, want https://old.example", got)
				}
			},
		},
		{
			name: "daemon name",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.DaemonName = "studio-mini"
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindDaemonName)
			},
			setInput: func(m *GeneralSettingsModel) {
				m.stringInput.SetValue("studio-scratch")
			},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := m.stringInput.Value(); got != "studio-mini" {
					t.Fatalf("string input = %q, want studio-mini", got)
				}
			},
		},
		{
			name: "worktree",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.WorktreeBaseDir = "/tmp/old-worktrees"
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindWorktree)
			},
			setInput: func(m *GeneralSettingsModel) {
				m.worktreeDirInput.SetValue("/tmp/new-worktrees")
			},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := m.worktreeDirInput.Value(); got != "/tmp/old-worktrees" {
					t.Fatalf("worktree input = %q, want /tmp/old-worktrees", got)
				}
			},
		},
		{
			name: "poll interval",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.PollIntervalSeconds = 15
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindPollInterval)
			},
			setInput: func(m *GeneralSettingsModel) {
				m.pollIntervalInput.SetValue("45")
			},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := m.pollIntervalInput.Value(); got != "15" {
					t.Fatalf("poll interval input = %q, want 15", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempConfigHome(t)
			m := tc.setup(t)
			m.cursor = tc.rowIndex(t, m)
			m = updateGeneralSettings(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			if m.editingRow < 0 {
				t.Fatal("enter did not open editor")
			}
			tc.setInput(&m)
			m = updateGeneralSettings(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})

			if m.err != nil {
				t.Fatalf("cancel retained err: %v", m.err)
			}
			if m.editingRow >= 0 {
				t.Fatal("esc did not leave editor")
			}
			tc.assertKept(t, m)
		})
	}
}

func TestGeneralSettings_FailedSaveRollsBackActivationRows(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(t *testing.T) GeneralSettingsModel
		rowIndex   func(t *testing.T, m GeneralSettingsModel) int
		press      tea.KeyPressMsg
		assertKept func(t *testing.T, m GeneralSettingsModel)
	}{
		{
			name: "plugin bool",
			setup: func(t *testing.T) GeneralSettingsModel {
				return NewGeneralSettingsModel(&settingsAgentStub{
					stubClient: &stubClient{},
					agents: []client.AgentInfo{{
						Name: "claude",
						UserSettings: []client.UserSetting{{
							Key:   "dangerously_skip_permissions",
							Label: "Skip",
							Type:  client.SettingTypeBool,
						}},
					}},
				}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKindAndPlugin(t, m, settingsRowKindBool, "claude", "dangerously_skip_permissions")
			},
			press: tea.KeyPressMsg{Code: ' ', Text: " "},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if config.PluginConfigBool(&m.settings, "claude", "dangerously_skip_permissions") {
					t.Fatal("plugin bool changed after failed save")
				}
			},
		},
		{
			name: "plugin enum",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.Plugins = []config.PluginConfig{{Name: "codex", Enabled: true, Config: map[string]string{"model": "a"}}}
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{
					stubClient: &stubClient{},
					agents: []client.AgentInfo{{
						Name: "codex",
						UserSettings: []client.UserSetting{{
							Key:           "model",
							Label:         "Model",
							Type:          client.SettingTypeEnum,
							AllowedValues: []string{"a", "b"},
						}},
					}},
				}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKindAndPlugin(t, m, settingsRowKindEnum, "codex", "model")
			},
			press: tea.KeyPressMsg{Code: tea.KeyEnter},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := config.PluginConfigString(&m.settings, "codex", "model"); got != "a" {
					t.Fatalf("plugin enum = %q, want a", got)
				}
			},
		},
		{
			name: "agent enabled",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.DefaultAgent = "codex"
				settings.KnownAgentProviders = []string{"claude", "codex"}
				settings.Plugins = []config.PluginConfig{
					{Name: "claude", Enabled: false},
					{Name: "codex", Enabled: true},
				}
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				for i, row := range m.rows {
					if row.Kind == settingsRowKindAgentEnabled && row.Plugin == "claude" {
						return i
					}
				}
				t.Fatal("claude enabled row not found")
				return -1
			},
			press: tea.KeyPressMsg{Code: tea.KeyEnter},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if pluginEnabled(m.settings, "claude") {
					t.Fatal("claude enabled after failed save")
				}
				if got := m.settings.DefaultAgent; got != "codex" {
					t.Fatalf("DefaultAgent = %q, want codex", got)
				}
			},
		},
		{
			name: "event tracing",
			setup: func(t *testing.T) GeneralSettingsModel {
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindEventTracing)
			},
			press: tea.KeyPressMsg{Code: ' ', Text: " "},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if m.settings.EventTracingEnabled {
					t.Fatal("event tracing enabled after failed save")
				}
				if m.settings.PostHogProjectToken != "" || m.settings.PostHogHost != "" {
					t.Fatalf("PostHog defaults seeded after failed save: token=%q host=%q",
						m.settings.PostHogProjectToken, m.settings.PostHogHost)
				}
			},
		},
		{
			name: "error tracking",
			setup: func(t *testing.T) GeneralSettingsModel {
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindErrorTracking)
			},
			press: tea.KeyPressMsg{Code: tea.KeyEnter},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if m.settings.ErrorTrackingEnabled {
					t.Fatal("error tracking enabled after failed save")
				}
			},
		},
		{
			name: "rotation",
			setup: func(t *testing.T) GeneralSettingsModel {
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindRotation)
			},
			press: tea.KeyPressMsg{Code: tea.KeyEnter},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if !m.settings.ManagedAccounts.ManagedAccountsEnabled() {
					t.Fatal("rotation disabled after failed save")
				}
				if m.settings.ManagedAccounts.Enabled != nil {
					t.Fatal("rotation pointer changed after failed save")
				}
			},
		},
		{
			name: "notifications",
			setup: func(t *testing.T) GeneralSettingsModel {
				return NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindNotifications)
			},
			press: tea.KeyPressMsg{Code: ' ', Text: " "},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if !config.NotificationsEnabled(m.settings) {
					t.Fatal("notifications disabled after failed save")
				}
				if m.settings.NotificationsEnabled != nil {
					t.Fatal("notifications pointer changed after failed save")
				}
			},
		},
		{
			name: "default agent",
			setup: func(t *testing.T) GeneralSettingsModel {
				settings := config.DefaultSettings()
				settings.DefaultAgent = "claude"
				settings.Plugins = []config.PluginConfig{
					{Name: "claude", Enabled: true},
					{Name: "codex", Enabled: true},
				}
				if err := config.Save(settings); err != nil {
					t.Fatalf("config.Save: %v", err)
				}
				return NewGeneralSettingsModel(&settingsAgentStub{
					stubClient: &stubClient{},
					agents: []client.AgentInfo{
						{Name: "claude", UserSettings: []client.UserSetting{{Key: "x", Label: "X", Type: client.SettingTypeBool}}},
						{Name: "codex", UserSettings: []client.UserSetting{{Key: "y", Label: "Y", Type: client.SettingTypeBool}}},
					},
				}, context.Background())
			},
			rowIndex: func(t *testing.T, m GeneralSettingsModel) int {
				return rowIndexByKind(t, m, settingsRowKindDefaultAgent)
			},
			press: tea.KeyPressMsg{Code: tea.KeyEnter},
			assertKept: func(t *testing.T, m GeneralSettingsModel) {
				if got := m.settings.DefaultAgent; got != "claude" {
					t.Fatalf("DefaultAgent = %q, want claude", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempConfigHome(t)
			m := tc.setup(t)
			m.cursor = tc.rowIndex(t, m)
			makeSettingsSaveFail(t)
			m = updateGeneralSettings(t, m, tc.press)

			if m.err == nil {
				t.Fatal("failed save did not surface an error")
			}
			tc.assertKept(t, m)
		})
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

// daemonNameRow focuses the Daemon name row and pins the model's machine
// hostname so the fallback rendering is deterministic across machines.
func daemonNameRow(t *testing.T, m GeneralSettingsModel, hostname string) GeneralSettingsModel {
	t.Helper()
	m.hostname = hostname
	for i, row := range m.rows {
		if row.Kind == settingsRowKindDaemonName {
			m.cursor = i
			return m
		}
	}
	t.Fatal("daemon name row not found")
	return m
}

func typeInto(t *testing.T, m GeneralSettingsModel, text string) GeneralSettingsModel {
	t.Helper()
	for _, r := range text {
		updated, _ := m.Update(keyPress(r))
		next, ok := updated.(GeneralSettingsModel)
		if !ok {
			t.Fatalf("updated model = %T, want GeneralSettingsModel", updated)
		}
		m = next
	}
	return m
}

func TestGeneralSettings_DaemonNameRowRendersMachineHostnameByDefault(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	m = daemonNameRow(t, m, "studio-imac")

	view := m.View().Content
	if !strings.Contains(view, "Daemon name: studio-imac") {
		t.Errorf("daemon name row should fall back to the machine hostname. Got:\n%s", view)
	}
	if !strings.Contains(view, "restart daemon") {
		t.Errorf("daemon name row should state that a restart applies it. Got:\n%s", view)
	}
	if m.settings.DaemonName != "" {
		t.Errorf("DaemonName = %q, want no persisted override", m.settings.DaemonName)
	}
}

func TestGeneralSettings_DaemonNameRowSitsBetweenPollIntervalAndRotation(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())

	pollInterval, daemonName, rotation := -1, -1, -1
	for i, row := range m.rows {
		switch row.Kind { //nolint:exhaustive // only the three ordered rows matter here
		case settingsRowKindPollInterval:
			pollInterval = i
		case settingsRowKindDaemonName:
			daemonName = i
		case settingsRowKindRotation:
			rotation = i
		}
	}
	if pollInterval < 0 || daemonName < 0 || rotation < 0 {
		t.Fatalf("missing rows: poll=%d daemon=%d rotation=%d", pollInterval, daemonName, rotation)
	}
	if daemonName != pollInterval+1 || rotation != daemonName+1 {
		t.Fatalf("daemon name row misplaced: poll=%d daemon=%d rotation=%d", pollInterval, daemonName, rotation)
	}
}

// TestGeneralSettings_DownKeyCountsMatchTheProofScenarios encodes, in the fast
// suite, the fixed key counts the committed TUI proof scenarios navigate by:
// bos662-daemon-name-settings presses down twice to reach Daemon name and
// bos459-settings-notifications-toggle presses down four times to reach the
// notifications toggle. Adjacency alone does not pin those — inserting a row
// ABOVE Worktree re-targets both scenarios while leaving adjacency intact, and
// the breakage would then only surface in the release-tier proof run.
func TestGeneralSettings_DownKeyCountsMatchTheProofScenarios(t *testing.T) {
	withTempConfigHome(t)

	for _, tc := range []struct {
		name  string
		downs int
		want  settingsRowKind
	}{
		{name: "bos662 reaches the daemon name row in 2 downs", downs: 2, want: settingsRowKindDaemonName},
		{name: "bos459 reaches the notifications row in 4 downs", downs: 4, want: settingsRowKindNotifications},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			if m.cursor != 0 {
				t.Fatalf("precondition: cursor = %d, want the scenarios' starting row 0", m.cursor)
			}
			for i := 0; i < tc.downs; i++ {
				updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
				next, ok := updated.(GeneralSettingsModel)
				if !ok {
					t.Fatalf("updated model = %T, want GeneralSettingsModel", updated)
				}
				m = next
			}
			if got := m.rows[m.cursor].Kind; got != tc.want {
				t.Fatalf("after %d downs the cursor is on row kind %v (%q), want %v",
					tc.downs, got, m.rows[m.cursor].Label, tc.want)
			}
		})
	}
}

// TestGeneralSettings_DaemonNameEditorShowsBlankResetHint pins the editor hint
// in the fast suite. Every other "restart daemon" assertion renders the RESTING
// row, so without this the hint's only guard is the release-tier bos662 proof
// scenario — the same late-and-expensive failure class the down-count test
// exists to close.
func TestGeneralSettings_DaemonNameEditorShowsBlankResetHint(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	m = daemonNameRow(t, m, "studio-imac")

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)
	if m.editingRow < 0 {
		t.Fatal("enter did not open the daemon name editor")
	}

	view := m.View().Content
	for _, want := range []string{"Blank uses this machine's hostname", "studio-imac", daemonRestartHint} {
		if !strings.Contains(view, want) {
			t.Errorf("daemon name editor hint missing %q. Got:\n%s", want, view)
		}
	}
}

func TestGeneralSettings_DaemonNameEditPersistsTrimmedValue(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	m = daemonNameRow(t, m, "studio-imac")

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)
	if m.editingRow < 0 {
		t.Fatal("enter did not open the daemon name editor")
	}
	m = typeInto(t, m, "  studio-mini  ")
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)

	if m.editingRow >= 0 {
		t.Fatalf("enter did not commit the daemon name edit (err=%v)", m.err)
	}
	if m.settings.DaemonName != "studio-mini" {
		t.Fatalf("DaemonName = %q, want trimmed %q", m.settings.DaemonName, "studio-mini")
	}
	persisted, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if persisted.DaemonName != "studio-mini" {
		t.Fatalf("persisted DaemonName = %q, want %q", persisted.DaemonName, "studio-mini")
	}
	view := m.View().Content
	if !strings.Contains(view, "Daemon name: studio-mini") {
		t.Errorf("daemon name row should show the custom name. Got:\n%s", view)
	}
	if !strings.Contains(view, "restart daemon") {
		t.Errorf("daemon name row should still state the restart requirement. Got:\n%s", view)
	}
}

func TestGeneralSettings_DaemonNameBlankClearsOverride(t *testing.T) {
	withTempConfigHome(t)
	seed, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	seed.DaemonName = "studio-mini"
	if err := config.Save(seed); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	m = daemonNameRow(t, m, "studio-imac")
	if m.settings.DaemonName != "studio-mini" {
		t.Fatalf("precondition: DaemonName = %q, want the seeded override", m.settings.DaemonName)
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)
	for range "studio-mini" {
		updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		m = updated.(GeneralSettingsModel)
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)

	if m.settings.DaemonName != "" {
		t.Fatalf("DaemonName = %q, want cleared", m.settings.DaemonName)
	}
	persisted, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if persisted.DaemonName != "" {
		t.Fatalf("persisted DaemonName = %q, want cleared", persisted.DaemonName)
	}
	if config.DaemonDisplayName(persisted, "studio-imac") != "studio-imac" {
		t.Fatal("cleared override did not restore hostname resolution")
	}
	if !strings.Contains(m.View().Content, "Daemon name: studio-imac") {
		t.Errorf("cleared daemon name should render the machine hostname. Got:\n%s", m.View().Content)
	}
}

func TestGeneralSettings_DaemonNameCancelRestoresSavedValue(t *testing.T) {
	withTempConfigHome(t)
	seed, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	seed.DaemonName = "studio-mini"
	if err := config.Save(seed); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	m = daemonNameRow(t, m, "studio-imac")

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)
	m = typeInto(t, m, "-scratch")
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(GeneralSettingsModel)

	if m.editingRow >= 0 {
		t.Fatal("esc did not leave the daemon name editor")
	}
	if m.settings.DaemonName != "studio-mini" {
		t.Fatalf("DaemonName = %q, want the saved value after cancel", m.settings.DaemonName)
	}
	persisted, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if persisted.DaemonName != "studio-mini" {
		t.Fatalf("persisted DaemonName = %q, want the saved value after cancel", persisted.DaemonName)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)
	if got := m.stringInput.Value(); got != "studio-mini" {
		t.Fatalf("reopened editor value = %q, want the saved value", got)
	}
}

// TestGeneralSettings_DaemonNameFailedSaveKeepsSavedValue pins the rollback on
// the failed-persist path: when config.Save fails the editor stays open, so the
// model must keep the previously-saved name. Otherwise cancelling leaves the
// resting row showing an unsaved rename that the next successful save of any
// other setting would silently persist.
func TestGeneralSettings_DaemonNameFailedSaveKeepsSavedValue(t *testing.T) {
	withTempConfigHome(t)
	goodPath := os.Getenv("BOSS_SETTINGS_PATH")
	seed, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	seed.DaemonName = "studio-mini"
	if err := config.Save(seed); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	m = daemonNameRow(t, m, "studio-imac")

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)
	m = typeInto(t, m, "-scratch")

	// A directory where the settings file belongs makes the atomic write fail.
	badPath := filepath.Join(t.TempDir(), "settings-dir")
	if err := os.Mkdir(badPath, 0o755); err != nil {
		t.Fatalf("os.Mkdir(%q): %v", badPath, err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", badPath)
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)
	t.Setenv("BOSS_SETTINGS_PATH", goodPath)

	if m.err == nil {
		t.Fatal("failed daemon name save did not surface an error")
	}
	if m.editingRow < 0 {
		t.Fatal("failed daemon name save closed the editor")
	}
	if m.settings.DaemonName != "studio-mini" {
		t.Fatalf("DaemonName = %q, want the saved value after a failed save", m.settings.DaemonName)
	}
	if got := m.stringInput.Value(); got != "studio-mini-scratch" {
		t.Fatalf("editor value = %q, want the candidate kept for a retry", got)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(GeneralSettingsModel)
	if m.settings.DaemonName != "studio-mini" {
		t.Fatalf("DaemonName = %q, want the saved value after cancelling a failed save", m.settings.DaemonName)
	}
	if !strings.Contains(m.View().Content, "Daemon name: studio-mini (") {
		t.Errorf("resting row should show the saved name. Got:\n%s", m.View().Content)
	}

	// A later successful save of an unrelated setting must not persist the
	// cancelled rename.
	for i, row := range m.rows {
		if row.Kind == settingsRowKindErrorTracking {
			m.cursor = i
			break
		}
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)
	if m.err != nil {
		t.Fatalf("unrelated save failed: %v", m.err)
	}
	persisted, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if persisted.DaemonName != "studio-mini" {
		t.Fatalf("persisted DaemonName = %q, want the saved value", persisted.DaemonName)
	}
}

func TestGeneralSettings_DaemonNamePromptsWhenHostnameUnresolvable(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	// os.Hostname() can fail; the row must not render a bare blank for it.
	m = daemonNameRow(t, m, "")

	view := m.View().Content
	if !strings.Contains(view, "Daemon name: (machine hostname unavailable — set a name;") {
		t.Errorf("unresolvable hostname should prompt for a name, not render a blank. Got:\n%s", view)
	}
	if strings.Contains(view, "Daemon name: unknown") {
		t.Errorf("the row must not present a placeholder as the name bossd will advertise. Got:\n%s", view)
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(GeneralSettingsModel)
	if editView := m.View().Content; !strings.Contains(editView, "hostname (unknown)") {
		t.Errorf("editor hint should render the placeholder too. Got:\n%s", editView)
	}
}

func TestGeneralSettings_DaemonNameOverrideWinsWhenHostnameUnresolvable(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	m = daemonNameRow(t, m, "")
	m.settings.DaemonName = "studio-mini"

	// An unresolvable machine hostname must not displace a configured override
	// — neither with a placeholder nor with the unavailable-hostname prompt.
	view := m.View().Content
	if !strings.Contains(view, "Daemon name: studio-mini") {
		t.Errorf("override should render even with no machine hostname. Got:\n%s", view)
	}
	if strings.Contains(view, "machine hostname unavailable") {
		t.Errorf("the unavailable-hostname prompt shadowed a configured override. Got:\n%s", view)
	}
}

func TestGeneralSettings_CapturesMachineHostnameAtConstruction(t *testing.T) {
	withTempConfigHome(t)
	// Every other daemon-name test pins m.hostname for determinism, so this is
	// the only assertion that the constructor captures it at all — without it,
	// dropping the capture would leave the row rendering "unknown" in
	// production while the suite stayed green.
	want, err := os.Hostname()
	if err != nil {
		t.Skipf("os.Hostname unavailable: %v", err)
	}

	m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	if m.hostname != want {
		t.Fatalf("hostname = %q, want the machine hostname %q", m.hostname, want)
	}
}

// TestEnabledAgentNamesIncludesOptedInExperimental pins the default-agent picker
// against the same opt-in path: the persisted entry stays "enabled": false while
// experimental_plugins is what the daemon honours, so reading the persisted flag
// omitted an agent the daemon had actually loaded.
func TestEnabledAgentNamesIncludesOptedInExperimental(t *testing.T) {
	settings := config.Settings{
		ExperimentalPlugins: []string{"opencode"},
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "opencode", Enabled: false},
		},
	}
	agents := []client.AgentInfo{{Name: "claude"}, {Name: "opencode"}}

	got := enabledAgentNames(settings, agents)

	found := false
	for _, n := range got {
		if n == "opencode" {
			found = true
		}
	}
	if !found {
		t.Errorf("enabledAgentNames = %v, want the opted-in opencode included", got)
	}
}

// TestEnabledAgentNamesExcludesLegacyEnabledExperimental is the other direction:
// a pre-gate "enabled": true entry with no opt-in is forced off by the daemon, so
// offering it as a default would name an agent that will not load.
func TestEnabledAgentNamesExcludesLegacyEnabledExperimental(t *testing.T) {
	settings := config.Settings{
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "opencode", Enabled: true},
		},
	}
	agents := []client.AgentInfo{{Name: "claude"}, {Name: "opencode"}}

	for _, n := range enabledAgentNames(settings, agents) {
		if n == "opencode" {
			t.Errorf("enabledAgentNames offered opencode as a default with no opt-in; the daemon forces it off")
		}
	}
}
