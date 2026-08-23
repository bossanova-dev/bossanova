package views

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestListFilter_InitialStateIsIdle(t *testing.T) {
	f := newListFilter()
	if f.Active() {
		t.Errorf("new filter should not be Active")
	}
	if f.Applied() {
		t.Errorf("new filter should not be Applied")
	}
	if f.Engaged() {
		t.Errorf("new filter should not be Engaged")
	}
	if f.Query() != "" {
		t.Errorf("new filter Query() = %q, want empty", f.Query())
	}
	if f.Height() != 0 {
		t.Errorf("new filter Height() = %d, want 0", f.Height())
	}
	if f.View() != "" {
		t.Errorf("new filter View() should be empty")
	}
}

func TestListFilter_ActivateReset(t *testing.T) {
	f := newListFilter()
	_ = f.Activate()
	if !f.Active() || !f.Engaged() {
		t.Errorf("after Activate: Active=%v Engaged=%v, want both true", f.Active(), f.Engaged())
	}
	if f.Height() != 1 {
		t.Errorf("engaged Height() = %d, want 1", f.Height())
	}

	f.input.SetValue("abc")
	f.Reset()
	if f.Active() || f.Applied() || f.Engaged() {
		t.Errorf("after Reset: Active=%v Applied=%v Engaged=%v, want all false",
			f.Active(), f.Applied(), f.Engaged())
	}
	if f.Query() != "" {
		t.Errorf("Reset should clear query, got %q", f.Query())
	}
}

func TestListFilter_DismissPreservesRawInput(t *testing.T) {
	f := newListFilter()
	_ = f.Activate()
	f.input.SetValue("  abc  ")
	f.Dismiss()
	if f.Active() || f.Applied() || f.Engaged() {
		t.Errorf("after Dismiss: Active=%v Applied=%v Engaged=%v, want all false",
			f.Active(), f.Applied(), f.Engaged())
	}
	if got := f.input.Value(); got != "  abc  " {
		t.Fatalf("Dismiss raw input = %q, want byte-identical value", got)
	}
	if got := f.Query(); got != "abc" {
		t.Fatalf("Dismiss Query() = %q, want trimmed query", got)
	}
}

func TestListFilter_FalseCommitResetAndDismiss(t *testing.T) {
	for _, tc := range []struct {
		name     string
		value    string
		resetRaw string
		dismiss  string
	}{
		{name: "empty", value: "", resetRaw: "", dismiss: ""},
		{name: "whitespace", value: "   ", resetRaw: "", dismiss: "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reset := newListFilter()
			_ = reset.Activate()
			reset.input.SetValue(tc.value)
			if reset.Commit() {
				t.Fatal("Commit() = true, want false")
			}
			reset.Reset()
			if got := reset.input.Value(); got != tc.resetRaw {
				t.Fatalf("Reset raw input after false Commit() = %q, want %q", got, tc.resetRaw)
			}

			dismiss := newListFilter()
			_ = dismiss.Activate()
			dismiss.input.SetValue(tc.value)
			if dismiss.Commit() {
				t.Fatal("Commit() = true, want false")
			}
			dismiss.Dismiss()
			if got := dismiss.input.Value(); got != tc.dismiss {
				t.Fatalf("Dismiss raw input after false Commit() = %q, want %q", got, tc.dismiss)
			}
			if dismiss.Active() || dismiss.Applied() || dismiss.Engaged() {
				t.Fatalf("Dismiss state after false Commit(): Active=%v Applied=%v Engaged=%v, want all false",
					dismiss.Active(), dismiss.Applied(), dismiss.Engaged())
			}
		})
	}
}

func TestListFilter_CommitEmpty(t *testing.T) {
	f := newListFilter()
	_ = f.Activate()
	// No input set → commit returns false and leaves filter un-applied.
	if got := f.Commit(); got {
		t.Errorf("Commit() with empty query returned true, want false")
	}
	if f.Applied() {
		t.Errorf("Applied() should be false after committing empty query")
	}
}

func TestListFilter_CommitNonEmpty(t *testing.T) {
	f := newListFilter()
	_ = f.Activate()
	f.input.SetValue("login")
	if got := f.Commit(); !got {
		t.Errorf("Commit() with query returned false, want true")
	}
	if !f.Applied() {
		t.Errorf("Applied() should be true after committing non-empty query")
	}
	if f.Active() {
		t.Errorf("Active() should be false after Commit")
	}
	if !f.Engaged() {
		t.Errorf("Engaged() should be true after Commit with query")
	}
}

func TestListFilter_ActivateFromAppliedPreservesQuery(t *testing.T) {
	f := newListFilter()
	_ = f.Activate()
	f.input.SetValue("login")
	_ = f.Commit()
	_ = f.Activate()
	if !f.Active() {
		t.Errorf("Activate should set Active")
	}
	if f.Applied() {
		t.Errorf("Activate should clear Applied so we are back in editing mode")
	}
	if f.Query() != "login" {
		t.Errorf("Activate from applied state should preserve query, got %q", f.Query())
	}
}

func TestListFilter_UpdateReportsChangedForKeyInput(t *testing.T) {
	f := newListFilter()
	_ = f.Activate()

	_, changed := f.Update(tea.KeyPressMsg{Code: 'l'})
	if !changed {
		t.Fatal("Update key input changed = false, want true")
	}
	if got := f.Query(); got != "l" {
		t.Fatalf("Query() = %q, want %q", got, "l")
	}

	_, changed = f.Update(tea.PasteMsg{Content: ""})
	if changed {
		t.Fatal("Update empty paste changed = true, want false")
	}
}

func TestListFilter_UpdateAcceptsPasteMsg(t *testing.T) {
	f := newListFilter()
	_ = f.Activate()

	_, changed := f.Update(tea.PasteMsg{Content: "ENG-102 dark"})
	if !changed {
		t.Fatal("Update paste changed = false, want true")
	}
	if got := f.Query(); got != "ENG-102 dark" {
		t.Fatalf("Query() = %q, want pasted text", got)
	}
}

func TestListFilter_MatchesEmptyQueryAlwaysTrue(t *testing.T) {
	f := newListFilter()
	cases := []string{"", "anything", "#42 Fix login bug main"}
	for _, tc := range cases {
		if !f.Matches(tc) {
			t.Errorf("Matches(%q) with empty query = false, want true", tc)
		}
	}
}

func TestListFilter_MatchesCaseInsensitive(t *testing.T) {
	f := newListFilter()
	_ = f.Activate()
	f.input.SetValue("LoGiN")
	cases := []struct {
		haystack string
		want     bool
	}{
		{"fix login bug", true},
		{"FIX LOGIN BUG", true},
		{"Add login screen", true},
		{"  login  ", true},
		{"refactor auth flow", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := f.Matches(tc.haystack); got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.haystack, got, tc.want)
		}
	}
}

func TestListFilter_QueryTrimmed(t *testing.T) {
	f := newListFilter()
	_ = f.Activate()
	f.input.SetValue("   login   ")
	if got := f.Query(); got != "login" {
		t.Errorf("Query() = %q, want %q (trimmed)", got, "login")
	}
}

func TestListFilter_ViewIncludesCount(t *testing.T) {
	f := newListFilter()
	_ = f.Activate()
	f.input.SetValue("login")
	f.SetCounts(3, 47)
	v := f.View()
	if !strings.Contains(v, "3 of 47") {
		t.Errorf("View() = %q, expected to contain '3 of 47'", v)
	}
}

func TestListFilter_ActionBarNonEmpty(t *testing.T) {
	f := newListFilter()
	if len(f.ActionBar()) == 0 {
		t.Errorf("ActionBar() returned empty slice")
	}
}
