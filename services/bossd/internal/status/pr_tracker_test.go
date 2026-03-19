package status

import (
	"sync"
	"testing"

	"github.com/recurser/bossalib/vcs"
)

func TestPRTracker_Set_and_Get(t *testing.T) {
	tr := NewPRTracker()

	tr.Set("sess-1", vcs.PRDisplayInfo{Status: vcs.PRDisplayStatusChecking, HasFailures: true})

	e := tr.Get("sess-1")
	if e == nil {
		t.Fatal("expected entry, got nil")
	}
	if e.Status != vcs.PRDisplayStatusChecking {
		t.Errorf("Status = %d, want %d", e.Status, vcs.PRDisplayStatusChecking)
	}
	if !e.HasFailures {
		t.Error("expected HasFailures=true")
	}
	if e.UpdatedAt.IsZero() {
		t.Error("expected non-zero UpdatedAt")
	}
}

func TestPRTracker_Get_NotFound(t *testing.T) {
	tr := NewPRTracker()
	if e := tr.Get("nonexistent"); e != nil {
		t.Errorf("expected nil for nonexistent key, got %v", e)
	}
}

func TestPRTracker_Set_Overwrites(t *testing.T) {
	tr := NewPRTracker()

	tr.Set("sess-1", vcs.PRDisplayInfo{Status: vcs.PRDisplayStatusIdle})
	tr.Set("sess-1", vcs.PRDisplayInfo{Status: vcs.PRDisplayStatusMerged})

	e := tr.Get("sess-1")
	if e == nil {
		t.Fatal("expected entry, got nil")
	}
	if e.Status != vcs.PRDisplayStatusMerged {
		t.Errorf("Status = %d, want %d", e.Status, vcs.PRDisplayStatusMerged)
	}
}

func TestPRTracker_GetBatch(t *testing.T) {
	tr := NewPRTracker()

	tr.Set("sess-1", vcs.PRDisplayInfo{Status: vcs.PRDisplayStatusPassing})
	tr.Set("sess-2", vcs.PRDisplayInfo{Status: vcs.PRDisplayStatusFailing, HasFailures: true})

	batch := tr.GetBatch([]string{"sess-1", "sess-2", "sess-3"})

	// sess-1 present.
	if e, ok := batch["sess-1"]; !ok || e.Status != vcs.PRDisplayStatusPassing {
		t.Errorf("sess-1: expected Passing, got %v", batch["sess-1"])
	}

	// sess-2 present with HasFailures.
	if e, ok := batch["sess-2"]; !ok || e.Status != vcs.PRDisplayStatusFailing || !e.HasFailures {
		t.Errorf("sess-2: expected Failing+HasFailures, got %v", batch["sess-2"])
	}

	// sess-3 not present.
	if _, ok := batch["sess-3"]; ok {
		t.Error("sess-3: expected not in batch")
	}
}

func TestPRTracker_GetBatch_ReturnsCopies(t *testing.T) {
	tr := NewPRTracker()
	tr.Set("sess-1", vcs.PRDisplayInfo{Status: vcs.PRDisplayStatusIdle})

	batch := tr.GetBatch([]string{"sess-1"})

	// Mutating the returned entry should not affect the tracker's internal state.
	batch["sess-1"].Status = vcs.PRDisplayStatusMerged

	e := tr.Get("sess-1")
	if e.Status != vcs.PRDisplayStatusIdle {
		t.Errorf("internal entry mutated: Status = %d, want %d", e.Status, vcs.PRDisplayStatusIdle)
	}
}

func TestPRTracker_Remove(t *testing.T) {
	tr := NewPRTracker()
	tr.Set("sess-1", vcs.PRDisplayInfo{Status: vcs.PRDisplayStatusPassing})

	tr.Remove("sess-1")

	if e := tr.Get("sess-1"); e != nil {
		t.Errorf("expected nil after remove, got %v", e)
	}
}

func TestPRTracker_Remove_Nonexistent(t *testing.T) {
	tr := NewPRTracker()
	// Should not panic.
	tr.Remove("nonexistent")
}

func TestPRTracker_Concurrency(t *testing.T) {
	tr := NewPRTracker()
	var wg sync.WaitGroup
	const n = 100

	// Concurrent writers.
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "sess-" + string(rune('A'+i%26))
			tr.Set(id, vcs.PRDisplayInfo{Status: vcs.PRDisplayStatusChecking})
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
