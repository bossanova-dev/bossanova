package views

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
