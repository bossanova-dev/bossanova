package views

// Structural guards for the two View switches that the exhaustive linter does
// NOT cover, and for the enter<View> extractions (BOS-529).
//
// delegateToActiveView is lint-guarded: it has no default arm and no waiver, so
// `.golangci.yml`'s exhaustive check fails the build when a new View constant
// is added without a delegation arm. The other two switches over the full enum
// have no such protection, for different reasons:
//
//   - renderActiveView keeps a default arm ("Unknown view"), and
//     default-signifies-exhaustive turns that into an opt-out.
//   - handleSwitchView carries an explicit waiver, because two constants are
//     genuinely unreachable by a switchViewMsg: ViewBugReport (pushed by ctrl+g)
//     and ViewOnboarding (entered only via SetInitialView). The waiver's own
//     reason comment names only the first; the notRoutable table below is the
//     complete list.
//
// The tests below stand in for the missing lint coverage on both.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// allViewConstants is the enum, written out. Go has no reflection over consts,
// so the list cannot derive itself at runtime — but
// TestAllViewConstantsAreListed parses the package source to prove it is
// complete, so "hand-written" does not mean "unverified".
var allViewConstants = []struct {
	view View
	name string
}{
	{ViewHome, "ViewHome"},
	{ViewNewSession, "ViewNewSession"},
	{ViewAttach, "ViewAttach"},
	{ViewChatPicker, "ViewChatPicker"},
	{ViewRepoAdd, "ViewRepoAdd"},
	{ViewRepoList, "ViewRepoList"},
	{ViewRepoSettings, "ViewRepoSettings"},
	{ViewTrash, "ViewTrash"},
	{ViewSettings, "ViewSettings"},
	{ViewSessionSettings, "ViewSessionSettings"},
	{ViewLogin, "ViewLogin"},
	{ViewBugReport, "ViewBugReport"},
	{ViewCron, "ViewCron"},
	{ViewCronForm, "ViewCronForm"},
	{ViewOnboarding, "ViewOnboarding"},
	{ViewAccounts, "ViewAccounts"},
	{ViewAccountEdit, "ViewAccountEdit"},
	{ViewAccountRegister, "ViewAccountRegister"},
	{ViewGeneralSettings, "ViewGeneralSettings"},
}

// TestAllViewConstantsAreListed makes allViewConstants a trustworthy stand-in
// for the enum, in two steps.
//
// First, every entry must sit at its own iota value, contiguously from zero —
// that is what lets the guards below iterate the list as if it were the enum,
// and it catches a constant inserted mid-block (which shifts every later value)
// without the list being updated.
//
// Second, and the reason this test earns its keep: it parses the package's own
// source for the `View` const block and asserts the list covers it. Without
// that, the whole file is blind to the exact scenario it exists for — a 20th
// constant appended to the enum and never listed here would leave every guard
// below green while covering nothing. The package already does source parsing
// for this class of drift (see TestEveryFormFileIsCovered), and BUILD.bazel
// already ships the non-test sources as `data`, so this costs nothing new.
//
// The const block is located by scanning rather than by filename so that moving
// or renaming the file holding the enum does not silently disable the check.
func TestAllViewConstantsAreListed(t *testing.T) {
	for i, tc := range allViewConstants {
		if int(tc.view) != i {
			t.Fatalf("allViewConstants[%d] is %s (= %d); the list must stay in iota order "+
				"and contiguous, because the guards below iterate it as the enum",
				i, tc.name, int(tc.view))
		}
	}

	declared := viewConstantNamesFromSource(t)
	if len(declared) == 0 {
		t.Fatal("found no `View` const block in the package source; this test can no " +
			"longer verify the list and must be repaired, not deleted")
	}

	listed := make(map[string]bool, len(allViewConstants))
	for _, tc := range allViewConstants {
		listed[tc.name] = true
	}
	for _, name := range declared {
		if !listed[name] {
			t.Errorf("View constant %s is declared in the enum but missing from "+
				"allViewConstants.\n"+
				"Every guard in this file iterates that list, so an unlisted constant is "+
				"covered by nothing: it would not be checked for a renderActiveView arm or "+
				"a handleSwitchView arm, and both of those switches are outside the "+
				"exhaustive linter's reach. Add it to allViewConstants, then to the "+
				"`constructed` or `notRoutable` table in the switch-view guard.", name)
		}
	}
	if len(declared) != len(allViewConstants) {
		t.Errorf("enum declares %d View constants, allViewConstants lists %d",
			len(declared), len(allViewConstants))
	}
}

// viewConstantNamesFromSource returns the names in the package's `View` const
// block, read from the source files themselves.
func viewConstantNamesFromSource(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST || len(gen.Specs) == 0 {
				continue
			}
			// Only the first spec in an iota block carries the type.
			first, ok := gen.Specs[0].(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := first.Type.(*ast.Ident)
			if !ok || ident.Name != "View" {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, n := range vs.Names {
					names = append(names, n.Name)
				}
			}
		}
	}
	return names
}

// TestRenderActiveViewHandlesEveryViewConstant stands in for the exhaustive lint
// coverage renderActiveView gives up by keeping a default arm.
//
// A View wired into delegateToActiveView (which the linter forces) but missed in
// renderActiveView receives messages and updates its model correctly while
// rendering the literal string "Unknown view" — a fully functional screen that
// displays nothing. The linter cannot see it, so this does.
//
// It iterates the hand-written allViewConstants, which
// TestAllViewConstantsAreListed proves complete against the source, so a newly
// added constant is covered here too rather than silently skipped.
//
// Kills: deleting any arm from renderActiveView's switch.
func TestRenderActiveViewHandlesEveryViewConstant(t *testing.T) {
	for _, tc := range allViewConstants {
		t.Run(tc.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.activeView = tc.view

			v := a.renderActiveView()

			if strings.Contains(v.Content, "Unknown view") {
				t.Fatalf("%s rendered the default arm.\n"+
					"renderActiveView is missing a case for this View. Because the switch "+
					"keeps a default arm, `exhaustive` does not flag the omission: the view "+
					"routes and updates normally and simply renders nothing.", tc.name)
			}
		})
	}
}

// TestEnterViewMutationsLandOnTheReturnedApp pins that each enter<View> method's
// state actually reaches the App that handleSwitchView returns.
//
// The enter<View> methods have POINTER receivers and are called on
// handleSwitchView's value-receiver copy, so every one of them depends on that
// copy being the thing returned. Flipping an enter method to a value receiver,
// or reordering the return so `a` is read before the call, discards the whole
// construction silently: the view switches, renders empty, and no test that
// merely checks activeView notices.
//
// Only two of the six had a local witness before this test. The others were
// covered incidentally or not at all — notably TestSwitchToNewSessionSeedsHeight
// cannot see a lost enterNewSession, because it seeds width/height via a
// WindowSizeMsg first and so passes even when the fresh construction is thrown
// away. This test therefore seeds ONLY App.width/height and leaves every target
// sub-model zeroed, so the asserted value can only come from the enter method.
//
// Kills: value receivers on any enter<View>; `return a, a.enterX()` evaluated
// operand-first; any enter method that stops seeding width.
func TestEnterViewMutationsLandOnTheReturnedApp(t *testing.T) {
	const (
		width  = 123
		height = 45
	)

	tests := []struct {
		name    string
		msg     switchViewMsg
		widthOf func(App) int
	}{
		{
			name:    "enterNewSession",
			msg:     switchViewMsg{view: ViewNewSession},
			widthOf: func(a App) int { return a.newSession.width },
		},
		{
			name:    "enterChatPicker",
			msg:     switchViewMsg{view: ViewChatPicker, sessionID: "session-1"},
			widthOf: func(a App) int { return a.chatPicker.width },
		},
		{
			name:    "enterRepoAdd",
			msg:     switchViewMsg{view: ViewRepoAdd},
			widthOf: func(a App) int { return a.repoAdd.width },
		},
		{
			name:    "enterRepoSettings",
			msg:     switchViewMsg{view: ViewRepoSettings, sessionID: "session-1"},
			widthOf: func(a App) int { return a.repoSettings.width },
		},
		{
			name:    "enterSettings",
			msg:     switchViewMsg{view: ViewSettings},
			widthOf: func(a App) int { return a.settings.width },
		},
		{
			name:    "enterLogin",
			msg:     switchViewMsg{view: ViewLogin},
			widthOf: func(a App) int { return a.login.width },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			// Seed the App dimensions directly rather than via a WindowSizeMsg:
			// that message fans out to the sub-models too, which would pre-satisfy
			// the assertion below and make a discarded construction invisible.
			a.width = width
			a.height = height

			if got := tt.widthOf(a); got != 0 {
				t.Fatalf("precondition: target sub-model already %d wide before the switch; "+
					"the assertion below would pass without the enter method running", got)
			}

			model, _ := a.Update(tt.msg)
			got := model.(App)

			if got.activeView != tt.msg.view {
				t.Fatalf("activeView = %v, want %v", got.activeView, tt.msg.view)
			}
			if w := tt.widthOf(got); w != width {
				t.Fatalf("sub-model width = %d, want %d.\n"+
					"%s's mutation did not reach the App that handleSwitchView returned. It "+
					"has a pointer receiver and runs against handleSwitchView's own value "+
					"copy, so the freshly constructed model is only kept if that copy is what "+
					"gets returned. A value receiver here, or reading `a` before the call, "+
					"drops the entire construction and the view renders unseeded.",
					w, width, tt.name)
			}
		})
	}
}

// TestHandleSwitchViewConstructsEveryRoutableView stands in for the exhaustive
// lint coverage handleSwitchView gives up by carrying a waiver.
//
// The waiver is legitimate — two constants are genuinely not reachable by a
// switchViewMsg — but it disables the check for the other seventeen too. That
// matters because a missing arm here is the quietest failure in the router: the
// switch falls through to its trailing `return a, nil`, so activeView flips to
// the new view while no sub-model is ever constructed and no Init runs. The
// screen renders a zero-value model and swallows every key.
//
// Adding a View constant already trips two other guards (the exhaustive linter
// on delegateToActiveView, and TestRenderActiveViewHandlesEveryViewConstant), so
// without this one handleSwitchView is the single remaining place a new view can
// be forgotten while the build stays green.
//
// A non-nil returned cmd is NOT a usable witness: six arms legitimately return
// nil because their Init does (repo add, settings, general settings, account
// edit, account register) — the same nil the fall-through returns. Each view
// therefore names a field its own arm seeds, which only construction can set.
func TestHandleSwitchViewConstructsEveryRoutableView(t *testing.T) {
	const (
		width  = 100
		height = 40
	)

	// Views deliberately absent from handleSwitchView's switch, which is why it
	// carries a waiver. Keep this list and the waiver's reason comment in step.
	notRoutable := map[View]string{
		ViewBugReport:  "pushed by ctrl+g in handleGlobalKey, never as a switchViewMsg",
		ViewOnboarding: "only ever entered via SetInitialView at startup",
	}

	// constructed reports whether the arm actually built its sub-model. Every
	// entry names a field the arm assigns, so it stays zero on a fall-through.
	constructed := map[View]func(App) bool{
		ViewHome:            func(a App) bool { return a.home.width == width },
		ViewNewSession:      func(a App) bool { return a.newSession.width == width },
		ViewAttach:          func(a App) bool { return a.attach.SessionID() != "" },
		ViewChatPicker:      func(a App) bool { return a.chatPicker.width == width },
		ViewRepoAdd:         func(a App) bool { return a.repoAdd.width == width },
		ViewRepoList:        func(a App) bool { return a.repoList.width == width },
		ViewRepoSettings:    func(a App) bool { return a.repoSettings.width == width },
		ViewTrash:           func(a App) bool { return a.trash.width == width },
		ViewSettings:        func(a App) bool { return a.settings.width == width },
		ViewSessionSettings: func(a App) bool { return a.sessionSettings.width == width },
		ViewLogin:           func(a App) bool { return a.login.width == width },
		ViewCron:            func(a App) bool { return a.cronList.width == width },
		ViewCronForm:        func(a App) bool { return a.cronForm.width == width },
		ViewAccounts:        func(a App) bool { return a.accountsList.width == width },
		ViewAccountEdit:     func(a App) bool { return a.accountEdit.width == width },
		ViewAccountRegister: func(a App) bool { return a.accountRegister.width == width },
		ViewGeneralSettings: func(a App) bool { return a.generalSettings.width == width },
	}

	for _, tc := range allViewConstants {
		t.Run(tc.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.width = width
			a.height = height

			model, _ := a.handleSwitchView(switchViewMsg{view: tc.view, sessionID: "session-1"})
			got := model.(App)

			if got.activeView != tc.view {
				t.Fatalf("activeView = %v, want %v — handleSwitchView sets it before the "+
					"switch, so this must hold even for a non-routable view",
					got.activeView, tc.view)
			}

			if why, skip := notRoutable[tc.view]; skip {
				if constructed[tc.view] != nil {
					t.Fatalf("%s is listed both as non-routable and as constructible; the "+
						"two tables must not overlap", tc.name)
				}
				t.Logf("%s is deliberately absent from handleSwitchView: %s", tc.name, why)
				return
			}

			witness := constructed[tc.view]
			if witness == nil {
				t.Fatalf("%s has no construction witness.\n"+
					"A View is either routable via switchViewMsg — in which case add the field "+
					"its handleSwitchView arm seeds to the `constructed` table — or it is not, "+
					"in which case add it to `notRoutable` with the reason. Leaving it out of "+
					"both means nothing checks that the arm exists.", tc.name)
			}
			if !witness(got) {
				t.Fatalf("%s: handleSwitchView did not construct its sub-model.\n"+
					"The arm is missing, so the switch fell through to its trailing "+
					"`return a, nil`: activeView flipped to %v but no model was built and no "+
					"Init ran, leaving a zero-value screen that swallows every key. The "+
					"//nolint:exhaustive waiver on that switch means the linter cannot tell "+
					"you this.", tc.name, tc.view)
			}
		})
	}
}
