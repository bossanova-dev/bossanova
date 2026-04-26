package upstream

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// fakeClock is a deterministic Clock for coalescer tests. AfterFunc
// registers a callback; the test advances virtual time by calling
// Advance, which fires any callbacks whose deadline has passed. Now()
// reports the current virtual time. No goroutine of its own — every
// effect is driven by the test's single goroutine so the test never
// races the coalescer's fire path.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clock    *fakeClock
	deadline time.Time
	fn       func()
	fired    bool
	stopped  bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	f.AfterFunc(d, func() {
		f.mu.Lock()
		t := f.now
		f.mu.Unlock()
		ch <- t
	})
	return ch
}

func (f *fakeClock) AfterFunc(d time.Duration, fn func()) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{clock: f, deadline: f.now.Add(d), fn: fn}
	f.timers = append(f.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// pendingTimers counts timers that are neither fired nor stopped —
// i.e. timers the clock can still trigger via Advance. Used by
// waitForTimers to spin-wait until the goroutine under test has
// actually reached its AfterFunc call site.
func (f *fakeClock) pendingTimers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.timers {
		if !t.fired && !t.stopped {
			n++
		}
	}
	return n
}

// waitForTimers polls until the fake clock has at least n pending
// timers or the deadline expires. Needed because the goroutine under
// test registers its AfterFunc asynchronously relative to the test's
// next Advance — without this wait, Advance can run while the clock
// has zero timers, silently firing nothing.
func waitForTimers(clock *fakeClock, n int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if clock.pendingTimers() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Advance moves virtual time forward by d and fires any callbacks
// whose deadline now lies in the past. Callbacks run synchronously
// under the clock's own mutex dropped temporarily — this matches the
// semantics of time.AfterFunc, which invokes fn on a goroutine.
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	fire := []*fakeTimer{}
	for _, t := range f.timers {
		if !t.fired && !t.stopped && !t.deadline.After(f.now) {
			t.fired = true
			fire = append(fire, t)
		}
	}
	f.mu.Unlock()

	for _, t := range fire {
		t.fn()
	}
}

func TestCoalescer_WithinWindow_SendsLatestOnly(t *testing.T) {
	clock := newFakeClock()
	c := NewStatusCoalescer(clock, 100*time.Millisecond, zerolog.Nop())

	in := make(chan *pb.ChatStatusDelta, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx, in)
	}()

	// 5 publishes for the same (session, chat) in quick succession (virtual
	// time unchanged). Only the latest should survive the flush.
	for i := 0; i < 5; i++ {
		in <- &pb.ChatStatusDelta{
			SessionId: "s1",
			ClaudeId:  "c1",
			Status:    pb.ChatStatus(i%4 + 1), // vary so "latest wins" is meaningful
		}
	}

	// Drain publishes into the coalescer before advancing.
	time.Sleep(20 * time.Millisecond)

	// Advance just past the window — expect exactly one emission.
	clock.Advance(150 * time.Millisecond)

	deadline := time.After(500 * time.Millisecond)
	emissions := 0
	var last *pb.ChatStatusDelta
	for emissions == 0 {
		select {
		case s, ok := <-c.Out():
			if !ok {
				t.Fatalf("out channel closed before any emission")
			}
			emissions++
			last = s
		case <-deadline:
			t.Fatalf("no emission after window")
		}
		// After the first, drain anything extra that lands in the
		// next 30ms so the "only one" assertion is meaningful.
		select {
		case s, ok := <-c.Out():
			if ok {
				emissions++
				_ = s
			}
		case <-time.After(30 * time.Millisecond):
		}
	}
	if emissions != 1 {
		t.Fatalf("expected 1 emission, got %d", emissions)
	}
	if last.GetSessionId() != "s1" {
		t.Fatalf("unexpected emission: %+v", last)
	}

	cancel()
	<-runDone
}

func TestCoalescer_AcrossSessions_AllEmitted(t *testing.T) {
	clock := newFakeClock()
	c := NewStatusCoalescer(clock, 100*time.Millisecond, zerolog.Nop())

	in := make(chan *pb.ChatStatusDelta, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx, in)
	}()

	for _, ids := range []struct{ sessionID, claudeID string }{
		{"s1", "c1"},
		{"s2", "c2"},
		{"s3", "c3"},
	} {
		in <- &pb.ChatStatusDelta{
			SessionId: ids.sessionID,
			ClaudeId:  ids.claudeID,
			Status:    pb.ChatStatus_CHAT_STATUS_WORKING,
		}
	}
	// Settle before advancing.
	time.Sleep(20 * time.Millisecond)

	clock.Advance(150 * time.Millisecond)

	got := map[string]bool{}
	deadline := time.After(500 * time.Millisecond)
	for len(got) < 3 {
		select {
		case s, ok := <-c.Out():
			if !ok {
				t.Fatalf("out channel closed early, got=%v", got)
			}
			got[s.GetSessionId()] = true
		case <-deadline:
			ids := make([]string, 0, len(got))
			for k := range got {
				ids = append(ids, k)
			}
			sort.Strings(ids)
			t.Fatalf("expected 3 session IDs, got %v", ids)
		}
	}

	cancel()
	<-runDone
}

func TestCoalescer_Shutdown_DrainsPending(t *testing.T) {
	clock := newFakeClock()
	c := NewStatusCoalescer(clock, 1*time.Second, zerolog.Nop())

	// Publish directly (bypass Run) so the test holds Drain's invariant
	// regardless of ticker timing.
	c.Publish(&pb.ChatStatusDelta{SessionId: "s1", ClaudeId: "c1", Status: pb.ChatStatus_CHAT_STATUS_WORKING})
	c.Publish(&pb.ChatStatusDelta{SessionId: "s2", ClaudeId: "c2", Status: pb.ChatStatus_CHAT_STATUS_IDLE})

	drained := c.Drain()
	if len(drained) != 2 {
		t.Fatalf("drain returned %d, want 2", len(drained))
	}
	// Second Drain after draining must be empty.
	if again := c.Drain(); len(again) != 0 {
		t.Fatalf("second drain returned %d, want 0", len(again))
	}
}

// TestCoalescer_SameSession_DifferentChats_BothEmitted is the critical
// regression test for the (session_id, claude_id) keying. Two chats in
// the same session must NOT collapse into a single entry — the
// downstream consumer keys statuses by claude_id and per-chat fidelity
// has to survive coalescing.
func TestCoalescer_SameSession_DifferentChats_BothEmitted(t *testing.T) {
	clock := newFakeClock()
	c := NewStatusCoalescer(clock, 100*time.Millisecond, zerolog.Nop())

	in := make(chan *pb.ChatStatusDelta, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx, in)
	}()

	// Same session, two different chats with different statuses. Under
	// the old session-only keying these would collapse to one entry —
	// the bug this test exists to prevent.
	in <- &pb.ChatStatusDelta{
		SessionId: "s1",
		ClaudeId:  "c1",
		Status:    pb.ChatStatus_CHAT_STATUS_WORKING,
	}
	in <- &pb.ChatStatusDelta{
		SessionId: "s1",
		ClaudeId:  "c2",
		Status:    pb.ChatStatus_CHAT_STATUS_IDLE,
	}

	// Settle before advancing so both publishes are in pending.
	time.Sleep(20 * time.Millisecond)
	clock.Advance(150 * time.Millisecond)

	gotByClaudeID := map[string]pb.ChatStatus{}
	deadline := time.After(500 * time.Millisecond)
	for len(gotByClaudeID) < 2 {
		select {
		case s, ok := <-c.Out():
			if !ok {
				t.Fatalf("out channel closed early, got=%v", gotByClaudeID)
			}
			if s.GetSessionId() != "s1" {
				t.Fatalf("unexpected session_id %q", s.GetSessionId())
			}
			gotByClaudeID[s.GetClaudeId()] = s.GetStatus()
		case <-deadline:
			t.Fatalf("expected 2 emissions for distinct claude_ids, got %v", gotByClaudeID)
		}
	}

	if got, want := gotByClaudeID["c1"], pb.ChatStatus_CHAT_STATUS_WORKING; got != want {
		t.Fatalf("c1 status = %v, want %v", got, want)
	}
	if got, want := gotByClaudeID["c2"], pb.ChatStatus_CHAT_STATUS_IDLE; got != want {
		t.Fatalf("c2 status = %v, want %v", got, want)
	}

	cancel()
	<-runDone
}

// TestCoalescer_LegacyEmptyClaudeID_CoalescesUnderSessionKey covers the
// backward-compat path: an older publisher emits ChatStatusDelta with
// claude_id == "". Two such deltas for the same session_id must
// collapse to a single emission (latest wins) because the key
// coalescerKey{sessionID:"s1", claudeID:""} collides for both. This
// preserves the previous session-only behavior and avoids fan-out
// surprises when a legacy daemon is connected.
func TestCoalescer_LegacyEmptyClaudeID_CoalescesUnderSessionKey(t *testing.T) {
	clock := newFakeClock()
	c := NewStatusCoalescer(clock, 100*time.Millisecond, zerolog.Nop())

	in := make(chan *pb.ChatStatusDelta, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		c.Run(ctx, in)
	}()

	// Two deltas, empty claude_id, different statuses. Latest must win.
	in <- &pb.ChatStatusDelta{
		SessionId: "s1",
		Status:    pb.ChatStatus_CHAT_STATUS_WORKING,
	}
	in <- &pb.ChatStatusDelta{
		SessionId: "s1",
		Status:    pb.ChatStatus_CHAT_STATUS_IDLE,
	}

	time.Sleep(20 * time.Millisecond)
	clock.Advance(150 * time.Millisecond)

	deadline := time.After(500 * time.Millisecond)
	emissions := 0
	var last *pb.ChatStatusDelta
	for emissions == 0 {
		select {
		case s, ok := <-c.Out():
			if !ok {
				t.Fatalf("out channel closed before any emission")
			}
			emissions++
			last = s
		case <-deadline:
			t.Fatalf("no emission after window")
		}
		// Drain anything else that lands shortly so the "exactly one"
		// assertion is meaningful.
		select {
		case s, ok := <-c.Out():
			if ok {
				emissions++
				_ = s
			}
		case <-time.After(30 * time.Millisecond):
		}
	}
	if emissions != 1 {
		t.Fatalf("expected exactly 1 emission for legacy empty claude_id, got %d", emissions)
	}
	if last.GetSessionId() != "s1" || last.GetClaudeId() != "" {
		t.Fatalf("unexpected emission: session=%q claude=%q", last.GetSessionId(), last.GetClaudeId())
	}
	if last.GetStatus() != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatalf("expected latest-wins (IDLE), got %v", last.GetStatus())
	}

	cancel()
	<-runDone
}
