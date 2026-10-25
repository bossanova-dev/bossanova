package upstream

import (
	"context"
	"sort"
	"testing"
	"testing/synctest"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/clocktest"
	"github.com/rs/zerolog"
)

func newFakeClock() *clocktest.Clock {
	return clocktest.New(time.Unix(1_700_000_000, 0))
}

func waitForTimer(clock *clocktest.Clock, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = clock.BlockUntil(ctx, 1)
}

func waitForTimerAtLeast(clock *clocktest.Clock, minDur, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return clock.WaitUntil(ctx, func() bool {
		for _, d := range clock.PendingDurations() {
			if d >= minDur {
				return true
			}
		}
		return false
	}) == nil
}

func waitForTimerExactly(clock *clocktest.Clock, want, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return clock.WaitUntil(ctx, func() bool {
		for _, d := range clock.PendingDurations() {
			if d == want {
				return true
			}
		}
		return false
	}) == nil
}

func TestCoalescer_WithinWindow_SendsLatestOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
				SessionId:      "s1",
				AgentSessionId: "c1",
				Status:         pb.ChatStatus(i%4 + 1), // vary so "latest wins" is meaningful
			}
		}

		// Drain publishes into the coalescer before advancing.
		synctest.Wait()

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
			// Settle the flush before checking for duplicate emissions.
			synctest.Wait()
			select {
			case s, ok := <-c.Out():
				if ok {
					emissions++
					_ = s
				}
			default:
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
	})
}

func TestCoalescer_AcrossSessions_AllEmitted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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

		for _, ids := range []struct{ sessionID, agentSessionID string }{
			{"s1", "c1"},
			{"s2", "c2"},
			{"s3", "c3"},
		} {
			in <- &pb.ChatStatusDelta{
				SessionId:      ids.sessionID,
				AgentSessionId: ids.agentSessionID,
				Status:         pb.ChatStatus_CHAT_STATUS_WORKING,
			}
		}
		// Settle before advancing.
		synctest.Wait()

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
	})
}

func TestCoalescer_Shutdown_DrainsPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := newFakeClock()
		c := NewStatusCoalescer(clock, 1*time.Second, zerolog.Nop())

		// Publish directly (bypass Run) so the test holds Drain's invariant
		// regardless of ticker timing.
		c.Publish(&pb.ChatStatusDelta{SessionId: "s1", AgentSessionId: "c1", Status: pb.ChatStatus_CHAT_STATUS_WORKING})
		c.Publish(&pb.ChatStatusDelta{SessionId: "s2", AgentSessionId: "c2", Status: pb.ChatStatus_CHAT_STATUS_IDLE})

		drained := c.Drain()
		if len(drained) != 2 {
			t.Fatalf("drain returned %d, want 2", len(drained))
		}
		// Second Drain after draining must be empty.
		if again := c.Drain(); len(again) != 0 {
			t.Fatalf("second drain returned %d, want 0", len(again))
		}
	})
}

// TestCoalescer_SameSession_DifferentChats_BothEmitted is the critical
// regression test for the (session_id, claude_id) keying. Two chats in
// the same session must NOT collapse into a single entry — the
// downstream consumer keys statuses by claude_id and per-chat fidelity
// has to survive coalescing.
func TestCoalescer_SameSession_DifferentChats_BothEmitted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
			SessionId:      "s1",
			AgentSessionId: "c1",
			Status:         pb.ChatStatus_CHAT_STATUS_WORKING,
		}
		in <- &pb.ChatStatusDelta{
			SessionId:      "s1",
			AgentSessionId: "c2",
			Status:         pb.ChatStatus_CHAT_STATUS_IDLE,
		}

		// Settle before advancing so both publishes are in pending.
		synctest.Wait()
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
				gotByClaudeID[s.GetAgentSessionId()] = s.GetStatus()
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
	})
}

// TestCoalescer_LegacyEmptyClaudeID_CoalescesUnderSessionKey covers the
// backward-compat path: an older publisher emits ChatStatusDelta with
// claude_id == "". Two such deltas for the same session_id must
// collapse to a single emission (latest wins) because the key
// coalescerKey{sessionID:"s1", agentSessionID:""} collides for both. This
// preserves the previous session-only behavior and avoids fan-out
// surprises when a legacy daemon is connected.
func TestCoalescer_LegacyEmptyClaudeID_CoalescesUnderSessionKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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

		synctest.Wait()
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
			// Settle the flush before checking for duplicate emissions.
			synctest.Wait()
			select {
			case s, ok := <-c.Out():
				if ok {
					emissions++
					_ = s
				}
			default:
			}
		}
		if emissions != 1 {
			t.Fatalf("expected exactly 1 emission for legacy empty claude_id, got %d", emissions)
		}
		if last.GetSessionId() != "s1" || last.GetAgentSessionId() != "" {
			t.Fatalf("unexpected emission: session=%q claude=%q", last.GetSessionId(), last.GetAgentSessionId())
		}
		if last.GetStatus() != pb.ChatStatus_CHAT_STATUS_IDLE {
			t.Fatalf("expected latest-wins (IDLE), got %v", last.GetStatus())
		}

		cancel()
		<-runDone
	})
}
