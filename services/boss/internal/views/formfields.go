package views

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// bossFormWidth is the width every huh form in the TUI is built at
// (.WithWidth), and therefore the width huh wraps a field validation error to.
// form_mouse.go replicates that wrap when measuring the group viewport.
const bossFormWidth = 70

// bossSelectMaxHeight caps how many lines a Select/MultiSelect field may
// occupy — i.e. how many options the Repo/Agent selects show at once. Past this
// the option viewport scrolls internally instead of ballooning the form and
// pushing the Enabled toggle and action bar below the terminal fold.
const bossSelectMaxHeight = 6

// newBossForm builds a huh form in the TUI's house style: the shared theme, no
// huh keybinding footer (every host renders the boss action bar itself — see
// form_nav_hints_test.go), and the shared width. Callers that need a height
// budget chain .WithHeight themselves, because only the tall forms want one:
// huh's viewport pads to the height it is given, so constraining a 1-2 field
// form would inflate it rather than shrink it (repo_add.go documents the case).
func newBossForm(fields ...huh.Field) *huh.Form {
	return huh.NewForm(
		huh.NewGroup(fields...),
	).WithTheme(bossHuhTheme()).WithShowHelp(false).WithWidth(bossFormWidth)
}

// bossSelectHeight returns the Height() a Select/MultiSelect should be built
// with: enough for every option plus its header lines, capped at
// bossSelectMaxHeight so a long list scrolls its viewport instead of pushing
// the action bar off a short terminal.
//
// headerLines is the number of lines huh renders above the option viewport —
// 1 for a title, +1 for a description — because huh subtracts exactly those
// from the height before sizing the viewport (field_select.go
// updateViewportSize). Passing a bare constant instead pads the field's block
// out to that many lines whatever the option count, which is the trailing
// whitespace BOS-567 removed.
func bossSelectHeight(numOptions, headerLines int) int {
	return min(numOptions+headerLines, bossSelectMaxHeight)
}

// bossSelect / bossMultiSelect build a huh Select/MultiSelect already sized to
// its content. Sizing is a constructor argument rather than something a caller
// may chain later because forgetting it is invisible: huh pads the field's
// block out to whatever height it holds, so a stale constant shows as trailing
// blank lines and no error. numOptions/headerLines are the same arguments
// bossSelectHeight documents; Height before Options is safe because huh
// re-derives the viewport size when the options land.
func bossSelect[T comparable](numOptions, headerLines int) *huh.Select[T] {
	return huh.NewSelect[T]().Height(bossSelectHeight(numOptions, headerLines))
}

func bossMultiSelect[T comparable](numOptions, headerLines int) *huh.MultiSelect[T] {
	return huh.NewMultiSelect[T]().Height(bossSelectHeight(numOptions, headerLines))
}

// bossTextMinLines / bossTextMaxLines bound how tall a huh Text field grows.
// huh wraps a bare textarea, whose own default is a fixed 6 rows however much
// (or little) it holds — one line of prompt sat in a six-line box, and a long
// one was clipped to the same six. The field is sized to its content between
// these two instead; past the max it scrolls internally.
const (
	bossTextMinLines = 1
	bossTextMaxLines = 10
)

// bossText builds a huh Text field already sized to value — the same
// constructor-argument idiom bossSelect uses, and for the same reason:
// forgetting to size a text field is invisible, because huh's textarea sits at
// a fixed six rows however much it holds rather than erroring.
//
// value is the field's content at build time, so an edit-mode prefill opens at
// the height its text needs. Only the initial height is the constructor's
// business: the host must go on re-sizing the field twice per message —
// with bossTextPendingInsert(msg) before handing the message to huh, and with
// the settled value after (CronFormModel.resizePromptFor /
// BugReportModel.resizeCommentFor are the two worked examples).
func bossText(value string) *huh.Text {
	return huh.NewText().Lines(bossTextLines(value, bossFormWrapWidth()))
}

// bossTextLines returns the number of rows a huh Text field should show for
// value: its wrapped line count, clamped to [bossTextMinLines, bossTextMaxLines].
//
// The count is computed here rather than read from the textarea because huh
// does not expose it: *huh.Text keeps its textarea private, so textarea's own
// row count is unreachable from this package (LineCount() counts logical lines,
// not the wrapped rows the box has to be tall enough for). wrapWidth must
// therefore match what huh gave the textarea — bossFormWrapWidth().
//
// Undercounting is not a cosmetic error. The textarea keeps its cursor visible
// by scrolling its own viewport, and it never scrolls back — so a box one row
// short of its content scrolls once and hides the first line for the rest of
// the edit.
func bossTextLines(value string, wrapWidth int) int {
	if value == "" {
		return bossTextMinLines
	}
	n := 0
	for _, logical := range strings.Split(value, "\n") {
		n += bossTextLogicalRows(logical, wrapWidth)
	}
	return min(max(n, bossTextMinLines), bossTextMaxLines)
}

// bossTextLogicalRows is how many rows one logical line of value occupies once
// the textarea has soft-wrapped it.
//
// lipgloss does the wrapping, but its row count is not the textarea's. bubbles
// closes each logical line with a `>=` test rather than `>` (textarea.go wrap)
// and counts the line's trailing space toward that width, so a line that fills
// the width costs one more row — the one the cursor moves onto. lipgloss both
// pads every row out to the width (so the trailing space is unmeasurable) and
// counts such a line as one, which is how a full line came to be sized a row
// short.
//
// Rather than restate bubbles' word-wrap here — two wrappers agreeing by
// coincidence is the drift this package keeps trying to avoid — take the
// larger of two conservative bounds. Over-counting costs a spare blank row;
// under-counting hides the text the user just typed, because the textarea
// scrolls to keep its cursor visible and never scrolls back.
func bossTextLogicalRows(line string, wrapWidth int) int {
	if wrapWidth <= 0 {
		return bossTextMinLines
	}
	rows := strings.Split(lipgloss.NewStyle().Width(wrapWidth).Render(line), "\n")
	n := len(rows)
	// The last row visibly reaches the edge, so the cursor is past it.
	if lipgloss.Width(strings.TrimRight(rows[n-1], " ")) >= wrapWidth {
		n++
	}
	// A line of w columns needs a row beyond the w/wrapWidth full ones for the
	// cursor, whatever the word breaks did. This is the bound that catches a
	// line ending in a space, whose width lipgloss has already padded away.
	return max(n, lipgloss.Width(line)/wrapWidth+1)
}

// bossTextPendingInsert returns the text msg is about to insert into a focused
// huh Text field — "" for every message that inserts nothing.
//
// A host re-sizes its Text field twice per message: once with
// value+bossTextPendingInsert(msg) *before* handing the message to huh, and
// once with the settled value after. The second call alone is not enough. huh
// snapshots each field's view at the end of its own Update (group.go
// buildView), and the textarea has by then already scrolled its viewport to
// keep the cursor visible — and it never scrolls back, because repositionView
// only moves the offset when the cursor leaves the window. A box one row short
// at the instant the key lands therefore loses its first line for the rest of
// the edit; sizing ahead of the message is what stops the scroll happening at
// all.
//
// Two insert paths are out of reach: huh's ctrl+e external editor returns
// through an unexported updateValueMsg, and bubbles' ctrl+v clipboard paste
// through an unexported pasteMsg, so neither can be type-switched on here. A
// value arriving either way can still open scrolled to its tail until the user
// moves the cursor up. Growing unconditionally to bossTextMaxLines instead of
// predicting would close that, but it renders every box at the cap — measured,
// the cron Prompt then shows ten rows for a one-line prompt — which is the
// whole feature.
func bossTextPendingInsert(msg tea.Msg) string {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		return msg.Content
	case tea.KeyPressMsg:
		// huh's Text binds NewLine to alt+enter and ctrl+j; a bare enter
		// advances the form instead of inserting anything.
		if msg.Code == tea.KeyEnter && msg.Mod != 0 {
			return "\n"
		}
		if msg.Code == 'j' && msg.Mod&tea.ModCtrl != 0 {
			return "\n"
		}
		return msg.Text
	}
	return ""
}

// bossFormWrapWidth returns the number of columns a form field has for its own
// content: bossFormWidth less the frame the focus gutter reserves. It is
// derived rather than written out because huh computes a Text field's textarea
// width the same way (field_text.go WithWidth:
// textarea.SetWidth(width - styles.Base.GetHorizontalFrameSize())), so a
// hardcoded number would silently stop matching the moment the gutter changed.
func bossFormWrapWidth() int {
	// The frame is the same for both focus states and both light/dark
	// resolutions — TestBossFormWrapWidth_MatchesBothFocusStates pins that — so
	// any one of them answers the question.
	return bossFormWidth - bossHuhTheme().Theme(true).Focused.Base.GetHorizontalFrameSize()
}

// fieldGutter returns the left-border chrome that marks a form field's focus
// state: a thick rule in colorSelected when focused, and a hidden border of the
// same width when blurred so text never jitters horizontally as focus moves. It
// is the single definition shared by bossHuhTheme (for huh fields) and by the
// hand-rolled settings row editors, so the two idioms cannot drift into
// look-alike gutters.
func fieldGutter(focused bool) lipgloss.Style {
	s := lipgloss.NewStyle().BorderLeft(true).PaddingLeft(1)
	if focused {
		return s.BorderStyle(lipgloss.ThickBorder()).BorderForeground(colorSelected)
	}
	return s.BorderStyle(lipgloss.HiddenBorder())
}

// buttonChipPaddingX and buttonChipMarginRight are huh's own Confirm-button
// geometry (its theme.go builds every button on Padding(0, 2).MarginRight(1)).
// bossHuhTheme restates them rather than layering colours onto ThemeBase's
// button style, because buttonStyle owns the colours and lipgloss's Inherit
// will not overwrite a foreground or background ThemeBase has already set.
// TestButtonChrome_GeometryStaysPerCallSite pins the resulting chip width.
const (
	buttonChipPaddingX    = 2
	buttonChipMarginRight = 1
)

// buttonStyle returns the colour rule for one button. It is the single
// definition shared by bossHuhTheme (huh's Confirm buttons) and renderButtonRow
// (the hand-rolled Yes/No rows in account_register.go and friends), so the two
// idioms cannot drift into look-alike palettes.
//
// A focused button — the one currently selected — is white on a filled chip:
// colorSelected when it is the primary action, colorMuted otherwise. An
// unfocused button drops the fill entirely and wears its own colour as text.
//
// Geometry (padding, margin) and emphasis (bold, faint) deliberately stay with
// the caller: huh's chips are a column wider on each side and carry a right
// margin, and only the hand-rolled rows bold or faint their labels. Folding
// either in would restyle a live screen, and BOS-567 de-duplicated these two
// styles without restyling them —TestButtonChrome_ColoursAgreeAcrossBothCallSites
// and TestButtonChrome_GeometryStaysPerCallSite pin both halves of that.
func buttonStyle(focused, primary bool) lipgloss.Style {
	fill := colorMuted
	if primary {
		fill = colorSelected
	}
	if !focused {
		return lipgloss.NewStyle().Foreground(fill)
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Background(fill)
}

// fieldRowGutterColumn is the column every settings row's focus gutter is
// drawn at: the same column the "❯ " chevron used to occupy, and the same one
// the cron form's gutter lands on (its host wraps the whole form in
// PaddingLeft(2)). TestSettingsRowsKeepTheirLeftColumn pins it.
const fieldRowGutterColumn = 2

// renderFieldRow renders one selectable settings row with the same focus
// chrome a huh field gets. It replaces the "❯ " / "  " prefix: the gutter
// occupies the same two columns (1 border + 1 padding), so every screen keeps
// its existing left margin and the row text stays at the same column it was.
func renderFieldRow(focused bool, text string) string {
	return renderIndentedFieldRow(focused, 0, text)
}

// renderIndentedFieldRow is renderFieldRow for a row that nests under another —
// repo settings' integration child fields ("API key", "Organization slug")
// under their expanded Linear/Sentry header. indent is how many columns the row
// sits right of a top-level one, and it shifts the gutter along with the text
// rather than the text alone: the cursor can land on these rows, so they are
// fields, and a screen that guttered its top-level rows while chevroning its
// children would be inconsistent with itself. The chevron idiom they replace
// wrote its own indent into the cursor string ("    " / "  ❯ ") inside a
// Padding(0, 2) wrapper, which put the glyph at column 2+indent and the text at
// column 4+indent — exactly the geometry this reproduces.
func renderIndentedFieldRow(focused bool, indent int, text string) string {
	if focused {
		text = styleSelected.Render(text)
	}
	return lipgloss.NewStyle().
		PaddingLeft(fieldRowGutterColumn + indent).
		Render(fieldGutter(focused).Render(text))
}

// bossConfirm builds a huh Confirm in the TUI's house style: buttons on the
// left margin, like every other field's content.
//
// huh defaults a Confirm's buttonAlignment to lipgloss.Center and then centres
// the button row inside a block as wide as that field's own title and
// description (field_confirm.go View), so a form of confirms rendered its
// Yes/No pairs at as many different columns as it had description lengths.
func bossConfirm() *huh.Confirm {
	return huh.NewConfirm().WithButtonAlignment(lipgloss.Left)
}
