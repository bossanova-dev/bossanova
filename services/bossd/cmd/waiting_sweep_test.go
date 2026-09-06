package main

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

type sweepChatLookupFake struct {
	chats map[string]*models.AgentChat
	err   error
	// errFor fails only the named agent_session_ids, which is what lets a test
	// distinguish "one bad id" from "the database is down" — the batch paths
	// must survive the former without losing the rest of the batch.
	errFor map[string]error
	calls  int
}

func (f *sweepChatLookupFake) GetByAgentSessionID(_ context.Context, agentSessionID string) (*models.AgentChat, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if err, ok := f.errFor[agentSessionID]; ok {
		return nil, err
	}
	return f.chats[agentSessionID], nil
}

type sweepRecomputerFake struct {
	sessions []string
	err      error
	// errFor fails only the named session ids — see sweepChatLookupFake.errFor.
	errFor map[string]error
}

func (f *sweepRecomputerFake) Recompute(_ context.Context, sessionID string) error {
	f.sessions = append(f.sessions, sessionID)
	if err, ok := f.errFor[sessionID]; ok {
		return err
	}
	return f.err
}

func (f *sweepRecomputerFake) sorted() []string {
	out := append([]string(nil), f.sessions...)
	sort.Strings(out)
	return out
}

func sweepFixture(t *testing.T) (*status.Tracker, *sweepChatLookupFake, *sweepRecomputerFake) {
	t.Helper()
	return status.NewTracker(),
		&sweepChatLookupFake{chats: map[string]*models.AgentChat{}},
		&sweepRecomputerFake{}
}

// The whole point of the sweep: a chat that has been WORKING for an hour never
// fires a tracker transition, so without a periodic pass an armed callback would
// only surface on the chat's next unrelated status change.
func TestSweepWaitingChats_RecomputesSessionsWithWorkingChats(t *testing.T) {
	tracker, chats, rec := sweepFixture(t)
	tracker.Update("agent-a", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	chats.chats["agent-a"] = &models.AgentChat{AgentSessionID: "agent-a", SessionID: "sess-1"}

	sweepWaitingChats(context.Background(), tracker, chats, rec)

	if got := rec.sorted(); len(got) != 1 || got[0] != "sess-1" {
		t.Fatalf("recomputed %v, want [sess-1]", got)
	}
}

// Recompute is not free, so the sweep must not fan out over chats that can never
// be promoted: waiting is a refinement of WORKING and nothing else.
func TestSweepWaitingChats_IgnoresNonWorkingChats(t *testing.T) {
	tracker, chats, rec := sweepFixture(t)
	for id, st := range map[string]bossanovav1.ChatStatus{
		"agent-idle":     bossanovav1.ChatStatus_CHAT_STATUS_IDLE,
		"agent-question": bossanovav1.ChatStatus_CHAT_STATUS_QUESTION,
		"agent-limited":  bossanovav1.ChatStatus_CHAT_STATUS_LIMITED,
		"agent-stopped":  bossanovav1.ChatStatus_CHAT_STATUS_STOPPED,
	} {
		tracker.Update(id, st, time.Now())
		chats.chats[id] = &models.AgentChat{AgentSessionID: id, SessionID: "sess-" + id}
	}

	sweepWaitingChats(context.Background(), tracker, chats, rec)

	if got := rec.sorted(); len(got) != 0 {
		t.Fatalf("recomputed %v, want none", got)
	}
	if chats.calls != 0 {
		t.Fatalf("chat lookups = %d, want 0 (no working chat to resolve)", chats.calls)
	}
}

// A session with several working chats is one recompute, not one per chat: the
// computer already walks every chat in the session it is handed.
func TestSweepWaitingChats_DedupesBySession(t *testing.T) {
	tracker, chats, rec := sweepFixture(t)
	for _, id := range []string{"agent-a", "agent-b", "agent-c"} {
		tracker.Update(id, bossanovav1.ChatStatus_CHAT_STATUS_WORKING, time.Now())
		chats.chats[id] = &models.AgentChat{AgentSessionID: id, SessionID: "sess-1"}
	}

	sweepWaitingChats(context.Background(), tracker, chats, rec)

	if got := rec.sorted(); len(got) != 1 || got[0] != "sess-1" {
		t.Fatalf("recomputed %v, want exactly [sess-1]", got)
	}
}

// A dead database or a chat row that has since been deleted must not take the
// 30s daemon sweep down with it.
func TestSweepWaitingChats_SurvivesLookupFailures(t *testing.T) {
	t.Run("lookup error", func(t *testing.T) {
		tracker, chats, rec := sweepFixture(t)
		tracker.Update("agent-a", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, time.Now())
		chats.err = errors.New("database is locked")

		sweepWaitingChats(context.Background(), tracker, chats, rec)

		if got := rec.sorted(); len(got) != 0 {
			t.Fatalf("recomputed %v, want none", got)
		}
	})

	t.Run("missing chat row", func(t *testing.T) {
		tracker, chats, rec := sweepFixture(t)
		tracker.Update("agent-a", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, time.Now())

		sweepWaitingChats(context.Background(), tracker, chats, rec)

		if got := rec.sorted(); len(got) != 0 {
			t.Fatalf("recomputed %v, want none", got)
		}
	})
}

// The sweep runs off a ticker whose context is canceled on shutdown; it must
// stop rather than issue a burst of doomed queries.
func TestSweepWaitingChats_StopsOnCanceledContext(t *testing.T) {
	tracker, chats, rec := sweepFixture(t)
	tracker.Update("agent-a", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	chats.chats["agent-a"] = &models.AgentChat{AgentSessionID: "agent-a", SessionID: "sess-1"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sweepWaitingChats(ctx, tracker, chats, rec)

	if got := rec.sorted(); len(got) != 0 {
		t.Fatalf("recomputed %v, want none", got)
	}
}

// --- BOS-1096: eviction recompute --------------------------------------------

// evictionFixture mirrors sweepFixture but returns no tracker: the whole point
// of this path is that it works from ids the tracker no longer holds.
func evictionFixture(t *testing.T) (*sweepChatLookupFake, *sweepRecomputerFake) {
	t.Helper()
	return &sweepChatLookupFake{chats: map[string]*models.AgentChat{}}, &sweepRecomputerFake{}
}

// The reason the hook is batched rather than per-id: a session whose chats all
// go stale on the same tick must cost one Recompute, not one per evicted chat.
func TestRecomputeEvictedSessions_DedupesBySession(t *testing.T) {
	chats, rec := evictionFixture(t)
	for _, id := range []string{"agent-a", "agent-b"} {
		chats.chats[id] = &models.AgentChat{AgentSessionID: id, SessionID: "sess-1"}
	}

	recomputeEvictedSessions([]string{"agent-a", "agent-b"}, chats, rec, zerolog.Nop())

	if got := rec.sorted(); len(got) != 1 || got[0] != "sess-1" {
		t.Fatalf("recomputed %v, want exactly [sess-1]", got)
	}
}

// Dedupe must not over-collapse: a sleep/wake evicts many sessions at once and
// every one of them is a frozen label.
func TestRecomputeEvictedSessions_RecomputesEachDistinctSession(t *testing.T) {
	chats, rec := evictionFixture(t)
	for i, id := range []string{"agent-a", "agent-b", "agent-c"} {
		chats.chats[id] = &models.AgentChat{
			AgentSessionID: id,
			SessionID:      []string{"sess-1", "sess-2", "sess-3"}[i],
		}
	}

	recomputeEvictedSessions([]string{"agent-a", "agent-b", "agent-c"}, chats, rec, zerolog.Nop())

	got := rec.sorted()
	want := []string{"sess-1", "sess-2", "sess-3"}
	if len(got) != len(want) {
		t.Fatalf("recomputed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recomputed %v, want %v", got, want)
		}
	}
}

// An id whose chat row is already gone is the rare case — DeleteChat clears the
// cached status while the row is still present, so an ordinary eviction still
// resolves. It must not cost the rest of the batch their recompute.
func TestRecomputeEvictedSessions_SkipsUnresolvableIDsWithoutAbortingBatch(t *testing.T) {
	chats, rec := evictionFixture(t)
	chats.chats["agent-live"] = &models.AgentChat{AgentSessionID: "agent-live", SessionID: "sess-1"}

	recomputeEvictedSessions([]string{"agent-gone", "agent-live"}, chats, rec, zerolog.Nop())

	if got := rec.sorted(); len(got) != 1 || got[0] != "sess-1" {
		t.Fatalf("recomputed %v, want [sess-1] — a missing chat row aborted the batch", got)
	}
}

// Same for a transient database error on one id. Each remaining session is a
// label that would otherwise stay frozen until a daemon restart.
func TestRecomputeEvictedSessions_SurvivesLookupErrorWithoutAbortingBatch(t *testing.T) {
	chats, rec := evictionFixture(t)
	chats.errFor = map[string]error{"agent-bad": errors.New("database is locked")}
	chats.chats["agent-live"] = &models.AgentChat{AgentSessionID: "agent-live", SessionID: "sess-1"}

	recomputeEvictedSessions([]string{"agent-bad", "agent-live"}, chats, rec, zerolog.Nop())

	if got := rec.sorted(); len(got) != 1 || got[0] != "sess-1" {
		t.Fatalf("recomputed %v, want [sess-1] — a lookup error aborted the batch", got)
	}
}

// A failed Recompute on one session must not strand the others; partial
// failures stay partial.
func TestRecomputeEvictedSessions_SurvivesRecomputeErrorWithoutAbortingBatch(t *testing.T) {
	chats, rec := evictionFixture(t)
	rec.errFor = map[string]error{"sess-1": errors.New("recompute exploded")}
	chats.chats["agent-a"] = &models.AgentChat{AgentSessionID: "agent-a", SessionID: "sess-1"}
	chats.chats["agent-b"] = &models.AgentChat{AgentSessionID: "agent-b", SessionID: "sess-2"}

	recomputeEvictedSessions([]string{"agent-a", "agent-b"}, chats, rec, zerolog.Nop())

	got := rec.sorted()
	if len(got) != 2 || got[0] != "sess-1" || got[1] != "sess-2" {
		t.Fatalf("recomputed %v, want [sess-1 sess-2] — an error stranded the rest", got)
	}
}

// The tracker never hands over an empty batch, but the helper is the thing a
// future caller would reach for, so it must not query on nothing either.
func TestRecomputeEvictedSessions_EmptyBatchDoesNothing(t *testing.T) {
	chats, rec := evictionFixture(t)

	recomputeEvictedSessions(nil, chats, rec, zerolog.Nop())

	if chats.calls != 0 {
		t.Fatalf("chat lookups = %d, want 0", chats.calls)
	}
	if got := rec.sorted(); len(got) != 0 {
		t.Fatalf("recomputed %v, want none", got)
	}
}

// wireEvictionRecompute is what main.go calls, so the tracker → hook →
// recompute path is exercised through the real callback body rather than a
// re-implementation of it.
//
// Driven through Remove rather than Cleanup: the tracker has no injectable
// clock, so a Cleanup-driven case here would have to sleep past StaleThreshold
// (15s) for what is the same hook and the same callback body. The Cleanup →
// hook edge is pinned in-package by the tracker's own eviction tests, where
// entries can be backdated directly, and the end-to-end compose is pinned by
// the display-computer tests.
func TestWireEvictionRecompute_RecomputesOnRemove(t *testing.T) {
	chats, rec := evictionFixture(t)
	tracker := status.NewTracker()
	chats.chats["agent-a"] = &models.AgentChat{AgentSessionID: "agent-a", SessionID: "sess-1"}

	wireEvictionRecompute(tracker, chats, rec, zerolog.Nop())

	tracker.Update("agent-a", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	tracker.Remove("agent-a")

	if got := rec.sorted(); len(got) != 1 || got[0] != "sess-1" {
		t.Fatalf("recomputed %v, want [sess-1]", got)
	}
}
