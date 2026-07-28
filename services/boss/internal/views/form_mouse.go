package views

// Click-to-focus hit testing for the huh forms this package hosts (BOS-512).
//
// huh v2 has no mouse support of its own — `grep -rn Mouse` across the module
// returns nothing — so boss owns the arithmetic that turns a click coordinate
// into a field index. huh's Form exposes no field enumeration (only
// NextField/PrevField/GetFocusedField), so each view retains the ordered slice
// it built the group from and hands it to formFields.
//
// Nothing here reads huh internals; the two places it *replicates* them are
// called out in comments (the field separator gap and the group's scroll
// offset), and each has a unit test acting as the tripwire for a huh bump.

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// linesBefore counts the rendered lines that sit above a form, given the exact
// prefix string the view writes before form.View(). Each form view builds that
// prefix in one place and uses it from both paths — View writes it, Update
// measures it — so a line added to the prefix moves the hit test with it.
func linesBefore(prefix string) int { return strings.Count(prefix, "\n") }

// formFields is the ordered field slice a view retains at build time, plus the
// blank-line gap huh renders between two adjacent fields. The zero value is
// inert: every lookup returns -1, so a view that has not built its form yet
// silently ignores clicks.
type formFields struct {
	fields []huh.Field
	gap    int
}

// newFormFields records the fields a view just handed to huh.NewGroup, in the
// same order. The gap is derived from the live boss theme rather than hardcoded
// so a FieldSeparator change cannot silently shift every hit test by a line.
func newFormFields(fields ...huh.Field) formFields {
	return formFields{fields: fields, gap: formFieldGap(bossHuhTheme())}
}

// formFieldGap reports how many blank lines huh puts between two adjacent
// fields in a group.
//
// REPLICATED from charm.land/huh/v2 group.go (getContent): the group writes
// field.View(), then the theme's rendered FieldSeparator, then the next
// field.View(). Concatenating "a" + sep + "b" yields lipgloss.Height(sep)-2
// blank lines between them, because the separator's first and last lines merge
// with the neighbouring fields' last and first lines. ThemeBase's separator is
// "\n\n" (height 3), so the gap is one blank line.
func formFieldGap(theme huh.Theme) int {
	sep := theme.Theme(true).FieldSeparator.Render()
	return max(lipgloss.Height(sep)-2, 0)
}

// heightOf returns the rendered height of one field. Fields are pointer types
// in huh and Group.Update stores the same pointers back, so the slice a view
// retains stays identical to the one the group renders.
func (f formFields) heightOf(i int) int {
	return lipgloss.Height(f.fields[i].View())
}

// contentHeight is the total height of the group's scrollable content: every
// field view plus the gap between each adjacent pair.
func (f formFields) contentHeight() int {
	if len(f.fields) == 0 {
		return 0
	}
	total := f.gap * (len(f.fields) - 1)
	for i := range f.fields {
		total += f.heightOf(i)
	}
	return total
}

// startLine returns the first content line occupied by fields[i], or -1 when
// the index is out of range.
func (f formFields) startLine(i int) int {
	if i < 0 || i >= len(f.fields) {
		return -1
	}
	line := 0
	for j := range i {
		line += f.heightOf(j) + f.gap
	}
	return line
}

// fieldAtLine maps a *content*-relative line offset (line 0 = the first line of
// the first field, ignoring any scroll offset) to the field index that occupies
// it. It returns -1 for a gap line, for a negative line, and for any line past
// the end of the content — deliberately never clamping, because focusing the
// wrong field is worse than ignoring a click.
func (f formFields) fieldAtLine(line int) int {
	if line < 0 {
		return -1
	}
	cur := 0
	for i := range f.fields {
		next := cur + f.heightOf(i)
		if line < next {
			return i
		}
		cur = next + f.gap
		if line < cur {
			return -1 // inside the gap between two fields
		}
	}
	return -1
}

// focusedIndex reports the index of form.GetFocusedField() by interface
// identity against the retained slice, or -1 when the form is nil or its
// focused field is not one of ours.
func (f formFields) focusedIndex(form *huh.Form) int {
	if form == nil || len(f.fields) == 0 {
		return -1
	}
	focused := form.GetFocusedField()
	if focused == nil {
		return -1
	}
	for i, field := range f.fields {
		if field == focused {
			return i
		}
	}
	return -1
}

// errorFooterHeight is how many lines huh's Group.View renders *below* the
// viewport for validation errors. Fields validate on blur, so on a form with
// required fields this is routinely non-zero — declining to hit-test while it
// is would make click-to-focus dead exactly when a user is fixing a mistake.
//
// REPLICATED from charm.land/huh/v2 group.go (Footer + View) and wrap.go: with
// help suppressed the footer is one wrapped error message per erroring field,
// joined by newlines, and View separates it from the viewport with one empty
// line. huh wraps at the group's width with lipgloss.Wrap and the breakpoints
// below. TestFormFieldsErrorFooterHeight is the tripwire for a huh bump.
func (f formFields) errorFooterHeight() int {
	styles := bossHuhTheme().Theme(true)
	var parts []string
	for _, field := range f.fields {
		if err := field.Error(); err != nil {
			parts = append(parts, lipgloss.Wrap(
				styles.Focused.ErrorMessage.Render(err.Error()), bossFormWidth, huhWrapBreakpoints))
		}
	}
	if len(parts) == 0 {
		return 0
	}
	return 1 + lipgloss.Height(strings.Join(parts, "\n"))
}

// huhWrapBreakpoints mirrors huh's wrap.go, which calls
// lipgloss.Wrap(s, limit, ",.-; ").
const huhWrapBreakpoints = ",.-; "

// visibleHeight is the height of the group's scrolling viewport.
//
// Group.View renders header, viewport, then an error footer. Every form in this
// package is built without a group title or description, so the header is empty
// (assertFormStartsAtFirstField pins that), and .WithShowHelp(false) removes
// the keybinding footer — leaving the viewport, which pads its content out to
// its full height, plus whatever errorFooterHeight accounts for.
func visibleHeight(form *huh.Form, fields formFields) int {
	return lipgloss.Height(form.View()) - fields.errorFooterHeight()
}

// scrollOffset returns the content line rendered at the top of the viewport.
//
// REPLICATED from charm.land/huh/v2 group.go (getContent + buildView): the
// group sets the viewport's Y offset to the cumulative height of the fields
// *before* the focused one — i.e. the focused field's first content line — and
// the viewport clamps that to [0, contentHeight-visibleHeight]. A huh bump that
// changes that composition is caught by assertViewportTopMatchesOffset, which
// compares this figure against the line huh actually renders at the top of the
// viewport rather than re-deriving it from this same function.
//
// ASSUMES no field zooms. getContent short-circuits to rendering only the
// focused field, with offset 0, when Selected().Zoom() is true. In huh v2.0.3
// only FilePicker zooms and no form in this package builds one; adding a
// huh.NewFilePicker() would break every hit test on that form until this
// function learns the zoomed layout.
func (f formFields) scrollOffset(focused, visible int) int {
	maxOffset := max(f.contentHeight()-visible, 0)
	if maxOffset == 0 {
		return 0
	}
	start := f.startLine(focused)
	if start <= 0 {
		return 0
	}
	return min(start, maxOffset)
}

// formOnScreen reports whether a form will actually render something. It is the
// question every form host must ask before enabling mouse reporting or hit
// testing — "form != nil" is NOT the same question.
//
// huh's Form.View returns "" once the form is completed or aborted (submit sets
// quitting), and several views in this package stay on their form branch in
// exactly that state: the cron view after a failed save, the add-repo view
// while its RegisterRepo round-trip is in flight. On those screens there is no
// field to click. Worse, lipgloss.Height("") is 1, so a hit test against an
// empty form measures a one-line viewport and maps line 0 to field 0 — it would
// focus a field the user cannot see. Guarding here rather than relying on huh's
// Form.Update declining a non-normal state makes that a drawn boundary instead
// of a third-party coincidence.
func formOnScreen(form *huh.Form) bool {
	return form != nil && form.View() != ""
}

// fieldAtViewLine maps a line offset within the *rendered* form (line 0 = the
// first line of form.View()) to a field index, accounting for the group's
// scroll offset when the content is taller than the viewport. Clicks outside
// the visible window and clicks on a gap line return -1. A displayed validation
// error does NOT disable hit testing — errorFooterHeight exists precisely so
// clicks keep working while a user is fixing a mistake, which is when they are
// most likely to reach for another field.
func (f formFields) fieldAtViewLine(form *huh.Form, line int) int {
	if !formOnScreen(form) || len(f.fields) == 0 || line < 0 {
		return -1
	}
	visible := visibleHeight(form, f)
	if line >= visible {
		return -1
	}
	if f.contentHeight() > visible {
		line += f.scrollOffset(f.focusedIndex(form), visible)
	}
	return f.fieldAtLine(line)
}

// focusCmd walks focus from the currently focused field to target using huh's
// own NextField/PrevField, which already step over Skip() fields, blur the old
// field and focus the new one.
//
// Walking rather than jumping is deliberate — huh exposes no "focus field N" —
// and it has one visible consequence: blurring runs a field's validator, so a
// click that skips several required fields can raise their errors just as
// pressing tab that many times would. That is huh's model of a form, and
// diverging from it (suppressing validation on a click-through) would make the
// mouse and keyboard paths behave differently on the same transition.
//
// It returns nil when there is nothing to do:
// no form, an out-of-range target, a target that huh will never focus because
// it is skipped, or a target that already has focus (re-focusing a text input
// would reset its cursor).
func (f formFields) focusCmd(form *huh.Form, target int) tea.Cmd {
	if form == nil || target < 0 || target >= len(f.fields) {
		return nil
	}
	if f.fields[target].Skip() {
		return nil
	}
	cur := f.focusedIndex(form)
	if cur < 0 || cur == target {
		return nil
	}
	var cmds []tea.Cmd
	// Bounded by the field count: NextField/PrevField advance by at least one
	// focusable field per call, and target is known to be focusable, so the
	// walk lands exactly on it rather than stepping past.
	for range len(f.fields) {
		if cur == target {
			break
		}
		if cur < target {
			cmds = append(cmds, form.NextField())
		} else {
			cmds = append(cmds, form.PrevField())
		}
		next := f.focusedIndex(form)
		if next == cur {
			break // no progress; refuse to spin
		}
		cur = next
	}
	return tea.Batch(cmds...)
}

// resizeForm re-constrains a built form to a new height and resyncs the
// scrolling viewport underneath it. Views handling a terminal resize must use
// this instead of a bare form.WithHeight.
//
// huh's Group.WithHeight (group.go) only calls viewport.SetHeight; it does NOT
// call buildView, and bubbles' viewport.SetHeight does not re-clamp yOffset. So
// after a bare WithHeight the group keeps rendering at the offset computed for
// the OLD height while scrollOffset predicts one for the NEW height, and every
// click on a scrolled form lands on the wrong field — which, because focusCmd
// walks with NextField/PrevField, also blurs and so validates every field it
// steps over. Group.Update runs buildView unconditionally for any message, so
// pushing the size through the form is what re-establishes the invariant
// fieldAtViewLine depends on. Nothing in huh reacts to a WindowSizeMsg except
// Form.Update's own arm, which is a no-op once the form has a width and height,
// so this costs nothing beyond the rebuild it exists for.
//
// The height <= 0 guard is not incidental. The views return 0 from formHeight
// when the terminal size is unknown, and huh's Form.WithHeight ignores a
// non-positive height — leaving f.height at 0, which is exactly the condition
// that makes Form.Update's WindowSizeMsg arm take over and re-derive the height
// from the message. Forwarding a zero-height size would then collapse the group
// to nothing. A bare WithHeight(0) is a no-op, so declining here preserves that.
//
// Build-time WithHeight calls need none of this: they are followed by
// form.Init(), and huh's Group.Init ends in buildView.
//
// TestFormResizeResyncsTheScrollOffset is the regression guard.
func resizeForm(form *huh.Form, height int, size tea.WindowSizeMsg) tea.Cmd {
	if form == nil || height <= 0 {
		return nil
	}
	form.WithHeight(height)
	_, cmd := form.Update(size)
	return cmd
}

// handleMouse is the single entry point a form view calls from Update, before
// delegating to huh. linesBeforeForm is the number of lines that view renders
// above form.View(); the click's Y is already content-relative because App
// subtracts its own banner/toast chrome (see App.chromeHeight).
//
// The bool reports whether msg was a mouse message. It is true for *every*
// mouse message — right/middle clicks, wheel, motion, release — because huh
// cannot interpret any of them, so the view must swallow them all rather than
// forward them. Only a left click can produce a command.
func (f formFields) handleMouse(msg tea.Msg, form *huh.Form, linesBeforeForm int) (tea.Cmd, bool) {
	if _, ok := msg.(tea.MouseMsg); !ok {
		return nil, false
	}
	click, ok := msg.(tea.MouseClickMsg)
	if !ok || click.Button != tea.MouseLeft {
		return nil, true
	}
	target := f.fieldAtViewLine(form, click.Y-linesBeforeForm)
	if target < 0 {
		return nil, true
	}
	return f.focusCmd(form, target), true
}
