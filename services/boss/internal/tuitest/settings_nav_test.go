package tuitest_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
)

func openSettingsHub(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if err := h.Driver.WaitForText(waitTimeout, "[s]ettings"); err != nil {
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
