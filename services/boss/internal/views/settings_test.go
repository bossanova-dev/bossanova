package views

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
)

// settingsAgentStub embeds *stubClient so it satisfies BossClient — only
// ListAgents matters for these tests, the rest panic.
type settingsAgentStub struct {
	*stubClient
	agents []client.AgentInfo
}

func (s *settingsAgentStub) ListAgents(context.Context) ([]client.AgentInfo, error) {
	return s.agents, nil
}

func TestSettings_RendersBuiltInRowsWithoutAgents(t *testing.T) {
	withTempConfigHome(t)
	m := NewSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
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

func TestSettings_RendersErrorTrackingRow(t *testing.T) {
	withTempConfigHome(t)
	m := NewSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
	view := m.View().Content
	if !strings.Contains(view, "Enable error tracking") {
		t.Errorf("settings view missing error tracking row.\nGot:\n%s", view)
	}
}

func TestSettings_ErrorTrackingToggle(t *testing.T) {
	withTempConfigHome(t)
	m := NewSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
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
	sm := newModel.(SettingsModel)
	if !sm.settings.ErrorTrackingEnabled {
		t.Errorf("ErrorTrackingEnabled did not flip to true after Enter")
	}

	newModel, _ = sm.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	sm = newModel.(SettingsModel)
	if sm.settings.ErrorTrackingEnabled {
		t.Errorf("ErrorTrackingEnabled did not flip back to false")
	}
}

func TestSettings_EventTracingToggleSeedsDefaults(t *testing.T) {
	withTempConfigHome(t)
	m := NewSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())

	for i, row := range m.rows {
		if row.Kind == settingsRowKindEventTracing {
			m.cursor = i
			break
		}
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(SettingsModel)

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

func TestSettings_RendersAgentSectionForEachAgent(t *testing.T) {
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

	m := NewSettingsModel(stub, context.Background())
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

func TestSettings_BoolRowToggles(t *testing.T) {
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
	m := NewSettingsModel(stub, context.Background())

	// Cursor should land on first non-header row. Walk to the bool row.
	for i, row := range m.rows {
		if row.Kind == settingsRowKindBool {
			m.cursor = i
			break
		}
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(SettingsModel)

	if !config.PluginConfigBool(&m.settings, "claude", "dangerously_skip_permissions") {
		t.Error("space did not toggle bool setting on")
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(SettingsModel)
	if config.PluginConfigBool(&m.settings, "claude", "dangerously_skip_permissions") {
		t.Error("second toggle did not clear bool setting")
	}
}

func TestSettings_EnumRowCycles(t *testing.T) {
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
	m := NewSettingsModel(stub, context.Background())

	for i, row := range m.rows {
		if row.Kind == settingsRowKindEnum {
			m.cursor = i
			break
		}
	}

	// First press cycles from "" → first allowed ("a") via nextEnumValue,
	// because empty string isn't in the list (treated as "not present").
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(SettingsModel)
	if got := config.PluginConfigString(&m.settings, "codex", "model"); got != "a" {
		t.Errorf("first cycle: got %q, want a", got)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(SettingsModel)
	if got := config.PluginConfigString(&m.settings, "codex", "model"); got != "b" {
		t.Errorf("second cycle: got %q, want b", got)
	}

	// Cycle past end wraps to start.
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(SettingsModel)
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	m = updated.(SettingsModel)
	if got := config.PluginConfigString(&m.settings, "codex", "model"); got != "a" {
		t.Errorf("wrap cycle: got %q, want a", got)
	}
}

func TestSettings_DefaultAgentRowAppearsForMultiAgent(t *testing.T) {
	withTempConfigHome(t)
	multi := &settingsAgentStub{
		stubClient: &stubClient{},
		agents: []client.AgentInfo{
			{Name: "claude", UserSettings: []client.UserSetting{{Key: "x", Label: "X", Type: client.SettingTypeBool}}},
			{Name: "codex", UserSettings: []client.UserSetting{{Key: "y", Label: "Y", Type: client.SettingTypeBool}}},
		},
	}
	m := NewSettingsModel(multi, context.Background())
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
	m2 := NewSettingsModel(single, context.Background())
	for _, r := range m2.rows {
		if r.Kind == settingsRowKindDefaultAgent {
			t.Error("Default agent row should not appear with a single agent")
		}
	}
}

func TestSettings_DefaultAgentAgentsThenTracingOrder(t *testing.T) {
	withTempConfigHome(t)
	stub := &settingsAgentStub{
		stubClient: &stubClient{},
		agents: []client.AgentInfo{
			{Name: "claude", UserSettings: []client.UserSetting{{Key: "x", Label: "X", Type: client.SettingTypeBool}}},
			{Name: "codex", UserSettings: []client.UserSetting{{Key: "y", Label: "Y", Type: client.SettingTypeBool}}},
		},
	}
	m := NewSettingsModel(stub, context.Background())

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

func TestSettings_ErrorTrackingImmediatelyFollowsEventTracingWhenTracingEnabled(t *testing.T) {
	withTempConfigHome(t)
	m := NewSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())

	for i, row := range m.rows {
		if row.Kind == settingsRowKindEventTracing {
			m.cursor = i
			break
		}
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(SettingsModel)

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

func TestSettings_NextEnumValueWraps(t *testing.T) {
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

func TestSettings_CursorSkipsHeaderRows(t *testing.T) {
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
	m := NewSettingsModel(stub, context.Background())

	// Walk down through every row; cursor should never land on a header.
	for range m.rows {
		if m.cursor >= 0 && m.cursor < len(m.rows) {
			if m.rows[m.cursor].IsHeader {
				t.Errorf("cursor landed on header row at index %d", m.cursor)
			}
		}
		updated, _ := m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
		m = updated.(SettingsModel)
	}
}
