package ui

import (
	"image/color"
	"math"
	"strings"

	"charm.land/lipgloss/v2"
)

// A meter turns warning at 70% and danger at 90%.
const (
	meterWarnFraction     = .7
	meterCriticalFraction = .9
)

// MeterGlyph draws both runs of a meter, so only colour separates filled from
// empty and the two sit at one height. It is a box-drawing line rather than a
// full block: a line sits mid-cell, so meters on adjacent rows keep the
// terminal's line gap instead of fusing into one solid wall.
const MeterGlyph = "━"

// meterMutedFill is a muted meter's filled run: lighter than the ColorMuted
// track, so a disabled row's meter still shows how full it is.
var meterMutedFill = lipgloss.Color("#9A9A9A")

// meterColor is the filled run's colour for fraction.
func meterColor(fraction float64) color.Color {
	switch {
	case fraction >= meterCriticalFraction:
		return ColorDanger
	case fraction >= meterWarnFraction:
		return ColorWarning
	default:
		return ColorSuccess
	}
}

// RenderMeter renders fraction, clamped to [0, 1], as a width-cell bar: the
// filled run in its severity colour and the remainder muted.
func RenderMeter(fraction float64, width int) string {
	if width <= 0 {
		return ""
	}
	fraction = math.Max(0, math.Min(1, fraction))
	return renderMeter(fraction, width, meterColor(fraction))
}

// RenderMutedMeter is RenderMeter for a muted row: the filled run drops its
// severity colour but stays lighter than the track.
func RenderMutedMeter(fraction float64, width int) string {
	if width <= 0 {
		return ""
	}
	return renderMeter(math.Max(0, math.Min(1, fraction)), width, meterMutedFill)
}

func renderMeter(fraction float64, width int, fill color.Color) string {
	filled := int(math.Round(fraction * float64(width)))
	return meterRun(filled, fill) + meterRun(width-filled, ColorMuted)
}

// meterRun renders n glyphs in c, or nothing when n is zero so no empty colour
// escape lands in the cell.
func meterRun(n int, c color.Color) string {
	if n <= 0 {
		return ""
	}
	return lipgloss.NewStyle().Foreground(c).Render(strings.Repeat(MeterGlyph, n))
}
