package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// navigateToRepoSettings navigates from home → repo list → repo settings for the first repo.
func navigateToRepoSettings(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Bossanova")
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "PATH"); err != nil {
		t.Fatal(err)
	}

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
	if !strings.Contains(screen, "Auto-merge") {
		t.Fatalf("expected auto-merge checkbox on screen:\n%s", screen)
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
	time.Sleep(200 * time.Millisecond)
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

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
}

func TestTUI_RepoSettingsView_ToggleCheckbox(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	navigateToRepoSettings(t, h)

	// Navigate to "Auto-merge PRs" checkbox (row 3).
	for range 3 {
		if err := h.Driver.SendKey('j'); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Record initial state.
	screenBefore := h.Driver.Screen()
	hadCheck := strings.Contains(screenBefore, "[x]")

	// Toggle.
	if err := h.Driver.SendKey(' '); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	screenAfter := h.Driver.Screen()
	hasCheck := strings.Contains(screenAfter, "[x]")

	if hadCheck == hasCheck {
		t.Fatalf("expected checkbox state to change; before:\n%s\nafter:\n%s", screenBefore, screenAfter)
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
	time.Sleep(300 * time.Millisecond)

	// Type a new name. Clear existing first by selecting all, then type.
	// The TUI text input likely starts with current value selected.
	if err := h.Driver.SendString("renamed-app"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// Press enter to save.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	// The name should be updated in the display.
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "renamed-app") {
		t.Fatalf("expected 'renamed-app' after edit; screen:\n%s", screen)
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
