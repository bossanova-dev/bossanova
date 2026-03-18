package views

import (
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/recurser/bossalib/buildinfo"
)

// Shared layout constants for consistent TUI styling.
const (
	shortIDLen    = 7 // characters shown for truncated UUIDs
	colGap        = 2 // spaces between table columns
	actionBarPadY = 1 // blank lines above action bar
)

// colSep is the string used to separate table columns.
var colSep = strings.Repeat(" ", colGap)

// State color scheme matching the TS implementation.
var (
	colorGreen  = lipgloss.Color("#04B575")
	colorYellow = lipgloss.Color("#DBBD70")
	colorRed    = lipgloss.Color("#FF6347")
	colorCyan   = lipgloss.Color("#00CED1")
	colorGray   = lipgloss.Color("#626262")

	styleTitle     = lipgloss.NewStyle().Bold(true).Padding(0, 2)
	styleSelected  = lipgloss.NewStyle().Bold(true)
	styleActionBar = lipgloss.NewStyle().Faint(true).Padding(actionBarPadY, 2)
	styleError     = lipgloss.NewStyle().Foreground(colorRed).Padding(1, 2)
	styleSubtle    = lipgloss.NewStyle().Faint(true)
)

// bannerGradient defines a horizontal color gradient for the B icon (dawn palette).
var bannerGradient = []color.Color{
	lipgloss.Color("#00C6FF"),
	lipgloss.Color("#00AAFF"),
	lipgloss.Color("#008EFF"),
	lipgloss.Color("#0072FF"),
}

func renderBanner() string {
	cwd, _ := os.Getwd()
	if home, err := os.UserHomeDir(); err == nil {
		cwd = strings.Replace(cwd, home, "~", 1)
	}

	// Logo chars per row, matching `npx oh-my-logo "B" dawn --filled --block-font tiny`.
	row1 := []string{" ", "█", "▄", "▄"}
	row2 := []string{" ", "█", "▄", "█"}

	colorize := func(chars []string) string {
		var b strings.Builder
		for i, ch := range chars {
			b.WriteString(lipgloss.NewStyle().Foreground(bannerGradient[i]).Render(ch))
		}
		return b.String()
	}

	banner := colorize(row1) + "  Bossanova v" + buildinfo.Version + "\n" +
		colorize(row2) + "  " + styleSubtle.Render(cwd)

	return lipgloss.NewStyle().Padding(1, 1, 1, 1).Render(banner)
}

// renderError renders an error message that wraps to the given terminal width.
// If width is 0 (unknown), it falls back to no width constraint.
func renderError(msg string, width int) string {
	s := styleError
	if width > 0 {
		// Account for padding (2 chars each side).
		s = s.Width(width - 4)
	}
	return s.Render(msg)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
