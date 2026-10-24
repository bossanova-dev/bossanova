package ui

func RenderError(msg string, width int) string {
	s := StyleError
	// Mirror statusWrapWidth's floor. lipgloss wraps at width minus the
	// horizontal padding, so a width at or below the padding leaves no columns
	// to wrap into and silently renders exactly like Width(0). This guard is
	// therefore behaviour-neutral — nothing can make a block fit a terminal
	// narrower than the padding itself — and exists so the fallback is stated
	// rather than stumbled into, the same reason statusWrapWidth spells its
	// floor out. Ask the style for its padding so it cannot drift from theme.go.
	if width > StyleError.GetHorizontalPadding() {
		s = s.Width(width)
	}
	return s.Render(msg)
}
