package repos

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var focusGutterGlyph = []rune(lipgloss.ThickBorder().Left)[0]

func stripANSI(s string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return re.ReplaceAllString(s, "")
}
func lineIndexContaining(t *testing.T, lines []string, want string) int {
	t.Helper()
	for i, line := range lines {
		if strings.Contains(line, want) {
			return i
		}
	}
	t.Fatalf("no rendered line contains %q; lines:\n%s", want, strings.Join(lines, "\n"))
	return -1
}
func keyPress(ch rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: ch, Text: string(ch)}
}
func specialKeyPress(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code}
}

type fakeGitHubActions struct{ open func(string) error }

func (f fakeGitHubActions) OpenURL(url string) error {
	if f.open != nil {
		return f.open(url)
	}
	return nil
}
func (fakeGitHubActions) EnrichInstallURL(_ context.Context, url, _ string) string { return url }
func columnTitles(cols []table.Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Title
	}
	return out
}
func viewString(m tea.Model) string {
	return m.View().Content
}
