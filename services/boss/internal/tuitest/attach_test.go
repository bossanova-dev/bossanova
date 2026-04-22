package tuitest_test

import (
	"errors"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
)

// TestTUI_AttachView_BackKey exercises AttachView's error-state recovery:
// when EnsureTmuxSession fails, AttachView renders its error screen with
// an [esc] back action bar. Pressing esc must detach the view and route
// the app back to ChatPicker (the view AttachView is entered from).
//
// Scope note: the planning doc's original intent was to verify esc during
// the normal attach flow, but AttachView delegates to `tmux attach-session`
// via tea.ExecProcess once launched — at that point key events go to tmux,
// not to Bubble Tea. The error-path test below is the largest piece of
// AttachView that is testable without the refactor tracked in bd-4cd.
func TestTUI_AttachView_BackKey(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	// Force EnsureTmuxSession to error so AttachView stops at its error screen
	// rather than shelling out to tmux, which isn't runnable inside the PTY
	// harness. This makes the esc-back path deterministic.
	h.Daemon.SetEnsureTmuxError(errors.New("tmux unavailable in test env"))

	// Home → session row.
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// Enter on the first session. With no active chats the auto-enter
	// resolver routes to ChatPicker (never directly into AttachView).
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[n]ew chat"); err != nil {
		t.Fatalf("expected ChatPicker; screen:\n%s", h.Driver.Screen())
	}

	// Press 'n' to start a new chat — routes into AttachView.
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	// EnsureTmuxSession fires, our injected error turns into an attachErrMsg,
	// and the error view is rendered with an [esc] back action bar.
	if err := h.Driver.WaitForText(waitTimeout, "ensure tmux session"); err != nil {
		t.Fatalf("expected attach error screen; screen:\n%s", h.Driver.Screen())
	}
	if !h.Driver.ScreenContains("[esc] back") {
		t.Fatalf("expected [esc] back action bar; screen:\n%s", h.Driver.Screen())
	}

	// Press esc — AttachView sets detach=true; App routes back to ChatPicker.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	// We're back in ChatPicker — the [n]ew chat action bar is visible again
	// and the attach error text is gone.
	if err := h.Driver.WaitForText(waitTimeout, "[n]ew chat"); err != nil {
		t.Fatalf("expected ChatPicker after esc; screen:\n%s", h.Driver.Screen())
	}
	if h.Driver.ScreenContains("ensure tmux session") {
		t.Fatalf("expected to leave attach error; screen:\n%s", h.Driver.Screen())
	}

	// esc must not have ended the session — it should still be in the daemon's
	// store, unarchived.
	var found bool
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" {
			found = true
			if s.ArchivedAt != nil {
				t.Fatal("esc from attach error should not archive/end the session")
			}
		}
	}
	if !found {
		t.Fatal("session sess-aaa-111 missing from daemon state after esc")
	}
}
