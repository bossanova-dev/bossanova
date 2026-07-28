package views

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// formViewFiles are the source files holding a view that renders a huh form.
// TestEveryFormFileIsCovered keeps this list honest against the package source,
// so the hand-written table below cannot silently miss a newly added form view.
var formViewFiles = []string{
	"repo_add.go",
	"newsession_create.go",
	"cron_form.go",
	"bugreport.go",
}

// TestEveryFormViewRendersNavigationHints is the regression guard for BOS-509:
// every view that renders a huh form suppresses huh's own keybinding footer
// with .WithShowHelp(false), so each one must supply the boss action bar with
// the shared field-movement hints.
//
// The table is hand-maintained (it covers view models rather than parsing
// source, so it also proves the hints survive each view's real render path —
// banner, padding, phases). TestEveryFormFileIsCovered is what makes it a real
// guard: it fails when a form view lands in a file this table does not cover.
func TestEveryFormViewRendersNavigationHints(t *testing.T) {
	tests := []struct {
		name string
		view func(t *testing.T) string
	}{
		{name: "repo add input form", view: repoAddInputFormView},
		{name: "repo add details form", view: repoAddDetailsFormView},
		{name: "new session form", view: newSessionFormView},
		{name: "cron form", view: cronFormView},
		{name: "bug report form", view: bugReportFormView},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := tt.view(t)
			for _, want := range formNavHints() {
				if !strings.Contains(view, want) {
					t.Errorf("form view missing navigation hint %q; view:\n%s", want, view)
				}
			}
			if strings.Contains(view, "[enter] next field") {
				t.Errorf("form view claims enter moves to the next field; view:\n%s", view)
			}
			assertActionBarFitsEightyColumns(t, view)
		})
	}
}

// TestEveryFormFileIsCovered walks the package source for the marker every huh
// form in this package carries — a call to newBossForm, which applies
// .WithShowHelp(false) and so suppresses huh's own keybinding footer, obliging
// the view to render the boss action bar itself — and asserts the set of files
// holding one matches formViewFiles. A sixth form view added tomorrow fails
// here, which is the prompt to add it to the table above.
//
// The marker is the *call*, not the .WithShowHelp selector it used to be
// (BOS-567 moved that selector into the shared newBossForm constructor, where
// it now appears exactly once and says nothing about which views render a
// form). formfields.go defines newBossForm but never calls it, so it correctly
// stays out of the found set.
func TestEveryFormFileIsCovered(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Parse rather than grep: the marker is discussed in doc comments too,
		// and only a real call should count.
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "newBossForm" {
				found[name] = true
			}
			return true
		})
	}

	covered := map[string]bool{}
	for _, name := range formViewFiles {
		covered[name] = true
		if !found[name] {
			t.Errorf("formViewFiles lists %s, but it no longer builds a huh form; drop it from the table", name)
		}
	}
	for name := range found {
		if !covered[name] {
			t.Errorf("%s builds a huh form but is not covered by TestEveryFormViewRendersNavigationHints; "+
				"render its action bar with formActionBar and add it to formViewFiles", name)
		}
	}
}

func repoAddInputFormView(t *testing.T) string {
	t.Helper()
	m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
	m.phase = repoAddPhaseInput
	m.sourceMode = sourceModeOpen
	m.buildInputForm()
	return repoAddFormView(t, m)
}

func repoAddDetailsFormView(t *testing.T) string {
	t.Helper()
	m := NewRepoAddModel(&repoAddStubClient{}, context.Background())
	m.phase = repoAddPhaseDetails
	m.sourceMode = sourceModeOpen
	m.buildDetailsForm()
	return repoAddFormView(t, m)
}

func repoAddFormView(t *testing.T, m RepoAddModel) string {
	t.Helper()
	if cmd := m.form.Init(); cmd != nil {
		cmd()
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	return updated.(RepoAddModel).View().Content
}

func newSessionFormView(t *testing.T) string {
	t.Helper()
	sc := &stubClient{repos: oneRepo()}
	m := NewNewSessionModel(sc, context.Background())
	m = sendMsg(t, m, reposMsg{repos: sc.repos})
	m.selectedType = sessionTypeNewPR
	m.phase = newSessionPhaseForm
	m.buildForm()
	return m.View().Content
}

func cronFormView(t *testing.T) string {
	t.Helper()
	m := CronFormModel{
		ctx:         context.Background(),
		repos:       []*pb.Repo{{Id: "a", DisplayName: "repo-a"}},
		reposReady:  true,
		agentsReady: true,
		width:       80,
		height:      40,
	}
	m.buildForm()
	return m.View().Content
}

func bugReportFormView(t *testing.T) string {
	t.Helper()
	return NewBugReportModel(nil, context.Background(), nil, ViewHome, nil, nil).View().Content
}

// assertActionBarFitsEightyColumns guards the width budget that the height
// budget depends on. The three shared nav hints cost 70 columns rendered, which
// is why formActionBar puts them on a line of their own — folded together with
// a verbose submit verb the bar reaches 101. That matters beyond aesthetics:
// repoAddChrome (and cronFormChrome) budget exactly formActionBarLines of bar
// text, so a line that wraps on an 80-column terminal silently eats a line the
// budget reserved and clips the bar these views exist to show.
//
// Both of the bar's text lines are checked: the nav-hint line, and the caller's
// action line immediately below it.
func assertActionBarFitsEightyColumns(t *testing.T, view string) {
	t.Helper()
	const maxWidth = 80
	lines := strings.Split(view, "\n")
	checked := 0
	for i, line := range lines {
		if !strings.Contains(line, formNavHints()[0]) {
			continue
		}
		for _, barLine := range lines[i:min(i+formActionBarLines, len(lines))] {
			checked++
			if got := lipgloss.Width(barLine); got > maxWidth {
				t.Errorf("action bar line is %d columns, want <= %d; it will wrap and clip:\n%s",
					got, maxWidth, barLine)
			}
		}
	}
	if checked < formActionBarLines {
		t.Errorf("only found %d action-bar lines to check, want at least %d; "+
			"has the bar stopped rendering its nav hints?", checked, formActionBarLines)
	}
}

// bossFieldConstructors maps each bare huh constructor this package must not
// call directly to the shared wrapper that replaces it. The wrappers bake in
// the house style — the shared theme and width, left-aligned confirm buttons,
// content-fit select heights, content-fit text rows — so a call site that skips
// one silently reintroduces exactly the drift BOS-567 removed.
//
// NewForm is in the list because TestEveryFormFileIsCovered keys on a
// newBossForm call: without this entry a view that hand-rolled huh.NewForm
// would be invisible to *both* gates — it would not be a form file that test
// knows about, and it would carry no banned field constructor either.
var bossFieldConstructors = map[string]string{
	"NewForm":        "newBossForm",
	"NewConfirm":     "bossConfirm",
	"NewText":        "bossText(value)",
	"NewSelect":      "bossSelect[T](len(options), headerLines)",
	"NewMultiSelect": "bossMultiSelect[T](len(options), headerLines)",
}

// TestEveryTextHostSizesAheadOfTheMessage is the mechanical guard for the one
// part of the Text contract that fails silently and invisibly.
//
// A host that only re-sizes its box *after* form.Update looks correct and
// tests green at every length but one: the moment a line reaches the wrap
// width the textarea scrolls to find its cursor, and it never scrolls back, so
// the opening of the prompt is gone for the rest of the edit. The fix is to
// grow the field for bossTextPendingInsert(msg) before huh sees the message —
// so any file that builds a bossText must name that helper.
//
// Deliberately a source check rather than a behavioural one: the behaviour is
// pinned on the bug-report modal (TestBugReportView_CommentSurvivesTheWrapBoundary)
// and the cron form (TestCronFormView_PromptSurvivesAPasteAtTheWrapBoundary),
// and this is what makes a *third* host fail loudly instead of quietly.
func TestEveryTextHostSizesAheadOfTheMessage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "formfields.go" {
			continue // where both helpers are defined
		}

		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		called := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if ident, ok := call.Fun.(*ast.Ident); ok {
					called[ident.Name] = true
				}
			}
			return true
		})
		if called["bossText"] && !called["bossTextPendingInsert"] {
			t.Errorf("%s builds a bossText but never calls bossTextPendingInsert; re-size the field with "+
				"it before form.Update, or the box scrolls off its own first line once a line fills the width", name)
		}
	}
}

// TestEveryConfirmIsLeftAligned is the mechanical half of BOS-567 ask #4: it
// walks the package's own non-test sources for a bare huh field constructor
// outside formfields.go, so a fifth form added tomorrow cannot quietly go back
// to centred confirm buttons or a fixed-height select.
//
// Test files are deliberately skipped: form_mouse_test.go constructs a huh form
// inline on purpose, to exercise the raw huh path the mouse hit test replicates.
func TestEveryConfirmIsLeftAligned(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "formfields.go" {
			continue // the one file allowed to reach for huh directly
		}

		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "huh" {
				return true
			}
			if want, banned := bossFieldConstructors[sel.Sel.Name]; banned {
				t.Errorf("%s calls huh.%s directly; use %s instead so it picks up the shared form style",
					name, sel.Sel.Name, want)
			}
			return true
		})
	}
}
