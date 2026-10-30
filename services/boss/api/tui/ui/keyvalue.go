package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// KeyValue is one row of a key/value block.
type KeyValue struct {
	Key   string
	Value string
}

// KeyColumnWidth is the width of the widest "Key:" in rows, so blocks rendered
// separately can still start their values in one column.
func KeyColumnWidth(rows ...KeyValue) int {
	width := 0
	for _, r := range rows {
		width = max(width, lipgloss.Width(r.Key+":"))
	}
	return width
}

// PadKey renders "Key:" padded to keyWidth plus one separating space, so the
// value that follows starts in the shared value column.
func PadKey(key string, keyWidth int) string {
	k := key + ":"
	return k + strings.Repeat(" ", max(0, keyWidth-lipgloss.Width(k))+1)
}

// KeyValueLine aligns a field value and hangs wrapped continuation lines at
// its value column. Widths too narrow to read leave the value unconstrained.
func KeyValueLine(key, value string, keyWidth, maxWidth int) string {
	prefix := PadKey(key, keyWidth)
	valueColumn := lipgloss.Width(prefix)
	const minValueWrapWidth = 20
	wrapWidth := maxWidth - valueColumn
	if maxWidth <= 0 || wrapWidth < minValueWrapWidth || lipgloss.Width(value) <= wrapWidth {
		return prefix + value
	}
	wrapped := lipgloss.Wrap(value, wrapWidth, "")
	return prefix + strings.ReplaceAll(wrapped, "\n", "\n"+strings.Repeat(" ", valueColumn))
}

// RenderKeyValues renders rows one per line with muted keys and every value
// left-aligned in one column at keyWidth. Each line ends in a newline.
func RenderKeyValues(rows []KeyValue, keyWidth int) string {
	key := lipgloss.NewStyle().Foreground(ColorMuted)
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(key.Render(PadKey(r.Key, keyWidth)))
		b.WriteString(r.Value)
		b.WriteString("\n")
	}
	return b.String()
}
