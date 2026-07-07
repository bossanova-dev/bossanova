package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/upstream"
)

type fakeRepoStore struct {
	db.RepoStore
	byID map[string]*models.Repo
}

func (f *fakeRepoStore) Get(_ context.Context, id string) (*models.Repo, error) {
	if repo, ok := f.byID[id]; ok {
		return repo, nil
	}
	return nil, sql.ErrNoRows
}

type fakeStreamAccountLabeler struct {
	labels map[string]string
}

func (f fakeStreamAccountLabeler) Label(_ context.Context, accountID string) (string, error) {
	if label, ok := f.labels[accountID]; ok {
		return label, nil
	}
	return "System default", nil
}

type fakeRotationEventStore struct {
	db.RotationEventStore
	bySession map[string][]db.RotationEvent
}

func (f fakeRotationEventStore) Insert(context.Context, db.RotationEvent) error { return nil }

func (f fakeRotationEventStore) RecentBySession(_ context.Context, sessionID string, limit int) ([]db.RotationEvent, error) {
	evs := append([]db.RotationEvent(nil), f.bySession[sessionID]...)
	if limit > 0 && len(evs) > limit {
		evs = evs[:limit]
	}
	return evs, nil
}

func TestPublishAuthFailedSessionDelta_SetAndClear(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	bus := upstream.NewStreamBus(zerolog.Nop())
	defer bus.Close()
	events := bus.Subscribe(ctx)

	agentSessionID := "agent-1"
	sessionID := "sess-1"
	repoID := "repo-1"
	accountID := "acct-1"
	now := time.Now()
	sessions := &fakeSessionStore{byID: map[string]*models.Session{
		sessionID: {
			ID:        sessionID,
			RepoID:    repoID,
			Title:     "auth stream",
			State:     machine.ImplementingPlan,
			AccountID: &accountID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}}
	chats := &fakeAgentChatStore{byAgentSessionID: map[string]*models.AgentChat{
		agentSessionID: {ID: "chat-1", SessionID: sessionID, AgentSessionID: agentSessionID},
	}}
	repos := &fakeRepoStore{byID: map[string]*models.Repo{
		repoID: {ID: repoID, DisplayName: "repo display", OriginURL: "git@github.com:acme/repo.git"},
	}}
	tracker := status.NewTracker()
	rotationEvents := fakeRotationEventStore{bySession: map[string][]db.RotationEvent{
		sessionID: {{
			ID:          "rot-1",
			SessionID:   sessionID,
			Provider:    "claude",
			Trigger:     "ROTATION_TRIGGER_USAGE_LIMITED",
			FromAccount: "acct-a",
			ToAccount:   "acct-b",
			Outcome:     "ROTATION_OUTCOME_ROTATED",
			CreatedAt:   now,
		}},
	}}
	hydrator := &streamSessionHydrator{
		agentChats:        chats,
		rawSessions:       sessions,
		repos:             repos,
		chatStatusTracker: tracker,
		rotationEvents:    rotationEvents,
		accountLabeler:    fakeStreamAccountLabeler{labels: map[string]string{accountID: "Claude Team"}},
		logger:            zerolog.Nop(),
	}
	tracker.SetOnAuthChange(func(id string) {
		publishAuthFailedSessionDelta(ctx, id, hydrator, bus, zerolog.Nop())
	})

	tracker.SetAuthFailed(agentSessionID, true)
	setEvent := nextStreamEvent(t, events)
	setSession := requireSessionDelta(t, setEvent)
	if got := setSession.GetAttentionStatus().GetReason(); got != bossanovav1.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Fatalf("set delta attention reason = %v, want AGENT_AUTH_FAILED", got)
	}
	if got := setSession.GetBlockedReason(); got != "agent-auth-failed" {
		t.Fatalf("set delta blocked_reason = %q, want agent-auth-failed", got)
	}
	if got := setSession.GetRepoDisplayName(); got != "repo display" {
		t.Fatalf("set delta repo_display_name = %q, want repo display", got)
	}
	if got := setSession.GetAccountLabel(); got != "Claude Team" {
		t.Fatalf("set delta account_label = %q, want Claude Team", got)
	}
	if len(setSession.GetRotationEvents()) != 1 {
		t.Fatalf("set delta rotation_events len = %d, want 1", len(setSession.GetRotationEvents()))
	}

	tracker.SetAuthFailed(agentSessionID, false)
	clearEvent := nextStreamEvent(t, events)
	clearSession := requireSessionDelta(t, clearEvent)
	if clearSession.GetAttentionStatus() != nil {
		t.Fatalf("clear delta attention_status = %+v, want nil", clearSession.GetAttentionStatus())
	}
	if clearSession.BlockedReason != nil {
		t.Fatalf("clear delta blocked_reason = %q, want nil", clearSession.GetBlockedReason())
	}
	if got := clearSession.GetAccountLabel(); got != "Claude Team" {
		t.Fatalf("clear delta account_label = %q, want Claude Team", got)
	}
	if len(clearSession.GetRotationEvents()) != 1 {
		t.Fatalf("clear delta rotation_events len = %d, want 1", len(clearSession.GetRotationEvents()))
	}

	tracker.SetAuthFailed(agentSessionID, true)
	_ = nextStreamEvent(t, events)
	tracker.Remove(agentSessionID)
	removeClearEvent := nextStreamEvent(t, events)
	removeClearSession := requireSessionDelta(t, removeClearEvent)
	if removeClearSession.GetAttentionStatus() != nil {
		t.Fatalf("remove clear delta attention_status = %+v, want nil", removeClearSession.GetAttentionStatus())
	}
}

func nextStreamEvent(t *testing.T, events <-chan upstream.StreamEvent) upstream.StreamEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("stream bus channel closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for stream event")
		return upstream.StreamEvent{}
	}
}

func requireSessionDelta(t *testing.T, ev upstream.StreamEvent) *bossanovav1.Session {
	t.Helper()
	if ev.Session == nil {
		t.Fatalf("event session = nil: %+v", ev)
	}
	if ev.Session.Kind != bossanovav1.SessionDelta_KIND_UPDATED {
		t.Fatalf("event kind = %v, want UPDATED", ev.Session.Kind)
	}
	if ev.Session.Session == nil {
		t.Fatalf("event session payload = nil")
	}
	return ev.Session.Session
}
