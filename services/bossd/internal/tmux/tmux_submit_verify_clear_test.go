package tmux

import (
	"context"
	"strings"
	"testing"
)

// pressedCtrlU reports whether the factory recorded a send-keys carrying C-u —
// the destructive keystroke these tests exist to withhold.
func pressedCtrlU(f *sendPlanRecordingFactory) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.subcommand != "send-keys" {
			continue
		}
		for _, a := range c.args {
			if a == "C-u" {
				return true
			}
		}
	}
	return false
}

// TestClearComposer_PressesOnlyWhenTheComposerStillHoldsThePayload pins the
// precondition ClearComposer's own doc comment states: C-u is licensed by a
// drawn composer that still holds the payload, NOT by the OutcomeNotSubmitted
// verdict alone.
//
// The verdict is reachable through an infrastructure error that never read a
// composer — sendEnter can return an error after tmux already delivered the
// Enter (classifySubmit, tmux_submit_verify.go) — so on an ALIVE pane the
// payload may be running, or an attached user may have typed replacement text.
// Pane liveness alone does not distinguish those; only the composer does.
func TestClearComposer_PressesOnlyWhenTheComposerStillHoldsThePayload(t *testing.T) {
	const payload = "switched to Codex Work"

	// panes is consumed in order and the last value is reused, so a case that
	// licenses a press supplies the post-clear pane too — otherwise the clear
	// loop re-reads the holding pane and exhausts its presses.
	tests := []struct {
		name      string
		panes     []string
		wantPress bool
		wantErr   bool
	}{
		{
			// The one state that earns the key: box drawn, payload still sitting
			// at the prompt for the next Enter to send.
			name:      "composer still holds the payload",
			panes:     []string{"• done\n❯ " + payload + "\n", "• done\n❯\n"},
			wantPress: true,
		},
		{
			// No prompt marker at all: the pane is running full-screen, so the
			// payload was submitted despite the not-submitted verdict. A key here
			// lands in a working agent.
			name:      "no composer drawn (pane is running)",
			panes:     []string{"• Thinking…\n  ⎿ working\n"},
			wantPress: false,
		},
		{
			// Box drawn but holding something else — an attached human typed over
			// it. Clearing would erase text this code did not put there.
			name:      "composer holds replacement text a user typed",
			panes:     []string{"• done\n❯ never mind, do the other thing\n"},
			wantPress: false,
		},
		{
			// Already empty: nothing to clear, and the loop would have returned
			// nil after a press anyway.
			name:      "composer cleared to a bare glyph",
			panes:     []string{"• done\n❯\n"},
			wantPress: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &sendPlanRecordingFactory{capturePaneOutputs: tt.panes}
			c := NewClient(WithCommandFactory(f.factory))
			err := c.ClearComposer(context.Background(), "boss-test-sess", payload)
			if tt.wantErr && err == nil {
				t.Fatalf("ClearComposer error = nil, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ClearComposer error = %v, want nil", err)
			}
			if got := pressedCtrlU(f); got != tt.wantPress {
				t.Errorf("sent C-u = %v, want %v — the first pane read was %q",
					got, tt.wantPress, strings.TrimSpace(tt.panes[0]))
			}
		})
	}
}

// TestClearComposer_UnreadablePaneIsAnErrorNotABlindPress proves an unreadable
// pane does not degrade into pressing anyway. "I could not see the composer" is
// exactly the unknown state that must NOT be answered with a destructive key,
// and the caller is told so rather than being handed a silent success.
func TestClearComposer_UnreadablePaneIsAnErrorNotABlindPress(t *testing.T) {
	failFrom := 0
	f := &sendPlanRecordingFactory{failCapturePaneFrom: &failFrom}
	c := NewClient(WithCommandFactory(f.factory))

	if err := c.ClearComposer(context.Background(), "boss-test-sess", "switched to Codex Work"); err == nil {
		t.Fatal("ClearComposer error = nil, want an error when the pane cannot be read")
	}
	if pressedCtrlU(f) {
		t.Error("sent C-u despite never having read the composer")
	}
}
