package tui

import (
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

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

// TestDetectNewRotationEvents_UnspecifiedFallsBackToDetail ensures a rotation
// event with no meaningful outcome label (UNSPECIFIED, e.g. a BOS-409
// stale-port notice) still produces an informative toast — the detail string,
// not a bare "<title>: " with the label dropped.
func TestDetectNewRotationEvents_UnspecifiedFallsBackToDetail(t *testing.T) {
	sess := &pb.Session{
		Id:    "sess-1",
		Title: "Fix the bug",
		RotationEvents: []*pb.RotationEvent{{
			Id:      "ev-1",
			Outcome: pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
		}},
	}
	seen, _ := detectNewRotationEvents(nil, []*pb.Session{sess}) // seed pass

	sess.RotationEvents = []*pb.RotationEvent{{
		Id:      "ev-2",
		Outcome: pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED,
		Detail:  "stale failover-proxy port (BOS-409)",
	}}
	_, toasts := detectNewRotationEvents(seen, []*pb.Session{sess})
	if len(toasts) != 1 {
		t.Fatalf("toasts = %v, want exactly one", toasts)
	}
	if !strings.Contains(toasts[0], "stale failover-proxy port (BOS-409)") {
		t.Errorf("toast = %q, want the detail string as fallback label", toasts[0])
	}
	if strings.HasSuffix(toasts[0], ": ") {
		t.Errorf("toast = %q, should not end with a bare title separator", toasts[0])
	}
}
