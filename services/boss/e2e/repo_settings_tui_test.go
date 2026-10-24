package e2e_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// navigateToRepoSettings navigates from home → settings → repo list → repo settings for the first repo.
func navigateToRepoSettings(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Bossanova")
	}); err != nil {
		t.Fatal(err)
	}

	openSettingsView(t, h, 'r', "PATH")

	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Name"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_RepoSettingsView_Content(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	navigateToRepoSettings(t, h)

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "my-app") {
		t.Fatalf("expected repo name 'my-app' on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Merge strategy") {
		t.Fatalf("expected 'Merge strategy' on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Automations") {
		t.Fatalf("expected 'Automations' section heading on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Integrations") {
		t.Fatalf("expected 'Integrations' section heading on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Auto-merge Dependabot PRs") {
		t.Fatalf("expected Dependabot auto-merge checkbox on screen:\n%s", screen)
	}
}

func TestTUI_RepoSettingsView_CycleMergeStrategy(t *testing.T) {
	repos := []*pb.Repo{
		{Id: "repo-1", DisplayName: "my-app", LocalPath: "/tmp/my-app", DefaultBaseBranch: "main", MergeStrategy: "merge"},
	}
	h := tuitest.New(t,
		tuitest.WithRepos(repos...),
	)

	navigateToRepoSettings(t, h)

	// Navigate to "Merge strategy" (row 2: Name=0, Setup command=1, Merge strategy=2).
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool { return fieldCursorOnLine(screen, "Merge") }); err != nil {
		t.Fatal(err)
	}

	// Should show "Merge commit" initially.
	if err := h.Driver.WaitForText(waitTimeout, "Merge commit"); err != nil {
		t.Fatalf("expected 'Merge commit'; screen:\n%s", h.Driver.Screen())
	}

	// Press enter to cycle to "Rebase".
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Rebase"); err != nil {
		t.Fatalf("expected 'Rebase' after cycling; screen:\n%s", h.Driver.Screen())
	}

	// Press enter again to cycle to "Squash".
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Squash"); err != nil {
		t.Fatalf("expected 'Squash' after cycling; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitFor(waitTimeout, func(string) bool {
		persisted := realRepos(t, h)
		return len(persisted) == 1 && persisted[0].GetMergeStrategy() == "squash"
	}); err != nil {
		t.Fatalf("merge strategy was not persisted: %v", err)
	}
}

func TestTUI_RepoSettingsView_ToggleCheckbox(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	navigateToRepoSettings(t, h)

	// Navigate to the "Mark ready for review when checks pass" checkbox (row 4,
	// the CanAutoMerge flag).
	for range 4 {
		if err := h.Driver.SendKey('j'); err != nil {
			t.Fatal(err)
		}
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool { return fieldCursorOnLine(screen, "Mark ready") }); err != nil {
		t.Fatal(err)
	}

	// Record initial state of the specific "Mark ready" checkbox — not a
	// whole-screen [x] scan, which other default-on toggles (e.g.
	// archive-after-merge) would confound.
	const markReady = "[x] Mark ready for review when checks pass"
	screenBefore := h.Driver.Screen()
	hadCheck := strings.Contains(screenBefore, markReady)

	// Toggle.
	if err := h.Driver.SendKey(' '); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool { return strings.Contains(screen, markReady) != hadCheck }); err != nil {
		t.Fatalf("checkbox did not change: %v", err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(string) bool {
		persisted := realRepos(t, h)
		return len(persisted) == 1 && persisted[0].GetCanAutoMerge() != hadCheck
	}); err != nil {
		t.Fatalf("checkbox was not persisted: %v", err)
	}

}

func TestTUI_RepoSettingsView_EditName(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	navigateToRepoSettings(t, h)

	// First row is Name. Press enter to edit.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[enter] save"); err != nil {
		t.Fatal(err)
	}

	// Append to the existing name, then verify the persisted result.
	if err := h.Driver.SendString("renamed-app"); err != nil {
		t.Fatal(err)
	}

	// Press enter to save.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(string) bool {
		for _, repo := range realRepos(t, h) {
			if strings.Contains(repo.GetDisplayName(), "renamed-app") {
				return true
			}
		}
		return false
	}); err != nil {
		t.Fatalf("repo name was not persisted: %v", err)
	}

}

func TestTUI_RepoSettingsView_Back(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	navigateToRepoSettings(t, h)

	// Press esc to go back.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	// Should return to repo list.
	if err := h.Driver.WaitForText(waitTimeout, "PATH"); err != nil {
		t.Fatalf("expected repo list after esc; screen:\n%s", h.Driver.Screen())
	}
}
