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
	if tr.AuthFailed("chat-1") {
		t.Fatal("AuthFailed after one SetAuthFailed(true) = true, want false")
	}

	tr.SetAuthFailed("chat-1", true)
	if !tr.AuthFailed("chat-1") {
		t.Fatal("AuthFailed after two consecutive SetAuthFailed(true) = false, want true")
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

	// absent → first observation: not yet effective, no transition.
	tr.SetAuthFailed("chat-1", true)
	// first → effective failed: a transition, fires once.
	tr.SetAuthFailed("chat-1", true)
	// failed → failed (the poller re-observes login-required every tick): no
	// transition, must NOT re-fire and storm the stream.
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

func TestAuthFailedSinceStaysAtEffectiveTransition(t *testing.T) {
	tr := NewTracker()
	tr.SetAuthFailed("chat-1", true)
	if _, ok := tr.AuthFailedSince("chat-1"); ok {
		t.Fatal("AuthFailedSince after one observation = true, want false")
	}
	tr.SetAuthFailed("chat-1", true)
	since, ok := tr.AuthFailedSince("chat-1")
	if !ok {
		t.Fatal("AuthFailedSince after effective transition = false, want true")
	}
	time.Sleep(time.Millisecond)
	tr.SetAuthFailed("chat-1", true)
	again, ok := tr.AuthFailedSince("chat-1")
	if !ok {
		t.Fatal("AuthFailedSince after repeated observation = false, want true")
	}
	if !again.Equal(since) {
		t.Fatalf("AuthFailedSince moved from %v to %v; want stable episode start", since, again)
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

	tr.SetAuthFailed("chat-1", true) // no fire yet (first observation)
	tr.SetAuthFailed("chat-1", true) // fire 1 (second consecutive observation)

	// Age the marker past StaleThreshold: effectively absent per AuthFailed.
	tr.mu.Lock()
	marker := tr.authFailed["chat-1"]
	marker.observedAt = time.Now().Add(-StaleThreshold - time.Second)
	tr.authFailed["chat-1"] = marker
	tr.mu.Unlock()

	tr.SetAuthFailed("chat-1", true) // first observation after stale marker, no fire
	tr.SetAuthFailed("chat-1", true) // fire 2 (stale/effectively-absent → effective)

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

	tr.SetAuthFailed("chat-1", true)
	tr.SetAuthFailed("chat-1", true) // fire 1 (absent -> effective)
	tr.mu.Lock()
	marker := tr.authFailed["chat-1"]
	marker.observedAt = time.Now().Add(-StaleThreshold - time.Second)
	tr.authFailed["chat-1"] = marker
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
	tr.SetAuthFailed("chat-1", true)

	// Backdate the marker beyond StaleThreshold: a stale marker must read as
	// absent (fail toward not flagging).
	tr.mu.Lock()
	marker := tr.authFailed["chat-1"]
	marker.observedAt = time.Now().Add(-StaleThreshold - time.Second)
	tr.authFailed["chat-1"] = marker
	tr.mu.Unlock()

	if tr.AuthFailed("chat-1") {
		t.Fatal("AuthFailed on stale marker = true, want false")
	}
}

func TestRemove_ClearsAuthFailed(t *testing.T) {
	tr := NewTracker()
	tr.SetAuthFailed("chat-1", true)
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

	tr.SetAuthFailed("chat-1", true)
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
	tr.SetAuthFailed("fresh", true)
	tr.SetAuthFailed("stale", true)
	tr.SetAuthFailed("stale", true)
	tr.mu.Lock()
	marker := tr.authFailed["stale"]
	marker.observedAt = time.Now().Add(-StaleThreshold - time.Second)
	tr.authFailed["stale"] = marker
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
	tr.SetAuthFailed("fresh", true)
	tr.SetAuthFailed("stale", true)
	tr.SetAuthFailed("stale", true)
	tr.mu.Lock()
	marker := tr.authFailed["stale"]
	marker.observedAt = time.Now().Add(-StaleThreshold - time.Second)
	tr.authFailed["stale"] = marker
	tr.mu.Unlock()

	tr.Cleanup()

	mu.Lock()
	got := append([]string(nil), fired...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("onAuthChange fired %d times (%v), want 3 (two effective sets + stale clear)", len(got), got)
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

// --- transient-API-error marker (BOS-518) -----------------------------------
//
// These mirror the auth-marker tests above one for one: the transient marker is
// the same shape of poller-written, staleness-bounded, transition-gated overlay,
// so it must behave identically.

func TestSetTransientAPIError_and_TransientAPIError(t *testing.T) {
	tr := NewTracker()

	if tr.TransientAPIError("chat-1") {
		t.Fatal("TransientAPIError on unknown chat = true, want false")
	}

	tr.SetTransientAPIError("chat-1", true)
	if !tr.TransientAPIError("chat-1") {
		t.Fatal("TransientAPIError after SetTransientAPIError(true) = false, want true")
	}

	// Clearing removes the marker immediately.
	tr.SetTransientAPIError("chat-1", false)
	if tr.TransientAPIError("chat-1") {
		t.Fatal("TransientAPIError after SetTransientAPIError(false) = true, want false")
	}
}

func TestSetTransientAPIError_FiresChangeHookOnlyOnTransition(t *testing.T) {
	tr := NewTracker()
	var mu sync.Mutex
	var fired []string
	tr.SetOnTransientAPIErrorChange(func(id string) {
		mu.Lock()
		fired = append(fired, id)
		mu.Unlock()
	})

	// absent → present: a transition, fires once.
	tr.SetTransientAPIError("chat-1", true)
	// present → present (the poller re-observes the banner every tick while the
	// pane still shows it): no transition, must NOT storm the hook.
	tr.SetTransientAPIError("chat-1", true)
	tr.SetTransientAPIError("chat-1", true)
	// present → cleared: a transition, fires once.
	tr.SetTransientAPIError("chat-1", false)
	// cleared → cleared (poller keeps reporting a healthy pane): no fire.
	tr.SetTransientAPIError("chat-1", false)

	mu.Lock()
	got := append([]string(nil), fired...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("onTransientAPIErrorChange fired %d times (%v), want 2 (set + clear transitions)", len(got), got)
	}
	for _, id := range got {
		if id != "chat-1" {
			t.Errorf("onTransientAPIErrorChange fired for %q, want chat-1", id)
		}
	}
}

func TestSetTransientAPIError_StaleMarkerCountsAsTransition(t *testing.T) {
	tr := NewTracker()
	var fires int
	var mu sync.Mutex
	tr.SetOnTransientAPIErrorChange(func(string) {
		mu.Lock()
		fires++
		mu.Unlock()
	})

	tr.SetTransientAPIError("chat-1", true) // fire 1 (absent → fresh)

	// Age the marker past StaleThreshold: effectively absent per TransientAPIError.
	tr.mu.Lock()
	tr.transientAPIError["chat-1"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	tr.SetTransientAPIError("chat-1", true) // fire 2 (stale/effectively-absent → fresh)

	mu.Lock()
	got := fires
	mu.Unlock()
	if got != 2 {
		t.Fatalf("onTransientAPIErrorChange fired %d times, want 2 (a stale marker refreshing is a transition)", got)
	}
}

func TestSetTransientAPIError_FalseClearsStaleMarkerWithChangeHook(t *testing.T) {
	tr := NewTracker()
	var fires int
	var mu sync.Mutex
	tr.SetOnTransientAPIErrorChange(func(string) {
		mu.Lock()
		fires++
		mu.Unlock()
	})

	tr.SetTransientAPIError("chat-1", true) // fire 1 (absent → fresh)
	tr.mu.Lock()
	tr.transientAPIError["chat-1"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	// The stale marker already reads false locally, but downstream consumers may
	// still hold the last fresh "transient failure" state — clearing must publish.
	tr.SetTransientAPIError("chat-1", false) // fire 2 (stale marker removed)

	mu.Lock()
	got := fires
	mu.Unlock()
	if got != 2 {
		t.Fatalf("onTransientAPIErrorChange fired %d times, want 2 (set + stale clear)", got)
	}
}

func TestTransientAPIError_Stale(t *testing.T) {
	tr := NewTracker()
	tr.SetTransientAPIError("chat-1", true)

	// Backdate the marker beyond StaleThreshold: a stale marker must read as
	// absent (fail toward not flagging, so auto-resume never fires on history).
	tr.mu.Lock()
	tr.transientAPIError["chat-1"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	if tr.TransientAPIError("chat-1") {
		t.Fatal("TransientAPIError on stale marker = true, want false")
	}
}

func TestRemove_ClearsTransientAPIError(t *testing.T) {
	tr := NewTracker()
	tr.SetTransientAPIError("chat-1", true)
	tr.Remove("chat-1")
	if tr.TransientAPIError("chat-1") {
		t.Fatal("TransientAPIError after Remove = true, want false")
	}
}

func TestRemove_FiresTransientAPIErrorChangeWhenMarkerCleared(t *testing.T) {
	tr := NewTracker()
	var fired []string
	var mu sync.Mutex
	tr.SetOnTransientAPIErrorChange(func(id string) {
		mu.Lock()
		fired = append(fired, id)
		mu.Unlock()
	})

	tr.SetTransientAPIError("chat-1", true) // set transition
	tr.Remove("chat-1")                     // clear transition
	tr.Remove("chat-1")                     // no marker left, no extra transition

	mu.Lock()
	got := append([]string(nil), fired...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("onTransientAPIErrorChange fired %d times (%v), want 2", len(got), got)
	}
	for _, id := range got {
		if id != "chat-1" {
			t.Errorf("onTransientAPIErrorChange fired for %q, want chat-1", id)
		}
	}
}

func TestCleanup_RemovesStaleTransientAPIError(t *testing.T) {
	tr := NewTracker()
	tr.SetTransientAPIError("fresh", true)
	tr.SetTransientAPIError("stale", true)
	tr.mu.Lock()
	tr.transientAPIError["stale"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	tr.Cleanup()

	tr.mu.RLock()
	_, freshOK := tr.transientAPIError["fresh"]
	_, staleOK := tr.transientAPIError["stale"]
	tr.mu.RUnlock()
	if !freshOK {
		t.Error("Cleanup removed a fresh transient-API marker")
	}
	if staleOK {
		t.Error("Cleanup kept a stale transient-API marker")
	}
}

func TestCleanup_FiresTransientAPIErrorChangeForRemovedStaleMarkers(t *testing.T) {
	tr := NewTracker()
	var fired []string
	var mu sync.Mutex
	tr.SetOnTransientAPIErrorChange(func(id string) {
		mu.Lock()
		fired = append(fired, id)
		mu.Unlock()
	})

	tr.SetTransientAPIError("fresh", true)
	tr.SetTransientAPIError("stale", true)
	tr.mu.Lock()
	tr.transientAPIError["stale"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	tr.Cleanup()

	mu.Lock()
	got := append([]string(nil), fired...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("onTransientAPIErrorChange fired %d times (%v), want 3 (two sets + stale clear)", len(got), got)
	}
	if got[2] != "stale" {
		t.Fatalf("cleanup clear fired for %q, want stale", got[2])
	}
}

// --- BOS-667: agent-stalled marker -------------------------------------------
//
// The stalled marker mirrors the auth-failed marker exactly (set/clear, freshness
// self-heal, edge-triggered hook). These tests are the auth-marker tests with the
// stalled marker substituted, so any divergence between the two shapes shows up
// as a diff between the two blocks.

func TestSetStalled_and_Stalled(t *testing.T) {
	tr := NewTracker()

	if tr.Stalled("chat-1") {
		t.Fatal("Stalled on unknown chat = true, want false")
	}

	tr.SetStalled("chat-1", true)
	if !tr.Stalled("chat-1") {
		t.Fatal("Stalled after SetStalled(true) = false, want true")
	}

	// Clearing removes the marker immediately, so a chat whose agent resumed
	// making progress stops flagging on the very next poll tick.
	tr.SetStalled("chat-1", false)
	if tr.Stalled("chat-1") {
		t.Fatal("Stalled after SetStalled(false) = true, want false")
	}
}

// TestSetStalled_FiresOnStalledChangeOnlyOnTransition is the debounce guard: the
// poller calls SetStalled on EVERY 3s tick, so an ungated hook would emit a
// SessionDelta every three seconds for as long as the stall persists.
func TestSetStalled_FiresOnStalledChangeOnlyOnTransition(t *testing.T) {
	tr := NewTracker()
	var mu sync.Mutex
	var fired []string
	tr.SetOnStalledChange(func(id string) {
		mu.Lock()
		fired = append(fired, id)
		mu.Unlock()
	})

	tr.SetStalled("chat-1", true) // absent → stalled: one transition
	tr.SetStalled("chat-1", true) // repeated identical tick: no fire
	tr.SetStalled("chat-1", true) // repeated identical tick: no fire
	tr.SetStalled("chat-1", false)
	tr.SetStalled("chat-1", false) // already clear: no fire

	mu.Lock()
	got := append([]string(nil), fired...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("onStalledChange fired %d times (%v), want 2 (set + clear transitions)", len(got), got)
	}
	for _, id := range got {
		if id != "chat-1" {
			t.Errorf("onStalledChange fired for %q, want chat-1", id)
		}
	}
}

func TestSetStalled_StaleMarkerCountsAsTransition(t *testing.T) {
	tr := NewTracker()
	var fires int
	var mu sync.Mutex
	tr.SetOnStalledChange(func(string) {
		mu.Lock()
		fires++
		mu.Unlock()
	})

	tr.SetStalled("chat-1", true) // fire 1 (absent → fresh)

	tr.mu.Lock()
	tr.stalled["chat-1"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	tr.SetStalled("chat-1", true) // fire 2 (stale/effectively-absent → fresh)

	mu.Lock()
	got := fires
	mu.Unlock()
	if got != 2 {
		t.Fatalf("onStalledChange fired %d times, want 2 (a stale marker refreshing is a transition)", got)
	}
}

func TestSetStalled_FalseClearsStaleMarkerWithStalledChange(t *testing.T) {
	tr := NewTracker()
	var fires int
	var mu sync.Mutex
	tr.SetOnStalledChange(func(string) {
		mu.Lock()
		fires++
		mu.Unlock()
	})

	tr.SetStalled("chat-1", true)
	tr.mu.Lock()
	tr.stalled["chat-1"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	tr.SetStalled("chat-1", false) // fire 2 (stale marker removed)

	mu.Lock()
	got := fires
	mu.Unlock()
	if got != 2 {
		t.Fatalf("onStalledChange fired %d times, want 2 (set + stale clear)", got)
	}
}

func TestStalled_Stale(t *testing.T) {
	tr := NewTracker()
	tr.SetStalled("chat-1", true)

	tr.mu.Lock()
	tr.stalled["chat-1"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	if tr.Stalled("chat-1") {
		t.Fatal("Stalled on stale marker = true, want false")
	}
}

func TestRemove_ClearsStalled(t *testing.T) {
	tr := NewTracker()
	tr.SetStalled("chat-1", true)
	tr.Remove("chat-1")
	if tr.Stalled("chat-1") {
		t.Fatal("Stalled after Remove = true, want false")
	}
}

func TestRemove_FiresOnStalledChangeWhenMarkerCleared(t *testing.T) {
	tr := NewTracker()
	var fired []string
	var mu sync.Mutex
	tr.SetOnStalledChange(func(id string) {
		mu.Lock()
		fired = append(fired, id)
		mu.Unlock()
	})

	tr.SetStalled("chat-1", true) // set transition
	tr.Remove("chat-1")           // clear transition
	tr.Remove("chat-1")           // no marker left, no extra transition

	mu.Lock()
	got := append([]string(nil), fired...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("onStalledChange fired %d times (%v), want 2", len(got), got)
	}
}

func TestCleanup_RemovesStaleStalled(t *testing.T) {
	tr := NewTracker()
	tr.SetStalled("fresh", true)
	tr.SetStalled("stale", true)
	tr.mu.Lock()
	tr.stalled["stale"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	tr.Cleanup()

	tr.mu.RLock()
	_, freshOK := tr.stalled["fresh"]
	_, staleOK := tr.stalled["stale"]
	tr.mu.RUnlock()
	if !freshOK {
		t.Error("Cleanup removed a fresh stalled marker")
	}
	if staleOK {
		t.Error("Cleanup kept a stale stalled marker")
	}
}

func TestCleanup_FiresOnStalledChangeForRemovedStaleMarkers(t *testing.T) {
	tr := NewTracker()
	var fired []string
	var mu sync.Mutex
	tr.SetOnStalledChange(func(id string) {
		mu.Lock()
		fired = append(fired, id)
		mu.Unlock()
	})

	tr.SetStalled("fresh", true)
	tr.SetStalled("stale", true)
	tr.mu.Lock()
	tr.stalled["stale"] = time.Now().Add(-StaleThreshold - time.Second)
	tr.mu.Unlock()

	tr.Cleanup()

	mu.Lock()
	got := append([]string(nil), fired...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("onStalledChange fired %d times (%v), want 3 (two sets + stale clear)", len(got), got)
	}
	if got[2] != "stale" {
		t.Fatalf("cleanup clear fired for %q, want stale", got[2])
	}
}

// --- BOS-805: spinner-aware liveness side-map -------------------------------

// The liveness reading must survive Update recreating the Entry on every
// heartbeat — that is the entire reason it is a side-map rather than an Entry
// field, and the failure it prevents is silent (the value simply reads zero).
func TestTracker_LivenessSurvivesHeartbeat(t *testing.T) {
	tracker := NewTracker()
	substantiveAt := time.Now().Add(-30 * time.Minute)

	tracker.SetLiveness("chat-1", true, substantiveAt, true)
	for i := 0; i < 3; i++ {
		tracker.Update("chat-1", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	}

	spinnerPresent, gotSubstantive, seeded := tracker.Liveness("chat-1")
	if !spinnerPresent {
		t.Error("spinnerPresent = false after heartbeats, want true")
	}
	if !gotSubstantive.Equal(substantiveAt) {
		t.Errorf("lastSubstantiveOutputAt = %v, want %v", gotSubstantive, substantiveAt)
	}
	if !seeded {
		t.Error("lastOutputSeeded = false after heartbeats, want true")
	}
}

func TestTracker_LivenessUnknownChat(t *testing.T) {
	tracker := NewTracker()
	spinnerPresent, substantiveAt, seeded := tracker.Liveness("nobody")
	if spinnerPresent || seeded || !substantiveAt.IsZero() {
		t.Errorf("Liveness(unknown) = (%v, %v, %v), want (false, zero, false)",
			spinnerPresent, substantiveAt, seeded)
	}
}

func TestTracker_LivenessObservation(t *testing.T) {
	tracker := NewTracker()
	substantiveAt := time.Now().Add(-time.Minute)

	if got := tracker.LivenessObservation("chat-1"); got.Present {
		t.Fatal("unknown chat observation Present = true, want false")
	}

	tracker.SetLiveness("chat-1", false, substantiveAt, true)
	first := tracker.LivenessObservation("chat-1")
	if !first.Present {
		t.Fatal("known chat observation Present = false, want true")
	}
	if first.ObservedAt.IsZero() {
		t.Fatal("ObservedAt is zero")
	}
	if !first.LastSubstantiveOutputAt.Equal(substantiveAt) {
		t.Errorf("LastSubstantiveOutputAt = %v, want %v", first.LastSubstantiveOutputAt, substantiveAt)
	}
	if !first.LastSubstantiveOutputSeed {
		t.Error("LastSubstantiveOutputSeed = false, want true")
	}

	tracker.SetLiveness("chat-1", false, substantiveAt, true)
	second := tracker.LivenessObservation("chat-1")
	if !second.ObservedAt.After(first.ObservedAt) {
		t.Errorf("ObservedAt did not advance on identical liveness write: first=%v second=%v", first.ObservedAt, second.ObservedAt)
	}
}

// A spinner nobody is re-observing is not evidence the agent is working NOW, so
// the marker reads as absent once it is staler than StaleThreshold — the same
// fail-toward-not-flagging rule AuthFailed and Stalled use. The substantive
// timestamp deliberately does NOT decay: its age IS the signal.
func TestTracker_LivenessStaleSpinnerReadsAbsent(t *testing.T) {
	tracker := NewTracker()
	substantiveAt := time.Now().Add(-time.Hour)
	tracker.SetLiveness("chat-1", true, substantiveAt, false)

	tracker.mu.Lock()
	tracker.liveness["chat-1"].spinnerAt = time.Now().Add(-StaleThreshold - time.Second)
	tracker.mu.Unlock()

	spinnerPresent, gotSubstantive, _ := tracker.Liveness("chat-1")
	if spinnerPresent {
		t.Error("spinnerPresent = true for a marker past StaleThreshold, want false")
	}
	if !gotSubstantive.Equal(substantiveAt) {
		t.Errorf("lastSubstantiveOutputAt = %v, want the undecayed %v", gotSubstantive, substantiveAt)
	}
}

func TestTracker_LivenessReclaimedOnRemoveAndCleanup(t *testing.T) {
	t.Run("remove", func(t *testing.T) {
		tracker := NewTracker()
		tracker.SetLiveness("chat-1", true, time.Now(), true)
		tracker.Remove("chat-1")
		tracker.mu.RLock()
		defer tracker.mu.RUnlock()
		if _, ok := tracker.liveness["chat-1"]; ok {
			t.Error("liveness marker survived Remove")
		}
	})

	t.Run("cleanup", func(t *testing.T) {
		tracker := NewTracker()
		tracker.Update("chat-1", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
		tracker.SetLiveness("chat-1", true, time.Now(), true)
		tracker.mu.Lock()
		tracker.entries["chat-1"].ReceivedAt = time.Now().Add(-StaleThreshold - time.Second)
		tracker.mu.Unlock()

		tracker.Cleanup()

		tracker.mu.RLock()
		defer tracker.mu.RUnlock()
		if _, ok := tracker.liveness["chat-1"]; ok {
			t.Error("liveness marker survived Cleanup of a stale entry")
		}
	})
}
