package main

import (
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/status"
)

func requireChatCounts(t *testing.T, sess *bossanovav1.Session, chats, working int32) {
	t.Helper()
	if sess.ChatCount == nil || sess.WorkingChatCount == nil {
		t.Fatalf("chat_count = %v, working_chat_count = %v, want both set", sess.ChatCount, sess.WorkingChatCount)
	}
	if sess.GetChatCount() != chats || sess.GetWorkingChatCount() != working {
		t.Fatalf("counts = %d chats, %d working; want %d, %d", sess.GetChatCount(), sess.GetWorkingChatCount(), chats, working)
	}
}

// Every recompute after a chat changes reaches bosso with the session's
// current counts, and a recompute that leaves them unchanged sends nothing:
// the event after the no-op is the next real change.
func TestChatCountRecomputer_PublishesOnlyWhenCountsMove(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	bus := upstream.NewStreamBus(zerolog.Nop())
	defer bus.Close()
	events := bus.Subscribe(ctx)

	sessionID := "sess-counts"
	now := time.Now()
	chats := &fakeDispatcherChatStore{byAgentSessionID: map[string]*domain.AgentChat{
		"agent-a": {ID: "chat-a", SessionID: sessionID, AgentSessionID: "agent-a"},
	}}
	tracker := status.NewTracker()
	hydrator := &streamSessionHydrator{
		agentChats: chats,
		rawSessions: &fakeSessionStore{byID: map[string]*models.Session{
			sessionID: {ID: sessionID, State: machine.ImplementingPlan, CreatedAt: now, UpdatedAt: now},
		}},
		repos:             &fakeRepoStore{byID: map[string]*models.Repo{}},
		chatStatusTracker: tracker,
		rotationEvents:    fakeRotationEventStore{bySession: map[string][]db.RotationEvent{}},
		logger:            zerolog.Nop(),
	}
	display := &sweepRecomputerFake{}
	recompute := chatCountRecomputer{display: display, hydrator: hydrator, streamBus: bus, logger: zerolog.Nop()}

	if err := recompute.Recompute(ctx, sessionID); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	requireChatCounts(t, requireSessionDelta(t, nextStreamEvent(t, events)), 1, 0)

	if err := recompute.Recompute(ctx, sessionID); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	tracker.Update("agent-a", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, now)
	if err := recompute.Recompute(ctx, sessionID); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	requireChatCounts(t, requireSessionDelta(t, nextStreamEvent(t, events)), 1, 1)

	chats.byAgentSessionID["agent-b"] = &domain.AgentChat{ID: "chat-b", SessionID: sessionID, AgentSessionID: "agent-b"}
	tracker.Update("agent-b", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, now)
	if err := recompute.Recompute(ctx, sessionID); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	requireChatCounts(t, requireSessionDelta(t, nextStreamEvent(t, events)), 2, 1)

	if got := len(display.sessions); got != 4 {
		t.Fatalf("display recomputes = %d, want 4", got)
	}
}

// A session whose chats cannot be listed reports no counts, never zero.
func TestStreamSessionHydrator_LeavesCountsAbsentWithoutChats(t *testing.T) {
	t.Parallel()

	sess := &bossanovav1.Session{Id: "sess-no-chats"}
	(&streamSessionHydrator{logger: zerolog.Nop()}).Hydrate(t.Context(), sess)
	if sess.ChatCount != nil || sess.WorkingChatCount != nil {
		t.Fatalf("chat_count = %v, working_chat_count = %v, want both absent", sess.ChatCount, sess.WorkingChatCount)
	}
}
