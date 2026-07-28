package views

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// --- Stub field -------------------------------------------------------------

// stubField is a huh.Field with a caller-chosen rendered height, so the
// line→field arithmetic can be asserted against exact numbers instead of
// whatever a real Input happens to render this week.
type stubField struct {
	name   string
	height int
}

func newStubField(name string, height int) *stubField {
	return &stubField{name: name, height: height}
}

func (s *stubField) Init() tea.Cmd { return nil }
func (s *stubField) Update(tea.Msg) (huh.Model, tea.Cmd) {
	return s, nil
}

func (s *stubField) View() string {
	lines := make([]string, s.height)
	for i := range lines {
		lines[i] = s.name
	}
	return strings.Join(lines, "\n")
}

func (s *stubField) Blur() tea.Cmd  { return nil }
func (s *stubField) Focus() tea.Cmd { return nil }
func (s *stubField) Error() error   { return nil }
func (s *stubField) Run() error     { return nil }
func (s *stubField) RunAccessible(io.Writer, io.Reader) error {
	return nil
}
func (s *stubField) Skip() bool                               { return false }
func (s *stubField) Zoom() bool                               { return false }
func (s *stubField) KeyBinds() []key.Binding                  { return nil }
func (s *stubField) WithTheme(huh.Theme) huh.Field            { return s }
func (s *stubField) WithKeyMap(*huh.KeyMap) huh.Field         { return s }
func (s *stubField) WithWidth(int) huh.Field                  { return s }
func (s *stubField) WithHeight(int) huh.Field                 { return s }
func (s *stubField) WithPosition(huh.FieldPosition) huh.Field { return s }
func (s *stubField) GetKey() string                           { return s.name }
func (s *stubField) GetValue() any                            { return s.name }

// stubFormFields builds a formFields over stub fields with a one-line gap,
// matching the boss theme's field separator.
func stubFormFields(fields ...huh.Field) formFields {
	return formFields{fields: fields, gap: 1}
}

// --- Gap derivation ---------------------------------------------------------

// TestFormFieldGapMatchesTheme is the tripwire for the first thing form_mouse.go
// replicates from huh: the blank-line gap between adjacent fields. huh's
// ThemeBase separator is "\n\n", which puts exactly one blank line between two
// field views. If a huh bump changes that, every hit test shifts by a line per
// field and this fails first.
func TestFormFieldGapMatchesTheme(t *testing.T) {
	if got := formFieldGap(bossHuhTheme()); got != 1 {
		t.Fatalf("formFieldGap = %d, want 1", got)
	}

	// Prove the derivation against the composition huh performs: field, gap,
	// field. Two 1-line views separated by the theme separator must render at
	// the derived total height.
	sep := bossHuhTheme().Theme(true).FieldSeparator.Render()
	joined := "a" + sep + "b"
	want := 1 + 1 + formFieldGap(bossHuhTheme())
	if got := lipgloss.Height(joined); got != want {
		t.Fatalf("joined height = %d, want %d (gap derivation is wrong)", got, want)
	}
}

// --- fieldAtLine ------------------------------------------------------------

// TestFormFieldsFieldAtLine walks the whole line space of a four-field form:
// the first line of the first field, the last line of the last field, gap
// lines, a multi-line field (the shape of repo_add's Automation MultiSelect
// with an explicit Height), a line before the form and a line after it.
func TestFormFieldsFieldAtLine(t *testing.T) {
	// heights 2, 3, 1, 2 with a 1-line gap:
	//   0,1   f0
	//   2     gap
	//   3,4,5 f1
	//   6     gap
	//   7     f2
	//   8     gap
	//   9,10  f3
	ff := stubFormFields(
		newStubField("f0", 2),
		newStubField("f1", 3),
		newStubField("f2", 1),
		newStubField("f3", 2),
	)

	if got, want := ff.contentHeight(), 11; got != want {
		t.Fatalf("contentHeight = %d, want %d", got, want)
	}

	tests := []struct {
		name string
		line int
		want int
	}{
		{name: "line above the form", line: -1, want: -1},
		{name: "first line of first field", line: 0, want: 0},
		{name: "last line of first field", line: 1, want: 0},
		{name: "gap after first field", line: 2, want: -1},
		{name: "first line of multi-line field", line: 3, want: 1},
		{name: "middle line of multi-line field", line: 4, want: 1},
		{name: "last line of multi-line field", line: 5, want: 1},
		{name: "gap after multi-line field", line: 6, want: -1},
		{name: "single-line field", line: 7, want: 2},
		{name: "gap before last field", line: 8, want: -1},
		{name: "first line of last field", line: 9, want: 3},
		{name: "last line of last field", line: 10, want: 3},
		{name: "line just after the form", line: 11, want: -1},
		{name: "line well after the form", line: 40, want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ff.fieldAtLine(tt.line); got != tt.want {
				t.Errorf("fieldAtLine(%d) = %d, want %d", tt.line, got, tt.want)
			}
		})
	}
}

func TestFormFieldsStartLine(t *testing.T) {
	ff := stubFormFields(
		newStubField("f0", 2),
		newStubField("f1", 3),
		newStubField("f2", 1),
	)
	for i, want := range []int{0, 3, 7} {
		if got := ff.startLine(i); got != want {
			t.Errorf("startLine(%d) = %d, want %d", i, got, want)
		}
	}
	if got := ff.startLine(3); got != -1 {
		t.Errorf("startLine(out of range) = %d, want -1", got)
	}
	if got := ff.startLine(-1); got != -1 {
		t.Errorf("startLine(-1) = %d, want -1", got)
	}
}

// TestFormFieldsZeroValueIsInert guards the "view has not built its form yet"
// path: every lookup must decline rather than panic.
func TestFormFieldsZeroValueIsInert(t *testing.T) {
	var ff formFields
	if got := ff.fieldAtLine(0); got != -1 {
		t.Errorf("fieldAtLine on zero value = %d, want -1", got)
	}
	if got := ff.focusedIndex(nil); got != -1 {
		t.Errorf("focusedIndex(nil) = %d, want -1", got)
	}
	if got := ff.fieldAtViewLine(nil, 0); got != -1 {
		t.Errorf("fieldAtViewLine(nil) = %d, want -1", got)
	}
	if cmd := ff.focusCmd(nil, 0); cmd != nil {
		t.Error("focusCmd on zero value returned a command")
	}
	if got := ff.contentHeight(); got != 0 {
		t.Errorf("contentHeight on zero value = %d, want 0", got)
	}
}

// --- focusedIndex -----------------------------------------------------------

// TestFormFieldsFocusedIndex asserts the lookup is by interface identity
// against the retained slice, and that a focused field that is not in the slice
// reports -1 rather than a plausible-looking wrong index.
func TestFormFieldsFocusedIndex(t *testing.T) {
	a := huh.NewInput().Title("A")
	b := huh.NewInput().Title("B")
	c := huh.NewInput().Title("C")
	form := newSingleGroupForm(t, a, b, c)

	ff := formFields{fields: []huh.Field{a, b, c}, gap: 1}
	if got := ff.focusedIndex(form); got != 0 {
		t.Fatalf("focusedIndex after Init = %d, want 0", got)
	}

	form.NextField()
	if got := ff.focusedIndex(form); got != 1 {
		t.Fatalf("focusedIndex after NextField = %d, want 1", got)
	}

	// A slice that does not contain the focused field must not guess.
	other := formFields{fields: []huh.Field{c, a}, gap: 1}
	if got := other.focusedIndex(form); got != -1 {
		t.Fatalf("focusedIndex for a foreign focused field = %d, want -1", got)
	}
}

// --- focusCmd ---------------------------------------------------------------

// TestFormFieldsFocusCmdDirection drives focus forward and backward across a
// form, and asserts a Skip() field (huh.NewNote) is stepped over rather than
// focused.
func TestFormFieldsFocusCmdDirection(t *testing.T) {
	build := func(t *testing.T) (*huh.Form, formFields, []huh.Field) {
		t.Helper()
		fields := []huh.Field{
			huh.NewInput().Title("zero"),
			huh.NewNote().Title("note"), // Skip() == true
			huh.NewInput().Title("two"),
			huh.NewInput().Title("three"),
		}
		form := newSingleGroupForm(t, fields...)
		return form, formFields{fields: fields, gap: 1}, fields
	}

	t.Run("forward across a skipped note", func(t *testing.T) {
		form, ff, _ := build(t)
		if got := ff.focusedIndex(form); got != 0 {
			t.Fatalf("initial focus = %d, want 0", got)
		}
		cmd := ff.focusCmd(form, 3)
		if cmd == nil {
			t.Fatal("focusCmd returned nil for a reachable forward target")
		}
		if got := ff.focusedIndex(form); got != 3 {
			t.Fatalf("focus after forward walk = %d, want 3", got)
		}
	})

	t.Run("backward to the first field", func(t *testing.T) {
		form, ff, _ := build(t)
		if cmd := ff.focusCmd(form, 3); cmd == nil {
			t.Fatal("setup walk returned nil")
		}
		cmd := ff.focusCmd(form, 0)
		if cmd == nil {
			t.Fatal("focusCmd returned nil for a reachable backward target")
		}
		if got := ff.focusedIndex(form); got != 0 {
			t.Fatalf("focus after backward walk = %d, want 0", got)
		}
	})

	t.Run("skipped field is never focused", func(t *testing.T) {
		form, ff, fields := build(t)
		if !fields[1].Skip() {
			t.Fatal("test premise broken: huh.NewNote no longer reports Skip()")
		}
		if cmd := ff.focusCmd(form, 1); cmd != nil {
			t.Error("focusCmd targeted a Skip() field")
		}
		if got := ff.focusedIndex(form); got != 0 {
			t.Errorf("focus moved to %d after a click on a skipped field, want 0", got)
		}
	})

	t.Run("already focused is a no-op", func(t *testing.T) {
		form, ff, _ := build(t)
		if cmd := ff.focusCmd(form, 0); cmd != nil {
			t.Error("focusCmd re-focused the already-focused field")
		}
	})

	t.Run("out of range target", func(t *testing.T) {
		form, ff, _ := build(t)
		for _, target := range []int{-1, 4, 99} {
			if cmd := ff.focusCmd(form, target); cmd != nil {
				t.Errorf("focusCmd(%d) returned a command", target)
			}
		}
	})
}

// --- scroll offset ----------------------------------------------------------

// TestFormFieldsScrollOffset pins the second thing form_mouse.go replicates
// from huh: the group viewport's Y offset. huh sets it to the focused field's
// first content line and the viewport clamps it to the last full window.
func TestFormFieldsScrollOffset(t *testing.T) {
	// Five 2-line fields with a 1-line gap: content height 14, lines
	// 0-1 f0, 3-4 f1, 6-7 f2, 9-10 f3, 12-13 f4.
	ff := stubFormFields(
		newStubField("f0", 2),
		newStubField("f1", 2),
		newStubField("f2", 2),
		newStubField("f3", 2),
		newStubField("f4", 2),
	)
	if got, want := ff.contentHeight(), 14; got != want {
		t.Fatalf("contentHeight = %d, want %d", got, want)
	}

	tests := []struct {
		name    string
		focused int
		visible int
		want    int
	}{
		{name: "content fits, no scroll", focused: 3, visible: 14, want: 0},
		{name: "viewport taller than content", focused: 4, visible: 40, want: 0},
		{name: "first field focused", focused: 0, visible: 6, want: 0},
		{name: "middle field focused", focused: 2, visible: 6, want: 6},
		{name: "last field clamped to the final window", focused: 4, visible: 6, want: 8},
		{name: "unknown focused field", focused: -1, visible: 6, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ff.scrollOffset(tt.focused, tt.visible); got != tt.want {
				t.Errorf("scrollOffset(%d, %d) = %d, want %d",
					tt.focused, tt.visible, got, tt.want)
			}
		})
	}
}

// TestFormFieldsFieldAtViewLineScrolled exercises the whole scrolled mapping,
// including the two ways it must decline: a line below the visible window, and
// a validation error whose footer huh renders below the viewport (which would
// otherwise inflate the measured window).
func TestFormFieldsFieldAtViewLineScrolled(t *testing.T) {
	// A cron_form-shaped form: more content than height. Real huh fields so
	// form.View() renders a real viewport at the height we constrain it to.
	fields := []huh.Field{
		huh.NewInput().Title("one"),
		huh.NewInput().Title("two"),
		huh.NewInput().Title("three"),
		huh.NewInput().Title("four"),
		huh.NewInput().Title("five"),
		huh.NewInput().Title("six"),
	}
	form := newSingleGroupForm(t, fields...)
	form.WithHeight(8)
	ff := formFields{fields: fields, gap: formFieldGap(bossHuhTheme())}

	visible := visibleHeight(form, ff)
	if visible != 8 {
		t.Fatalf("visibleHeight = %d, want 8 (huh no longer pads the viewport to its height?)", visible)
	}
	if ff.contentHeight() <= visible {
		t.Fatalf("premise broken: content %d fits in %d, nothing scrolls", ff.contentHeight(), visible)
	}

	// Focus the third field: far enough down to scroll, but not so far that the
	// viewport's clamp takes over — an offset sitting ON the clamp would agree
	// with a wrong formula by accident, so pin the free-running case first.
	if cmd := ff.focusCmd(form, 2); cmd == nil {
		t.Fatal("could not focus field 2")
	}
	if mid, maxOffset := ff.scrollOffset(2, visible), ff.contentHeight()-visible; mid == 0 || mid >= maxOffset {
		t.Fatalf("premise broken: focusing field 2 gives offset %d, want 0 < offset < %d "+
			"(an offset at the clamp cannot discriminate a wrong formula)", mid, maxOffset)
	}
	// The offset boss replicates must match the one huh actually scrolled to.
	// Every other assertion in this test derives its expectation from
	// scrollOffset, i.e. from the same expression fieldAtViewLine evaluates —
	// only a comparison against huh's rendered output can catch a wrong offset.
	assertViewportTopMatchesOffset(t, form, ff)

	// Now the fourth field, whose offset the viewport clamps to the final
	// window — the other half of scrollOffset's behaviour.
	if cmd := ff.focusCmd(form, 3); cmd == nil {
		t.Fatal("could not focus field 3")
	}
	offset := ff.scrollOffset(3, visible)
	if offset == 0 {
		t.Fatal("premise broken: focusing field 3 produced no scroll offset")
	}
	assertViewportTopMatchesOffset(t, form, ff)

	// The first visible line belongs to whichever field owns content line
	// `offset` — derived from the same helper, so a drift breaks this test.
	wantTop := ff.fieldAtLine(offset)
	if got := ff.fieldAtViewLine(form, 0); got != wantTop {
		t.Errorf("fieldAtViewLine(0) = %d, want %d (scroll offset ignored?)", got, wantTop)
	}

	// A click on the last visible line still maps inside the window.
	if got := ff.fieldAtViewLine(form, visible-1); got != ff.fieldAtLine(offset+visible-1) {
		t.Errorf("fieldAtViewLine(%d) disagrees with the scrolled content mapping", visible-1)
	}

	// Clicks outside the visible window are ignored, never clamped.
	for _, line := range []int{-1, visible, visible + 1, visible + 20} {
		if got := ff.fieldAtViewLine(form, line); got != -1 {
			t.Errorf("fieldAtViewLine(%d) = %d, want -1 (outside the visible window)", line, got)
		}
	}
}

// TestFormFieldsErrorFooterHeight is the tripwire for the third thing
// form_mouse.go replicates from huh: the validation-error block Group.View
// renders below the viewport. huh fields validate on blur, so on a form with
// required fields that footer appears the moment focus leaves an empty one —
// if it were not subtracted, the measured viewport would grow and every
// scrolled hit test would drift.
func TestFormFieldsErrorFooterHeight(t *testing.T) {
	failing := func(title string) huh.Field {
		return huh.NewInput().Title(title).Validate(func(string) error {
			return errors.New("required")
		})
	}
	fields := []huh.Field{
		failing("one"),
		huh.NewInput().Title("two"),
		huh.NewInput().Title("three"),
		huh.NewInput().Title("four"),
		huh.NewInput().Title("five"),
		huh.NewInput().Title("six"),
	}
	form := newSingleGroupForm(t, fields...)
	form.WithHeight(8)
	ff := formFields{fields: fields, gap: formFieldGap(bossHuhTheme())}

	if got := ff.errorFooterHeight(); got != 0 {
		t.Fatalf("errorFooterHeight with no errors = %d, want 0", got)
	}
	clean := visibleHeight(form, ff)
	rendered := lipgloss.Height(form.View())
	if clean != rendered {
		t.Fatalf("visibleHeight %d != rendered height %d with no errors", clean, rendered)
	}

	// Blurring the first field runs its validator and surfaces the footer.
	form.NextField()
	if fields[0].Error() == nil {
		t.Fatal("premise broken: huh no longer validates a field on blur")
	}
	if got := ff.errorFooterHeight(); got == 0 {
		t.Fatal("errorFooterHeight = 0 while a field reports an error")
	}
	if got := lipgloss.Height(form.View()); got <= rendered {
		t.Fatalf("premise broken: the error footer did not grow the form (%d -> %d)", rendered, got)
	}
	if got := visibleHeight(form, ff); got != clean {
		t.Errorf("visibleHeight = %d with an error footer, want %d (the footer was not subtracted)",
			got, clean)
	}
}

// TestFormFieldsHitTestSurvivesAValidationError is the behavioural half: a
// scrolled form must still map clicks correctly once an error footer appears,
// because that is exactly when a user reaches for another field.
func TestFormFieldsHitTestSurvivesAValidationError(t *testing.T) {
	fields := []huh.Field{
		huh.NewInput().Title("one").Validate(func(string) error {
			return errors.New("required")
		}),
		huh.NewInput().Title("two"),
		huh.NewInput().Title("three"),
		huh.NewInput().Title("four"),
		huh.NewInput().Title("five"),
		huh.NewInput().Title("six"),
	}
	form := newSingleGroupForm(t, fields...)
	form.WithHeight(8)
	ff := formFields{fields: fields, gap: formFieldGap(bossHuhTheme())}
	if ff.contentHeight() <= visibleHeight(form, ff) {
		t.Fatal("premise broken: the form does not scroll")
	}

	form.NextField() // focus field 1, and error field 0 on the way out
	if fields[0].Error() == nil {
		t.Fatal("premise broken: no validation error was raised")
	}

	// The error footer must not have shifted what huh scrolled to either.
	assertViewportTopMatchesOffset(t, form, ff)

	visible := visibleHeight(form, ff)
	offset := ff.scrollOffset(ff.focusedIndex(form), visible)
	for line := range visible {
		want := ff.fieldAtLine(offset + line)
		if got := ff.fieldAtViewLine(form, line); got != want {
			t.Fatalf("fieldAtViewLine(%d) = %d, want %d while an error footer is showing",
				line, got, want)
		}
	}
}

// TestFormFieldsFieldAtViewLineUnscrolled covers the common case: a form short
// enough that the viewport shows all of it, where view lines are content lines.
func TestFormFieldsFieldAtViewLineUnscrolled(t *testing.T) {
	a := huh.NewInput().Title("A")
	b := huh.NewInput().Title("B")
	form := newSingleGroupForm(t, a, b)
	ff := formFields{fields: []huh.Field{a, b}, gap: formFieldGap(bossHuhTheme())}

	if ff.contentHeight() > visibleHeight(form, ff) {
		t.Skip("unconstrained form unexpectedly scrolls; covered by the scrolled test")
	}
	if got, want := ff.fieldAtViewLine(form, 0), 0; got != want {
		t.Errorf("fieldAtViewLine(0) = %d, want %d", got, want)
	}
	if got, want := ff.fieldAtViewLine(form, ff.startLine(1)), 1; got != want {
		t.Errorf("fieldAtViewLine(startLine(1)) = %d, want %d", got, want)
	}
	if got := ff.fieldAtViewLine(form, -1); got != -1 {
		t.Errorf("fieldAtViewLine(-1) = %d, want -1", got)
	}
}

// --- handleMouse ------------------------------------------------------------

// TestFormFieldsHandleMouse asserts the swallow contract: every mouse-shaped
// message is claimed (huh cannot interpret any of them) but only a left click
// on an unfocused field produces a command.
func TestFormFieldsHandleMouse(t *testing.T) {
	build := func(t *testing.T) (*huh.Form, formFields) {
		t.Helper()
		fields := []huh.Field{
			huh.NewInput().Title("one"),
			huh.NewInput().Title("two"),
			huh.NewInput().Title("three"),
		}
		return newSingleGroupForm(t, fields...), formFields{
			fields: fields,
			gap:    formFieldGap(bossHuhTheme()),
		}
	}

	const linesBefore = 2

	t.Run("left click on another field focuses it", func(t *testing.T) {
		form, ff := build(t)
		y := linesBefore + ff.startLine(2)
		cmd, handled := ff.handleMouse(tea.MouseClickMsg{Y: y, Button: tea.MouseLeft}, form, linesBefore)
		if !handled {
			t.Fatal("mouse click was not claimed")
		}
		if cmd == nil {
			t.Fatal("left click on field 2 produced no command")
		}
		if got := ff.focusedIndex(form); got != 2 {
			t.Fatalf("focus after click = %d, want 2", got)
		}
	})

	t.Run("left click on the focused field is a no-op", func(t *testing.T) {
		form, ff := build(t)
		y := linesBefore + ff.startLine(0)
		cmd, handled := ff.handleMouse(tea.MouseClickMsg{Y: y, Button: tea.MouseLeft}, form, linesBefore)
		if !handled {
			t.Fatal("mouse click was not claimed")
		}
		if cmd != nil {
			t.Error("clicking the focused field re-focused it (would reset the cursor)")
		}
	})

	t.Run("click above the form changes nothing", func(t *testing.T) {
		form, ff := build(t)
		cmd, handled := ff.handleMouse(tea.MouseClickMsg{Y: 0, Button: tea.MouseLeft}, form, linesBefore)
		if !handled {
			t.Fatal("mouse click was not claimed")
		}
		if cmd != nil {
			t.Error("a click above the form produced a command")
		}
		if got := ff.focusedIndex(form); got != 0 {
			t.Errorf("focus = %d after a click above the form, want 0", got)
		}
	})

	t.Run("non-left buttons, wheel and motion are swallowed", func(t *testing.T) {
		y := 0
		msgs := []tea.Msg{
			tea.MouseClickMsg{Y: 0, Button: tea.MouseRight},
			tea.MouseClickMsg{Y: 0, Button: tea.MouseMiddle},
			tea.MouseWheelMsg{Y: 0, Button: tea.MouseWheelDown},
			tea.MouseMotionMsg{Y: 0},
			tea.MouseReleaseMsg{Y: 0, Button: tea.MouseLeft},
		}
		for _, msg := range msgs {
			form, ff := build(t)
			y = linesBefore + ff.startLine(2)
			switch m := msg.(type) {
			case tea.MouseClickMsg:
				m.Y = y
				msg = m
			case tea.MouseWheelMsg:
				m.Y = y
				msg = m
			case tea.MouseMotionMsg:
				m.Y = y
				msg = m
			case tea.MouseReleaseMsg:
				m.Y = y
				msg = m
			}
			cmd, handled := ff.handleMouse(msg, form, linesBefore)
			if !handled {
				t.Errorf("%T was forwarded to huh instead of being swallowed", msg)
			}
			if cmd != nil {
				t.Errorf("%T produced a focus command", msg)
			}
			if got := ff.focusedIndex(form); got != 0 {
				t.Errorf("%T moved focus to %d", msg, got)
			}
		}
	})

	t.Run("non-mouse messages are not claimed", func(t *testing.T) {
		form, ff := build(t)
		if _, handled := ff.handleMouse(tea.WindowSizeMsg{Width: 80, Height: 24}, form, linesBefore); handled {
			t.Error("a non-mouse message was claimed by handleMouse")
		}
	})
}

// --- shared helpers ---------------------------------------------------------

// newSingleGroupForm builds and initialises a form the same way the views do,
// so tests exercise the real theme, width and help suppression.
func newSingleGroupForm(t *testing.T, fields ...huh.Field) *huh.Form {
	t.Helper()
	form := huh.NewForm(
		huh.NewGroup(fields...),
	).WithTheme(bossHuhTheme()).WithShowHelp(false).WithWidth(bossFormWidth)
	if cmd := form.Init(); cmd != nil {
		cmd()
	}
	return form
}

// formGroupCount reports how many groups a built form holds.
//
// huh exposes no group enumeration, so this reaches the unexported selector by
// reflection — read-only (reflect.Value.Len needs no exported access), test-only,
// and the single place any view test depends on huh's internals. It backs the
// single-group invariant every form in this package relies on for hit testing:
// the line arithmetic assumes one flat vertical stack of fields.
func formGroupCount(t *testing.T, form *huh.Form) int {
	t.Helper()
	if form == nil {
		t.Fatal("formGroupCount: nil form")
	}
	sel := reflect.ValueOf(form).Elem().FieldByName("selector")
	if !sel.IsValid() || sel.IsNil() {
		t.Fatal("huh.Form no longer has a selector field; update formGroupCount")
	}
	items := sel.Elem().FieldByName("items")
	if !items.IsValid() {
		t.Fatal("huh selector no longer has an items field; update formGroupCount")
	}
	return items.Len()
}

// formFieldCount reports how many fields a built form's single group holds,
// reaching the same unexported selectors formGroupCount does and under the same
// terms: read-only, test-only, and loud on a huh rename. It backs the retained-
// slice mirror guard — boss hands the same fields to huh.NewGroup and to
// newFormFields, and nothing but this correlates the two.
func formFieldCount(t *testing.T, form *huh.Form) int {
	t.Helper()
	if form == nil {
		t.Fatal("formFieldCount: nil form")
	}
	sel := reflect.ValueOf(form).Elem().FieldByName("selector")
	if !sel.IsValid() || sel.IsNil() {
		t.Fatal("huh.Form no longer has a selector field; update formFieldCount")
	}
	groups := sel.Elem().FieldByName("items")
	if !groups.IsValid() {
		t.Fatal("huh selector no longer has an items field; update formFieldCount")
	}
	if groups.Len() != 1 {
		t.Fatalf("form has %d groups; formFieldCount assumes the single-group "+
			"invariant assertSingleGroup pins", groups.Len())
	}
	group := groups.Index(0)
	if group.Kind() != reflect.Pointer || group.IsNil() {
		t.Fatal("huh selector items are no longer *Group; update formFieldCount")
	}
	gsel := group.Elem().FieldByName("selector")
	if !gsel.IsValid() || gsel.IsNil() {
		t.Fatal("huh.Group no longer has a selector field; update formFieldCount")
	}
	fields := gsel.Elem().FieldByName("items")
	if !fields.IsValid() {
		t.Fatal("huh group selector no longer has an items field; update formFieldCount")
	}
	return fields.Len()
}

// assertSingleGroup is the guard the plan calls the single-group invariant: a
// future multi-group form must fail loudly here rather than silently mis-map
// every click.
func assertSingleGroup(t *testing.T, form *huh.Form) {
	t.Helper()
	if got := formGroupCount(t, form); got != 1 {
		t.Errorf("form has %d huh groups, want 1; form_mouse.go's hit testing "+
			"assumes a single flat group of fields", got)
	}
}

// assertViewportTopMatchesOffset is the tripwire for the scroll offset
// form_mouse.go replicates from huh's group.go (getContent + buildView).
//
// It is deliberately NOT expressed in terms of scrollOffset alone. Comparing
// fieldAtViewLine(form, 0) against fieldAtLine(scrollOffset(...)) is
// tautological — fieldAtViewLine evaluates exactly that expression, so it holds
// for any offset value, right or wrong. This instead compares the line huh
// *renders* at the top of its viewport against the content line boss predicts,
// reconstructing the content the way group.go composes it. A huh bump that
// changed the scroll composition (say, centring the focused field) would move
// the rendered line and fail here, which is the whole point of the tripwire.
func assertViewportTopMatchesOffset(t *testing.T, form *huh.Form, fields formFields) {
	t.Helper()
	visible := visibleHeight(form, fields)
	// Mirror fieldAtViewLine's own branch: content that fits is never offset.
	// The unscrolled case matters as much as the scrolled one — a form that
	// grew past needing to scroll while huh kept a stale offset is the worst
	// version of this bug, because the hit test stops compensating entirely.
	offset := 0
	if fields.contentHeight() > visible {
		offset = fields.scrollOffset(fields.focusedIndex(form), visible)
	}
	content := formContentLines(fields)
	if offset < 0 || offset >= len(content) {
		t.Fatalf("scrollOffset = %d, outside the %d content lines", offset, len(content))
	}

	trim := func(s string) string { return strings.TrimRight(s, " ") }
	want := trim(content[offset])
	// Guard against a vacuous comparison: if a neighbouring content line is
	// identical (two blank lines, say), matching it would prove nothing about
	// the offset. Pick a scenario where the lines actually differ.
	for _, delta := range []int{-1, 1} {
		if n := offset + delta; n >= 0 && n < len(content) && trim(content[n]) == want {
			t.Fatalf("content lines %d and %d are identical (%q); this assertion "+
				"could not detect an off-by-one scroll offset", offset, n, want)
		}
	}
	if got := trim(strings.SplitN(form.View(), "\n", 2)[0]); got != want {
		t.Errorf("huh renders %q at the top of its viewport, but the replicated "+
			"scroll offset (%d) predicts %q — huh's group.go composition changed "+
			"and form_mouse.go scrollOffset must be updated", got, offset, want)
	}
}

// formContentLines reconstructs the group's scrollable content the way huh's
// group.go getContent does: every retained field's View() in order, joined by
// the theme's rendered FieldSeparator.
func formContentLines(fields formFields) []string {
	sep := bossHuhTheme().Theme(true).FieldSeparator.Render()
	views := make([]string, 0, len(fields.fields))
	for i := range fields.fields {
		views = append(views, fields.fields[i].View())
	}
	return strings.Split(strings.Join(views, sep), "\n")
}

// assertFormStartsAtFirstField pins visibleHeight's other premise: that no form
// in this package gives its group a Title or Description. huh's Group.View
// renders that header *above* the viewport, so one would push every field down
// a line without changing the height the helper measures. Line 0 of the
// rendered form must therefore be line 0 of the first field.
func assertFormStartsAtFirstField(t *testing.T, form *huh.Form, fields formFields) {
	t.Helper()
	if len(fields.fields) == 0 {
		t.Fatal("no retained fields to compare against")
	}
	if fields.scrollOffset(fields.focusedIndex(form), visibleHeight(form, fields)) != 0 {
		t.Skip("form is scrolled; line 0 is not the first field by construction")
	}
	firstLine := func(s string) string {
		return strings.TrimRight(strings.SplitN(s, "\n", 2)[0], " ")
	}
	if got, want := firstLine(form.View()), firstLine(fields.fields[0].View()); got != want {
		t.Errorf("form.View() starts with %q, want the first field's first line %q; "+
			"a group header would shift every hit test", got, want)
	}
}
