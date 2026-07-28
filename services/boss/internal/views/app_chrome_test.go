package views

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// TestAppChromeHeightMatchesViewPrepend is the drift guard for BOS-512's
// coordinate translation. App.chromeHeight tells the form views how far down
// the screen their own line 0 renders; if View ever prepends something
// chromePrefix does not describe (the BOS-506 toast was exactly such an
// addition), every click lands on the wrong field. Asserting the helper against
// what View actually emitted makes the two impossible to separate.
func TestAppChromeHeightMatchesViewPrepend(t *testing.T) {
	tests := []struct {
		name  string
		toast string
	}{
		{name: "no toast"},
		{name: "with toast", toast: "Session rotated to account two"},
		{name: "with a toast long enough to wrap", toast: strings.Repeat("rotation notice ", 12)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.width = 80
			a.height = 40
			a.activeView = ViewSettings
			if tt.toast != "" {
				a.toast, _ = a.toast.Show(tt.toast)
			}

			child := a.settings.View().Content
			if child == "" {
				t.Fatal("test premise broken: the settings view rendered no content")
			}

			full := a.View().Content
			if !strings.HasSuffix(full, child) {
				t.Fatalf("App.View no longer ends with the child's content verbatim;\nchild:\n%s", child)
			}

			prefix := strings.TrimSuffix(full, child)
			// View joins prefix and child with "\n", so the child's first line
			// index is the number of newlines in everything before it.
			wantLines := strings.Count(prefix, "\n")
			if got := a.chromeHeight(); got != wantLines {
				t.Errorf("chromeHeight() = %d, but View prepends %d lines above the child",
					got, wantLines)
			}
			if got, want := lipgloss.Height(a.chromePrefix()), wantLines; got != want {
				t.Errorf("Height(chromePrefix()) = %d, want %d", got, want)
			}
		})
	}
}

// TestAppChromeHeightCountsTheToast pins the specific regression the plan's
// recon caught: the toast is variable-height and was added to View after this
// ticket was written, so a hardcoded banner figure would be short.
func TestAppChromeHeightCountsTheToast(t *testing.T) {
	a := NewApp(nil, nil)
	a.width = 80
	a.activeView = ViewSettings

	bare := a.chromeHeight()
	if bare != bannerHeight {
		t.Fatalf("chromeHeight with no toast = %d, want bannerHeight (%d)", bare, bannerHeight)
	}

	a.toast, _ = a.toast.Show("Session rotated")
	withToast := a.chromeHeight()
	if withToast != bare+a.toast.Height(a.width) {
		t.Errorf("chromeHeight with a toast = %d, want %d (banner %d + toast %d)",
			withToast, bare+a.toast.Height(a.width), bare, a.toast.Height(a.width))
	}
}

// TestAppViewMouseModeIsOffOutsideForms is the App-level half of the
// "form screens only" scoping. TestNonFormPhasesDisableMouse covers the four
// form views' own non-form phases; this covers the composed frame — the
// tea.View the renderer actually diffs — for every top-level screen at its
// default state. A view that leaked MouseModeCellMotion here would turn mouse
// reporting on for a whole screen that has no hit testing.
func TestAppViewMouseModeIsOffOutsideForms(t *testing.T) {
	// Every View constant. Kept explicit rather than derived so a new screen has
	// to be classified by a human; `TestViewGeneralSettingsIsHighestViewValue`
	// pins that ViewGeneralSettings stays last, so the length check below makes
	// the table mechanically complete.
	//
	// seed gives a view a real sub-model where NewApp leaves a zero value that
	// renders nothing — an empty view would assert MouseModeNone vacuously.
	all := []struct {
		name string
		view View
		seed func(a *App)
	}{
		{name: "ViewHome", view: ViewHome},
		{name: "ViewNewSession", view: ViewNewSession},
		{name: "ViewAttach", view: ViewAttach},
		{name: "ViewChatPicker", view: ViewChatPicker},
		{name: "ViewRepoAdd", view: ViewRepoAdd},
		{name: "ViewRepoList", view: ViewRepoList},
		{name: "ViewRepoSettings", view: ViewRepoSettings},
		{name: "ViewTrash", view: ViewTrash},
		{name: "ViewSettings", view: ViewSettings},
		{name: "ViewSessionSettings", view: ViewSessionSettings},
		{name: "ViewLogin", view: ViewLogin},
		{name: "ViewBugReport", view: ViewBugReport, seed: func(a *App) {
			// NewApp leaves bugReport zero-valued, whose View is empty. Seed a
			// real model on a NON-form phase: the form phase legitimately
			// reports MouseModeCellMotion and is covered by the positive
			// control below.
			a.bugReport = NewBugReportModel(nil, context.Background(), nil, ViewHome, nil, nil)
			a.bugReport.phase = bugReportPhaseSubmitting
		}},
		{name: "ViewCron", view: ViewCron},
		{name: "ViewCronForm", view: ViewCronForm},
		{name: "ViewOnboarding", view: ViewOnboarding},
		{name: "ViewAccounts", view: ViewAccounts},
		{name: "ViewAccountEdit", view: ViewAccountEdit},
		{name: "ViewAccountRegister", view: ViewAccountRegister},
		{name: "ViewGeneralSettings", view: ViewGeneralSettings},
	}
	if got := len(all); View(got-1) != ViewGeneralSettings {
		t.Fatalf("this table lists %d views but the highest View constant is %d; "+
			"a new screen was added without classifying its MouseMode", got, ViewGeneralSettings)
	}

	for _, tt := range all {
		t.Run(tt.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.width = 80
			a.height = 40
			a.activeView = tt.view
			if tt.seed != nil {
				tt.seed(&a)
			}
			v := a.View()
			if v.Content == "" {
				t.Fatal("this screen rendered no content; the assertion below would be vacuous — " +
					"give it a seed func")
			}
			if got := v.MouseMode; got != tea.MouseModeNone {
				t.Errorf("App.View().MouseMode = %v on %s, want MouseModeNone", got, tt.name)
			}
		})
	}
}

// TestAppViewMouseModeFollowsTheActiveForm is the positive control for the
// sweep above — without it, a bug that hardcoded MouseModeNone in App.View
// would pass every assertion — and the automated evidence for the acceptance
// criterion "opening a form and then attaching to a session leaves no stray
// mouse mode in the outer terminal".
//
// bubbletea's renderer diffs MouseMode between frames and emits the disable
// sequence when it drops (cursed_renderer.go), so a frame reporting
// MouseModeNone after one reporting MouseModeCellMotion IS the reset. Asserting
// the transition here is what makes that reset a tested property rather than a
// manual observation.
func TestAppViewMouseModeFollowsTheActiveForm(t *testing.T) {
	a := NewApp(nil, nil)
	a.width = 80
	a.height = 40
	a.activeView = ViewBugReport
	a.bugReport = NewBugReportModel(nil, context.Background(), nil, ViewHome, nil, nil)

	form := a.View()
	if form.Content == "" {
		t.Fatal("test premise broken: the bug-report form rendered no content")
	}
	if got := form.MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("App.View().MouseMode on a form screen = %v, want MouseModeCellMotion "+
			"(App must preserve the child view's mode)", got)
	}

	// Navigating into an attached session must drop it again.
	a.activeView = ViewAttach
	a.attach = NewAttachModel(nil, context.Background(), nil, "session-1", "")
	if got := a.View().MouseMode; got != tea.MouseModeNone {
		t.Errorf("App.View().MouseMode after attaching = %v, want MouseModeNone; "+
			"a stray mouse mode would leak into the terminal Claude Code owns", got)
	}
}

// TestTranslateMouseY covers the translation itself: every mouse message shape
// is shifted, a click on the chrome becomes negative rather than clamped, and
// non-mouse messages pass through untouched.
func TestTranslateMouseY(t *testing.T) {
	const chrome = 5

	t.Run("click", func(t *testing.T) {
		got := translateMouseY(tea.MouseClickMsg{Y: 9, X: 3, Button: tea.MouseLeft}, chrome)
		click, ok := got.(tea.MouseClickMsg)
		if !ok {
			t.Fatalf("translateMouseY returned %T, want tea.MouseClickMsg", got)
		}
		if click.Y != 4 {
			t.Errorf("Y = %d, want 4", click.Y)
		}
		if click.X != 3 || click.Button != tea.MouseLeft {
			t.Errorf("translation altered X or Button: %+v", click)
		}
	})

	t.Run("a click on the chrome goes negative, not clamped", func(t *testing.T) {
		got := translateMouseY(tea.MouseClickMsg{Y: 1, Button: tea.MouseLeft}, chrome).(tea.MouseClickMsg)
		if got.Y != -4 {
			t.Errorf("Y = %d, want -4 (clamping would hide a chrome click as a form click)", got.Y)
		}
	})

	t.Run("other mouse shapes", func(t *testing.T) {
		if got := translateMouseY(tea.MouseWheelMsg{Y: 9}, chrome).(tea.MouseWheelMsg); got.Y != 4 {
			t.Errorf("wheel Y = %d, want 4", got.Y)
		}
		if got := translateMouseY(tea.MouseMotionMsg{Y: 9}, chrome).(tea.MouseMotionMsg); got.Y != 4 {
			t.Errorf("motion Y = %d, want 4", got.Y)
		}
		if got := translateMouseY(tea.MouseReleaseMsg{Y: 9}, chrome).(tea.MouseReleaseMsg); got.Y != 4 {
			t.Errorf("release Y = %d, want 4", got.Y)
		}
	})

	t.Run("non-mouse messages pass through", func(t *testing.T) {
		in := tea.WindowSizeMsg{Width: 80, Height: 24}
		if got := translateMouseY(in, chrome); got != tea.Msg(in) {
			t.Errorf("translateMouseY mutated a non-mouse message: %+v", got)
		}
	})

	t.Run("zero chrome is a no-op", func(t *testing.T) {
		in := tea.MouseClickMsg{Y: 9, Button: tea.MouseLeft}
		if got := translateMouseY(in, 0).(tea.MouseClickMsg); got.Y != 9 {
			t.Errorf("Y = %d, want 9", got.Y)
		}
	})
}
