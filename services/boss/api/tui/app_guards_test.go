package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"
)

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
	for _, tc := range allViewConstants(t) {
		t.Run(tc.Name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.activeView = tc.View

			v := a.renderActiveView()

			if strings.Contains(v.Content, "Unknown view") {
				t.Fatalf("%s rendered the default arm.\n"+
					"renderActiveView is missing a case for this View. Because the switch "+
					"keeps a default arm, `exhaustive` does not flag the omission: the view "+
					"routes and updates normally and simply renders nothing.", tc.Name)
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
		msg     ui.SwitchViewMsg
		widthOf func(App) int
	}{
		{
			name:    "enterNewSession",
			msg:     ui.SwitchViewMsg{View: ui.ViewNewSession},
			widthOf: func(a App) int { return a.newSession.Width() },
		},
		{
			name:    "enterChatPicker",
			msg:     ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: "session-1"},
			widthOf: func(a App) int { return a.chatPicker.Width() },
		},
		{
			name:    "enterRepoAdd",
			msg:     ui.SwitchViewMsg{View: ui.ViewRepoAdd},
			widthOf: func(a App) int { return a.repoAdd.Width() },
		},
		{
			name:    "enterRepoSettings",
			msg:     ui.SwitchViewMsg{View: ui.ViewRepoSettings, SessionID: "session-1"},
			widthOf: func(a App) int { return a.repoSettings.Width() },
		},
		{
			name:    "enterSettings",
			msg:     ui.SwitchViewMsg{View: ui.ViewSettings},
			widthOf: func(a App) int { return a.settings.Width() },
		},
		{
			name:    "enterLogin",
			msg:     ui.SwitchViewMsg{View: ui.ViewLogin},
			widthOf: func(a App) int { return a.login.Width() },
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

			if got.activeView != tt.msg.View {
				t.Fatalf("activeView = %v, want %v", got.activeView, tt.msg.View)
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
	notRoutable := map[ui.View]string{
		ui.ViewBugReport:  "pushed by ctrl+g in handleGlobalKey, never as a switchViewMsg",
		ui.ViewOnboarding: "only ever entered via SetInitialView at startup",
	}

	// constructed reports whether the arm actually built its sub-model. Every
	// entry names a field the arm assigns, so it stays zero on a fall-through.
	constructed := map[ui.View]func(App) bool{
		ui.ViewHome:                 func(a App) bool { return a.home.Width == width },
		ui.ViewNewSession:           func(a App) bool { return a.newSession.Width() == width },
		ui.ViewAttach:               func(a App) bool { return a.attach.SessionID() != "" },
		ui.ViewChatPicker:           func(a App) bool { return a.chatPicker.Width() == width },
		ui.ViewRepoAdd:              func(a App) bool { return a.repoAdd.Width() == width },
		ui.ViewRepoList:             func(a App) bool { return a.repoList.Width() == width },
		ui.ViewRepoSettings:         func(a App) bool { return a.repoSettings.Width() == width },
		ui.ViewTrash:                func(a App) bool { return a.trash.Width() == width },
		ui.ViewSettings:             func(a App) bool { return a.settings.Width() == width },
		ui.ViewSessionSettings:      func(a App) bool { return a.sessionSettings.Width() == width },
		ui.ViewLogin:                func(a App) bool { return a.login.Width() == width },
		ui.ViewCron:                 func(a App) bool { return a.cronList.Width() == width },
		ui.ViewCronForm:             func(a App) bool { return a.cronForm.Width() == width },
		ui.ViewAgentAccounts:        func(a App) bool { return a.accountsList.Width() == width },
		ui.ViewAgentAccountEdit:     func(a App) bool { return a.accountEdit.Width() == width },
		ui.ViewAgentAccountRegister: func(a App) bool { return a.accountRegister.Width() == width },
		ui.ViewGeneralSettings:      func(a App) bool { return a.generalSettings.Width() == width },
	}

	for _, tc := range allViewConstants(t) {
		t.Run(tc.Name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.width = width
			a.height = height

			model, _ := a.handleSwitchView(ui.SwitchViewMsg{View: tc.View, SessionID: "session-1"})
			got := model.(App)

			if got.activeView != tc.View {
				t.Fatalf("activeView = %v, want %v — handleSwitchView sets it before the "+
					"switch, so this must hold even for a non-routable view",
					got.activeView, tc.View)
			}

			if why, skip := notRoutable[tc.View]; skip {
				if constructed[tc.View] != nil {
					t.Fatalf("%s is listed both as non-routable and as constructible; the "+
						"two tables must not overlap", tc.Name)
				}
				t.Logf("%s is deliberately absent from handleSwitchView: %s", tc.Name, why)
				return
			}

			witness := constructed[tc.View]
			if witness == nil {
				t.Fatalf("%s has no construction witness.\n"+
					"A View is either routable via switchViewMsg — in which case add the field "+
					"its handleSwitchView arm seeds to the `constructed` table — or it is not, "+
					"in which case add it to `notRoutable` with the reason. Leaving it out of "+
					"both means nothing checks that the arm exists.", tc.Name)
			}
			if !witness(got) {
				t.Fatalf("%s: handleSwitchView did not construct its sub-model.\n"+
					"The arm is missing, so the switch fell through to its trailing "+
					"`return a, nil`: activeView flipped to %v but no model was built and no "+
					"Init ran, leaving a zero-value screen that swallows every key. The "+
					"//nolint:exhaustive waiver on that switch means the linter cannot tell "+
					"you this.", tc.Name, tc.View)
			}
		})
	}
}

// allViewConstants reads the enum whose explicit iota list is proved in ui's
// TestAllViewConstantsAreListed, so routing guards follow every declared view.
func allViewConstants(t *testing.T) []struct {
	View ui.View
	Name string
} {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "../../api/tui/ui/nav.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var views []struct {
		View ui.View
		Name string
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST || len(gen.Specs) == 0 {
			continue
		}
		first := gen.Specs[0].(*ast.ValueSpec)
		typ, ok := first.Type.(*ast.Ident)
		if !ok || typ.Name != "View" {
			continue
		}
		for _, spec := range gen.Specs {
			for _, name := range spec.(*ast.ValueSpec).Names {
				views = append(views, struct {
					View ui.View
					Name string
				}{ui.View(len(views)), name.Name})
			}
		}
	}
	if len(views) == 0 {
		t.Fatal("found no View constants")
	}
	return views
}
