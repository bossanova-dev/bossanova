package settings

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func fieldValue(t *testing.T, view, key string) string {
	t.Helper()
	lines := strings.Split(stripANSI(view), "\n")
	for i, line := range lines {
		at := strings.Index(line, key+":")
		if at < 0 {
			continue
		}
		tail := line[at+len(key)+1:]
		valueColumn := lipgloss.Width(line[:at+len(key)+1]) + len(tail) - len(strings.TrimLeft(tail, " "))
		values := []string{strings.TrimSpace(tail)}
		for _, next := range lines[i+1:] {
			text := strings.TrimLeft(next, " │┃")
			column := lipgloss.Width(next) - lipgloss.Width(text)
			if text == "" || column != valueColumn {
				break
			}
			values = append(values, strings.TrimSpace(text))
		}
		return strings.Join(values, " ")
	}
	t.Fatalf("missing field %q in:\n%s", key, view)
	return ""
}

func fieldValueColumn(t *testing.T, view, key string) int {
	t.Helper()
	for _, line := range strings.Split(stripANSI(view), "\n") {
		at := strings.Index(line, key+":")
		if at < 0 {
			continue
		}
		tail := line[at+len(key)+1:]
		return lipgloss.Width(line[:at+len(key)+1]) + len(tail) - len(strings.TrimLeft(tail, " "))
	}
	t.Fatalf("missing field %q", key)
	return -1
}

func TestGeneralSettingsValueRowsAlignBySection(t *testing.T) {
	withTempConfigHome(t)
	m := NewGeneralSettingsModel(&settingsAgentStub{agents: []rpc.AgentInfo{{Name: "test", UserSettings: []rpc.UserSetting{
		{Key: "model", Label: "Model", Type: rpc.SettingTypeString},
		{Key: "approval", Label: "Approval policy", Type: rpc.SettingTypeEnum, AllowedValues: []string{"ask"}},
		{Key: "toggle", Label: "A deliberately very long checkbox label", Type: rpc.SettingTypeBool},
	}}}}, context.Background(), nil)
	m.width = 80
	m.settings.EventTracingEnabled = true
	m.rebuildRows()
	view := m.View().Content
	for _, keys := range [][]string{{"Worktree base directory", "Poll interval (seconds)", "Daemon name", "MCP HTTP token"}, {"Model", "Approval policy"}, {"PostHog project token", "PostHog host"}} {
		want := fieldValueColumn(t, view, keys[0])
		for _, key := range keys[1:] {
			if got := fieldValueColumn(t, view, key); got != want {
				t.Errorf("%s value column=%d, want %d", key, got, want)
			}
		}
	}
	for key, want := range map[string]string{"Model": "(not set)", "Approval policy": "ask (default)"} {
		if got := fieldValue(t, view, key); got != want {
			t.Errorf("%s value = %q, want %q", key, got, want)
		}
	}
	// Checkbox labels cannot widen the neighboring value rows.
	if got, want := fieldValueColumn(t, view, "Model"), 4+lipgloss.Width("Approval policy:")+1; got != want {
		t.Errorf("agent column=%d want %d", got, want)
	}
}

func TestGeneralSettingsLongHostnameWrapsAtValueColumn(t *testing.T) {
	withTempConfigHome(t)
	m := newGeneralSettingsModelWith(&settingsAgentStub{}, context.Background(), func() (string, error) { return strings.Repeat("hostname", 18), nil }, nil)
	// Darwin may prefer the machine ComputerName over the injected raw hostname.
	// Pin the already-derived display name as the daemon preview tests do.
	m = daemonNameRow(t, m, strings.Repeat("hostname", 18))
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80})
	m = updated.(GeneralSettingsModel)
	view := m.View().Content
	for _, line := range strings.Split(stripANSI(view), "\n") {
		if got := lipgloss.Width(line); got > 80 {
			t.Errorf("line width=%d: %s", got, line)
		}
	}
	value := fieldValue(t, view, "Daemon name")
	if !strings.Contains(value, "machine hostname; restart daemon to apply") {
		t.Errorf("hint lost or continuation misaligned: %q", value)
	}
}

func TestSessionSettingsLongNameFitsEightyColumns(t *testing.T) {
	m := NewSessionSettingsModel(nil, context.Background(), "session")
	m.session = &pb.Session{Title: strings.Repeat("a long session title ", 12)}
	m.width = 80
	for _, line := range strings.Split(stripANSI(m.View().Content), "\n") {
		if got := lipgloss.Width(line); got > 80 {
			t.Errorf("line width=%d: %s", got, line)
		}
	}
}
