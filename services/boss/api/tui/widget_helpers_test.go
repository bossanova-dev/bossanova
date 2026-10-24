package tui

import (
	"image/color"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
) // focusGutterGlyph is the rune the focused field's left border paints. It is a
// deliberate second reference to lipgloss's border definition rather than a read
// of the constructed style: deriving it from the theme would make every gutter
// assertion below tautological. The cost is that swapping the theme to another
// border fails these tests until this is pointed at the new border too — which
// is the intended trade, since the gutter is a user-visible contract.
var focusGutterGlyph = []rune(lipgloss.ThickBorder().Left)[0]

// paintedIn reports whether rendered output carries c as an SGR colour. lipgloss
// emits colours as an "r;g;b" triple, so this renders a probe in c and looks for
// that triple — no palette hex is hard-coded, and the check works for foreground
// and background alike. It matches the bare triple rather than a "38;2;"-prefixed
// sequence so one helper covers both, which means it cannot tell apart two palette
// entries that share a hex — colorSelected and colorInfo are both #4CA7F8, so
// "painted in colorSelected" here means "painted in that colour", not "reached via
// that constant".
func paintedIn(t *testing.T, rendered string, c color.Color) bool {
	t.Helper()

	probe := lipgloss.NewStyle().Foreground(c).Render("x")
	const fgPrefix = "38;2;"
	start := strings.Index(probe, fgPrefix)
	if start < 0 {
		t.Fatalf("cannot derive the SGR triple for %v: lipgloss rendered %q without a %q prefix", c, probe, fgPrefix)
	}
	rest := probe[start+len(fgPrefix):]
	end := strings.IndexByte(rest, 'm')
	if end < 0 {
		t.Fatalf("cannot derive the SGR triple for %v: unterminated sequence in %q", c, probe)
	}
	return strings.Contains(rendered, rest[:end])
}

// renderedColumnOf returns the terminal column (0-based) at which needle first
// appears in a rendered block, measured in display cells so a wide rune or a
// leading style escape cannot skew the arithmetic.

// firstNonSpaceColumn returns the column (0-based) of the first non-space
// character in the first line of rendered that has one.

// newSingleGroupForm builds and initialises a form the same way the views do,
// so tests exercise the real theme, width and help suppression.
func newSingleGroupForm(t *testing.T, fields ...huh.Field) *huh.Form {
	t.Helper()
	form := ui.NewBossForm(fields...)
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
// newui.FormFields, and nothing but this correlates the two.
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
func assertViewportTopMatchesOffset(t *testing.T, form *huh.Form, fields ui.FormFields) {
	t.Helper()
	visible := ui.VisibleHeight(form, fields)
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
func formContentLines(fields ui.FormFields) []string {
	sep := ui.BossHuhTheme().Theme(true).FieldSeparator.Render()
	views := make([]string, 0, len(fields.Fields))
	for i := range fields.Fields {
		views = append(views, fields.Fields[i].View())
	}
	return strings.Split(strings.Join(views, sep), "\n")
}

// assertFormStartsAtFirstField pins visibleHeight's other premise: that no form
// in this package gives its group a Title or Description. huh's Group.View
// renders that header *above* the viewport, so one would push every field down
// a line without changing the height the helper measures. Line 0 of the
// rendered form must therefore be line 0 of the first field.
func assertFormStartsAtFirstField(t *testing.T, form *huh.Form, fields ui.FormFields) {
	t.Helper()
	if len(fields.Fields) == 0 {
		t.Fatal("no retained fields to compare against")
	}
	if fields.ScrollOffset(fields.FocusedIndex(form), ui.VisibleHeight(form, fields)) != 0 {
		t.Skip("form is scrolled; line 0 is not the first field by construction")
	}
	firstLine := func(s string) string {
		return strings.TrimRight(strings.SplitN(s, "\n", 2)[0], " ")
	}
	if got, want := firstLine(form.View()), firstLine(fields.Fields[0].View()); got != want {
		t.Errorf("form.View() starts with %q, want the first field's first line %q; "+
			"a group header would shift every hit test", got, want)
	}
}

// struckThrough reports whether the last SGR sequence before text in s turns
// strikethrough (parameter 9) on.
func struckThrough(s, text string) bool {
	at := strings.Index(s, text)
	if at < 0 {
		return false
	}
	var last string
	for rest := s[:at]; ; {
		start := strings.Index(rest, "\x1b[")
		if start < 0 {
			break
		}
		rest = rest[start+2:]
		end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > ';' })
		if end >= 0 && rest[end] == 'm' {
			last = rest[:end]
		}
	}
	return slices.Contains(strings.Split(last, ";"), "9")
}
