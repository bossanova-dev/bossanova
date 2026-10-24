package tui

import (
	"github.com/recurser/boss/api/tui/ui"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// detectNewRotationEvents diffs the newest rotation-event id per session
// against prev, returning the refreshed map and a toast string for every
// session whose newest rotation-event id changed. The first observation
// (prev == nil) seeds the map without emitting any toasts, so a TUI restart
// does not replay historical rotations.
func detectNewRotationEvents(prev map[string]string, sessions []*pb.Session) (map[string]string, []string) {
	next := make(map[string]string, len(sessions))
	var toasts []string
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		evs := sess.GetRotationEvents()
		if len(evs) == 0 {
			continue
		}
		latest := evs[0] // hydrated newest-first
		next[sess.GetId()] = latest.GetId()
		if prev == nil {
			continue // seed pass: record state without toasting
		}
		if prev[sess.GetId()] != latest.GetId() {
			// A generic (UNSPECIFIED) event has no outcome label (BOS-432), so
			// fall back to its detail — and a bare "rotation event" only if
			// neither is present — so the toast is never a bare "<title>: ".
			summary := ui.RotationEventLabel(latest)
			if summary == "" {
				summary = ui.RotationEventDetail(latest)
			}
			if summary == "" {
				summary = "rotation event"
			}
			toasts = append(toasts, sess.GetTitle()+": "+summary)
		}
	}
	return next, toasts
}
