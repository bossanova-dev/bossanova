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
