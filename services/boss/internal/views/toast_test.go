package views

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestToast_ShowRendersText verifies Show makes the text visible in View and
// that an empty toast renders nothing.
func TestToast_ShowRendersText(t *testing.T) {
	tm := newToastModel()
	if got := tm.View(80); got != "" {
		t.Errorf("empty toast View = %q, want empty", got)
	}
	tm, cmd := tm.Show("session-x: acct-a switched to acct-b")
	if cmd == nil {
		t.Error("Show returned a nil expire command")
	}
	got := tm.View(80)
	if !strings.Contains(got, "acct-a switched to acct-b") {
		t.Errorf("toast View = %q, want it to contain the shown text", got)
	}
}

func TestToastDurationIsSixSeconds(t *testing.T) {
	if got, want := toastDuration, 6*time.Second; got != want {
		t.Fatalf("toastDuration = %s, want %s", got, want)
	}
}

// TestToast_ExpireClearsByID verifies a matching-id expire clears the toast,
// while a stale id (from a superseded toast) is ignored.
func TestToast_ExpireClearsByID(t *testing.T) {
	tm := newToastModel()
	tm, _ = tm.Show("first")

	// A stale expire (id from before the current toast) must not clear it.
	tm, consumed := tm.Update(toastExpireMsg{id: tm.id - 1})
	if consumed {
		t.Error("expire msg reported consumed; want false")
	}
	if tm.View(80) == "" {
		t.Error("stale expire cleared the toast; want it to remain visible")
	}

	// The matching expire clears it.
	tm, _ = tm.Update(toastExpireMsg{id: tm.id})
	if got := tm.View(80); got != "" {
		t.Errorf("toast View = %q after matching expire, want empty", got)
	}
}

// TestToast_EscDismisses verifies Esc clears a visible toast and reports the
// key consumed, while Esc on an empty toast is not consumed.
func TestToast_EscDismisses(t *testing.T) {
	tm := newToastModel()
	if _, consumed := tm.Update(tea.KeyPressMsg{Code: tea.KeyEsc}); consumed {
		t.Error("Esc on empty toast reported consumed; want false")
	}

	tm, _ = tm.Show("visible")
	tm, consumed := tm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !consumed {
		t.Error("Esc on visible toast reported not consumed; want true")
	}
	if got := tm.View(80); got != "" {
		t.Errorf("toast View = %q after Esc, want empty", got)
	}
}

// TestDetectNewRotationEvents_SeedThenDiff verifies the first observation seeds
// silently, an unchanged newest event produces no toast, and a changed newest
// event produces exactly one toast naming the session.
func TestDetectNewRotationEvents_SeedThenDiff(t *testing.T) {
	sess := &pb.Session{
		Id:    "sess-1",
		Title: "Fix the bug",
		RotationEvents: []*pb.RotationEvent{{
			Id:          "ev-1",
			Outcome:     pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
			FromAccount: "acct-a",
			ToAccount:   "acct-b",
		}},
	}

	// Seed pass (prev == nil): no toasts, map recorded.
	seen, toasts := detectNewRotationEvents(nil, []*pb.Session{sess})
	if len(toasts) != 0 {
		t.Errorf("seed pass toasts = %v, want none", toasts)
	}
	if seen["sess-1"] != "ev-1" {
		t.Errorf("seed map[sess-1] = %q, want ev-1", seen["sess-1"])
	}

	// Unchanged newest event: no toast.
	seen, toasts = detectNewRotationEvents(seen, []*pb.Session{sess})
	if len(toasts) != 0 {
		t.Errorf("unchanged toasts = %v, want none", toasts)
	}

	// New rotation event lands: exactly one toast naming the session.
	sess.RotationEvents = []*pb.RotationEvent{{
		Id:          "ev-2",
		Outcome:     pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
		FromAccount: "acct-b",
		ToAccount:   "acct-c",
	}}
	seen, toasts = detectNewRotationEvents(seen, []*pb.Session{sess})
	if len(toasts) != 1 {
		t.Fatalf("changed toasts = %v, want exactly one", toasts)
	}
	if !strings.Contains(toasts[0], "Fix the bug") || !strings.Contains(toasts[0], "acct-b switched to acct-c") {
		t.Errorf("toast = %q, want session title + rotation label", toasts[0])
	}
	if seen["sess-1"] != "ev-2" {
		t.Errorf("post-diff map[sess-1] = %q, want ev-2", seen["sess-1"])
	}
}
