package settings

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
)

func TestGeneralSettings_BothTracingTogglesAreCheckboxes(t *testing.T) {
	const (
		eventLabel = "Enable event tracing (for debugging problems)"
		errorLabel = "Enable error tracking (sends panics to Sentry)"
	)

	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			withTempConfigHome(t)
			m := NewGeneralSettingsModel(nil, context.Background(), nil)
			m.settings.EventTracingEnabled = enabled
			m.settings.ErrorTrackingEnabled = enabled
			m.rebuildRows()
			view := ansi.Strip(m.View().Content)

			for _, label := range []string{eventLabel, errorLabel} {
				want := ui.RenderCheckboxLabel(enabled, label)
				if !strings.Contains(view, want) {
					t.Errorf("general settings missing %q; view:\n%s", want, view)
				}
				// The exact shape the error-tracking row used to render. Pinned
				// as a negative so a revert to the value row fails here rather
				// than passing a label-only Contains.
				for _, stale := range []string{label + ": ON", label + ": OFF"} {
					if strings.Contains(view, stale) {
						t.Errorf("general settings still renders %q as a value row (%q), not a checkbox", label, stale)
					}
				}
			}
		})
	}
}
