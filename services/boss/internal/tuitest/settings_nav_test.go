package tuitest_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
)

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
