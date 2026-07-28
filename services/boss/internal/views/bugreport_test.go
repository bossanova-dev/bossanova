package views

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

func TestBugReportUpdateHandlesSubmitResults(t *testing.T) {
	m := NewBugReportModel(nil, context.Background(), nil, ViewHome, nil, nil)

	updated, cmd := m.Update(bugReportSubmitMsg{err: errors.New("no network")})
	errModel := updated.(BugReportModel)
	if cmd != nil {
		t.Fatal("error submit result returned command, want nil")
	}
	if errModel.phase != bugReportPhaseError {
		t.Fatalf("phase = %v, want error", errModel.phase)
	}
	if errModel.err == nil || errModel.err.Error() != "no network" {
		t.Fatalf("err = %v, want no network", errModel.err)
	}

	updated, cmd = m.Update(bugReportSubmitMsg{reportID: "report-1234567890"})
	successModel := updated.(BugReportModel)
	if cmd == nil {
		t.Fatal("success submit result returned nil command, want dismiss tick")
	}
	if successModel.phase != bugReportPhaseSuccess {
		t.Fatalf("phase = %v, want success", successModel.phase)
	}
	if successModel.reportID != "report-1234567890" {
		t.Fatalf("reportID = %q", successModel.reportID)
	}
}

func TestBugReportUpdateDismissesTerminalPhases(t *testing.T) {
	cases := []struct {
		name  string
		model BugReportModel
		msg   tea.Msg
	}{
		{
			name:  "success key",
			model: BugReportModel{phase: bugReportPhaseSuccess},
			msg:   tea.KeyPressMsg{Code: 'x', Text: "x"},
		},
		{
			name:  "error esc",
			model: BugReportModel{phase: bugReportPhaseError},
			msg:   tea.KeyPressMsg{Code: tea.KeyEsc},
		},
		{
			name:  "error enter",
			model: BugReportModel{phase: bugReportPhaseError},
			msg:   tea.KeyPressMsg{Code: tea.KeyEnter},
		},
		{
			name:  "dismiss message",
			model: BugReportModel{phase: bugReportPhaseSuccess},
			msg:   bugReportDismissMsg{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, cmd := tc.model.Update(tc.msg)
			got := updated.(BugReportModel)
			if cmd != nil {
				t.Fatal("dismiss transition returned command, want nil")
			}
			if !got.done {
				t.Fatal("done = false, want true")
			}
		})
	}
}

func TestBugReportUpdateEditingCancelAndFormStates(t *testing.T) {
	t.Run("esc cancels editing", func(t *testing.T) {
		m := BugReportModel{phase: bugReportPhaseEditing}
		updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		got := updated.(BugReportModel)
		if cmd != nil {
			t.Fatal("cancel returned command, want nil")
		}
		if !got.cancel {
			t.Fatal("cancel = false, want true")
		}
	})

	t.Run("nil form ignores input", func(t *testing.T) {
		m := BugReportModel{phase: bugReportPhaseEditing}
		updated, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
		got := updated.(BugReportModel)
		if cmd != nil {
			t.Fatal("nil form returned command, want nil")
		}
		if got.cancel || got.done {
			t.Fatalf("unexpected terminal flags: cancel=%v done=%v", got.cancel, got.done)
		}
	})

	t.Run("aborted form cancels", func(t *testing.T) {
		m := NewBugReportModel(nil, context.Background(), nil, ViewHome, nil, nil)
		m.form.State = huh.StateAborted
		updated, cmd := m.Update(struct{}{})
		got := updated.(BugReportModel)
		if cmd != nil {
			t.Fatal("aborted form returned command, want nil")
		}
		if !got.cancel {
			t.Fatal("cancel = false, want true")
		}
	})

	t.Run("completed form submits", func(t *testing.T) {
		m := NewBugReportModel(nil, context.Background(), nil, ViewHome, nil, nil)
		m.form.State = huh.StateCompleted
		updated, cmd := m.Update(struct{}{})
		got := updated.(BugReportModel)
		if cmd == nil {
			t.Fatal("completed form returned nil command, want submit batch")
		}
		if got.phase != bugReportPhaseSubmitting {
			t.Fatalf("phase = %v, want submitting", got.phase)
		}
	})
}

func TestBugReportViewTerminalStates(t *testing.T) {
	t.Run("submitting", func(t *testing.T) {
		m := BugReportModel{phase: bugReportPhaseSubmitting}
		if got := m.View().Content; !strings.Contains(got, "Submitting report") {
			t.Fatalf("view = %q, want submitting message", got)
		}
	})

	t.Run("success truncates report reference", func(t *testing.T) {
		m := BugReportModel{phase: bugReportPhaseSuccess, reportID: "abcdefghjklmnop"}
		got := m.View().Content
		if !strings.Contains(got, "ref abcdefg.") {
			t.Fatalf("view = %q, want short report ref", got)
		}
		if strings.Contains(got, "abcdefgh") {
			t.Fatalf("view = %q, report ref was not truncated", got)
		}
	})

	t.Run("success without report reference", func(t *testing.T) {
		m := BugReportModel{phase: bugReportPhaseSuccess}
		got := m.View().Content
		if !strings.Contains(got, "Report submitted. Thanks.") {
			t.Fatalf("view = %q, want no-ref success message", got)
		}
	})

	t.Run("error", func(t *testing.T) {
		m := BugReportModel{phase: bugReportPhaseError, err: errors.New("denied"), width: 80}
		got := m.View().Content
		if !strings.Contains(got, "Could not submit report: denied") {
			t.Fatalf("view = %q, want error message", got)
		}
		if !strings.Contains(got, "[esc] dismiss") {
			t.Fatalf("view = %q, want dismiss action", got)
		}
	})

	t.Run("nil form", func(t *testing.T) {
		m := BugReportModel{phase: bugReportPhaseEditing}
		if got := m.View().Content; got != "" {
			t.Fatalf("view = %q, want empty", got)
		}
	})
}

func TestBugReportView_ActionBarNavigationHints(t *testing.T) {
	m := NewBugReportModel(nil, context.Background(), nil, ViewHome, nil, nil)

	view := m.View().Content
	for _, want := range []string{
		"[tab] next field",
		"[shift+tab] previous field",
		"[enter] submit",
		"[esc] cancel",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("bug report action bar missing %q; view:\n%s", want, view)
		}
	}
}

// TestBugReportView_CommentGrowsWithContent mirrors the cron form's Prompt
// sizing on the other huh Text field in the package (BOS-567 ask #3): the box
// opens at one row rather than the textarea's fixed six, and grows as the user
// types.
func TestBugReportView_CommentGrowsWithContent(t *testing.T) {
	m := NewBugReportModel(nil, context.Background(), nil, ViewHome, nil, nil)
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}

	rows := func(view string) int {
		t.Helper()
		lines := strings.Split(view, "\n")
		title := lineIndexContaining(t, lines, "What went wrong?")
		bar := lineIndexContaining(t, lines, formNavHints()[0])
		block := lines[title+1 : bar]
		for len(block) > 0 && strings.TrimSpace(block[len(block)-1]) == "" {
			block = block[:len(block)-1]
		}
		return len(block)
	}

	if got := rows(m.View().Content); got != 1 {
		t.Fatalf("empty comment renders %d rows, want 1; view:\n%s", got, m.View().Content)
	}

	for _, key := range []tea.KeyPressMsg{
		keyPress('a'), cronPromptNewline, keyPress('b'), cronPromptNewline, keyPress('c'),
	} {
		updated, _ := m.Update(key)
		next, ok := updated.(BugReportModel)
		if !ok {
			t.Fatalf("updated model = %T, want BugReportModel", updated)
		}
		m = next
	}

	if got := rows(m.View().Content); got != 3 {
		t.Fatalf("three-line comment renders %d rows, want 3; view:\n%s", got, m.View().Content)
	}
}

// TestBugReportView_CommentSurvivesTheWrapBoundary is the regression for the
// one input length where sizing the box to its content used to hide that
// content: a line that exactly fills the wrap width.
//
// bubbles' textarea closes a logical line with `>=` rather than `>`, so such a
// line occupies two rows — the text and the row the cursor moves onto. A
// one-row box therefore cannot show the cursor, the textarea scrolls its own
// viewport to reveal it, and it never scrolls back: the line the user just
// typed is gone for the rest of the edit. The pre-BOS-567 fixed six-row box
// hid the bug by being too tall to notice.
func TestBugReportView_CommentSurvivesTheWrapBoundary(t *testing.T) {
	m := NewBugReportModel(nil, context.Background(), nil, ViewHome, nil, nil)
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}

	// Prose rather than one long run: the boundary has to survive real word
	// wrapping, and it is crossed here three times.
	const prose = "The daemon dropped my session while CI was still running, and the " +
		"chat row vanished from boss chats even though the transcript was still " +
		"on disk under the project slug. Reproduced twice this morning."

	typed := ""
	for _, r := range prose {
		updated, _ := m.Update(keyPress(r))
		next, ok := updated.(BugReportModel)
		if !ok {
			t.Fatalf("updated model = %T, want BugReportModel", updated)
		}
		m = next
		typed += string(r)

		// The opening of the comment must stay on screen the whole way: once
		// the textarea scrolls past it, it never comes back.
		opening := typed[:min(len(typed), 40)]
		if !strings.Contains(m.View().Content, opening) {
			t.Fatalf("after typing %d columns the box no longer shows the opening %q; view:\n%s",
				len(typed), opening, m.View().Content)
		}
	}
}
