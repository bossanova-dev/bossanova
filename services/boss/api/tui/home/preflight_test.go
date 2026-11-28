package home

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/preflight"
)

func TestPreflightViewDaemonWaitAddsBlankLineBeforeWaiting(t *testing.T) {
	model := PreflightModel{
		issue: preflight.Issue{
			Title:  "Cannot connect to the bossd daemon",
			Detail: "daemon failed\nTry one of:\n\n  bossd",
		},
		retryCheck: func() bool { return false },
	}

	rendered := trimLineRightSpace(stripANSI(model.View().Content))
	if !strings.Contains(rendered, "  bossd\n\n  Waiting for the daemon") {
		t.Fatalf("daemon wait view missing blank line before waiting copy:\n%s", rendered)
	}
}

func TestPreflightWarningContinues(t *testing.T) {
	warningIssue := preflight.Issue{
		Title:   "codex is not logged in",
		Detail:  "Sessions using this agent fail until its CLI is logged in. Boss starts anyway.\n\n  codex: codex login",
		Warning: true,
	}
	warningStyled := lipgloss.NewStyle().Bold(true).Foreground(ui.ColorWarning).Padding(0, 2).Render(warningIssue.Title)
	dangerStyled := lipgloss.NewStyle().Bold(true).Foreground(ui.ColorDanger).Padding(0, 2).Render(warningIssue.Title)
	if warningStyled == dangerStyled {
		t.Fatal("warning and danger title styles rendered identically; colour check would be vacuous")
	}

	m := NewPreflightModel(warningIssue)
	raw := m.View().Content
	if !strings.Contains(raw, warningStyled) {
		t.Errorf("warning view did not render title in ColorWarning:\n%q", raw)
	}
	if strings.Contains(raw, dangerStyled) {
		t.Errorf("warning view rendered title in ColorDanger:\n%q", raw)
	}
	plain := stripANSI(raw)
	if !strings.Contains(plain, "Press enter to continue.") {
		t.Errorf("warning view missing continue footer:\n%s", plain)
	}
	if strings.Contains(plain, "Press q to quit.") {
		t.Errorf("warning view should not show quit footer:\n%s", plain)
	}

	dismissKeys := []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{"enter", tea.KeyPressMsg{Code: tea.KeyEnter}},
		{"space", tea.KeyPressMsg{Code: tea.KeySpace}},
		{"esc", tea.KeyPressMsg{Code: tea.KeyEscape}},
		{"q", tea.KeyPressMsg{Code: 'q', Text: "q"}},
	}
	for _, tc := range dismissKeys {
		t.Run("warning dismiss "+tc.name, func(t *testing.T) {
			final, cmd := NewPreflightModel(warningIssue).Update(tc.msg)
			if !isQuitCmd(cmd) {
				t.Fatalf("%s did not quit warning screen", tc.name)
			}
			if err := preflightResult(final, nil); err != nil {
				t.Fatalf("preflightResult after %s = %v, want nil", tc.name, err)
			}
		})
	}

	if _, cmd := NewPreflightModel(warningIssue).Update(tea.KeyPressMsg{Code: 'a', Text: "a"}); cmd != nil {
		t.Fatal("unrelated key should not dismiss warning screen")
	}

	t.Run("non-warning keeps danger title footer and ctrl+c nil result", func(t *testing.T) {
		blockingIssue := preflight.Issue{
			Title:  "codex was not found for this project",
			Detail: "install codex",
		}
		blockDanger := lipgloss.NewStyle().Bold(true).Foreground(ui.ColorDanger).Padding(0, 2).Render(blockingIssue.Title)
		blockWarning := lipgloss.NewStyle().Bold(true).Foreground(ui.ColorWarning).Padding(0, 2).Render(blockingIssue.Title)

		bm := NewPreflightModel(blockingIssue)
		blockRaw := bm.View().Content
		if !strings.Contains(blockRaw, blockDanger) {
			t.Errorf("blocking view did not render title in ColorDanger:\n%q", blockRaw)
		}
		if strings.Contains(blockRaw, blockWarning) {
			t.Errorf("blocking view rendered title in ColorWarning:\n%q", blockRaw)
		}
		blockPlain := stripANSI(blockRaw)
		if !strings.Contains(blockPlain, "Press q to quit.") {
			t.Errorf("blocking view missing quit footer:\n%s", blockPlain)
		}
		if strings.Contains(blockPlain, "Press enter to continue.") {
			t.Errorf("blocking view should not show continue footer:\n%s", blockPlain)
		}

		for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: tea.KeySpace}} {
			if _, cmd := bm.Update(key); cmd != nil {
				t.Fatalf("blocking screen should not quit on %q", key.String())
			}
		}

		final, cmd := bm.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		if !isQuitCmd(cmd) {
			t.Fatal("ctrl+c did not quit blocking screen")
		}
		if err := preflightResult(final, nil); err != nil {
			t.Fatalf("preflightResult after blocking ctrl+c = %v, want nil", err)
		}
	})
}

func TestPreflightWarningCtrlCCancels(t *testing.T) {
	m := NewPreflightModel(preflight.Issue{
		Title:   "codex is not logged in",
		Detail:  "  codex: codex login",
		Warning: true,
	})
	final, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !isQuitCmd(cmd) {
		t.Fatal("ctrl+c did not quit warning screen")
	}
	pm, ok := final.(PreflightModel)
	if !ok {
		t.Fatalf("final model = %T, want PreflightModel", final)
	}
	if !pm.cancelled {
		t.Fatal("ctrl+c on warning screen did not set cancelled flag")
	}
	err := preflightResult(final, nil)
	if !IsPreflightCancelled(err) {
		t.Fatalf("preflightResult = %v, want ErrPreflightCancelled", err)
	}
}

func TestPreflightResultMapsCancelled(t *testing.T) {
	runErr := errors.New("terminal broken")

	if err := preflightResult(PreflightModel{cancelled: true}, nil); !errors.Is(err, ErrPreflightCancelled) {
		t.Fatalf("cancelled model returned %v, want ErrPreflightCancelled", err)
	}
	if err := preflightResult(PreflightModel{cancelled: false}, nil); err != nil {
		t.Fatalf("non-cancelled model returned %v, want nil", err)
	}
	if err := preflightResult(PreflightModel{cancelled: true}, runErr); !errors.Is(err, runErr) {
		t.Fatalf("run error returned %v, want %v", err, runErr)
	}
	if err := preflightResult(PreflightModel{cancelled: false}, runErr); !errors.Is(err, runErr) {
		t.Fatalf("run error returned %v, want %v", err, runErr)
	}
}

func isQuitCmd(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func stripANSI(s string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return re.ReplaceAllString(s, "")
}

func trimLineRightSpace(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.Join(lines, "\n")
}
