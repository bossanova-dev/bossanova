package status

import (
	"sync"
	"testing"

	"github.com/recurser/bossalib/vcs"
)

func TestDisplayTracker_Set_and_Get(t *testing.T) {
	tr := NewDisplayTracker()

	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusChecking, HasFailures: true})

	e := tr.Get("sess-1")
	if e == nil {
		t.Fatal("expected entry, got nil")
		return
	}
	if e.Status != vcs.DisplayStatusChecking {
		t.Errorf("Status = %d, want %d", e.Status, vcs.DisplayStatusChecking)
	}
	if !e.HasFailures {
		t.Error("expected HasFailures=true")
	}
	if e.UpdatedAt.IsZero() {
		t.Error("expected non-zero UpdatedAt")
	}
}

func TestDisplayTracker_Get_NotFound(t *testing.T) {
	tr := NewDisplayTracker()
	if e := tr.Get("nonexistent"); e != nil {
		t.Errorf("expected nil for nonexistent key, got %v", e)
	}
}

func TestDisplayTracker_Set_Overwrites(t *testing.T) {
	tr := NewDisplayTracker()

	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusIdle})
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusMerged})

	e := tr.Get("sess-1")
	if e == nil {
		t.Fatal("expected entry, got nil")
		return
	}
	if e.Status != vcs.DisplayStatusMerged {
		t.Errorf("Status = %d, want %d", e.Status, vcs.DisplayStatusMerged)
	}
}

func TestDisplayTracker_GetBatch(t *testing.T) {
	tr := NewDisplayTracker()

	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	tr.Set("sess-2", vcs.DisplayInfo{Status: vcs.DisplayStatusFailing, HasFailures: true})

	batch := tr.GetBatch([]string{"sess-1", "sess-2", "sess-3"})

	// sess-1 present.
	if e, ok := batch["sess-1"]; !ok || e.Status != vcs.DisplayStatusPassing {
		t.Errorf("sess-1: expected Passing, got %v", batch["sess-1"])
	}

	// sess-2 present with HasFailures.
	if e, ok := batch["sess-2"]; !ok || e.Status != vcs.DisplayStatusFailing || !e.HasFailures {
		t.Errorf("sess-2: expected Failing+HasFailures, got %v", batch["sess-2"])
	}

	// sess-3 not present.
	if _, ok := batch["sess-3"]; ok {
		t.Error("sess-3: expected not in batch")
	}
}

func TestDisplayTracker_GetBatch_ReturnsCopies(t *testing.T) {
	tr := NewDisplayTracker()
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusIdle})

	batch := tr.GetBatch([]string{"sess-1"})

	// Mutating the returned entry should not affect the tracker's internal state.
	batch["sess-1"].Status = vcs.DisplayStatusMerged

	e := tr.Get("sess-1")
	if e.Status != vcs.DisplayStatusIdle {
		t.Errorf("internal entry mutated: Status = %d, want %d", e.Status, vcs.DisplayStatusIdle)
	}
}

func TestDisplayTracker_Remove(t *testing.T) {
	tr := NewDisplayTracker()
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})

	tr.Remove("sess-1")

	if e := tr.Get("sess-1"); e != nil {
		t.Errorf("expected nil after remove, got %v", e)
	}
}

func TestDisplayTracker_Remove_Nonexistent(t *testing.T) {
	tr := NewDisplayTracker()
	// Should not panic.
	tr.Remove("nonexistent")
}

func TestDisplayTracker_Concurrency(t *testing.T) {
	tr := NewDisplayTracker()
	var wg sync.WaitGroup
	const n = 100

	// Concurrent writers.
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "sess-" + string(rune('A'+i%26))
			tr.Set(id, vcs.DisplayInfo{Status: vcs.DisplayStatusChecking})
		}(i)
	}

	// Concurrent readers.
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "sess-" + string(rune('A'+i%26))
			tr.Get(id)
		}(i)
	}

	// Concurrent batch reads.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.GetBatch([]string{"sess-A", "sess-B", "sess-C"})
		}()
	}

	// Concurrent removes.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "sess-" + string(rune('A'+i%26))
			tr.Remove(id)
		}(i)
	}

	wg.Wait()
}

func TestDisplayTracker_OnChange_InitialSet(t *testing.T) {
	tr := NewDisplayTracker()

	done := make(chan struct{})
	var capturedSessionID string
	var capturedOld, capturedNew *DisplayEntry

	tr.SetOnChange(func(sessionID string, oldEntry, newEntry *DisplayEntry) {
		capturedSessionID = sessionID
		capturedOld = oldEntry
		capturedNew = newEntry
		close(done)
	})

	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	<-done

	if capturedSessionID != "sess-1" {
		t.Errorf("sessionID = %q, want %q", capturedSessionID, "sess-1")
	}
	if capturedOld != nil {
		t.Errorf("oldEntry = %v, want nil for initial set", capturedOld)
	}
	if capturedNew == nil {
		t.Fatal("newEntry is nil")
	}
	if capturedNew.Status != vcs.DisplayStatusPassing {
		t.Errorf("newEntry.Status = %d, want %d", capturedNew.Status, vcs.DisplayStatusPassing)
	}
}

func TestDisplayTracker_OnChange_StatusChange(t *testing.T) {
	tr := NewDisplayTracker()

	// Set initial status
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusChecking})

	done := make(chan struct{})
	var capturedOld, capturedNew *DisplayEntry

	tr.SetOnChange(func(sessionID string, oldEntry, newEntry *DisplayEntry) {
		capturedOld = oldEntry
		capturedNew = newEntry
		close(done)
	})

	// Change status - should trigger callback
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusFailing, HasFailures: true})
	<-done

	if capturedOld == nil {
		t.Fatal("oldEntry is nil")
	}
	if capturedOld.Status != vcs.DisplayStatusChecking {
		t.Errorf("oldEntry.Status = %d, want %d", capturedOld.Status, vcs.DisplayStatusChecking)
	}

	if capturedNew == nil {
		t.Fatal("newEntry is nil")
	}
	if capturedNew.Status != vcs.DisplayStatusFailing {
		t.Errorf("newEntry.Status = %d, want %d", capturedNew.Status, vcs.DisplayStatusFailing)
	}
	if !capturedNew.HasFailures {
		t.Error("expected newEntry.HasFailures=true")
	}
}

func TestDisplayTracker_OnChange_NoCallbackOnSameStatus(t *testing.T) {
	tr := NewDisplayTracker()

	// Set initial status
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})

	called := false
	tr.SetOnChange(func(sessionID string, oldEntry, newEntry *DisplayEntry) {
		called = true
	})

	// Set same status again - should NOT trigger callback
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})

	// Wait briefly to ensure callback doesn't fire
	// Since the callback won't be called, we can't use a channel-based wait
	// In a real test, we might use a timeout or mock time
	// For this test, we'll just check the flag
	if called {
		t.Error("onChange called when status did not change")
	}
}

func TestDisplayTracker_OnChange_NilCallback(t *testing.T) {
	tr := NewDisplayTracker()

	// Setting with nil callback should not panic
	tr.SetOnChange(nil)
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusFailing})
}

func TestSetSettingUp(t *testing.T) {
	tr := NewDisplayTracker()
	tr.SetSettingUp("s1", true)
	if e := tr.Get("s1"); e == nil || !e.SettingUp {
		t.Fatalf("expected SettingUp=true, got %+v", e)
	}
	// Clearing the flag on an entry that exists ONLY because of the
	// transient setup flag (no polled PR status yet) must remove the entry
	// entirely. Otherwise a zero-Status placeholder lingers, and callers
	// that treat entry presence as authoritative (e.g. MergeSession's
	// "no entry -> allow merge" fallback) would mis-read a passing PR as
	// "not passing".
	tr.SetSettingUp("s1", false)
	if e := tr.Get("s1"); e != nil {
		t.Fatalf("expected setup-only entry removed on clear, got %+v", e)
	}
}

func TestSetSettingUpClearKeepsRealStatus(t *testing.T) {
	tr := NewDisplayTracker()
	tr.SetSettingUp("s1", true)
	// A PR poll lands a real status before setup finishes.
	tr.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	tr.SetSettingUp("s1", false)
	e := tr.Get("s1")
	if e == nil {
		t.Fatalf("entry with real PR status must not be removed on clear")
	}
	if e.SettingUp {
		t.Fatalf("expected SettingUp cleared, got %+v", e)
	}
	if e.Status != vcs.DisplayStatusPassing {
		t.Fatalf("expected Status preserved as Passing, got %+v", e)
	}
}

func TestSetSettingUpClearWhenAbsentNoOp(t *testing.T) {
	tr := NewDisplayTracker()
	// Clearing when no entry exists must not fabricate a placeholder entry.
	tr.SetSettingUp("s1", false)
	if e := tr.Get("s1"); e != nil {
		t.Fatalf("clear-when-absent fabricated an entry: %+v", e)
	}
}

func TestSetPreservesSettingUp(t *testing.T) {
	tr := NewDisplayTracker()
	tr.SetSettingUp("s1", true)
	// A PR display poll update must not clobber the SettingUp flag.
	tr.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusDraft})
	if e := tr.Get("s1"); e == nil || !e.SettingUp {
		t.Fatalf("Set() clobbered SettingUp; got %+v", e)
	}
}
