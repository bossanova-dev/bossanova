package ui

import (
	"math"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestRenderMeterFillsProportionallyAtFixedWidth(t *testing.T) {
	for _, width := range []int{5, 10} {
		for _, tc := range []struct {
			fraction float64
			filled   int
		}{{-1, 0}, {0, 0}, {.5, (width + 1) / 2}, {1, width}, {2, width}} {
			meter := RenderMeter(tc.fraction, width)
			if got := ansi.Strip(meter); got != strings.Repeat(MeterGlyph, width) {
				t.Fatalf("meter(%v, %d) = %q, want %d glyphs of one height", tc.fraction, width, got, width)
			}
			want := meterRun(tc.filled, meterColor(math.Max(0, math.Min(1, tc.fraction)))) + meterRun(width-tc.filled, ColorMuted)
			if meter != want {
				t.Errorf("meter(%v, %d) = %q, want %d filled cells then muted ones (%q)", tc.fraction, width, meter, tc.filled, want)
			}
		}
	}
	if got := RenderMeter(.5, 0); got != "" {
		t.Errorf("zero-width meter = %q", got)
	}
}

func TestRenderMutedMeterStillShowsItsFill(t *testing.T) {
	full, empty := RenderMutedMeter(.9, 5), RenderMutedMeter(0, 5)
	if full == empty {
		t.Fatal("a muted 90% meter renders the same as an empty one")
	}
	if strings.Contains(full, lipgloss.NewStyle().Foreground(ColorDanger).Render(MeterGlyph)) {
		t.Error("a muted meter keeps its severity colour")
	}
	if got := ansi.Strip(full); got != strings.Repeat(MeterGlyph, 5) {
		t.Errorf("muted meter = %q, want 5 glyphs", got)
	}
}

func TestRenderMeterColoursBySeverity(t *testing.T) {
	for _, tc := range []struct {
		fraction float64
		want     lipgloss.Style
	}{
		{.699, lipgloss.NewStyle().Foreground(ColorSuccess)},
		{.7, lipgloss.NewStyle().Foreground(ColorWarning)},
		{.899, lipgloss.NewStyle().Foreground(ColorWarning)},
		{.9, lipgloss.NewStyle().Foreground(ColorDanger)},
	} {
		filled := tc.want.Render(strings.Repeat(MeterGlyph, int(tc.fraction*10+.5)))
		if !strings.HasPrefix(RenderMeter(tc.fraction, 10), filled) {
			t.Errorf("meter(%v) does not use the expected severity colour", tc.fraction)
		}
	}
}
