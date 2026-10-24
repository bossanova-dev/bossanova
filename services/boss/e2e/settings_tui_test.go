package e2e_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/realstack"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestTUI_SettingsView_Content(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	openGeneralSettings(t, h)

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Worktree base directory") {
		t.Fatalf("expected worktree setting on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Poll interval") {
		t.Fatalf("expected poll interval setting on screen:\n%s", screen)
	}
}

func TestTUI_SettingsView_EditPollIntervalPersists(t *testing.T) {
	h := tuitest.New(t, tuitest.WithRepos(testRepos()...))
	openGeneralSettings(t, h)
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool { return fieldCursorOnLine(screen, "Poll interval") }); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[enter] save"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey(0x15); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendString("45"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		response, err := h.Stack.Client().GetSettings(ctx, connect.NewRequest(&pb.GetSettingsRequest{}))
		return err == nil && response.Msg.GetSettings().GetPollIntervalSeconds() == 45
	}); err != nil {
		t.Fatalf("poll interval was not persisted: %v", err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('g'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Poll interval (seconds): 45"); err != nil {
		t.Fatal(err)
	}
}

func TestTUI_SettingsView_JKNavigation(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	openGeneralSettings(t, h)

	// Move through the built-in settings and return to the worktree row.
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('k'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('k'); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool { return fieldCursorOnLine(screen, "Worktree base directory") }); err != nil {
		t.Fatal(err)
	}

	// All settings should still be visible.
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Poll interval") {
		t.Fatalf("expected poll interval setting on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Worktree base directory") {
		t.Fatalf("expected worktree setting on screen:\n%s", screen)
	}
}

func TestTUI_SettingsView_EditCancel(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	openGeneralSettings(t, h)

	// Press enter to edit.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[enter] save"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendString("DISCARDED"); err != nil {
		t.Fatal(err)
	}

	// Press escape to cancel the edit.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "[enter/space] toggle/edit"); err != nil {
		t.Fatal(err)
	}

	// Should still be in settings (not navigated away).
	if strings.Contains(h.Driver.Screen(), "DISCARDED") {
		t.Fatal("cancelled edit remained visible")
	}
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Worktree base directory") {
		t.Fatalf("expected to still be in settings after edit cancel; screen:\n%s", screen)
	}
}

func TestTUI_SettingsView_BackWithQ(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Settings"); err != nil {
		t.Fatal(err)
	}

	// Press esc to go back (q no longer works on sub-screens).
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatalf("expected to return to home after esc from settings; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_Settings_RepoAddFirstRepoReturnsToSettings(t *testing.T) {
	h := tuitest.New(t)
	repoPath := realstack.InitRepo(t, h.Stack.Home(), "widgets")

	// With no repos, the app lands on the Home empty state (no auto-route). Open
	// Settings via the 's' key (the hint is hidden on the zero-repo screen).
	waitForZeroRepoHome(t, h)
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Settings") &&
			strings.Contains(screen, "[r]epos")
	}); err != nil {
		t.Fatalf("expected settings repos action; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Press 'a' to add your first repository"); err != nil {
		t.Fatalf("expected empty repo list from settings; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Open project"); err != nil {
		t.Fatalf("expected source phase; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add a local repository"); err != nil {
		t.Fatalf("expected input phase; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey(0x15); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendString(repoPath); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Merge strategy"); err != nil {
		t.Fatalf("expected details phase; screen:\n%s", h.Driver.Screen())
	}
	// Accept every details field with its default, including untrusted.
	completeRepoAddDetails(t, h)

	// Completing the add returns to the settings hub it was launched from; the
	// repo's options were configured inline on the add wizard.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Settings") &&
			strings.Contains(screen, "[r]epos") &&
			strings.Contains(screen, "[c]ron")
	}); err != nil {
		t.Fatalf("expected settings after adding first repo from settings; screen:\n%s", h.Driver.Screen())
	}
	if got := len(realRepos(t, h)); got != 1 {
		t.Fatalf("registered repos = %d, want 1", got)
	}
}

// waitForZeroRepoHome waits for the Home empty state shown to a user with no
// repos. Zero-repo users are no longer auto-routed into the Add Repository
// wizard; they land on the welcome screen and add a repo via [enter].
func waitForZeroRepoHome(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if err := h.Driver.WaitForText(waitTimeout, "Welcome to Bossanova"); err != nil {
		t.Fatalf("expected zero-repo home empty state; screen:\n%s", h.Driver.Screen())
	}
}

func openSettingsHub(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	// Home is ready when either the [s]ettings hint (repos present) or the
	// zero-repo welcome screen is showing. The zero-repo empty state no longer
	// advertises [s]ettings, but the 's' key still opens the settings hub.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "[s]ettings") ||
			strings.Contains(screen, "Welcome to Bossanova")
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	waitForSettingsHub(t, h)
}

// onSettingsHub reports whether a screen is the Settings hub. It is the ONE
// definition of hub identity for this package — every call site goes through
// it so a future change to the hub's chrome has a single place to update.
//
// The hub is identified by its section rows rather than the action-bar
// accelerators (BOS-511): the rows are the screen's content, whereas the
// action bar is advisory chrome whose wording and length are free to change
// (it grew a [g]eneral entry in this very change).
func onSettingsHub(screen string) bool {
	return strings.Contains(screen, "Settings") &&
		strings.Contains(screen, "General") &&
		strings.Contains(screen, "Cron jobs") &&
		strings.Contains(screen, "Trash")
}

// waitForSettingsHub blocks until the Settings hub is on screen.
func waitForSettingsHub(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if err := h.Driver.WaitFor(waitTimeout, onSettingsHub); err != nil {
		t.Fatalf("expected settings hub sections; screen:\n%s", h.Driver.Screen())
	}
}

// openGeneralSettings opens the Settings hub and jumps to the General section,
// which owns the global toggles that used to live on the hub itself (BOS-511).
func openGeneralSettings(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	openSettingsHub(t, h)
	if err := h.Driver.SendKey('g'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Worktree base directory"); err != nil {
		t.Fatalf("expected the general settings list; screen:\n%s", h.Driver.Screen())
	}
}

func openSettingsView(t *testing.T, h *tuitest.Harness, key byte, waitText string) {
	t.Helper()

	openSettingsHub(t, h)
	if err := h.Driver.SendKey(key); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, waitText); err != nil {
		t.Fatalf("expected %q after settings action %q; screen:\n%s", waitText, key, h.Driver.Screen())
	}
}
