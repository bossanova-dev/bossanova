package tuitest_test

import (
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// navigateToSessionSettings walks Home → ChatPicker → SessionSettings for the
// first seeded session (sess-aaa-111 / "Add dark mode").
func navigateToSessionSettings(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	// ChatPicker appears regardless of chats being present. The action-bar
	// hint "[n]ew chat" is a stable marker.
	if err := h.Driver.WaitForText(waitTimeout, "[n]ew chat"); err != nil {
		t.Fatalf("expected ChatPicker; screen:\n%s", h.Driver.Screen())
	}
	// 's' routes to ViewSessionSettings.
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	// Loaded state shows "Name: <title>" and an "[enter/space] edit" action bar.
	if err := h.Driver.WaitForText(waitTimeout, "[enter/space] edit"); err != nil {
		t.Fatalf("expected SessionSettings view; screen:\n%s", h.Driver.Screen())
	}
}

// TestTUI_SessionSettingsView_UpdateTitle exercises the full edit-and-save
// path: enter edit mode, append to the existing title, hit enter to save, and
// assert the daemon received an UpdateSession request with the new title.
func TestTUI_SessionSettingsView_UpdateTitle(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	navigateToSessionSettings(t, h)

	// Press enter to activate the Name row for editing. The text input is
	// pre-populated with the current title ("Add dark mode") and the cursor
	// sits at the end, so SendString appends.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[enter] save"); err != nil {
		t.Fatalf("expected edit mode; screen:\n%s", h.Driver.Screen())
	}

	// Append " v2" and commit.
	if err := h.Driver.SendString(" v2"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	// Poll the captured UpdateSession request.
	deadline := time.Now().Add(waitTimeout)
	var calls []*pb.UpdateSessionRequest
	for time.Now().Before(deadline) {
		calls = h.Daemon.UpdateSessionCalls()
		if len(calls) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(calls) == 0 {
		t.Fatalf("UpdateSession was never called; screen:\n%s", h.Driver.Screen())
	}
	req := calls[len(calls)-1]
	if req.Id != "sess-aaa-111" {
		t.Fatalf("UpdateSession.Id = %q, want %q", req.Id, "sess-aaa-111")
	}
	if req.Title == nil {
		t.Fatalf("UpdateSession.Title was nil; req=%+v", req)
	}
	if *req.Title != "Add dark mode v2" {
		t.Fatalf("UpdateSession.Title = %q, want %q", *req.Title, "Add dark mode v2")
	}

	// Daemon-side state should reflect the new title.
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" && s.Title != "Add dark mode v2" {
			t.Fatalf("session title not updated server-side: got %q", s.Title)
		}
	}
}

// TestTUI_SessionSettingsView_Cancel verifies that escaping out of the Name
// edit does NOT save: no UpdateSession RPC is sent and the session title on
// the daemon stays at its original value.
func TestTUI_SessionSettingsView_Cancel(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	navigateToSessionSettings(t, h)

	// Activate edit mode, type a change, then cancel with esc.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[enter] save"); err != nil {
		t.Fatalf("expected edit mode; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendString(" DISCARDED"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	// Back out of edit mode — action bar flips to "[enter/space] edit".
	if err := h.Driver.WaitForText(waitTimeout, "[enter/space] edit"); err != nil {
		t.Fatalf("expected return to row view after esc; screen:\n%s", h.Driver.Screen())
	}

	// Give any (wrongly-dispatched) save command a moment to land before
	// asserting nothing fired.
	time.Sleep(250 * time.Millisecond)

	if calls := h.Daemon.UpdateSessionCalls(); len(calls) != 0 {
		t.Fatalf("expected no UpdateSession calls after esc-cancel; got %d: %+v", len(calls), calls)
	}

	// Daemon-side title must still be the original.
	for _, s := range h.Daemon.Sessions() {
		if s.Id == "sess-aaa-111" && s.Title != "Add dark mode" {
			t.Fatalf("session title mutated on esc-cancel: got %q, want %q", s.Title, "Add dark mode")
		}
	}
}
