package status

import (
	"sync"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestUpdate_and_Get(t *testing.T) {
	tr := NewTracker()
	now := time.Now()

	tr.Update("chat-1", pb.ChatStatus_CHAT_STATUS_WORKING, now)

	e := tr.Get("chat-1")
	if e == nil {
		t.Fatal("expected entry, got nil")
		return
	}
	if e.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("expected WORKING, got %v", e.Status)
	}
	if !e.LastOutputAt.Equal(now) {
		t.Errorf("expected LastOutputAt %v, got %v", now, e.LastOutputAt)
	}
}

func TestGet_NotFound(t *testing.T) {
	tr := NewTracker()
	if e := tr.Get("nonexistent"); e != nil {
		t.Errorf("expected nil for nonexistent key, got %v", e)
	}
}

func TestGet_Stale(t *testing.T) {
	tr := NewTracker()
	now := time.Now()

	tr.Update("chat-1", pb.ChatStatus_CHAT_STATUS_WORKING, now)

	// Manually backdate the entry to simulate staleness.
	tr.mu.Lock()
	tr.entries["chat-1"].ReceivedAt = now.Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	e := tr.Get("chat-1")
	if e != nil {
		t.Errorf("expected nil for stale entry, got %v", e)
	}
}

func TestGetBatch_Mixed(t *testing.T) {
	tr := NewTracker()
	now := time.Now()

	tr.Update("chat-1", pb.ChatStatus_CHAT_STATUS_WORKING, now)
	tr.Update("chat-2", pb.ChatStatus_CHAT_STATUS_IDLE, now)

	// Make chat-2 stale.
	tr.mu.Lock()
	tr.entries["chat-2"].ReceivedAt = now.Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	batch := tr.GetBatch([]string{"chat-1", "chat-2", "chat-3"})

	// chat-1 should be working.
	if e, ok := batch["chat-1"]; !ok || e.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("chat-1: expected WORKING, got %v", batch["chat-1"])
	}

	// chat-2 should be stopped (stale).
	if e, ok := batch["chat-2"]; !ok || e.Status != pb.ChatStatus_CHAT_STATUS_STOPPED {
		t.Errorf("chat-2: expected STOPPED (stale), got %v", batch["chat-2"])
	}

	// chat-3 should not exist.
	if _, ok := batch["chat-3"]; ok {
		t.Error("chat-3: expected not in batch")
	}
}

func TestRemove(t *testing.T) {
	tr := NewTracker()
	tr.Update("chat-1", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())

	tr.Remove("chat-1")

	if e := tr.Get("chat-1"); e != nil {
		t.Errorf("expected nil after remove, got %v", e)
	}
}

func TestCleanup(t *testing.T) {
	tr := NewTracker()
	now := time.Now()

	tr.Update("fresh", pb.ChatStatus_CHAT_STATUS_WORKING, now)
	tr.Update("stale", pb.ChatStatus_CHAT_STATUS_IDLE, now)

	// Make "stale" entry old.
	tr.mu.Lock()
	tr.entries["stale"].ReceivedAt = now.Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	tr.Cleanup()

	if e := tr.Get("fresh"); e == nil {
		t.Error("expected fresh entry to survive cleanup")
	}

	tr.mu.RLock()
	_, staleExists := tr.entries["stale"]
	tr.mu.RUnlock()
	if staleExists {
		t.Error("expected stale entry to be cleaned up")
	}
}

func TestSnapshot_FreshAndStale(t *testing.T) {
	tr := NewTracker()
	now := time.Now()

	tr.Update("fresh", pb.ChatStatus_CHAT_STATUS_WORKING, now)
	tr.Update("stale", pb.ChatStatus_CHAT_STATUS_IDLE, now)

	// Backdate the stale entry past StaleThreshold so Snapshot drops it.
	tr.mu.Lock()
	tr.entries["stale"].ReceivedAt = now.Add(-2 * StaleThreshold)
	tr.mu.Unlock()

	snap := tr.Snapshot()
	if _, ok := snap["fresh"]; !ok {
		t.Errorf("snapshot missing fresh entry: %+v", snap)
	}
	if _, ok := snap["stale"]; ok {
		t.Errorf("snapshot leaked stale entry: %+v", snap["stale"])
	}
	if snap["fresh"].Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("fresh.Status = %v, want WORKING", snap["fresh"].Status)
	}
}

func TestSnapshot_ReturnsCopies(t *testing.T) {
	tr := NewTracker()
	tr.Update("c1", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())

	snap := tr.Snapshot()
	// Mutating the returned value must not corrupt the tracker.
	snap["c1"].Status = pb.ChatStatus_CHAT_STATUS_STOPPED

	got := tr.Get("c1")
	if got == nil || got.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("snapshot leaked a live pointer; tracker state mutated: got=%+v", got)
	}
}

func TestSnapshot_UnchangedWorkingChatVisible(t *testing.T) {
	// Regression: a chat that's been WORKING since before the daemon's
	// last bosso reconnect must appear in Snapshot. Update suppresses
	// the OnUpdate hook on no-op heartbeats — the snapshot is the
	// recovery path for that case.
	tr := NewTracker()
	now := time.Now()
	tr.Update("long-running", pb.ChatStatus_CHAT_STATUS_WORKING, now)
	// Heartbeat with the same status; hook would NOT fire here.
	tr.Update("long-running", pb.ChatStatus_CHAT_STATUS_WORKING, now.Add(time.Second))

	snap := tr.Snapshot()
	entry, ok := snap["long-running"]
	if !ok {
		t.Fatal("Snapshot dropped a non-stale entry whose status hasn't changed")
	}
	if entry.Status != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Errorf("entry.Status = %v, want WORKING", entry.Status)
	}
}

func TestConcurrency(t *testing.T) {
	tr := NewTracker()
	var wg sync.WaitGroup
	const n = 100

	// Concurrent writers.
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "chat-" + string(rune('A'+i%26))
			tr.Update(id, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
		}(i)
	}

	// Concurrent readers.
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "chat-" + string(rune('A'+i%26))
			tr.Get(id)
		}(i)
	}

	// Concurrent cleanup.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.Cleanup()
		}()
	}

	wg.Wait()
}

func TestSetAuthFailed_and_AuthFailed(t *testing.T) {
	tr := NewTracker()

	if tr.AuthFailed("chat-1") {
		t.Fatal("AuthFailed on unknown chat = true, want false")
	}

	tr.SetAuthFailed("chat-1", true)
	if !tr.AuthFailed("chat-1") {
		t.Fatal("AuthFailed after SetAuthFailed(true) = false, want true")
	}

	// Clearing removes the marker immediately.
	tr.SetAuthFailed("chat-1", false)
	if tr.AuthFailed("chat-1") {
		t.Fatal("AuthFailed after SetAuthFailed(false) = true, want false")
	}
}

func TestSetAuthFailed_FiresOnAuthChangeOnlyOnTransition(t *testing.T) {
	tr := NewTracker()
	var mu sync.Mutex
	var fired []string
	tr.SetOnAuthChange(func(id string) {
		mu.Lock()
		fired = append(fired, id)
		mu.Unlock()
	})

	// absent → failed: a transition, fires once.
	tr.SetAuthFailed("chat-1", true)
	// failed → failed (the poller re-observes login-required every tick): no
	// transition, must NOT re-fire and storm the stream.
	tr.SetAuthFailed("chat-1", true)
	tr.SetAuthFailed("chat-1", true)
	// failed → cleared: a transition, fires once.
	tr.SetAuthFailed("chat-1", false)
	// cleared → cleared (poller keeps reporting not-login-required): no fire.
	tr.SetAuthFailed("chat-1", false)

	mu.Lock()
	got := append([]string(nil), fired...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("onAuthChange fired %d times (%v), want 2 (set + clear transitions)", len(got), got)
	}
	for _, id := range got {
		if id != "chat-1" {
			t.Errorf("onAuthChange fired for %q, want chat-1", id)
		}
	}
}

func TestSetAuthFailed_StaleMarkerCountsAsTransition(t *testing.T) {
	tr := NewTracker()
	var fires int
	var mu sync.Mutex
	tr.SetOnAuthChange(func(string) {
		mu.Lock()
		fires++
		mu.Unlock()
	})

	tr.SetAuthFailed("chat-1", true) // fire 1 (absent → fresh)

	// Age the marker past StaleThreshold: effectively absent per AuthFailed.
	tr.mu.Lock()
	tr.authFailed["chat-1"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	tr.SetAuthFailed("chat-1", true) // fire 2 (stale/effectively-absent → fresh)

	mu.Lock()
	got := fires
	mu.Unlock()
	if got != 2 {
		t.Fatalf("onAuthChange fired %d times, want 2 (a stale marker refreshing is a transition)", got)
	}
}

func TestSetAuthFailed_FalseClearsStaleMarkerWithAuthChange(t *testing.T) {
	tr := NewTracker()
	var fires int
	var mu sync.Mutex
	tr.SetOnAuthChange(func(string) {
		mu.Lock()
		fires++
		mu.Unlock()
	})

	tr.SetAuthFailed("chat-1", true) // fire 1 (absent -> fresh)
	tr.mu.Lock()
	tr.authFailed["chat-1"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	// Even though the stale marker already reads as AuthFailed=false locally,
	// remote stream state may still hold the last fresh "failed" delta. Clearing
	// the stale marker must publish a clear transition.
	tr.SetAuthFailed("chat-1", false) // fire 2 (stale marker removed)

	mu.Lock()
	got := fires
	mu.Unlock()
	if got != 2 {
		t.Fatalf("onAuthChange fired %d times, want 2 (set + stale clear)", got)
	}
}

func TestAuthFailed_Stale(t *testing.T) {
	tr := NewTracker()
	tr.SetAuthFailed("chat-1", true)

	// Backdate the marker beyond StaleThreshold: a stale marker must read as
	// absent (fail toward not flagging).
	tr.mu.Lock()
	tr.authFailed["chat-1"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	if tr.AuthFailed("chat-1") {
		t.Fatal("AuthFailed on stale marker = true, want false")
	}
}

func TestRemove_ClearsAuthFailed(t *testing.T) {
	tr := NewTracker()
	tr.SetAuthFailed("chat-1", true)
	tr.Remove("chat-1")
	if tr.AuthFailed("chat-1") {
		t.Fatal("AuthFailed after Remove = true, want false")
	}
}

func TestRemove_FiresOnAuthChangeWhenMarkerCleared(t *testing.T) {
	tr := NewTracker()
	var fired []string
	var mu sync.Mutex
	tr.SetOnAuthChange(func(id string) {
		mu.Lock()
		fired = append(fired, id)
		mu.Unlock()
	})

	tr.SetAuthFailed("chat-1", true) // set transition
	tr.Remove("chat-1")              // clear transition
	tr.Remove("chat-1")              // no marker left, no extra transition

	mu.Lock()
	got := append([]string(nil), fired...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("onAuthChange fired %d times (%v), want 2", len(got), got)
	}
	for _, id := range got {
		if id != "chat-1" {
			t.Errorf("onAuthChange fired for %q, want chat-1", id)
		}
	}
}

func TestUpdate_LimitResetCarried(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	resetAt := time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)

	tr.UpdateLimited("chat-1", resetAt, now)

	e := tr.Get("chat-1")
	if e == nil {
		t.Fatal("expected entry, got nil")
		return
	}
	if e.Status != pb.ChatStatus_CHAT_STATUS_LIMITED {
		t.Errorf("expected LIMITED, got %v", e.Status)
	}
	if !e.ResetAt.Equal(resetAt) {
		t.Errorf("expected ResetAt %v, got %v", resetAt, e.ResetAt)
	}
}

func TestUpdate_LimitEventFiresOncePerTransition(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	resetAt := time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)

	var mu sync.Mutex
	var enters, leaves int
	tr.SetOnLimitTransition(func(id string, entered bool) {
		mu.Lock()
		defer mu.Unlock()
		if id != "chat-1" {
			t.Errorf("onLimitTransition fired for %q, want chat-1", id)
		}
		if entered {
			enters++
		} else {
			leaves++
		}
	})

	// Enter LIMITED and re-poll it N times: exactly one enter event.
	for i := 0; i < 5; i++ {
		tr.UpdateLimited("chat-1", resetAt, now)
	}

	mu.Lock()
	gotEnters, gotLeaves := enters, leaves
	mu.Unlock()
	if gotEnters != 1 {
		t.Fatalf("enter events = %d, want 1 (once on the transition into LIMITED)", gotEnters)
	}
	if gotLeaves != 0 {
		t.Fatalf("leave events = %d, want 0 before leaving LIMITED", gotLeaves)
	}

	// Leave LIMITED: exactly one leave event.
	tr.Update("chat-1", pb.ChatStatus_CHAT_STATUS_IDLE, now)

	mu.Lock()
	gotEnters, gotLeaves = enters, leaves
	mu.Unlock()
	if gotEnters != 1 {
		t.Fatalf("enter events = %d after leaving, want still 1", gotEnters)
	}
	if gotLeaves != 1 {
		t.Fatalf("leave events = %d, want 1 (once on the transition out of LIMITED)", gotLeaves)
	}
}

func TestCleanup_RemovesStaleAuthFailed(t *testing.T) {
	tr := NewTracker()
	tr.SetAuthFailed("fresh", true)
	tr.SetAuthFailed("stale", true)
	tr.mu.Lock()
	tr.authFailed["stale"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	tr.Cleanup()

	tr.mu.RLock()
	_, freshOK := tr.authFailed["fresh"]
	_, staleOK := tr.authFailed["stale"]
	tr.mu.RUnlock()
	if !freshOK {
		t.Error("Cleanup removed a fresh auth marker")
	}
	if staleOK {
		t.Error("Cleanup kept a stale auth marker")
	}
}

func TestCleanup_FiresOnAuthChangeForRemovedStaleAuthMarkers(t *testing.T) {
	tr := NewTracker()
	var fired []string
	var mu sync.Mutex
	tr.SetOnAuthChange(func(id string) {
		mu.Lock()
		fired = append(fired, id)
		mu.Unlock()
	})

	tr.SetAuthFailed("fresh", true)
	tr.SetAuthFailed("stale", true)
	tr.mu.Lock()
	tr.authFailed["stale"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	tr.Cleanup()

	mu.Lock()
	got := append([]string(nil), fired...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("onAuthChange fired %d times (%v), want 3 (two sets + stale clear)", len(got), got)
	}
	if got[2] != "stale" {
		t.Fatalf("cleanup clear fired for %q, want stale", got[2])
	}
}

// TestCapturedOutput covers the BOS-477 ephemeral store: set then get returns
// the tail; an unseen key returns ""; and setting "" clears a prior value.
func TestCapturedOutput(t *testing.T) {
	tr := NewTracker()

	if got := tr.CapturedOutput("agent-1"); got != "" {
		t.Fatalf("unseen key CapturedOutput = %q, want \"\"", got)
	}

	tr.SetCapturedOutput("agent-1", "Error: Session ID abc is already in use")
	if got := tr.CapturedOutput("agent-1"); got != "Error: Session ID abc is already in use" {
		t.Fatalf("CapturedOutput = %q, want the stored tail", got)
	}

	tr.SetCapturedOutput("agent-1", "")
	if got := tr.CapturedOutput("agent-1"); got != "" {
		t.Fatalf("after clear CapturedOutput = %q, want \"\"", got)
	}
}

// TestCapturedOutput_RemovedOnRemove verifies Remove drops the ephemeral tail so
// it does not outlive the chat (BOS-477), matching the authFailed treatment.
func TestCapturedOutput_RemovedOnRemove(t *testing.T) {
	tr := NewTracker()
	tr.SetCapturedOutput("agent-1", "boom")
	tr.Remove("agent-1")
	if got := tr.CapturedOutput("agent-1"); got != "" {
		t.Fatalf("after Remove CapturedOutput = %q, want \"\"", got)
	}
}

// TestCapturedOutput_CleanedWhenEntryStale verifies Cleanup GCs a captured tail
// once its companion STOPPED entry ages past StaleThreshold, so the ephemeral
// diagnostic store cannot leak for the daemon's lifetime (BOS-477).
func TestCapturedOutput_CleanedWhenEntryStale(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	// The poller sets the tail alongside a STOPPED heartbeat at pane death.
	tr.Update("agent-1", pb.ChatStatus_CHAT_STATUS_STOPPED, now)
	tr.SetCapturedOutput("agent-1", "Error: Session ID abc is already in use")
	// Backdate the entry so Cleanup treats it (and its captured tail) as stale.
	tr.entries["agent-1"].ReceivedAt = now.Add(-StaleThreshold - time.Second)

	tr.Cleanup()

	if got := tr.CapturedOutput("agent-1"); got != "" {
		t.Fatalf("after Cleanup CapturedOutput = %q, want \"\" (GC'd with stale entry)", got)
	}
}
