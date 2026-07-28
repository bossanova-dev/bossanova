package views

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// TestRepoAdd_DetailsForm_RendersAllAutomationOptions guards against a huh
// MultiSelect viewport-height bug that clipped the last option: with no explicit
// Height, huh sized the option viewport to (options height - title height),
// dropping the trailing "Automatic repair" row. All three automation options
// must render so the add wizard matches the edit screen.
func TestRepoAdd_DetailsForm_RendersAllAutomationOptions(t *testing.T) {
	m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.buildDetailsForm()
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = updated.(RepoAddModel)

	view := m.View().Content
	for _, want := range []string{
		"Mark ready for review when checks pass",
		"Auto-merge Dependabot PRs",
		"Automatic repair (failing checks, conflicts, review feedback)",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("automation option %q not rendered; view:\n%s", want, view)
		}
	}
}

// TestRepoAdd_DetailsForm_MergeStrategyFitsItsOptions is the rendered guard for
// the other select whose sizing BOS-567 changed. bossSelect's option count is a
// constructor argument, but nothing type-checks it against the options that
// land afterwards — and headerLines is a bare literal that no gate can see. Both
// failure modes are silent: too small clips the last strategy, too large pads
// the block with blank lines. Assert the block is exactly its three options.
func TestRepoAdd_DetailsForm_MergeStrategyFitsItsOptions(t *testing.T) {
	m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.buildDetailsForm()
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = updated.(RepoAddModel)

	view := m.View().Content
	for _, want := range []string{"Merge commit", "Rebase", "Squash"} {
		if !strings.Contains(view, want) {
			t.Errorf("merge strategy option %q not rendered; view:\n%s", want, view)
		}
	}

	lines := strings.Split(view, "\n")
	title := lineIndexContaining(t, lines, "Merge strategy")
	next := title + 1 + lineIndexContaining(t, lines[title+1:], "Automation")
	// huh separates fields with one blank line, so the block between the two
	// titles is the three options plus that separator.
	if got, want := next-title-1, 4; got != want {
		t.Errorf("merge strategy block is %d lines, want %d (3 options + huh's field separator); view:\n%s",
			got, want, view)
	}
}

// TestRepoAdd_InputForm_RendersNavigationHints covers the very first screen a
// new user meets (the zero-repo redirect lands here): the form must advertise
// how to move between its fields.
func TestRepoAdd_InputForm_RendersNavigationHints(t *testing.T) {
	m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
	m.phase = repoAddPhaseInput
	m.sourceMode = sourceModeOpen
	m.buildInputForm()
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = updated.(RepoAddModel)

	view := m.View().Content
	for _, want := range []string{
		"[tab] next field",
		"[shift+tab] previous field",
		"[enter] continue",
		"[esc] back",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("input form action bar missing %q; view:\n%s", want, view)
		}
	}
}

func TestRepoAdd_DetailsForm_RendersNavigationHints(t *testing.T) {
	m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.buildDetailsForm()
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = updated.(RepoAddModel)

	view := m.View().Content
	for _, want := range []string{
		"[tab] next field",
		"[shift+tab] previous field",
		"[enter] add repo",
		"[esc] back",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("details form action bar missing %q; view:\n%s", want, view)
		}
	}
}

// TestRepoAdd_DetailsForm_ActionBarFitsShortTerminal guards the height budget.
// The 5-field details form renders ~22 lines unconstrained; App.View prepends
// bannerOverhead more, which overflows an 80x24 terminal and clips exactly the
// action bar this view exists to advertise. repoAddChrome + formHeight keep the
// whole view within the terminal, as cron_form already does.
func TestRepoAdd_DetailsForm_ActionBarFitsShortTerminal(t *testing.T) {
	const termHeight = 24

	m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.height = termHeight
	m.buildDetailsForm()
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: termHeight})
	m = updated.(RepoAddModel)

	view := m.View().Content
	if total := lipgloss.Height(view) + bannerOverhead; total > termHeight {
		t.Errorf("details view is %d lines on a %d-line terminal; the action bar is clipped\n%s",
			total, termHeight, view)
	}
	for _, want := range []string{"[tab] next field", "[enter] add repo"} {
		if !strings.Contains(view, want) {
			t.Errorf("details view missing %q at height %d; view:\n%s", want, termHeight, view)
		}
	}
}

// TestRepoAdd_InputForm_NotHeightPadded pins the deliberate asymmetry: huh's
// viewport pads to whatever height it is given, so only the tall details form
// carries a budget. Budgeting the 1-field input form would inflate it to fill
// the terminal.
func TestRepoAdd_InputForm_NotHeightPadded(t *testing.T) {
	m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
	m.phase = repoAddPhaseInput
	m.sourceMode = sourceModeOpen
	m.height = 60
	m.buildInputForm()
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 60})

	if got := lipgloss.Height(updated.(RepoAddModel).View().Content); got > 12 {
		t.Errorf("input form view is %d lines on a 60-line terminal; it should not be padded to fill", got)
	}
}
