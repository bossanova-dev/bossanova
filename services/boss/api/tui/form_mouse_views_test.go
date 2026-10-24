package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/repos"

	"github.com/recurser/boss/api/tui/bugreport"
	"github.com/recurser/boss/api/tui/cron"
	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// formClickCase is one form view under test, reduced to the four things the
// click contract needs: the built model, its form, its retained field slice,
// and the lines it renders above the form.
type formClickCase struct {
	name string
	// file is the package source file that hosts this form. It ties the roster
	// to formViewFiles (which TestEveryFormFileIsCovered keeps honest against
	// the source), so a new form view cannot advertise "[click] select field"
	// via the shared nav hints without a click case proving it works.
	file string
	// build returns a fresh, initialised model plus an update function that
	// pushes a message through the model's real Update path.
	build func(t *testing.T) formClickSubject
	// target is the field index a click should focus. Every case uses a field
	// other than index 0 so "focus moved" is unambiguous.
	target int
}

type formClickSubject struct {
	form   *huh.Form
	fields ui.FormFields
	before int
	update func(msg tea.Msg)
	// view returns the model's current tea.View, for the MouseMode assertions.
	view func() tea.View
	// prefix is the exact string the view claims it renders above the form —
	// the ruler `before` is measured from. TestFormViewsRenderTheirFormPrefix
	// checks View() actually honours it.
	prefix string
}

func formClickCases() []formClickCase {
	return []formClickCase{
		{name: "repo add input form", file: "../../api/tui/repos/repo_add.go", target: 1, build: buildRepoAddInputSubject},
		{name: "repo add details form", file: "../../api/tui/repos/repo_add.go", target: 2, build: buildRepoAddDetailsSubject},
		{name: "new session form", file: "../../api/tui/newsession/newsession_create.go", target: 0, build: buildNewSessionSubject},
		{name: "cron form", file: "../../api/tui/cron/cron_form.go", target: 3, build: buildCronFormSubject},
		{name: "bug report form", file: "../../api/tui/bugreport/bugreport.go", target: 0, build: buildBugReportSubject},
	}
}

func buildRepoAddInputSubject(t *testing.T) formClickSubject {
	t.Helper()
	m := repoInputModel(t, true)
	initForm(t, m.Form())
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = updated.(repos.RepoAddModel)
	return formClickSubject{
		form:   m.Form(),
		fields: m.FormFields(),
		before: ui.LinesBefore(m.FormPrefix()),
		prefix: m.FormPrefix(),
		update: func(msg tea.Msg) { m.Update(msg) },
		view:   m.View,
	}
}

func buildRepoAddDetailsSubject(t *testing.T) formClickSubject {
	t.Helper()
	m := repoDetailsModel(t)
	initForm(t, m.Form())
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = updated.(repos.RepoAddModel)
	return formClickSubject{
		form:   m.Form(),
		fields: m.FormFields(),
		before: ui.LinesBefore(m.FormPrefix()),
		prefix: m.FormPrefix(),
		update: func(msg tea.Msg) { m.Update(msg) },
		view:   m.View,
	}
}

func buildNewSessionSubject(t *testing.T) formClickSubject {
	t.Helper()
	m := readyNewSessionForm(t)
	initForm(t, m.Form())
	return formClickSubject{
		form:   m.Form(),
		fields: m.FormFields(),
		before: ui.LinesBefore(m.FormPrefix()),
		prefix: m.FormPrefix(),
		update: func(msg tea.Msg) { m.Update(msg) },
		view:   m.View,
	}
}

func buildCronFormSubject(t *testing.T) formClickSubject {
	t.Helper()
	m := newReadyCronFormForTest(80, 60)
	initForm(t, m.Form())
	return formClickSubject{
		form:   m.Form(),
		fields: m.FormFields(),
		before: ui.LinesBefore(m.FormPrefix()),
		prefix: m.FormPrefix(),
		update: func(msg tea.Msg) { m.Update(msg) },
		view:   m.View,
	}
}

func buildBugReportSubject(t *testing.T) formClickSubject {
	t.Helper()
	m := NewBugReportModel(nil, context.Background(), nil, ui.ViewHome, nil, nil)
	initForm(t, m.Form())
	return formClickSubject{
		form:   m.Form(),
		fields: m.FormFields(),
		before: ui.LinesBefore(m.FormPrefix()),
		prefix: m.FormPrefix(),
		update: func(msg tea.Msg) { m.Update(msg) },
		view:   m.View,
	}
}

func initForm(t *testing.T, form *huh.Form) {
	t.Helper()
	if form == nil {
		t.Fatal("view built no huh form")
	}
	if cmd := form.Init(); cmd != nil {
		cmd()
	}
}

// TestFormViewsFocusFieldOnClick drives a left click through each form view's
// real Update path at a Y derived from that view's own formPrefix and the
// retained field slice — so a layout change that shifts the form without
// shifting the helper breaks this test rather than the user's clicks.
func TestFormViewsFocusFieldOnClick(t *testing.T) {
	for _, tt := range formClickCases() {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.build(t)
			if len(s.fields.Fields) == 0 {
				t.Fatal("view retained no field slice; click-to-focus is dead here")
			}
			if s.fields.FocusedIndex(s.form) != 0 {
				t.Fatalf("expected focus on field 0 after Init, got %d", s.fields.FocusedIndex(s.form))
			}

			if tt.target == 0 {
				// Single-field form: there is nothing to move to, so the only
				// meaningful assertion is that clicking it does not re-focus.
				s.update(tea.MouseClickMsg{Y: s.before, Button: tea.MouseLeft})
				if got := s.fields.FocusedIndex(s.form); got != 0 {
					t.Errorf("focus moved to %d on a single-field form", got)
				}
				return
			}

			y := s.before + s.fields.StartLine(tt.target)
			s.update(tea.MouseClickMsg{Y: y, Button: tea.MouseLeft})
			if got := s.fields.FocusedIndex(s.form); got != tt.target {
				t.Errorf("click at y=%d focused field %d, want %d", y, got, tt.target)
			}
		})
	}
}

// TestFormViewsRenderTheirFormPrefix closes the one hole every other click test
// shares: they all derive the click Y from linesBefore(m.FormPrefix()), which is
// the same expression Update evaluates. Nothing else asserts that View actually
// renders that prefix, and nothing but it, above the form — so a line added to
// a view's form branch without touching formPrefix would put every click one row
// low (wrong field focused, and validators fired on every field walked over)
// with the whole suite still green.
//
// This is the per-view analogue of TestAppChromeHeightMatchesViewPrepend, and
// it is what makes formPrefix a genuinely shared ruler rather than two copies
// that happen to agree.
func TestFormViewsRenderTheirFormPrefix(t *testing.T) {
	for _, tt := range formClickCases() {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.build(t)
			formView := s.form.View()
			if formView == "" {
				t.Fatal("the form rendered nothing; this assertion would be vacuous")
			}
			content := s.view().Content
			if content == "" {
				t.Fatal("the view rendered nothing")
			}
			if !strings.HasPrefix(content, s.prefix) {
				t.Fatalf("View() does not start with formPrefix();\nprefix: %q\ncontent starts: %q",
					s.prefix, content[:min(len(content), 200)])
			}

			// The form's first line must land at exactly `before`. Compare
			// trimmed, because views indent the form with PaddingLeft.
			lines := strings.Split(content, "\n")
			if s.before >= len(lines) {
				t.Fatalf("formPrefix claims %d lines but View() rendered only %d",
					s.before, len(lines))
			}
			want := strings.TrimSpace(strings.SplitN(formView, "\n", 2)[0])
			if want == "" {
				t.Fatal("the form's first line is blank; this assertion could not " +
					"detect an off-by-one — pick a fixture whose first field renders text")
			}
			if got := strings.TrimSpace(lines[s.before]); got != want {
				t.Errorf("View() renders %q at line %d, but the hit test expects the "+
					"form to start there (%q).\nSomething was added between formPrefix() "+
					"and form.View() without formPrefix() accounting for it, so every "+
					"click is offset.", got, s.before, want)
			}
		})
	}
}

// TestEveryFormFileHasAClickCase ties the click roster to formViewFiles, which
// TestEveryFormFileIsCovered keeps honest against the package source.
//
// formNavHints advertises "[click] select field" on EVERY form unconditionally,
// but click support is four opt-in per-view edits (retain the field slice, call
// handleMouse, set MouseMode). Without this guard a newly added form view would
// advertise an affordance it never implemented, and no test would notice.
func TestEveryFormFileHasAClickCase(t *testing.T) {
	covered := map[string]bool{}
	for _, tt := range formClickCases() {
		if tt.file == "" {
			t.Errorf("click case %q declares no source file", tt.name)
			continue
		}
		covered[tt.file] = true
	}
	for _, file := range formViewFiles {
		if !covered[file] {
			t.Errorf("%s renders a huh form and therefore advertises %q, but no "+
				"formClickCase proves clicking works there",
				file, "[click] select field")
		}
	}
}

// TestFormViewsRetainEveryField guards the hand-mirrored field slice. Each view
// passes its fields to huh.NewGroup AND, separately, to newFormFields; a field
// added to the group but not the mirror mis-maps every click below it, with no
// compile error and no other test failing. Counting huh's own group proves the
// two lists did not drift.
func TestFormViewsRetainEveryField(t *testing.T) {
	for _, tt := range formClickCases() {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.build(t)
			if got, want := len(s.fields.Fields), formFieldCount(t, s.form); got != want {
				t.Errorf("the view retained %d fields but its huh group holds %d; "+
					"a field was added to huh.NewGroup without adding it to "+
					"newFormFields, so every click below it maps to the wrong field",
					got, want)
			}
		})
	}
}

// TestFormViewsIgnoreClicksOutsideTheForm covers the three "change nothing"
// criteria: above the form, below it, and a non-left button.
func TestFormViewsIgnoreClicksOutsideTheForm(t *testing.T) {
	for _, tt := range formClickCases() {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.build(t)
			last := len(s.fields.Fields) - 1
			belowForm := s.before + s.fields.ContentHeight() + 10

			msgs := []struct {
				label string
				msg   tea.Msg
			}{
				{"above the form", tea.MouseClickMsg{Y: 0, Button: tea.MouseLeft}},
				{"below the form", tea.MouseClickMsg{Y: belowForm, Button: tea.MouseLeft}},
				{"right button on a field", tea.MouseClickMsg{
					Y: s.before + s.fields.StartLine(last), Button: tea.MouseRight,
				}},
				{"wheel over a field", tea.MouseWheelMsg{
					Y: s.before + s.fields.StartLine(last), Button: tea.MouseWheelDown,
				}},
				{"motion over a field", tea.MouseMotionMsg{
					Y: s.before + s.fields.StartLine(last),
				}},
			}

			for _, m := range msgs {
				s := tt.build(t)
				s.update(m.msg)
				if got := s.fields.FocusedIndex(s.form); got != 0 {
					t.Errorf("%s moved focus to %d, want 0", m.label, got)
				}
			}
		})
	}
}

// TestFormViewsClickOnFocusedFieldIsANoOp guards the cursor-position criterion:
// re-focusing a text input resets its cursor, so a click on the already-focused
// field must not step focus at all.
func TestFormViewsClickOnFocusedFieldIsANoOp(t *testing.T) {
	for _, tt := range formClickCases() {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.build(t)
			focused := s.fields.FocusedIndex(s.form)
			before := s.form.GetFocusedField()

			y := s.before + s.fields.StartLine(focused)
			s.update(tea.MouseClickMsg{Y: y, Button: tea.MouseLeft})

			if got := s.fields.FocusedIndex(s.form); got != focused {
				t.Errorf("focus moved from %d to %d", focused, got)
			}
			if s.form.GetFocusedField() != before {
				t.Error("clicking the focused field replaced the focused field")
			}
		})
	}
}

// TestFormViewsSkipFieldsAreNeverClicked pins the Skip() criterion on the one
// place a skipped field can appear inside a boss form: a note. None of the four
// forms ships one today, so the guard is expressed against the shared helper
// with a form shaped like one that did.
func TestFormViewsSkipFieldsAreNeverClicked(t *testing.T) {
	note := huh.NewNote().Title("read me")
	fields := []huh.Field{
		huh.NewInput().Title("first"),
		note,
		huh.NewInput().Title("last"),
	}
	form := newSingleGroupForm(t, fields...)
	ff := ui.FormFields{Fields: fields, Gap: ui.FormFieldGap(ui.BossHuhTheme())}

	cmd, handled := ff.HandleMouse(
		tea.MouseClickMsg{Y: ff.StartLine(1), Button: tea.MouseLeft}, form, 0)
	if !handled {
		t.Fatal("the click was not claimed")
	}
	if cmd != nil {
		t.Error("a click on a Skip() note produced a focus command")
	}
	if got := ff.FocusedIndex(form); got != 0 {
		t.Errorf("focus moved to %d after clicking a Skip() note, want 0", got)
	}
}

// TestCronFormClickWithScrollOffset is the scroll-aware case: the cron form is
// the tallest in the package, so on a short terminal its group scrolls and a
// naive hit test would focus the wrong field.
func TestCronFormClickWithScrollOffset(t *testing.T) {
	m := newReadyCronFormForTest(80, 24)
	initForm(t, m.Form())

	ff := m.FormFields()
	before := ui.LinesBefore(m.FormPrefix())
	visible := ui.VisibleHeight(m.Form(), ff)
	if ff.ContentHeight() <= visible {
		t.Fatalf("premise broken: cron form content %d fits in %d lines, nothing scrolls",
			ff.ContentHeight(), visible)
	}

	// Move focus down so the viewport scrolls, then click the top visible line.
	const focusTarget = 5
	if cmd := ff.FocusCmd(m.Form(), focusTarget); cmd == nil {
		t.Fatalf("could not focus field %d", focusTarget)
	}
	offset := ff.ScrollOffset(focusTarget, visible)
	if offset == 0 {
		t.Fatal("premise broken: the cron form did not scroll")
	}

	// huh scrolls the focused field to the very top of the window, so a click on
	// line 0 would map to the focused field with or without the offset. Pick a
	// line further down whose scrolled answer differs from its unscrolled one —
	// that is the only click that can tell the two apart.
	viewLine, want := -1, -1
	for k := 1; k < visible; k++ {
		scrolled := ff.FieldAtLine(offset + k)
		naive := ff.FieldAtLine(k)
		if scrolled >= 0 && scrolled != naive && scrolled != focusTarget {
			viewLine, want = k, scrolled
			break
		}
	}
	if viewLine < 0 {
		t.Fatalf("premise broken: no visible line distinguishes a scrolled hit test "+
			"from an unscrolled one (offset %d, visible %d)", offset, visible)
	}

	updated, _ := m.Update(tea.MouseClickMsg{Y: before + viewLine, Button: tea.MouseLeft})
	m = updated.(CronFormModel)
	if got := ff.FocusedIndex(m.Form()); got != want {
		t.Errorf("click on visible line %d focused field %d, want %d (scroll offset ignored?)",
			viewLine, got, want)
	}

	// A click below the visible window is ignored rather than clamped.
	m2 := m
	focusedBefore := ff.FocusedIndex(m2.Form())
	m2.Update(tea.MouseClickMsg{Y: before + visible + 2, Button: tea.MouseLeft})
	if got := ff.FocusedIndex(m2.Form()); got != focusedBefore {
		t.Errorf("a click below the visible window moved focus to %d", got)
	}
}

// TestFormResizeResyncsTheScrollOffset pins the invariant a bare
// form.WithHeight breaks: huh's Group.WithHeight resizes the viewport without
// re-running buildView, so the group keeps rendering at the offset computed for
// the previous height while scrollOffset predicts one for the new height. Every
// click on a scrolled form then lands on the wrong field.
//
// Both height-constrained views are covered, and both reachable triggers: a
// terminal resize, and — on the cron form — a submit error, whose extra block
// shrinks formHeight with no resize involved. The assertion is
// assertViewportTopMatchesOffset, which reads huh's rendered output rather than
// re-deriving the offset from the helper under test.
func TestFormResizeResyncsTheScrollOffset(t *testing.T) {
	// Each case names a NEW height at which scrollOffset's answer DIFFERS from
	// the one computed at the starting height, checked below — a resize that
	// happens to leave the offset unchanged cannot tell a resynced viewport
	// from a stale one.
	newCronForm := func(t *testing.T, height int) CronFormModel {
		t.Helper()
		m := newReadyCronFormForTest(80, height)
		initForm(t, m.Form())
		if m.FormFields().ContentHeight() <= ui.VisibleHeight(m.Form(), m.FormFields()) {
			t.Fatalf("premise broken: the cron form does not scroll at height %d", height)
		}
		return m
	}

	// requireOffsetChanged is what keeps every case below load-bearing.
	requireOffsetChanged := func(t *testing.T, label string, before, after int) {
		t.Helper()
		if before == after {
			t.Fatalf("%s: scroll offset is %d before and after; this case cannot "+
				"distinguish a resynced viewport from a stale one — repick the heights",
				label, before)
		}
	}

	for _, tt := range []struct {
		name    string
		to      int
		focused int
	}{
		// The grown height has to satisfy both guards below, which pull in
		// opposite directions. scrollOffset(5) saturates at its at-24 value for
		// every height up to 44 (the form is tall enough that the offset is
		// still clamped to max scroll), so anything in that band trips
		// requireOffsetChanged; and an offset landing inside a field's run of
		// blank rows trips assertViewportTopMatchesOffset, which needs the
		// viewport's top line to differ from the one above it. 58 clears the
		// plateau with enough headroom that adding one more field will not
		// silently push it back in.
		//
		// Shrinking is the mirror image: a mid-form field is already at max
		// scroll at 24, so only a field near the end (Enabled) has an offset
		// that shrinking can still push further down. BOS-567 compacted the
		// form — content-fit selects, a one-line Prompt — which is what moved
		// this case off field 5.
		{name: "cron form grown", to: 58, focused: 5},
		{name: "cron form shrunk", to: 18, focused: 9},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newCronForm(t, 24)
			ff := m.FormFields()
			if cmd := ff.FocusCmd(m.Form(), tt.focused); cmd == nil {
				t.Fatalf("could not focus field %d", tt.focused)
			}
			was := ff.ScrollOffset(tt.focused, ui.VisibleHeight(m.Form(), ff))
			assertViewportTopMatchesOffset(t, m.Form(), ff)

			updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: tt.to})
			m = updated.(CronFormModel)
			requireOffsetChanged(t, tt.name, was,
				ff.ScrollOffset(tt.focused, ui.VisibleHeight(m.Form(), ff)))
			assertViewportTopMatchesOffset(t, m.Form(), ff)
		})
	}

	// There is deliberately no submit-error case here. cronFormSavedMsg can only
	// arrive from handleSubmit, which is only reached with the form in
	// huh.StateCompleted; huh's Form.Update early-returns for any non-normal
	// state, so no resync is possible there — and none is needed, because a
	// completed form renders nothing. A test that reached that arm by setting
	// m.submitting directly would assert against a StateNormal form the
	// production path never produces, and would "verify" a call site that cannot
	// execute. TestCronFormSubmitErrorDisablesMouse covers what that screen
	// actually owes the user instead.

	// The repo-add cases include the worst version of the bug: growing the
	// terminal until the form no longer needs to scroll. fieldAtViewLine then
	// stops compensating altogether, so a viewport still parked at the old
	// offset mis-maps every line on the screen.
	for _, tt := range []struct {
		name          string
		to            int
		wantScrolling bool
	}{
		{name: "repo add details still scrolling", to: 34, wantScrolling: true},
		{name: "repo add details grown past scrolling", to: 40},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := repoDetailsModel(t)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			m = updated.(repos.RepoAddModel)
			initForm(t, m.Form())

			ff := m.FormFields()
			if ff.ContentHeight() <= ui.VisibleHeight(m.Form(), ff) {
				t.Fatal("premise broken: repo add details does not scroll at height 24")
			}
			if cmd := ff.FocusCmd(m.Form(), 1); cmd == nil {
				t.Fatal("could not focus field 1")
			}
			was := ff.ScrollOffset(1, ui.VisibleHeight(m.Form(), ff))
			assertViewportTopMatchesOffset(t, m.Form(), ff)

			updated, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: tt.to})
			m = updated.(repos.RepoAddModel)
			if scrolling := ff.ContentHeight() > ui.VisibleHeight(m.Form(), ff); scrolling != tt.wantScrolling {
				t.Fatalf("scrolling = %v at height %d, want %v", scrolling, tt.to, tt.wantScrolling)
			}
			now := 0
			if v := ui.VisibleHeight(m.Form(), ff); ff.ContentHeight() > v {
				now = ff.ScrollOffset(1, v)
			}
			requireOffsetChanged(t, tt.name, was, now)
			assertViewportTopMatchesOffset(t, m.Form(), ff)
		})
	}
}

// TestResizeFormDeclinesAnUnknownHeight pins resizeForm's height <= 0 guard,
// which is the difference between "leave the form alone" and "erase it".
//
// The views return 0 from formHeight while the terminal size is unknown, and a
// form built in that state is left unconstrained (huh's WithHeight ignores a
// non-positive height, so the form's own height stays 0). A bare WithHeight(0)
// is a second no-op, but resizeForm's other half is not: a zero form height is
// exactly the condition that makes huh's Form.Update WindowSizeMsg arm
// re-derive the height from the message, and a zero-height message then
// collapses the group to nothing.
func TestResizeFormDeclinesAnUnknownHeight(t *testing.T) {
	// Built before any WindowSizeMsg — the state a view is in on first render.
	m := newReadyCronFormForTest(0, 0)
	initForm(t, m.Form())
	if got := m.FormHeight(); got != 0 {
		t.Fatalf("premise broken: formHeight = %d before any WindowSizeMsg, want 0", got)
	}

	was := lipgloss.Height(m.Form().View())
	if was <= 1 {
		t.Fatalf("premise broken: the unconstrained form renders %d lines", was)
	}

	// A degenerate size: formHeight returns 0 for any terminal height <= 0.
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 0})
	m = updated.(CronFormModel)
	if got := m.FormHeight(); got != 0 {
		t.Fatalf("premise broken: formHeight = %d at a zero terminal height, want 0", got)
	}
	if got := lipgloss.Height(m.Form().View()); got != was {
		t.Errorf("the form collapsed from %d lines to %d on a zero terminal height; "+
			"resizeForm must decline a non-positive height rather than forward it to huh",
			was, got)
	}
	if m.Form().View() == "" {
		t.Error("the form disappeared entirely")
	}
}

// TestCompletedFormScreensDisableMouseAndTheClickHint covers every screen that
// keeps a non-nil *huh.Form while rendering no form at all.
//
// huh's Form.View returns "" once the form is completed (submit sets quitting),
// and two views stay on their form branch in that state: the cron view after a
// failed save, and the add-repo view for the whole RegisterRepo round-trip
// (the local-path path sets neither validating nor cloning, so every early
// return in View is skipped). On those screens there is nothing to click — yet
// lipgloss.Height("") is 1, so an ungated hit test would map the top line to
// field 0 and focus a field the user cannot see.
//
// Both halves are asserted: mouse reporting off, and the shared nav hints
// (which advertise "[click] select field") replaced by the plain action bar.
func TestCompletedFormScreensDisableMouseAndTheClickHint(t *testing.T) {
	tests := []struct {
		name string
		// build returns a model whose form is live, plus the form itself.
		build func(t *testing.T) (func() tea.View, *huh.Form, ui.FormFields, int)
	}{
		{name: "repo add details, RegisterRepo in flight", build: func(t *testing.T) (func() tea.View, *huh.Form, ui.FormFields, int) {
			t.Helper()
			m := repoDetailsModel(t)
			m.SetSize(80, 40)
			initForm(t, m.Form())
			return m.View, m.Form(), m.FormFields(), ui.LinesBefore(m.FormPrefix())
		}},
		{name: "cron form, failed save", build: func(t *testing.T) (func() tea.View, *huh.Form, ui.FormFields, int) {
			t.Helper()
			m := newReadyCronFormForTest(80, 40)
			initForm(t, m.Form())
			return m.View, m.Form(), m.FormFields(), ui.LinesBefore(m.FormPrefix())
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view, form, fields, before := tt.build(t)

			if got := view().MouseMode; got != tea.MouseModeCellMotion {
				t.Fatalf("premise broken: the live form reports %v, want MouseModeCellMotion", got)
			}

			// Complete the form the way a user does: submit.
			if cmd := form.NextGroup(); cmd != nil {
				cmd()
			}
			if form.View() != "" {
				t.Fatal("premise broken: huh still renders a completed form")
			}

			v := view()
			if v.Content == "" {
				t.Fatal("the screen rendered nothing; the assertions below would be vacuous")
			}
			if got := v.MouseMode; got != tea.MouseModeNone {
				t.Errorf("MouseMode = %v with no form on screen, want MouseModeNone", got)
			}
			for _, hint := range ui.FormNavHints() {
				if strings.Contains(v.Content, hint) {
					t.Errorf("the action bar still advertises %q with no fields on screen", hint)
				}
			}
			// And the hit test itself must decline, rather than leaning on huh's
			// Form.Update happening to ignore a completed form.
			if got := fields.FieldAtViewLine(form, 0); got != -1 {
				t.Errorf("fieldAtViewLine(0) = %d on a form that renders nothing, want -1", got)
			}
			if _, handled := fields.HandleMouse(
				tea.MouseClickMsg{Y: before, Button: tea.MouseLeft}, form, before); !handled {
				t.Error("handleMouse did not swallow a click on a form-less screen")
			}
		})
	}
}

// TestCronFormSubmitErrorDisablesMouse covers the one screen in this package
// that keeps a non-nil *huh.Form while rendering no form at all.
//
// A failed save leaves the cron view on its form branch with the form in
// huh.StateCompleted, and huh's Form.View returns "" once quitting — so the
// user sees a title, an error and an action bar, and nothing to click.
// "m.Form() != nil" is therefore not the same question as "a form is on screen",
// and the acceptance criterion scopes MouseModeCellMotion to the latter.
//
// It reaches that state by driving a REAL submit (NextGroup on the last field),
// not by setting m.submitting, because only the real path produces the
// completed form the branch actually sees.
func TestCronFormSubmitErrorDisablesMouse(t *testing.T) {
	m := newReadyCronFormForTest(80, 40)
	initForm(t, m.Form())

	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("premise broken: the live cron form reports %v, want MouseModeCellMotion", got)
	}

	// Complete the form the way the user does: submit from the last field.
	if cmd := m.Form().NextGroup(); cmd != nil {
		cmd()
	}
	if m.Form().State != huh.StateCompleted {
		t.Fatalf("premise broken: NextGroup left the form in state %v, want StateCompleted",
			m.Form().State)
	}
	if m.Form().View() != "" {
		t.Fatal("premise broken: huh still renders a completed form; this screen " +
			"may have a clickable form after all")
	}

	updated, _ := m.Update(cron.FormSavedMsg{Err: errors.New("boom")})
	m = updated.(CronFormModel)

	v := m.View()
	if v.Content == "" {
		t.Fatal("the submit-error screen rendered nothing; the assertion below would be vacuous")
	}
	if !strings.Contains(v.Content, "boom") {
		t.Fatalf("the submit-error screen does not show the error:\n%s", v.Content)
	}
	if got := v.MouseMode; got != tea.MouseModeNone {
		t.Errorf("MouseMode = %v on the submit-error screen, want MouseModeNone: "+
			"the form renders nothing there, so there is no field to click", got)
	}
}

// TestFormViewsSingleGroupInvariant asserts every form this package builds has
// exactly one huh group. The hit-test arithmetic in form_mouse.go treats a form
// as one flat vertical stack of fields; a second group would silently mis-map
// every click, so it must fail here first.
func TestFormViewsSingleGroupInvariant(t *testing.T) {
	for _, tt := range formClickCases() {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.build(t)
			assertSingleGroup(t, s.form)
			assertFormStartsAtFirstField(t, s.form, s.fields)
		})
	}
}

// --- MouseMode scoping ------------------------------------------------------

// TestFormViewsEnableMouseOnFormPhases asserts the affordance is on exactly
// where it works. A screenshot cannot show a terminal mode, so this is the only
// evidence for the "form screens only" scoping.
func TestFormViewsEnableMouseOnFormPhases(t *testing.T) {
	for _, tt := range formClickCases() {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.build(t).view().MouseMode; got != tea.MouseModeCellMotion {
				t.Errorf("form phase MouseMode = %v, want MouseModeCellMotion", got)
			}
		})
	}
}

// TestNonFormPhasesDisableMouse walks every non-form phase of the four form
// views. Each must report MouseModeNone so click-drag text selection is
// untouched outside the form screens.
func TestNonFormPhasesDisableMouse(t *testing.T) {
	tests := []struct {
		name string
		view func(t *testing.T) tea.View
	}{
		{"cron form loading", func(t *testing.T) tea.View {
			m := NewCronFormModel(nil, context.Background())
			m.SetSize(80, 40)

			return m.View()
		}},
		{"cron form saving", func(t *testing.T) tea.View {
			m := submittingCronFormForTest(80, 40)
			return m.View()
		}},
		{"bug report submitting", func(t *testing.T) tea.View {
			m := NewBugReportModel(nil, context.Background(), nil, ui.ViewHome, nil, nil)
			m.Form().State = huh.StateCompleted
			updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = updated.(BugReportModel)
			return m.View()
		}},
		{"bug report success", func(t *testing.T) tea.View {
			m := NewBugReportModel(nil, context.Background(), nil, ui.ViewHome, nil, nil)
			updated, _ := m.Update(bugreport.SubmitMsg{})
			m = updated.(BugReportModel)
			return m.View()
		}},
		{"bug report error", func(t *testing.T) tea.View {
			m := NewBugReportModel(nil, context.Background(), nil, ui.ViewHome, nil, nil)
			updated, _ := m.Update(bugreport.SubmitMsg{Err: errors.New("boom")})
			m = updated.(BugReportModel)

			return m.View()
		}},
		{"new session repo select", func(t *testing.T) tea.View {
			m := readyNewSession(t)
			return m.View()
		}},
		{"new session creating", func(t *testing.T) tea.View {
			m := readyNewSessionForm(t)
			m.Form().State = huh.StateCompleted
			next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			return next.View()
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := tt.view(t)
			if v.Content == "" {
				t.Fatal("phase rendered no content; the assertion below would be vacuous")
			}
			if got := v.MouseMode; got != tea.MouseModeNone {
				t.Errorf("MouseMode = %v on a non-form phase, want MouseModeNone", got)
			}
		})
	}
}
