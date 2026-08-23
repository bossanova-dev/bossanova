package views

import (
	"fmt"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// listFilter provides an opt-in "/"-triggered substring filter for table-backed
// list views. It owns the query state and match logic; callers keep ownership of
// their data slice and table rebuild.
//
// Lifecycle:
//
//	idle      -> Activate()   -> filtering (input focused, query empty)
//	filtering -> Commit()     -> applied   (input blurred, query kept)
//	applied   -> Activate()   -> filtering (input focused, query preserved)
//	any       -> Reset()      -> idle     (query cleared)
//	any       -> Dismiss()    -> idle     (query preserved)
type listFilter struct {
	input      textinput.Model
	active     bool
	applied    bool
	matchCount int
	totalCount int
}

func newListFilter() listFilter {
	ti := textinput.New()
	ti.Prompt = "/"
	ti.Placeholder = "filter"
	ti.SetWidth(60)

	styles := textinput.DefaultDarkStyles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(colorSelected)
	styles.Blurred.Prompt = styleSubtle
	styles.Focused.Placeholder = styleSubtle
	styles.Blurred.Placeholder = styleSubtle
	ti.SetStyles(styles)

	return listFilter{input: ti}
}

// Active reports whether the input is focused (user is typing).
func (f listFilter) Active() bool { return f.active }

// Applied reports whether a query has been committed (input blurred, query kept).
func (f listFilter) Applied() bool { return f.applied }

// Engaged reports whether any filter state is present.
func (f listFilter) Engaged() bool { return f.active || f.applied }

// Query returns the trimmed current query, or "" when empty.
func (f listFilter) Query() string {
	return strings.TrimSpace(f.input.Value())
}

// Matches reports whether haystack contains the query (case-insensitive).
// An empty query matches everything.
func (f listFilter) Matches(haystack string) bool {
	q := f.Query()
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(q))
}

// Activate focuses the input and enters editing mode. The existing query is
// preserved; callers choose Reset or Dismiss when exiting the filter.
func (f *listFilter) Activate() tea.Cmd {
	f.active = true
	f.applied = false
	return f.input.Focus()
}

// Commit blurs the input, keeping the query. Returns true when a non-empty query
// was committed; false when the trimmed query is empty.
func (f *listFilter) Commit() bool {
	f.active = false
	f.input.Blur()
	if f.Query() == "" {
		f.applied = false
		return false
	}
	f.applied = true
	return true
}

// Reset clears the query and exits filter mode entirely.
func (f *listFilter) Reset() {
	f.active = false
	f.applied = false
	f.input.SetValue("")
	f.input.Blur()
}

// Dismiss exits filter mode without clearing the raw input value.
func (f *listFilter) Dismiss() {
	f.active = false
	f.applied = false
	f.input.Blur()
}

// Update forwards a message to the textinput. Callers route messages here
// only while Active() is true. The bool reports whether the effective query
// changed after textinput processed the message.
func (f *listFilter) Update(msg tea.Msg) (tea.Cmd, bool) {
	prev := f.Query()
	var cmd tea.Cmd
	f.input, cmd = f.input.Update(normalizePrintableKey(msg))
	return cmd, f.Query() != prev
}

// normalizePrintableKey fills in the Text of a printable key press that carries
// only a Code. textinput inserts Key.Text, so a synthesized press (as tests and
// some terminals produce) would otherwise type nothing. Shared by every view
// that forwards raw key messages into a textinput.
func normalizePrintableKey(msg tea.Msg) tea.Msg {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return msg
	}
	key := k.Key()
	if key.Text == "" && key.Mod == 0 && unicode.IsPrint(key.Code) {
		key.Text = string(key.Code)
		return tea.KeyPressMsg(key)
	}
	return msg
}

// SetCounts records the matched and total row counts used by View().
func (f *listFilter) SetCounts(matched, total int) {
	f.matchCount = matched
	f.totalCount = total
}

// View renders the filter line (input + count suffix). Returns "" when not engaged.
func (f listFilter) View() string {
	if !f.Engaged() {
		return ""
	}
	line := f.input.View()
	if f.totalCount > 0 {
		count := fmt.Sprintf("  (%d of %d)", f.matchCount, f.totalCount)
		line += styleSubtle.Render(count)
	}
	return lipgloss.NewStyle().Padding(0, 2).Render(line)
}

// Height returns the number of rendered lines the filter line occupies
// (0 when not engaged, 1 when engaged). Add to clampedTableHeight overhead.
func (f listFilter) Height() int {
	if f.Engaged() {
		return 1
	}
	return 0
}

// ActionBar returns the action-bar group to show while the input is focused.
func (f listFilter) ActionBar() []string {
	return []string{"type to filter", "[↑↓] move", "[enter] accept", "[esc] clear"}
}
