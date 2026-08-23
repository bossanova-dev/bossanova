package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
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

func (f fakeRotationEventStore) ConfirmedAuthInvalidationSince(_ context.Context, sessionID, chatID string, since time.Time) (bool, error) {
	for _, ev := range f.bySession[sessionID] {
		if ev.ChatID == chatID &&
			ev.Trigger == "ROTATION_TRIGGER_AUTH_INVALIDATED" &&
			ev.Outcome != "" &&
			ev.Outcome != "ROTATION_OUTCOME_UNSPECIFIED" &&
			ev.Outcome != "ROTATION_OUTCOME_STATUS_ONLY_DISABLED" &&
			!ev.CreatedAt.Before(since) {
			return true, nil
		}
	}
	return false, nil
}

func (f fakeRotationEventStore) append(sessionID string, ev db.RotationEvent) {
	f.bySession[sessionID] = append([]db.RotationEvent{ev}, f.bySession[sessionID]...)
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
			ChatID:      agentSessionID,
			Provider:    "claude",
			Trigger:     "ROTATION_TRIGGER_AUTH_INVALIDATED",
			FromAccount: "acct-a",
			ToAccount:   "acct-b",
			Outcome:     "ROTATION_OUTCOME_ROTATED",
			CreatedAt:   now.Add(time.Hour),
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
		publishAgentMarkerSessionDelta(ctx, id, hydrator, bus, zerolog.Nop())
	})

	tracker.SetAuthFailed(agentSessionID, true)
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
	tracker.SetAuthFailed(agentSessionID, true)
	_ = nextStreamEvent(t, events)
	tracker.Remove(agentSessionID)
	removeClearEvent := nextStreamEvent(t, events)
	removeClearSession := requireSessionDelta(t, removeClearEvent)
	if removeClearSession.GetAttentionStatus() != nil {
		t.Fatalf("remove clear delta attention_status = %+v, want nil", removeClearSession.GetAttentionStatus())
	}
}

func TestPublishAuthFailedSessionDelta_RepublishesAfterCorroboratingAudit(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	bus := upstream.NewStreamBus(zerolog.Nop())
	defer bus.Close()
	events := bus.Subscribe(ctx)

	agentSessionID := "agent-auth-audit-1"
	sessionID := "sess-auth-audit-1"
	repoID := "repo-auth-audit-1"
	now := time.Now()
	sessions := &fakeSessionStore{byID: map[string]*models.Session{
		sessionID: {
			ID:        sessionID,
			RepoID:    repoID,
			Title:     "auth audit stream",
			State:     machine.ImplementingPlan,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}}
	chats := &fakeAgentChatStore{byAgentSessionID: map[string]*models.AgentChat{
		agentSessionID: {ID: "chat-auth-audit-1", SessionID: sessionID, AgentSessionID: agentSessionID},
	}}
	tracker := status.NewTracker()
	rotationEvents := fakeRotationEventStore{bySession: map[string][]db.RotationEvent{}}
	hydrator := &streamSessionHydrator{
		agentChats:        chats,
		rawSessions:       sessions,
		chatStatusTracker: tracker,
		rotationEvents:    rotationEvents,
		accountLabeler:    fakeStreamAccountLabeler{},
		logger:            zerolog.Nop(),
	}
	tracker.SetOnAuthChange(func(id string) {
		publishAgentMarkerSessionDelta(ctx, id, hydrator, bus, zerolog.Nop())
		rotationEvents.append(sessionID, db.RotationEvent{
			ID:        "rot-auth-audit-1",
			SessionID: sessionID,
			ChatID:    agentSessionID,
			Provider:  "claude",
			Trigger:   "ROTATION_TRIGGER_AUTH_INVALIDATED",
			Outcome:   "ROTATION_OUTCOME_ROTATED",
			CreatedAt: time.Now(),
		})
		publishAgentMarkerSessionDelta(ctx, id, hydrator, bus, zerolog.Nop())
	})

	tracker.SetAuthFailed(agentSessionID, true)
	tracker.SetAuthFailed(agentSessionID, true)

	first := requireSessionDelta(t, nextStreamEvent(t, events))
	if first.GetAttentionStatus() != nil {
		t.Fatalf("first delta attention_status = %+v, want nil before audit corroboration", first.GetAttentionStatus())
	}
	second := requireSessionDelta(t, nextStreamEvent(t, events))
	if got := second.GetAttentionStatus().GetReason(); got != bossanovav1.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Fatalf("second delta attention reason = %v, want AGENT_AUTH_FAILED after audit corroboration", got)
	}
}

// TestPublishStalledSessionDelta_SetAndClear is the BOS-667 twin of the auth
// test above, and exists for the same reason: the cloud/web read model is fed
// ONLY by the reverse stream, and a stalled chat's status stays WORKING, so if
// the SetOnStalledChange wiring in main.go were dropped or mis-wired nothing
// else would ever emit a delta carrying AGENT_STALLED — web would show a
// serenely "working" session forever and every unit test would still pass.
//
// It also pins the CLEAR direction, which is the fail-open half of the feature:
// once progress resumes the marker must disappear from the stream, not linger.
func TestPublishStalledSessionDelta_SetAndClear(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	bus := upstream.NewStreamBus(zerolog.Nop())
	defer bus.Close()
	events := bus.Subscribe(ctx)

	agentSessionID := "agent-stalled-1"
	sessionID := "sess-stalled-1"
	repoID := "repo-stalled-1"
	now := time.Now()
	sessions := &fakeSessionStore{byID: map[string]*models.Session{
		sessionID: {
			ID:        sessionID,
			RepoID:    repoID,
			Title:     "stalled stream",
			State:     machine.ImplementingPlan,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}}
	chats := &fakeAgentChatStore{byAgentSessionID: map[string]*models.AgentChat{
		agentSessionID: {ID: "chat-stalled-1", SessionID: sessionID, AgentSessionID: agentSessionID},
	}}
	repos := &fakeRepoStore{byID: map[string]*models.Repo{
		repoID: {ID: repoID, DisplayName: "repo display", OriginURL: "git@github.com:acme/repo.git"},
	}}
	tracker := status.NewTracker()
	hydrator := &streamSessionHydrator{
		agentChats:        chats,
		rawSessions:       sessions,
		repos:             repos,
		chatStatusTracker: tracker,
		rotationEvents:    fakeRotationEventStore{},
		accountLabeler:    fakeStreamAccountLabeler{},
		logger:            zerolog.Nop(),
	}
	tracker.SetOnStalledChange(func(id string) {
		publishAgentMarkerSessionDelta(ctx, id, hydrator, bus, zerolog.Nop())
	})

	tracker.SetStalled(agentSessionID, true)
	setSession := requireSessionDelta(t, nextStreamEvent(t, events))
	if got := setSession.GetAttentionStatus().GetReason(); got != bossanovav1.AttentionReason_ATTENTION_REASON_AGENT_STALLED {
		t.Fatalf("set delta attention reason = %v, want AGENT_STALLED", got)
	}
	if got := setSession.GetBlockedReason(); got != "agent-stalled" {
		t.Fatalf("set delta blocked_reason = %q, want agent-stalled", got)
	}
	if got := setSession.GetRepoDisplayName(); got != "repo display" {
		t.Fatalf("set delta repo_display_name = %q, want repo display", got)
	}

	tracker.SetStalled(agentSessionID, false)
	clearSession := requireSessionDelta(t, nextStreamEvent(t, events))
	if clearSession.GetAttentionStatus() != nil {
		t.Fatalf("clear delta attention_status = %+v, want nil", clearSession.GetAttentionStatus())
	}
	if clearSession.BlockedReason != nil {
		t.Fatalf("clear delta blocked_reason = %q, want nil", clearSession.GetBlockedReason())
	}

	// Removing the chat outright (pane gone) must also clear, or a session whose
	// chat died while stalled would keep the banner with nothing left to check.
	tracker.SetStalled(agentSessionID, true)
	_ = nextStreamEvent(t, events)
	tracker.Remove(agentSessionID)
	removeClearSession := requireSessionDelta(t, nextStreamEvent(t, events))
	if removeClearSession.GetAttentionStatus() != nil {
		t.Fatalf("remove clear delta attention_status = %+v, want nil", removeClearSession.GetAttentionStatus())
	}
}

// TestPublishStalledSessionDelta_AuthOutranksStalled pins the priority rule at
// the STREAM boundary, not just in the hydrator unit test: when a chat is both
// auth-failed and stalled, the delta web receives must name the actionable
// reason (/login), never the vaguer symptom.
func TestPublishStalledSessionDelta_AuthOutranksStalled(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	bus := upstream.NewStreamBus(zerolog.Nop())
	defer bus.Close()
	events := bus.Subscribe(ctx)

	agentSessionID := "agent-both-1"
	sessionID := "sess-both-1"
	repoID := "repo-both-1"
	now := time.Now()
	sessions := &fakeSessionStore{byID: map[string]*models.Session{
		sessionID: {
			ID:        sessionID,
			RepoID:    repoID,
			Title:     "both markers",
			State:     machine.ImplementingPlan,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}}
	chats := &fakeAgentChatStore{byAgentSessionID: map[string]*models.AgentChat{
		agentSessionID: {ID: "chat-both-1", SessionID: sessionID, AgentSessionID: agentSessionID},
	}}
	repos := &fakeRepoStore{byID: map[string]*models.Repo{
		repoID: {ID: repoID, DisplayName: "repo display", OriginURL: "git@github.com:acme/repo.git"},
	}}
	tracker := status.NewTracker()
	hydrator := &streamSessionHydrator{
		agentChats:        chats,
		rawSessions:       sessions,
		repos:             repos,
		chatStatusTracker: tracker,
		rotationEvents: fakeRotationEventStore{bySession: map[string][]db.RotationEvent{
			sessionID: {{
				ID:        "rot-auth",
				SessionID: sessionID,
				ChatID:    agentSessionID,
				Provider:  "claude",
				Trigger:   "ROTATION_TRIGGER_AUTH_INVALIDATED",
				Outcome:   "ROTATION_OUTCOME_ROTATED",
				CreatedAt: now.Add(time.Hour),
			}},
		}},
		accountLabeler: fakeStreamAccountLabeler{},
		logger:         zerolog.Nop(),
	}
	tracker.SetOnStalledChange(func(id string) {
		publishAgentMarkerSessionDelta(ctx, id, hydrator, bus, zerolog.Nop())
	})

	tracker.SetAuthFailed(agentSessionID, true) // no hook wired: publishes nothing
	tracker.SetAuthFailed(agentSessionID, true)
	tracker.SetStalled(agentSessionID, true)
	session := requireSessionDelta(t, nextStreamEvent(t, events))
	if got := session.GetAttentionStatus().GetReason(); got != bossanovav1.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Fatalf("attention reason = %v, want AGENT_AUTH_FAILED to outrank AGENT_STALLED", got)
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

// TestStreamHydrator_StampsDisplayStatus proves the reverse-stream projection
// carries the per-axis DisplayStatus from the in-memory tracker. The cloud/web
// read model is fed ONLY by this stream, and the web Merge button gates on
// DISPLAY_STATUS_PASSING — so without this hydration the button never shows on
// web even for a passing PR (BOS-365 follow-up).
func TestStreamHydrator_StampsDisplayStatus(t *testing.T) {
	t.Parallel()

	tracker := status.NewDisplayTracker()
	mergeable := true
	tracker.Set("sess-1", vcs.DisplayInfo{
		Status:    vcs.DisplayStatusPassing,
		Mergeable: &mergeable,
	})

	h := &streamSessionHydrator{
		displayTracker: tracker,
		logger:         zerolog.Nop(),
	}
	pbSess := &bossanovav1.Session{Id: "sess-1"}
	h.Hydrate(t.Context(), pbSess)

	if pbSess.GetDisplayStatus() != bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING {
		t.Fatalf("display_status = %v, want PASSING", pbSess.GetDisplayStatus())
	}
	if !pbSess.GetPrMergeable() {
		t.Errorf("pr_mergeable = %v, want true", pbSess.GetPrMergeable())
	}

	// A session with no tracker entry keeps DisplayStatus UNSPECIFIED (the
	// no-PR / not-yet-polled case) — the web then correctly hides Merge.
	unknown := &bossanovav1.Session{Id: "sess-unknown"}
	h.Hydrate(t.Context(), unknown)
	if unknown.GetDisplayStatus() != bossanovav1.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED {
		t.Fatalf("unknown session display_status = %v, want UNSPECIFIED", unknown.GetDisplayStatus())
	}
}
