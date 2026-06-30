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
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Settings") &&
			strings.Contains(screen, "[r]epos") &&
			strings.Contains(screen, "[c]ron") &&
			strings.Contains(screen, "[t]rash")
	}); err != nil {
		t.Fatalf("expected settings hub actions; screen:\n%s", h.Driver.Screen())
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
