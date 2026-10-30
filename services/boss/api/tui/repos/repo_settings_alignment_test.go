package repos

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
)

// fieldValue reads a value and its hanging continuations without pinning key padding.
func fieldValue(t *testing.T, view, key string) string {
	t.Helper()
	lines := strings.Split(stripANSI(view), "\n")
	for i, line := range lines {
		_, after, found := strings.Cut(line, key+":")
		if !found {
			continue
		}
		value := strings.TrimSpace(after)
		column := lipgloss.Width(line[:len(line)-len(strings.TrimLeft(after, " "))])
		for _, next := range lines[i+1:] {
			text := strings.TrimLeft(next, " │┃")
			if strings.TrimSpace(text) == "" || lipgloss.Width(next[:len(next)-len(text)]) != column {
				break
			}
			value += " " + strings.TrimSpace(text)
		}
		return value
	}
	t.Fatalf("field %q missing from view:\n%s", key, view)
	return ""
}

func repoValueColumn(t *testing.T, view, key string) int {
	t.Helper()
	for _, line := range strings.Split(stripANSI(view), "\n") {
		before, after, found := strings.Cut(line, key+":")
		if found {
			return lipgloss.Width(before + key + ":" + after[:len(after)-len(strings.TrimLeft(after, " "))])
		}
	}
	t.Fatalf("field %q missing from view:\n%s", key, view)
	return 0
}

func TestRepoSettingsValueColumnsStayStable(t *testing.T) {
	m := newLoadedOrgSettings(t, &fakeOrgClient{})
	m.repo.LinearApiKey = "lin_key"
	m.repo.SentryApiKey = "sentry_key"
	m.repo.SentryOrg = "acme"
	m.linearExpanded = true
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = updated.(RepoSettingsModel)
	collapsed := viewString(m)
	m.sentryExpanded = true
	expanded := viewString(m)
	for _, key := range []string{"Name", "Setup command", "Organization", "Merge strategy"} {
		if got, want := repoValueColumn(t, expanded, key), repoValueColumn(t, expanded, "Name"); got != want {
			t.Errorf("%s value column %d, want %d", key, got, want)
		}
	}
	childColumn := repoValueColumn(t, expanded, "Organization slug")
	if childColumn <= 6 {
		t.Errorf("child value column %d does not preserve indent", childColumn)
	}
	if got := repoValueColumn(t, expanded, "API key"); got != childColumn {
		t.Errorf("API key column %d, want %d", got, childColumn)
	}
	if got := repoValueColumn(t, collapsed, "API key"); got != childColumn {
		t.Errorf("collapsed child column %d, want %d", got, childColumn)
	}
	m.orgClient = nil
	hidden := viewString(m)
	if got, want := repoValueColumn(t, hidden, "Name"), repoValueColumn(t, expanded, "Name"); got != want {
		t.Errorf("hidden organization moved column %d to %d", want, got)
	}
}

func TestRepoSettingsLongValuesFitEightyColumns(t *testing.T) {
	m := newLoadedOrgSettings(t, &fakeOrgClient{})
	setup := strings.Repeat("install dependencies && ", 12) + "finish"
	m.repo.SetupScript = &setup
	m.repo.SentryApiKey = "key"
	m.sentryExpanded = true
	cursorToRow(t, &m, repoSettingsRowSetupScript)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = updated.(RepoSettingsModel)
	view := viewString(m)
	for _, line := range strings.Split(view, "\n") {
		if got := lipgloss.Width(line); got > 80 {
			t.Errorf("line width %d exceeds 80: %q", got, stripANSI(line))
		}
	}
	if got := fieldValue(t, view, "Setup command"); got != m.repo.GetSetupScript() {
		t.Errorf("setup value lost content: %q", got)
	}
	redValue := ui.StyleStatusDanger.Render("(not set)")
	if !strings.Contains(view, redValue) {
		t.Errorf("missing Sentry organization lost danger style:\n%s", view)
	}
}
