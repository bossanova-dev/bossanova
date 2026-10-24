package ui

import (
	"errors"
	"io"
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
func stubFormFields(fields ...huh.Field) FormFields {
	return FormFields{Fields: fields, Gap: 1}
}

// --- Gap derivation ---------------------------------------------------------

// TestFormFieldGapMatchesTheme is the tripwire for the first thing form_mouse.go
// replicates from huh: the blank-line gap between adjacent fields. huh's
// ThemeBase separator is "\n\n", which puts exactly one blank line between two
// field views. If a huh bump changes that, every hit test shifts by a line per
// field and this fails first.
func TestFormFieldGapMatchesTheme(t *testing.T) {
	if got := FormFieldGap(BossHuhTheme()); got != 1 {
		t.Fatalf("formFieldGap = %d, want 1", got)
	}

	// Prove the derivation against the composition huh performs: field, gap,
	// field. Two 1-line views separated by the theme separator must render at
	// the derived total height.
	sep := BossHuhTheme().Theme(true).FieldSeparator.Render()
	joined := "a" + sep + "b"
	want := 1 + 1 + FormFieldGap(BossHuhTheme())
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

	if got, want := ff.ContentHeight(), 11; got != want {
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
			if got := ff.FieldAtLine(tt.line); got != tt.want {
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
		if got := ff.StartLine(i); got != want {
			t.Errorf("startLine(%d) = %d, want %d", i, got, want)
		}
	}
	if got := ff.StartLine(3); got != -1 {
		t.Errorf("startLine(out of range) = %d, want -1", got)
	}
	if got := ff.StartLine(-1); got != -1 {
		t.Errorf("startLine(-1) = %d, want -1", got)
	}
}

// TestFormFieldsZeroValueIsInert guards the "view has not built its form yet"
// path: every lookup must decline rather than panic.
func TestFormFieldsZeroValueIsInert(t *testing.T) {
	var ff FormFields
	if got := ff.FieldAtLine(0); got != -1 {
		t.Errorf("fieldAtLine on zero value = %d, want -1", got)
	}
	if got := ff.FocusedIndex(nil); got != -1 {
		t.Errorf("focusedIndex(nil) = %d, want -1", got)
	}
	if got := ff.FieldAtViewLine(nil, 0); got != -1 {
		t.Errorf("fieldAtViewLine(nil) = %d, want -1", got)
	}
	if cmd := ff.FocusCmd(nil, 0); cmd != nil {
		t.Error("focusCmd on zero value returned a command")
	}
	if got := ff.ContentHeight(); got != 0 {
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

	ff := FormFields{Fields: []huh.Field{a, b, c}, Gap: 1}
	if got := ff.FocusedIndex(form); got != 0 {
		t.Fatalf("focusedIndex after Init = %d, want 0", got)
	}

	form.NextField()
	if got := ff.FocusedIndex(form); got != 1 {
		t.Fatalf("focusedIndex after NextField = %d, want 1", got)
	}

	// A slice that does not contain the focused field must not guess.
	other := FormFields{Fields: []huh.Field{c, a}, Gap: 1}
	if got := other.FocusedIndex(form); got != -1 {
		t.Fatalf("focusedIndex for a foreign focused field = %d, want -1", got)
	}
}

// --- focusCmd ---------------------------------------------------------------

// TestFormFieldsFocusCmdDirection drives focus forward and backward across a
// form, and asserts a Skip() field (huh.NewNote) is stepped over rather than
// focused.
func TestFormFieldsFocusCmdDirection(t *testing.T) {
	build := func(t *testing.T) (*huh.Form, FormFields, []huh.Field) {
		t.Helper()
		fields := []huh.Field{
			huh.NewInput().Title("zero"),
			huh.NewNote().Title("note"), // Skip() == true
			huh.NewInput().Title("two"),
			huh.NewInput().Title("three"),
		}
		form := newSingleGroupForm(t, fields...)
		return form, FormFields{Fields: fields, Gap: 1}, fields
	}

	t.Run("forward across a skipped note", func(t *testing.T) {
		form, ff, _ := build(t)
		if got := ff.FocusedIndex(form); got != 0 {
			t.Fatalf("initial focus = %d, want 0", got)
		}
		cmd := ff.FocusCmd(form, 3)
		if cmd == nil {
			t.Fatal("focusCmd returned nil for a reachable forward target")
		}
		if got := ff.FocusedIndex(form); got != 3 {
			t.Fatalf("focus after forward walk = %d, want 3", got)
		}
	})

	t.Run("backward to the first field", func(t *testing.T) {
		form, ff, _ := build(t)
		if cmd := ff.FocusCmd(form, 3); cmd == nil {
			t.Fatal("setup walk returned nil")
		}
		cmd := ff.FocusCmd(form, 0)
		if cmd == nil {
			t.Fatal("focusCmd returned nil for a reachable backward target")
		}
		if got := ff.FocusedIndex(form); got != 0 {
			t.Fatalf("focus after backward walk = %d, want 0", got)
		}
	})

	t.Run("skipped field is never focused", func(t *testing.T) {
		form, ff, fields := build(t)
		if !fields[1].Skip() {
			t.Fatal("test premise broken: huh.NewNote no longer reports Skip()")
		}
		if cmd := ff.FocusCmd(form, 1); cmd != nil {
			t.Error("focusCmd targeted a Skip() field")
		}
		if got := ff.FocusedIndex(form); got != 0 {
			t.Errorf("focus moved to %d after a click on a skipped field, want 0", got)
		}
	})

	t.Run("already focused is a no-op", func(t *testing.T) {
		form, ff, _ := build(t)
		if cmd := ff.FocusCmd(form, 0); cmd != nil {
			t.Error("focusCmd re-focused the already-focused field")
		}
	})

	t.Run("out of range target", func(t *testing.T) {
		form, ff, _ := build(t)
		for _, target := range []int{-1, 4, 99} {
			if cmd := ff.FocusCmd(form, target); cmd != nil {
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
	if got, want := ff.ContentHeight(), 14; got != want {
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
			if got := ff.ScrollOffset(tt.focused, tt.visible); got != tt.want {
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
	ff := FormFields{Fields: fields, Gap: FormFieldGap(BossHuhTheme())}

	visible := VisibleHeight(form, ff)
	if visible != 8 {
		t.Fatalf("visibleHeight = %d, want 8 (huh no longer pads the viewport to its height?)", visible)
	}
	if ff.ContentHeight() <= visible {
		t.Fatalf("premise broken: content %d fits in %d, nothing scrolls", ff.ContentHeight(), visible)
	}

	// Focus the third field: far enough down to scroll, but not so far that the
	// viewport's clamp takes over — an offset sitting ON the clamp would agree
	// with a wrong formula by accident, so pin the free-running case first.
	if cmd := ff.FocusCmd(form, 2); cmd == nil {
		t.Fatal("could not focus field 2")
	}
	if mid, maxOffset := ff.ScrollOffset(2, visible), ff.ContentHeight()-visible; mid == 0 || mid >= maxOffset {
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
	if cmd := ff.FocusCmd(form, 3); cmd == nil {
		t.Fatal("could not focus field 3")
	}
	offset := ff.ScrollOffset(3, visible)
	if offset == 0 {
		t.Fatal("premise broken: focusing field 3 produced no scroll offset")
	}
	assertViewportTopMatchesOffset(t, form, ff)

	// The first visible line belongs to whichever field owns content line
	// `offset` — derived from the same helper, so a drift breaks this test.
	wantTop := ff.FieldAtLine(offset)
	if got := ff.FieldAtViewLine(form, 0); got != wantTop {
		t.Errorf("fieldAtViewLine(0) = %d, want %d (scroll offset ignored?)", got, wantTop)
	}

	// A click on the last visible line still maps inside the window.
	if got := ff.FieldAtViewLine(form, visible-1); got != ff.FieldAtLine(offset+visible-1) {
		t.Errorf("fieldAtViewLine(%d) disagrees with the scrolled content mapping", visible-1)
	}

	// Clicks outside the visible window are ignored, never clamped.
	for _, line := range []int{-1, visible, visible + 1, visible + 20} {
		if got := ff.FieldAtViewLine(form, line); got != -1 {
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
	ff := FormFields{Fields: fields, Gap: FormFieldGap(BossHuhTheme())}

	if got := ff.errorFooterHeight(); got != 0 {
		t.Fatalf("errorFooterHeight with no errors = %d, want 0", got)
	}
	clean := VisibleHeight(form, ff)
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
	if got := VisibleHeight(form, ff); got != clean {
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
	ff := FormFields{Fields: fields, Gap: FormFieldGap(BossHuhTheme())}
	if ff.ContentHeight() <= VisibleHeight(form, ff) {
		t.Fatal("premise broken: the form does not scroll")
	}

	form.NextField() // focus field 1, and error field 0 on the way out
	if fields[0].Error() == nil {
		t.Fatal("premise broken: no validation error was raised")
	}

	// The error footer must not have shifted what huh scrolled to either.
	assertViewportTopMatchesOffset(t, form, ff)

	visible := VisibleHeight(form, ff)
	offset := ff.ScrollOffset(ff.FocusedIndex(form), visible)
	for line := range visible {
		want := ff.FieldAtLine(offset + line)
		if got := ff.FieldAtViewLine(form, line); got != want {
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
	ff := FormFields{Fields: []huh.Field{a, b}, Gap: FormFieldGap(BossHuhTheme())}

	if ff.ContentHeight() > VisibleHeight(form, ff) {
		t.Skip("unconstrained form unexpectedly scrolls; covered by the scrolled test")
	}
	if got, want := ff.FieldAtViewLine(form, 0), 0; got != want {
		t.Errorf("fieldAtViewLine(0) = %d, want %d", got, want)
	}
	if got, want := ff.FieldAtViewLine(form, ff.StartLine(1)), 1; got != want {
		t.Errorf("fieldAtViewLine(startLine(1)) = %d, want %d", got, want)
	}
	if got := ff.FieldAtViewLine(form, -1); got != -1 {
		t.Errorf("fieldAtViewLine(-1) = %d, want -1", got)
	}
}

// --- handleMouse ------------------------------------------------------------

// TestFormFieldsHandleMouse asserts the swallow contract: every mouse-shaped
// message is claimed (huh cannot interpret any of them) but only a left click
// on an unfocused field produces a command.
func TestFormFieldsHandleMouse(t *testing.T) {
	build := func(t *testing.T) (*huh.Form, FormFields) {
		t.Helper()
		fields := []huh.Field{
			huh.NewInput().Title("one"),
			huh.NewInput().Title("two"),
			huh.NewInput().Title("three"),
		}
		return newSingleGroupForm(t, fields...), FormFields{
			Fields: fields,
			Gap:    FormFieldGap(BossHuhTheme()),
		}
	}

	const linesBefore = 2

	t.Run("left click on another field focuses it", func(t *testing.T) {
		form, ff := build(t)
		y := linesBefore + ff.StartLine(2)
		cmd, handled := ff.HandleMouse(tea.MouseClickMsg{Y: y, Button: tea.MouseLeft}, form, linesBefore)
		if !handled {
			t.Fatal("mouse click was not claimed")
		}
		if cmd == nil {
			t.Fatal("left click on field 2 produced no command")
		}
		if got := ff.FocusedIndex(form); got != 2 {
			t.Fatalf("focus after click = %d, want 2", got)
		}
	})

	t.Run("left click on the focused field is a no-op", func(t *testing.T) {
		form, ff := build(t)
		y := linesBefore + ff.StartLine(0)
		cmd, handled := ff.HandleMouse(tea.MouseClickMsg{Y: y, Button: tea.MouseLeft}, form, linesBefore)
		if !handled {
			t.Fatal("mouse click was not claimed")
		}
		if cmd != nil {
			t.Error("clicking the focused field re-focused it (would reset the cursor)")
		}
	})

	t.Run("click above the form changes nothing", func(t *testing.T) {
		form, ff := build(t)
		cmd, handled := ff.HandleMouse(tea.MouseClickMsg{Y: 0, Button: tea.MouseLeft}, form, linesBefore)
		if !handled {
			t.Fatal("mouse click was not claimed")
		}
		if cmd != nil {
			t.Error("a click above the form produced a command")
		}
		if got := ff.FocusedIndex(form); got != 0 {
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
			y = linesBefore + ff.StartLine(2)
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
			cmd, handled := ff.HandleMouse(msg, form, linesBefore)
			if !handled {
				t.Errorf("%T was forwarded to huh instead of being swallowed", msg)
			}
			if cmd != nil {
				t.Errorf("%T produced a focus command", msg)
			}
			if got := ff.FocusedIndex(form); got != 0 {
				t.Errorf("%T moved focus to %d", msg, got)
			}
		}
	})

	t.Run("non-mouse messages are not claimed", func(t *testing.T) {
		form, ff := build(t)
		if _, handled := ff.HandleMouse(tea.WindowSizeMsg{Width: 80, Height: 24}, form, linesBefore); handled {
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
	).WithTheme(BossHuhTheme()).WithShowHelp(false).WithWidth(bossFormWidth)
	if cmd := form.Init(); cmd != nil {
		cmd()
	}
	return form
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
func assertViewportTopMatchesOffset(t *testing.T, form *huh.Form, fields FormFields) {
	t.Helper()
	visible := VisibleHeight(form, fields)
	// Mirror fieldAtViewLine's own branch: content that fits is never offset.
	// The unscrolled case matters as much as the scrolled one — a form that
	// grew past needing to scroll while huh kept a stale offset is the worst
	// version of this bug, because the hit test stops compensating entirely.
	offset := 0
	if fields.ContentHeight() > visible {
		offset = fields.ScrollOffset(fields.FocusedIndex(form), visible)
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
func formContentLines(fields FormFields) []string {
	sep := BossHuhTheme().Theme(true).FieldSeparator.Render()
	views := make([]string, 0, len(fields.Fields))
	for i := range fields.Fields {
		views = append(views, fields.Fields[i].View())
	}
	return strings.Split(strings.Join(views, sep), "\n")
}
