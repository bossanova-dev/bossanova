package views

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// TestRenderCheckboxLabel pins the one checkbox glyph pair the TUI renders and
// that the label text survives verbatim — the helper is a glyph plus a space,
// not a place to restyle a label.
func TestRenderCheckboxLabel(t *testing.T) {
	tests := []struct {
		name    string
		checked bool
		label   string
		want    string
	}{
		{name: "checked", checked: true, label: "Automatic repair", want: "[x] Automatic repair"},
		{name: "unchecked", checked: false, label: "Automatic repair", want: "[ ] Automatic repair"},
		{name: "checked empty label", checked: true, label: "", want: "[x] "},
		{name: "label with brackets survives", checked: false, label: "Use 'dangerous mode' [beta]", want: "[ ] Use 'dangerous mode' [beta]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderCheckboxLabel(tt.checked, tt.label); got != tt.want {
				t.Errorf("renderCheckboxLabel(%v, %q) = %q, want %q", tt.checked, tt.label, got, tt.want)
			}
		})
	}
}

// TestBossHuhTheme_CheckboxPrefixesMatchBothFocusStates is the regression guard
// for BOS-726: huh's ThemeBase renders a MultiSelect's selected option as
// "[•] ", which is the dot the add-repo Automation field showed.
//
// Both focus states are asserted because ThemeBase does `t.Blurred = t.Focused`
// at the end of its own construction — i.e. *before* bossHuhTheme is handed the
// value — so an override that sets only Focused looks right in a focused
// screenshot and reverts to "[•]" the moment focus moves off the field. That is
// the identical trap the Card assignments already document. Both isDark
// resolutions are asserted for the same reason
// TestBossFormWrapWidth_MatchesBothFocusStates does: the theme is a func of
// isDark, so one resolution proves nothing about the other.
//
// The negative (`•` appears nowhere) matters more than the positive here: a
// regression is a silent inherit from ThemeBase, never a compile error.
func TestBossHuhTheme_CheckboxPrefixesMatchBothFocusStates(t *testing.T) {
	for _, isDark := range []bool{true, false} {
		t.Run(fmt.Sprintf("isDark=%v", isDark), func(t *testing.T) {
			styles := bossHuhTheme().Theme(isDark)
			states := map[string]struct {
				selected   lipgloss.Style
				unselected lipgloss.Style
			}{
				"focused": {styles.Focused.SelectedPrefix, styles.Focused.UnselectedPrefix},
				"blurred": {styles.Blurred.SelectedPrefix, styles.Blurred.UnselectedPrefix},
			}

			for state, s := range states {
				selected, unselected := s.selected.String(), s.unselected.String()
				if !strings.Contains(selected, checkboxChecked) {
					t.Errorf("%s SelectedPrefix = %q, want it to contain %q", state, selected, checkboxChecked)
				}
				if !strings.Contains(unselected, checkboxUnchecked) {
					t.Errorf("%s UnselectedPrefix = %q, want it to contain %q", state, unselected, checkboxUnchecked)
				}
				for _, prefix := range []string{selected, unselected} {
					if strings.Contains(prefix, "•") {
						t.Errorf("%s prefix %q still carries huh ThemeBase's bullet; the override did not reach this state", state, prefix)
					}
				}
				// A prefix width change would shift every option label in the
				// multi-select sideways, so measure it rather than trusting
				// that "[x] " and "[•] " look the same length.
				if got, want := lipgloss.Width(selected), lipgloss.Width(unselected); got != want {
					t.Errorf("%s SelectedPrefix is %d columns and UnselectedPrefix is %d; the option labels will not line up", state, got, want)
				}
			}
		})
	}
}

// TestBossHuhTheme_PrefixIsExactlyTheHandRolledPrefix is the cross-idiom
// assertion the two tests above it cannot make.
//
// TestBossHuhTheme_CheckboxPrefixesMatchBothFocusStates compares huh's two
// prefixes to each other, and TestRenderCheckboxLabel checks the hand-rolled
// renderer alone; both still pass if huh's prefix carries a different separator
// from renderCheckboxLabel's — the glyphs agree and the fourth column does not,
// shifting every multi-select label one place against the settings rows.
//
// The two form-render tests below do fail on that, because the space sits
// inside their "[x] Auto-merge Dependabot PRs" literal — so this is not the
// only guard, and trimming those two as glyph-redundant would remove the
// behavioural half. What this one adds is the direct statement: desynchronise
// the separators and it names the two prefixes and their difference, where the
// form tests report a missing row and leave the reader to find the column.
//
// renderCheckboxLabel with an empty label IS the prefix, which is why the
// truth table above pins "[x] " as a case in its own right.
func TestBossHuhTheme_PrefixIsExactlyTheHandRolledPrefix(t *testing.T) {
	for _, isDark := range []bool{true, false} {
		t.Run(fmt.Sprintf("isDark=%v", isDark), func(t *testing.T) {
			styles := bossHuhTheme().Theme(isDark)
			for _, tc := range []struct {
				name    string
				checked bool
				prefix  lipgloss.Style
			}{
				{name: "selected", checked: true, prefix: styles.Focused.SelectedPrefix},
				{name: "unselected", checked: false, prefix: styles.Focused.UnselectedPrefix},
			} {
				want := renderCheckboxLabel(tc.checked, "")
				if got := tc.prefix.String(); got != want {
					t.Errorf("%s: huh prefix is %q but renderCheckboxLabel emits %q; the multi-select and the "+
						"hand-rolled rows would not line up", tc.name, got, want)
				}
			}
		})
	}
}

// TestRepoAddDetailsForm_AutomationRendersCheckboxGlyphs is the screen from the
// BOS-726 report: the add-repository details form's Automation multi-select.
// defaultRepoAutomation() turns two of the three options on, so one render
// shows both glyph states of the fixed prefix.
func TestRepoAddDetailsForm_AutomationRendersCheckboxGlyphs(t *testing.T) {
	// Styles are stripped because huh renders the prefix and the option label
	// as two separately styled spans (field_multiselect.go joins
	// SelectedPrefix.String() to SelectedOption.Render(key)), so a selected
	// option carries an escape sequence between the glyph and its text.
	view := stripANSI(repoAddDetailsFormView(t))

	for _, want := range []string{
		"[x] Auto-merge Dependabot PRs",
		"[x] Automatic repair",
		"[ ] Mark ready for review when checks pass",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("add-repo details form missing %q; view:\n%s", want, view)
		}
	}
	if strings.Contains(view, "•") {
		t.Errorf("add-repo details form still renders huh's bullet prefix; view:\n%s", view)
	}
}

// TestRepoAddDetailsForm_AutomationKeepsCheckboxGlyphsWhenBlurred is the
// behavioural half of the Blurred assertion in
// TestBossHuhTheme_CheckboxPrefixesMatchBothFocusStates: it walks focus off the
// Automation field entirely and re-reads the rendered form.
//
// The theme test alone would pass if huh ever stopped reading Blurred for a
// multi-select; this one only passes if the glyph a user actually sees survives
// losing focus, which is the state a focused screenshot of the bug could never
// have shown.
func TestRepoAddDetailsForm_AutomationKeepsCheckboxGlyphsWhenBlurred(t *testing.T) {
	m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.buildDetailsForm()
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	form := updated.(RepoAddModel)

	// Four tabs walk Name → Setup command → Merge strategy → Automation →
	// Confirm, leaving the multi-select blurred but still on screen. huh
	// advances the field through a returned command, so each one has to be run
	// and fed back before the move lands.
	const tabsToConfirmButtons = 4
	for range tabsToConfirmButtons {
		next, cmd := form.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		form = next.(RepoAddModel)
		if cmd == nil {
			t.Fatal("tab produced no command; huh did not schedule a field advance")
		}
		next, _ = form.Update(cmd())
		form = next.(RepoAddModel)
	}

	view := stripANSI(form.View().Content)
	// The focus gutter on the confirm row is what proves focus actually left
	// Automation — without it this test would re-assert the focused rendering.
	if !regexp.MustCompile(`┃ +Add Repository`).MatchString(view) {
		t.Fatalf("focus is not on the confirm buttons, so the Automation field is not blurred; view:\n%s", view)
	}
	for _, want := range []string{
		"[x] Auto-merge Dependabot PRs",
		"[x] Automatic repair",
		"[ ] Mark ready for review when checks pass",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("blurred Automation multi-select missing %q; view:\n%s", want, view)
		}
	}
	if strings.Contains(view, "•") {
		t.Errorf("blurred Automation multi-select fell back to huh's bullet prefix; view:\n%s", view)
	}
}

// TestGeneralSettings_BothTracingTogglesAreCheckboxes pins the pair of toggles
// the glyph sweep originally walked past. settingsRowKindEventTracing and
// settingsRowKindErrorTracking are declared "toggle" at their enum, flip a
// plain bool on the same keypress, both read "Enable …", and rebuildRows emits them as
// adjacent lines — but only the first rendered "[x] …"; the second rendered
// "…: ON", so one screen showed two affordances for two indistinguishable
// toggles.
//
// Both boolean states are driven because the OFF rendering is where the old
// value row and the checkbox diverge most ("…: OFF" versus "[ ] …"), and the
// negative assertion is the load-bearing half: re-introducing the value row
// would still satisfy a bare Contains on the label.
func TestGeneralSettings_BothTracingTogglesAreCheckboxes(t *testing.T) {
	const (
		eventLabel = "Enable event tracing (for debugging problems)"
		errorLabel = "Enable error tracking (sends panics to Sentry)"
	)

	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			withTempConfigHome(t)
			m := NewGeneralSettingsModel(&settingsAgentStub{stubClient: &stubClient{}}, context.Background())
			m.settings.EventTracingEnabled = enabled
			m.settings.ErrorTrackingEnabled = enabled
			m.rebuildRows()
			view := stripANSI(m.View().Content)

			for _, label := range []string{eventLabel, errorLabel} {
				want := renderCheckboxLabel(enabled, label)
				if !strings.Contains(view, want) {
					t.Errorf("general settings missing %q; view:\n%s", want, view)
				}
				// The exact shape the error-tracking row used to render. Pinned
				// as a negative so a revert to the value row fails here rather
				// than passing a label-only Contains.
				for _, stale := range []string{label + ": ON", label + ": OFF"} {
					if strings.Contains(view, stale) {
						t.Errorf("general settings still renders %q as a value row (%q), not a checkbox", label, stale)
					}
				}
			}
		})
	}
}

// checkboxRatchetExemptFile is the one file allowed to write the checkbox
// glyphs: renderCheckboxLabel and the huh theme prefixes both build from the
// constants defined there.
const checkboxRatchetExemptFile = "formfields.go"

// checkboxLiteralViolations reports the hand-rolled checkbox idioms in one
// parsed source file. Two shapes are banned, and the pair is deliberately
// narrow — see TestCheckboxGlyphRatchet_IsNotVacuous for the fixtures that pin
// both the catches and the misses:
//
//  1. a string literal containing "[x]" or "[ ]" — a checkbox glyph written out
//     rather than taken from the constants.
//  2. a fmt.Sprintf whose format literal holds "[%s]" *and* which is passed a
//     `check`-named argument — the exact `fmt.Sprintf("[%s] %s", check, label)`
//     idiom the nine call sites used.
//
// The `check` argument is what makes rule 2 safe. The format literal alone
// over-reaches: newsession_create.go renders a tracker id with the very same
// fmt.Sprintf("[%s] %s", …), and flagging that would force a lie into the
// source to appease a gate.
func checkboxLiteralViolations(file *ast.File) []string {
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(n.Value)
			if err != nil {
				return true
			}
			for _, glyph := range []string{checkboxChecked, checkboxUnchecked} {
				if strings.Contains(val, glyph) {
					found = append(found, fmt.Sprintf("string literal %s writes the checkbox glyph %q", n.Value, glyph))
				}
			}
		case *ast.CallExpr:
			if !isSprintfWithCheckArg(n) {
				return true
			}
			found = append(found, "fmt.Sprintf builds a checkbox row from a `check` value")
		}
		return true
	})
	return found
}

// isSprintfWithCheckArg reports whether call is the hand-rolled checkbox
// idiom: fmt.Sprintf over a "[%s]"-shaped format, handed a `check` value.
func isSprintfWithCheckArg(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Sprintf" {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
		return false
	}
	if len(call.Args) == 0 {
		return false
	}
	format, ok := call.Args[0].(*ast.BasicLit)
	if !ok || format.Kind != token.STRING {
		return false
	}
	val, err := strconv.Unquote(format.Value)
	if err != nil || !strings.Contains(val, "[%s]") {
		return false
	}
	for _, arg := range call.Args[1:] {
		if ident, ok := arg.(*ast.Ident); ok && strings.Contains(strings.ToLower(ident.Name), "check") {
			return true
		}
	}
	return false
}

// TestCheckboxGlyphRatchet_PackageSourcesAreClean is the BOS-726 ratchet: nine
// call sites across general_settings.go, onboarding.go and repo_settings.go
// each wrote their own "[x]" / "[ ]", which is how the huh multi-select came to
// disagree with all of them without anything failing. renderCheckboxLabel is
// now the single renderer, so a tenth site must fail here rather than ship a
// second style.
//
// Test files are deliberately in the exempt set: the existing suites assert the
// rendered "[x] Label" strings on purpose, and those assertions are the pinning
// evidence that this refactor changed no screen.
func TestCheckboxGlyphRatchet_PackageSourcesAreClean(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == checkboxRatchetExemptFile {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, v := range checkboxLiteralViolations(file) {
			t.Errorf("%s: %s; render checkbox rows with renderCheckboxLabel(checked, label) so the glyph "+
				"keeps a single definition in %s", name, v, checkboxRatchetExemptFile)
		}
	}
	if checked == 0 {
		t.Fatal("walked no package sources; the ratchet would pass vacuously")
	}
}

// checkboxRatchetViolatingFixture is a source file holding both banned shapes:
// the fmt.Sprintf-with-`check` idiom the nine sites used, and a written-out
// glyph literal.
const checkboxRatchetViolatingFixture = `package views

import "fmt"

func renderRow(checked bool, label string) string {
	check := " "
	if checked {
		check = "x"
	}
	return fmt.Sprintf("[%s] %s", check, label)
}

func renderHeader() string {
	return "[x] Enabled"
}
`

// checkboxRatchetAllowedFixture holds the near-misses the gate must not flag:
// newsession_create.go's tracker-id prefix (the identical format literal, no
// checkbox in sight) and the bracketed action-bar labels every view renders.
const checkboxRatchetAllowedFixture = `package views

import "fmt"

func issueTitle(id, title string) string {
	return fmt.Sprintf("[%s] %s", id, title)
}

func actionBar() []string {
	return []string{"[enter] save", "[esc] back", "[space] toggle"}
}
`

// TestCheckboxGlyphRatchet_IsNotVacuous proves the gate above can fail. A
// literal matcher that matched nothing would pass the package walk for exactly
// the same reason a correct one does, so the detector is pointed at a fixture
// that violates it and at one that only looks like it does.
func TestCheckboxGlyphRatchet_IsNotVacuous(t *testing.T) {
	tests := []struct {
		name         string
		src          string
		wantViolated bool
	}{
		{name: "hand-rolled checkbox row", src: checkboxRatchetViolatingFixture, wantViolated: true},
		{name: "tracker id prefix and action bar labels", src: checkboxRatchetAllowedFixture, wantViolated: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			got := checkboxLiteralViolations(file)
			if tt.wantViolated && len(got) == 0 {
				t.Errorf("the ratchet found nothing in a fixture that violates it, so it cannot fail on real "+
					"source either; fixture:\n%s", tt.src)
			}
			if !tt.wantViolated && len(got) != 0 {
				t.Errorf("the ratchet flagged %v in a fixture it must leave alone; fixture:\n%s", got, tt.src)
			}
		})
	}
}
