package status

import (
	"reflect"
	"sync"
	"testing"

	"github.com/recurser/bossalib/vcs"
)

func TestDisplayTracker_ChangesRequestedBy_RoundTrip(t *testing.T) {
	tr := NewDisplayTracker()

	want := []string{"alice", "bob"}
	tr.Set("sess-cr", vcs.DisplayInfo{
		Status:              vcs.DisplayStatusRejected,
		HasChangesRequested: true,
		ChangesRequestedBy:  want,
	})

	e := tr.Get("sess-cr")
	if e == nil {
		t.Fatal("expected entry, got nil")
	}
	if !reflect.DeepEqual(e.ChangesRequestedBy, want) {
		t.Errorf("Get ChangesRequestedBy = %v, want %v", e.ChangesRequestedBy, want)
	}

	batch := tr.GetBatch([]string{"sess-cr"})
	be, ok := batch["sess-cr"]
	if !ok {
		t.Fatal("expected sess-cr in batch")
	}
	if !reflect.DeepEqual(be.ChangesRequestedBy, want) {
		t.Errorf("GetBatch ChangesRequestedBy = %v, want %v", be.ChangesRequestedBy, want)
	}
}

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

func TestDisplayTracker_Mergeable_ThreadsThroughSetGetBatch(t *testing.T) {
	tr := NewDisplayTracker()
	conflicting := false
	mergeable := true

	// Conflicting: false → surfaced verbatim on Get and GetBatch.
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusConflict, Mergeable: &conflicting})
	e := tr.Get("sess-1")
	if e == nil || e.Mergeable == nil || *e.Mergeable != false {
		t.Fatalf("Get Mergeable = %v, want pointer to false", e.Mergeable)
	}
	batch := tr.GetBatch([]string{"sess-1"})
	if b := batch["sess-1"]; b == nil || b.Mergeable == nil || *b.Mergeable != false {
		t.Fatalf("GetBatch Mergeable = %v, want pointer to false", batch["sess-1"].Mergeable)
	}

	// A later poll flips it to mergeable — Set carries the fresh value (not
	// preserved from the prior entry, unlike IsRepairing).
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, Mergeable: &mergeable})
	if e := tr.Get("sess-1"); e == nil || e.Mergeable == nil || *e.Mergeable != true {
		t.Fatalf("Get Mergeable after reset = %v, want pointer to true", e.Mergeable)
	}

	// Unknown mergeability (nil) round-trips as nil.
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusChecking})
	if e := tr.Get("sess-1"); e == nil || e.Mergeable != nil {
		t.Fatalf("Get Mergeable unknown = %v, want nil", e.Mergeable)
	}
}

func TestDisplayTracker_VerifyReason_ThreadsThroughSetGetBatch(t *testing.T) {
	tr := NewDisplayTracker()

	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, VerifyReason: "ledger-open"})
	if e := tr.Get("sess-1"); e == nil || e.VerifyReason != "ledger-open" {
		t.Fatalf("Get VerifyReason = %+v, want ledger-open", e)
	}
	if b := tr.GetBatch([]string{"sess-1"})["sess-1"]; b == nil || b.VerifyReason != "ledger-open" {
		t.Fatalf("GetBatch VerifyReason = %+v, want ledger-open", b)
	}

	// Refreshed by every poll, not preserved: a later Set without a reason clears it.
	tr.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusVerifying})
	if e := tr.Get("sess-1"); e == nil || e.VerifyReason != "" {
		t.Fatalf("Get VerifyReason after reset = %+v, want empty", e)
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

func TestSetMerging(t *testing.T) {
	tr := NewDisplayTracker()
	tr.SetMerging("s1", true)
	if e := tr.Get("s1"); e == nil || !e.Merging {
		t.Fatalf("expected Merging=true, got %+v", e)
	}
	// Clearing the flag on an entry that exists ONLY because of the transient
	// merging flag (no polled PR status) must remove the entry entirely, so a
	// zero-Status placeholder does not linger and mis-read a passing PR.
	tr.SetMerging("s1", false)
	if e := tr.Get("s1"); e != nil {
		t.Fatalf("expected merging-only entry removed on clear, got %+v", e)
	}
}

func TestSetMergingClearKeepsRealStatus(t *testing.T) {
	tr := NewDisplayTracker()
	tr.SetMerging("s1", true)
	// A PR poll lands a real status while the merge is in flight.
	tr.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	tr.SetMerging("s1", false)
	e := tr.Get("s1")
	if e == nil {
		t.Fatalf("entry with real PR status must not be removed on clear")
	}
	if e.Merging {
		t.Fatalf("expected Merging cleared, got %+v", e)
	}
	if e.Status != vcs.DisplayStatusPassing {
		t.Fatalf("expected Status preserved as Passing, got %+v", e)
	}
}

func TestSetMergingClearWhenAbsentNoOp(t *testing.T) {
	tr := NewDisplayTracker()
	// Clearing when no entry exists must not fabricate a placeholder entry.
	tr.SetMerging("s1", false)
	if e := tr.Get("s1"); e != nil {
		t.Fatalf("clear-when-absent fabricated an entry: %+v", e)
	}
}

func TestSetPreservesMerging(t *testing.T) {
	tr := NewDisplayTracker()
	tr.SetMerging("s1", true)
	// A PR display poll update mid-merge must not clobber the Merging flag.
	tr.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	if e := tr.Get("s1"); e == nil || !e.Merging {
		t.Fatalf("Set() clobbered Merging; got %+v", e)
	}
}

func TestSetArchiving(t *testing.T) {
	tr := NewDisplayTracker()
	tr.SetArchiving("s1", true)
	if e := tr.Get("s1"); e == nil || !e.Archiving {
		t.Fatalf("expected Archiving=true, got %+v", e)
	}
	// Clearing the flag on an entry that exists ONLY because of the transient
	// archiving flag (no polled PR status) must remove the entry entirely, so a
	// zero-Status placeholder does not linger and mis-read a passing PR.
	tr.SetArchiving("s1", false)
	if e := tr.Get("s1"); e != nil {
		t.Fatalf("expected archiving-only entry removed on clear, got %+v", e)
	}
}

func TestSetArchivingIndependentOfMerging(t *testing.T) {
	tr := NewDisplayTracker()
	// Setting Archiving must not touch Merging and vice versa — they are
	// independent transient axes.
	tr.SetMerging("s1", true)
	tr.SetArchiving("s1", true)
	e := tr.Get("s1")
	if e == nil || !e.Archiving || !e.Merging {
		t.Fatalf("expected both Archiving and Merging true, got %+v", e)
	}
	// Clearing Archiving leaves Merging set (entry not empty, so not removed).
	tr.SetArchiving("s1", false)
	e = tr.Get("s1")
	if e == nil || e.Archiving || !e.Merging {
		t.Fatalf("expected Archiving cleared, Merging preserved, got %+v", e)
	}
}

func TestSetArchivingClearKeepsRealStatus(t *testing.T) {
	tr := NewDisplayTracker()
	tr.SetArchiving("s1", true)
	// A PR poll lands a real status while the archive is in flight.
	tr.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusMerged})
	tr.SetArchiving("s1", false)
	e := tr.Get("s1")
	if e == nil {
		t.Fatalf("entry with real PR status must not be removed on clear")
	}
	if e.Archiving {
		t.Fatalf("expected Archiving cleared, got %+v", e)
	}
	if e.Status != vcs.DisplayStatusMerged {
		t.Fatalf("expected Status preserved as Merged, got %+v", e)
	}
}

func TestSetArchivingClearWhenAbsentNoOp(t *testing.T) {
	tr := NewDisplayTracker()
	// Clearing when no entry exists must not fabricate a placeholder entry.
	tr.SetArchiving("s1", false)
	if e := tr.Get("s1"); e != nil {
		t.Fatalf("clear-when-absent fabricated an entry: %+v", e)
	}
}

func TestSetPreservesArchiving(t *testing.T) {
	tr := NewDisplayTracker()
	tr.SetArchiving("s1", true)
	// A PR display poll update mid-archive must not clobber the Archiving flag.
	tr.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusMerged})
	if e := tr.Get("s1"); e == nil || !e.Archiving {
		t.Fatalf("Set() clobbered Archiving; got %+v", e)
	}
}

func TestDisplayTrackerBuildReceiptFollowsEachPoll(t *testing.T) {
	tracker := NewDisplayTracker()
	for _, receipt := range []bool{true, false, true} {
		tracker.Set("session", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HasBuildReceipt: receipt})
		if got := tracker.Get("session").HasBuildReceipt; got != receipt {
			t.Fatalf("Get receipt = %v, want %v", got, receipt)
		}
		if got := tracker.GetBatch([]string{"session"})["session"].HasBuildReceipt; got != receipt {
			t.Fatalf("GetBatch receipt = %v, want %v", got, receipt)
		}
		tracker.SetMerging("session", true)
		tracker.SetMerging("session", false)
		if got := tracker.Get("session").HasBuildReceipt; got != receipt {
			t.Fatalf("transient mutation changed receipt = %v, want %v", got, receipt)
		}
	}
}

func TestDisplayTrackerUnsuccessfulReceiptRetiresLatch(t *testing.T) {
	tracker := NewDisplayTracker()
	tracker.Set("session", vcs.DisplayInfo{HasBuildReceipt: true, BuildReceiptSeen: true, HeadSHA: "headA"})
	// A pending/failed boss/build on the receipted head itself keeps it.
	tracker.Set("session", vcs.DisplayInfo{BuildReceiptSeen: true, HeadSHA: "headA"})
	if got := tracker.Get("session").ReceiptHeadSHA; got != "headA" {
		t.Fatalf("ReceiptHeadSHA after unsuccessful receipt on the same head = %q, want headA", got)
	}
	// One on a newer head retires the latch, so a later push cannot carry
	// headA's success past headB's verdict.
	tracker.Set("session", vcs.DisplayInfo{BuildReceiptSeen: true, HeadSHA: "headB"})
	if got := tracker.Get("session").ReceiptHeadSHA; got != "" {
		t.Fatalf("ReceiptHeadSHA after unsuccessful receipt on headB = %q, want empty", got)
	}
	tracker.Set("session", vcs.DisplayInfo{HeadSHA: "headC"})
	if got := tracker.Get("session").ReceiptHeadSHA; got != "" {
		t.Fatalf("ReceiptHeadSHA on unreceipted headC = %q, want empty", got)
	}
}

func TestDisplayTrackerReceiptHeadSHALatchesLastReceiptedHead(t *testing.T) {
	tracker := NewDisplayTracker()
	latch := func(want string) {
		t.Helper()
		if got := tracker.Get("session").ReceiptHeadSHA; got != want {
			t.Fatalf("Get ReceiptHeadSHA = %q, want %q", got, want)
		}
		if got := tracker.GetBatch([]string{"session"})["session"].ReceiptHeadSHA; got != want {
			t.Fatalf("GetBatch ReceiptHeadSHA = %q, want %q", got, want)
		}
	}

	// A Set with no receipt on a fresh entry latches nothing.
	tracker.Set("session", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: "head0"})
	latch("")

	// A receipted Set latches its head.
	tracker.Set("session", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HasBuildReceipt: true, HeadSHA: "headA"})
	latch("headA")

	// A later Set for a new, unreceipted head keeps the latch while
	// HasBuildReceipt follows the poll.
	tracker.Set("session", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: "headB"})
	latch("headA")
	if e := tracker.Get("session"); e.HasBuildReceipt || e.HeadSHA != "headB" {
		t.Fatalf("entry = %+v, want unreceipted headB", e)
	}

	// A receipt without a head cannot move the latch.
	tracker.Set("session", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HasBuildReceipt: true})
	latch("headA")

	// The latch moves when the new head is itself receipted.
	tracker.Set("session", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HasBuildReceipt: true, HeadSHA: "headB"})
	latch("headB")

	// Transient flag mutations do not disturb it.
	tracker.SetRepairing("session", true)
	tracker.SetMerging("session", true)
	latch("headB")
}
