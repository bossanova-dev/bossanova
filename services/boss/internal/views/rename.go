// Home's inline session-title editor, opened by the hidden [r] shortcut
// (BOS-837). It lives in its own file and mirrors confirmPrompt (confirm.go) —
// the other transient footer HomeModel already embeds — rather than adding
// another three fields to a struct that is already ~40 wide.

package views

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	// renamePromptLines is the content height of an active prompt's footer: the
	// input line plus the key hints, counted the same way
	// sessionTableFooterLineCount counts the action bar this replaces (content
	// lines only — tableHeight adds the action bar's own vertical padding). A
	// third line would push the table one row past its reservation, which is why
	// footer clamps instead of wrapping and the "cannot be empty" complaint
	// shares the hint line rather than taking one of its own.
	renamePromptLines = 2

	// renameInputWidth matches the chat picker's inline rename editor so the
	// two prompts line up at the same terminal width.
	renameInputWidth = 60

	// renameCharLimit bounds what can be typed or pasted into the editor.
	// bossd accepts any non-empty title, and the accepted title is echoed back
	// onto the status line, whose height tableHeight has to reserve — so an
	// unbounded paste would wrap to arbitrarily many rows and squeeze the board
	// off the screen. Well clear of any title a person would choose.
	renameCharLimit = 200
)

// renamePrompt is Home's inline title editor. Like confirmPrompt it is
// value-safe: it holds a textinput and two strings and no pointer into any
// model, so copying it along with HomeModel cannot alias another copy's state.
type renamePrompt struct {
	active bool
	// sessionID pins the write to the session that was selected when the prompt
	// opened. Home's 2s poll keeps running mid-rename and can reorder the list,
	// so resolving the target again at commit time would let the rename land on
	// whichever session had drifted under the cursor.
	sessionID string
	input     textinput.Model
	err       string
}

// newRenamePrompt returns an active prompt pre-filled with title, plus the
// command that focuses its input.
func newRenamePrompt(sessionID, title string) (renamePrompt, tea.Cmd) {
	input := textinput.New()
	input.Placeholder = "session title"
	input.Prompt = "Rename: "
	input.SetWidth(renameInputWidth)
	input.CharLimit = renameCharLimit
	input.SetValue(title)
	input.CursorEnd()
	r := renamePrompt{active: true, sessionID: sessionID, input: input}
	// Focus mutates the input, so it must run before r is copied into the
	// return values; `return r, r.input.Focus()` would evaluate r first and
	// hand back an unfocused copy.
	cmd := r.input.Focus()
	return r, cmd
}

// Active reports whether the prompt is open. Home's textEntryActive — and
// through it app.go's ctrl+x aliasing — is this value.
func (r renamePrompt) Active() bool { return r.active }

// SessionID is the session the prompt was opened on.
func (r renamePrompt) SessionID() string { return r.sessionID }

// Value is the edited title with surrounding whitespace removed, so the bytes
// that were tested for emptiness are the same bytes that get committed.
func (r renamePrompt) Value() string { return strings.TrimSpace(r.input.Value()) }

// Update applies an editing message to the title field and clears any stale
// complaint. enter and esc never reach it: handleRenameKey owns commit and
// cancel, exactly as handleConfirmKey owns y/n.
//
// The message is normalized for the reason listFilter.Update needs it
// (filter.go:104-126): a tea.KeyPressMsg carrying only Code — what tuitest and
// some terminals synthesize — has an empty Text, and textinput inserts Text, so
// without the back-fill the character is silently dropped. Non-key messages
// (bracketed paste) pass through untouched.
func (r renamePrompt) Update(msg tea.Msg) (renamePrompt, tea.Cmd) {
	var cmd tea.Cmd
	r.input, cmd = r.input.Update(normalizePrintableKey(msg))
	r.err = ""
	return r, cmd
}

// withEmptyTitleError attaches the one complaint this prompt can raise, leaving
// it open with the typed text intact: an empty title is a correctable mistake,
// and closing the prompt would discard what the operator typed.
func (r renamePrompt) withEmptyTitleError() renamePrompt {
	r.err = "Title cannot be empty"
	return r
}

// footer renders the editor that stands in for the action bar while the prompt
// is open: the input line, then the key hints. The "cannot be empty" complaint
// rides on the HINT line rather than the input line — the input is
// renameInputWidth wide plus its prompt, so at any ordinary terminal width there
// is no room left beside it and the clamp would cut the complaint down to a few
// unreadable characters.
//
// The two lines SPLIT styleActionBar's vertical padding — top above the input,
// bottom below the hints — instead of each carrying their own. That keeps the
// gap above the footer and the pad below it looking exactly like the action bar
// this replaces, while making the whole editor exactly one line taller than it,
// which is the one line tableHeight gives back.
//
// Both lines are clamped rather than wrapped (width == 0 means the terminal size
// is not known yet, so nothing is clamped): a wrapped line would silently cost a
// third row, which is exactly what tableHeight's reservation cannot absorb.
func (r renamePrompt) footer(width int) string {
	inputStyle := lipgloss.NewStyle().Padding(actionBarPadY, 2, 0, 2)
	hintStyle := styleActionBar.Padding(0, 2, actionBarPadY, 2)
	if width > 0 {
		inputStyle = inputStyle.MaxWidth(width)
		hintStyle = hintStyle.MaxWidth(width)
	}
	hints := "[enter] rename  [esc] cancel"
	if r.err != "" {
		hints += "  " + lipgloss.NewStyle().Foreground(colorDanger).Render(r.err)
	}
	return inputStyle.Render(r.input.View()) + "\n" + hintStyle.Render(hints)
}

// lineCount is the footer's rendered height, for tableHeight's reservation.
func (r renamePrompt) lineCount() int { return renamePromptLines }
