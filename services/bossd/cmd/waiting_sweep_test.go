package main

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
)

type sweepChatLookupFake struct {
	chats map[string]*models.AgentChat
	err   error
	calls int
}

func (f *sweepChatLookupFake) GetByAgentSessionID(_ context.Context, agentSessionID string) (*models.AgentChat, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.chats[agentSessionID], nil
}

type sweepRecomputerFake struct {
	sessions []string
	err      error
}

func (f *sweepRecomputerFake) Recompute(_ context.Context, sessionID string) error {
	f.sessions = append(f.sessions, sessionID)
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
