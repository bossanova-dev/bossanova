package views

// Tests for updateSub, the generic delegation helper introduced by BOS-529.
//
// updateSub replaced ~18 hand-written copies of this shape:
//
//	updated, cmd := a.home.Update(msg)
//	if m, ok := updated.(HomeModel); ok {
//		a.home = m
//	}
//
// The load-bearing detail is the `, ok` form: the original arms SKIP the write
// back when the returned tea.Model is not the sub-model's own concrete type.
// A generic rewrite is one keystroke away from `*dst = updated.(T)`, which
// panics instead of skipping — and nothing in the production suite notices,
// because no real sub-model's Update ever returns a foreign type. That makes
// the mismatch path unreachable from every other test in this package, so it
// needs a purpose-built witness.

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// stubSubModel stands in for a sub-model stored on the App. Update returns its
// own concrete type, like every real sub-model does.
type stubSubModel struct {
	marker string
}

func (m stubSubModel) Init() tea.Cmd { return nil }

func (m stubSubModel) Update(tea.Msg) (tea.Model, tea.Cmd) {
	return stubSubModel{marker: "updated"}, stubCmd
}

func (m stubSubModel) View() tea.View { return tea.NewView("") }

// foreignSubModel's Update returns a DIFFERENT concrete type, which is the
// case updateSub must survive without writing back and without panicking.
type foreignSubModel struct {
	marker string
}

func (m foreignSubModel) Init() tea.Cmd { return nil }

func (m foreignSubModel) Update(tea.Msg) (tea.Model, tea.Cmd) {
	return stubSubModel{marker: "wrong type"}, stubCmd
}

func (m foreignSubModel) View() tea.View { return tea.NewView("") }

func stubCmd() tea.Msg { return nil }

// TestUpdateSubWritesBackOnTypeMatch pins the ordinary path: a sub-model whose
// Update returns its own type is stored back through dst, and its command is
// returned to the caller.
func TestUpdateSubWritesBackOnTypeMatch(t *testing.T) {
	dst := stubSubModel{marker: "original"}

	cmd := updateSub(&dst, tea.WindowSizeMsg{Width: 10, Height: 10})

	if dst.marker != "updated" {
		t.Errorf("dst.marker = %q, want %q — updateSub did not write the updated model back", dst.marker, "updated")
	}
	if cmd == nil {
		t.Error("updateSub returned a nil cmd, want the sub-model's own cmd propagated")
	}
}

// TestUpdateSubSkipsWriteOnTypeMismatch pins the behaviour the hand-written
// arms had and the generic helper must not lose.
//
// Kills: `*dst = updated.(T)` (an unchecked assert, which panics here) and
// any variant that force-assigns a zero value over dst.
func TestUpdateSubSkipsWriteOnTypeMismatch(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("updateSub panicked on a type mismatch (%v); it must skip the write instead", r)
		}
	}()

	dst := foreignSubModel{marker: "original"}

	cmd := updateSub(&dst, tea.WindowSizeMsg{Width: 10, Height: 10})

	if dst.marker != "original" {
		t.Errorf("dst.marker = %q, want %q — updateSub overwrote dst despite the type mismatch", dst.marker, "original")
	}
	// The command is still propagated: the original arms captured cmd before
	// the assert, so a mismatch drops the model update but never the cmd.
	if cmd == nil {
		t.Error("updateSub returned a nil cmd, want the sub-model's cmd propagated even on a mismatch")
	}
}

// Characterization tests for the per-view POST-PROCESSING blocks in
// app_delegate.go (BOS-529).
//
// updateSub collapsed the repeated assert dance, but each delegation arm may
// also carry view-specific post-processing that runs after it — and that is the
// part the plan called "the most likely silent regression in this ticket".
// Deleting such a block compiles cleanly, passes `go vet`, and satisfies the
// exhaustive linter, so each one needs a witness.
//
// Most of them already have one: dropping ViewChatPicker's markArchiving or
// mergedOptimisticID, ViewTrash's RestoredSessionID route, ViewNewSession's
// Done→Attach hand-off or ViewAccountRegister's Done rebuild all turn the views
// suite red. Two did not, and the tests below close that gap.

// TestUpdateHomePropagatesSettingsBackToApp pins updateHome's settings
// propagation — the App-level half of the BossCloudValueDeliveredAt latch.
//
// HomeModel persists settings of its own (the one-time value-delivery
// milestone). updateHome copies them back onto the App so a later
// newHomeModel() re-seeds from the latched value instead of the stale startup
// snapshot; without it the latch re-stamps on every return-to-home, moving a
// timestamp whose whole contract is that it never moves.
//
// Kills: deleting `a.userSettings = a.home.settings` from updateHome. That
// deletion was verified to survive BOTH the views suite and the whole tuitest
// suite before this test existed.
func TestUpdateHomePropagatesSettingsBackToApp(t *testing.T) {
	const latched = "latched-by-home"

	a := NewApp(nil, nil)
	a.activeView = ViewHome
	// Diverge the two copies: only the propagation line can reconcile them.
	a.userSettings.DefaultAgent = "stale-app-snapshot"
	a.home.settings.DefaultAgent = latched

	if a.userSettings.DefaultAgent == latched {
		t.Fatal("precondition: App and Home already agree, so the assertion below " +
			"would pass without any propagation")
	}

	model, _ := a.Update(tickMsg{})
	got := model.(App)

	if got.home.settings.DefaultAgent != latched {
		t.Fatalf("home settings.DefaultAgent = %q, want %q — the sub-model's own "+
			"settings must survive the delegation", got.home.settings.DefaultAgent, latched)
	}
	if got.userSettings.DefaultAgent != latched {
		t.Fatalf("App userSettings.DefaultAgent = %q, want %q.\n"+
			"updateHome must copy the home model's settings back onto the App after "+
			"delegating. Dropping that line lets newHomeModel() re-seed every rebuilt "+
			"Home from the stale startup snapshot, so the BossCloudValueDeliveredAt "+
			"latch re-stamps on every return-to-home and the guest cloud promo reverts.",
			got.userSettings.DefaultAgent, latched)
	}
}

// TestUpdateBugReportResumesTickChainForTickDrivenViews pins updateBugReport's
// resumeTickCmd call.
//
// The bug-report modal swallows tickMsg while it is up, which breaks the
// self-perpetuating tick chain of whichever view was underneath. On dismissal
// the arm restarts that chain — but only for the tick-driven views, which is
// why it returns resumeTickCmd(a.activeView) rather than tickCmd() outright.
//
// The assertion is deliberately "a cmd / no cmd" contrast rather than an
// inspection of the message the cmd produces. Both halves together already
// discriminate the two realistic mutants, and invoking the returned cmd would
// block for a full pollInterval per case (tickCmd wraps tea.Tick), which would
// cost more wall-clock than the rest of this package's suite. Do not "improve"
// this by calling cmd().
//
// Kills: replacing `return a, resumeTickCmd(a.activeView)` with `return a, nil`
// (Home never refreshes again after a bug report) and with `return a, tickCmd()`
// (a tick chain started on views that have none). Both were verified to survive
// BOTH the views suite and the whole tuitest suite before this test existed.
func TestUpdateBugReportResumesTickChainForTickDrivenViews(t *testing.T) {
	tests := []struct {
		name         string
		previousView View
		wantTick     bool
	}{
		{name: "home is tick-driven", previousView: ViewHome, wantTick: true},
		{name: "chat picker is tick-driven", previousView: ViewChatPicker, wantTick: true},
		{name: "trash is not tick-driven", previousView: ViewTrash, wantTick: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewApp(nil, nil)
			a.bugReport = NewBugReportModel(nil, a.ctx, nil, tt.previousView, nil, nil)
			a.bugReport.phase = bugReportPhaseSuccess
			a.activeView = ViewBugReport

			// The success phase dismisses on any key, which is what drives the arm
			// into its Done() branch.
			model, cmd := a.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
			got := model.(App)

			if !got.bugReport.done {
				t.Fatal("precondition: the bug report was not dismissed, so the arm's " +
					"Done() branch never ran")
			}
			if got.activeView != tt.previousView {
				t.Fatalf("activeView = %v, want %v (the dismissed modal must restore the "+
					"view it was opened from)", got.activeView, tt.previousView)
			}

			if tt.wantTick && cmd == nil {
				t.Fatalf("cmd = nil, want a tick.\n"+
					"updateBugReport must return resumeTickCmd(a.activeView) when the modal "+
					"dismisses: the modal swallowed %v's self-perpetuating tickMsg chain, so "+
					"without the restart that view's status never refreshes again for the rest "+
					"of the session.", tt.previousView)
			}
			if !tt.wantTick && cmd != nil {
				t.Fatalf("cmd = non-nil, want nil.\n"+
					"resumeTickCmd returns nil for views with no tick chain; restarting one "+
					"for %v would schedule refreshes that view never asked for.",
					tt.previousView)
			}
		})
	}
}

// TestHandleSessionListWiresTheRotationToast pins handleSessionList's rotation
// bookkeeping — the BOS-176 toast wiring.
//
// detectNewRotationEvents and toastModel are unit-tested in toast_test.go, but
// the App-level wiring that connects them had no test anywhere: three separate
// one-line deletions inside handleSessionList left both the views suite and the
// whole tuitest suite green. Each is a silent, process-lifetime failure:
//
//   - dropping `a.rotationSeen = seen` means prev is always nil, so every poll
//     takes the seed path and no rotation is ever announced again;
//   - dropping the `a.toast.Show(toasts[0])` block detects the rotation and then
//     shows nothing;
//   - dropping `&& msg.err == nil` lets a failed poll clear the seen-map, so the
//     next good poll re-announces every session's latest event as if new.
//
// This drives whole App.Update calls rather than the handler directly, so the
// arm's guard conditions are part of what is pinned.
func TestHandleSessionListWiresTheRotationToast(t *testing.T) {
	// sessionWith builds a one-session poll result whose newest rotation event
	// has the given id. detectNewRotationEvents reads events newest-first.
	sessionWith := func(eventID string) []*pb.Session {
		return []*pb.Session{{
			Id:    "sess-1",
			Title: "Fix the bug",
			RotationEvents: []*pb.RotationEvent{{
				Id:          eventID,
				Outcome:     pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
				FromAccount: "acct-a",
				ToAccount:   "acct-b",
			}},
		}}
	}

	t.Run("first poll seeds silently, a new event then toasts", func(t *testing.T) {
		a := NewApp(nil, nil)
		a.activeView = ViewHome

		// Seed pass: rotationSeen starts nil, so this must record state WITHOUT
		// announcing history to a freshly started TUI.
		model, _ := a.Update(sessionListMsg{sessions: sessionWith("ev-1")})
		seeded := model.(App)

		if seeded.toast.text != "" {
			t.Fatalf("first poll raised toast %q, want silence — a fresh TUI must not "+
				"replay rotation history", seeded.toast.text)
		}
		if seeded.rotationSeen["sess-1"] != "ev-1" {
			t.Fatalf("rotationSeen[sess-1] = %q, want %q.\n"+
				"handleSessionList must store detectNewRotationEvents' returned map. "+
				"Without that assignment prev stays nil on every poll, so every poll is a "+
				"seed pass and no rotation is ever announced again.",
				seeded.rotationSeen["sess-1"], "ev-1")
		}

		// A genuinely new event id must now surface.
		model, _ = seeded.Update(sessionListMsg{sessions: sessionWith("ev-2")})
		toasted := model.(App)

		if toasted.toast.text == "" {
			t.Fatal("a new rotation event raised no toast.\n" +
				"handleSessionList must call a.toast.Show for the first detected toast; " +
				"without it the rotation is detected and then silently dropped.")
		}
		if !strings.Contains(toasted.toast.text, "Fix the bug") {
			t.Fatalf("toast = %q, want it to name the session", toasted.toast.text)
		}
		if toasted.rotationSeen["sess-1"] != "ev-2" {
			t.Fatalf("rotationSeen[sess-1] = %q, want ev-2 — the seen-map must advance so "+
				"the same event is not re-announced", toasted.rotationSeen["sess-1"])
		}
	})

	t.Run("a failed poll does not clear the seen map", func(t *testing.T) {
		a := NewApp(nil, nil)
		a.activeView = ViewHome

		model, _ := a.Update(sessionListMsg{sessions: sessionWith("ev-1")})
		seeded := model.(App)

		// A failed poll carries no sessions. It must not be treated as an
		// observation, or the seen-map would be replaced with an empty one.
		model, _ = seeded.Update(sessionListMsg{err: errors.New("daemon unreachable")})
		afterErr := model.(App)

		if afterErr.rotationSeen["sess-1"] != "ev-1" {
			t.Fatalf("rotationSeen[sess-1] = %q after a failed poll, want ev-1 preserved.\n"+
				"handleSessionList's rotation branch is guarded on msg.err == nil. Without "+
				"that guard a failed poll overwrites the seen-map with an empty one, and the "+
				"next successful poll re-announces every session's latest rotation as if it "+
				"had just happened.", afterErr.rotationSeen["sess-1"])
		}

		// And the re-announcement the guard prevents must indeed not happen.
		model, _ = afterErr.Update(sessionListMsg{sessions: sessionWith("ev-1")})
		if replayed := model.(App); replayed.toast.text != "" {
			t.Fatalf("unchanged rotation re-toasted as %q after a failed poll",
				replayed.toast.text)
		}
	})
}
