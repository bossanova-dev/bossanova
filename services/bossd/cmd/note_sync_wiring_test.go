package main

import (
	"testing"

	"github.com/recurser/bossd/internal/notesync"
)

// TestNoteSyncNudgerKeepsNilWorkerNil guards the typed-nil trap: a local-only
// daemon has no worker, and the server must see a nil interface (and so report
// is_worker_configured = false) rather than a non-nil interface wrapping a nil
// pointer.
func TestNoteSyncNudgerKeepsNilWorkerNil(t *testing.T) {
	if got := noteSyncNudger(nil); got != nil {
		t.Errorf("noteSyncNudger(nil) = %#v, want a nil interface", got)
	}
	w := notesync.New(notesync.Config{})
	if got := noteSyncNudger(w); got == nil {
		t.Error("noteSyncNudger(worker) = nil, want the worker")
	}
}
