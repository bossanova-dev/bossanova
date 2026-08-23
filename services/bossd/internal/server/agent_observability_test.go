package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

// listOneSession runs ListSessions against the shared display-status test
// harness and returns the single hydrated proto Session.
func listOneSession(t *testing.T, sess *models.Session, chats map[string][]*models.AgentChat, chatStatus *status.Tracker) *pb.Session {
	t.Helper()
	s := newListSessionsDisplayStatusTestServer([]*models.Session{sess}, chats, nil, chatStatus)
	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	return onlySession(t, resp.Msg.Sessions)
}

func listOneSessionWithRotationEvents(t *testing.T, sess *models.Session, chats map[string][]*models.AgentChat, chatStatus *status.Tracker, events []db.RotationEvent) *pb.Session {
	t.Helper()
	s := newListSessionsDisplayStatusTestServer([]*models.Session{sess}, chats, nil, chatStatus)
	s.rotationEvents = fakeRotationEventStore{events: events}
	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	return onlySession(t, resp.Msg.Sessions)
}

func implementingSessionWithChat(agentSessionID string) (*models.Session, map[string][]*models.AgentChat) {
	sess := &models.Session{
		ID:        "sess-1",
		RepoID:    "repo-1",
		Title:     "observability",
		State:     machine.ImplementingPlan,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	chats := map[string][]*models.AgentChat{
		sess.ID: {{ID: "chat-1", SessionID: sess.ID, AgentSessionID: agentSessionID}},
	}
	return sess, chats
}

type fakeRotationEventStore struct {
	db.RotationEventStore
	events []db.RotationEvent
}

func (f fakeRotationEventStore) RecentBySession(_ context.Context, sessionID string, limit int) ([]db.RotationEvent, error) {
	var out []db.RotationEvent
	for _, ev := range f.events {
		if ev.SessionID == sessionID {
			out = append(out, ev)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f fakeRotationEventStore) ConfirmedAuthInvalidationSince(_ context.Context, sessionID, chatID string, since time.Time) (bool, error) {
	for _, ev := range f.events {
		if ev.SessionID == sessionID &&
			ev.ChatID == chatID &&
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

func confirmedAuthRotationEvent(sessionID, chatID string) db.RotationEvent {
	return db.RotationEvent{
		ID:        "rot-confirmed",
		SessionID: sessionID,
		ChatID:    chatID,
		Trigger:   "ROTATION_TRIGGER_AUTH_INVALIDATED",
		Outcome:   "ROTATION_OUTCOME_ROTATED",
		CreatedAt: time.Now(),
	}
}

func unconfirmedAuthRotationEvent(sessionID, chatID string) db.RotationEvent {
	return db.RotationEvent{
		ID:        "rot-unconfirmed",
		SessionID: sessionID,
		ChatID:    chatID,
		Trigger:   "ROTATION_TRIGGER_AUTH_INVALIDATED",
		Outcome:   "ROTATION_OUTCOME_STATUS_ONLY_PROBE_UNCONFIRMED",
		Detail:    "auth probe did not confirm invalidation",
		CreatedAt: time.Now(),
	}
}

func confirmedAuthRotationProto(chatID ...string) *pb.RotationEvent {
	id := "agent-1"
	if len(chatID) > 0 {
		id = chatID[0]
	}
	return confirmedAuthRotationProtoAt(id, time.Now())
}

func confirmedAuthRotationProtoAt(chatID string, createdAt time.Time) *pb.RotationEvent {
	return &pb.RotationEvent{
		ChatId:    chatID,
		Trigger:   pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED,
		Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
		CreatedAt: timestamppb.New(createdAt),
	}
}

func unconfirmedAuthRotationProto(chatID ...string) *pb.RotationEvent {
	id := "agent-1"
	if len(chatID) > 0 {
		id = chatID[0]
	}
	return unconfirmedAuthRotationProtoAt(id, time.Now())
}

func unconfirmedAuthRotationProtoAt(chatID string, createdAt time.Time) *pb.RotationEvent {
	return &pb.RotationEvent{
		ChatId:    chatID,
		Trigger:   pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED,
		Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED,
		Detail:    "auth probe did not confirm invalidation",
		CreatedAt: timestamppb.New(createdAt),
	}
}

func setPersistentAuthFailed(tracker *status.Tracker, agentSessionID string) {
	tracker.SetAuthFailed(agentSessionID, true)
	tracker.SetAuthFailed(agentSessionID, true)
}

func TestListSessions_HeartbeatAdvancesOnChatOutput(t *testing.T) {
	agentSessionID := "agent-1"
	sess, chats := implementingSessionWithChat(agentSessionID)
	tracker := status.NewTracker()
	lastOutput := time.Now().Add(-2 * time.Second)
	tracker.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_WORKING, lastOutput)

	got := listOneSession(t, sess, chats, tracker)

	if got.GetLastAgentActivityAt() == nil {
		t.Fatal("last_agent_activity_at = nil, want set")
	}
	if !got.GetLastAgentActivityAt().AsTime().Equal(lastOutput) {
		t.Errorf("last_agent_activity_at = %v, want %v", got.GetLastAgentActivityAt().AsTime(), lastOutput)
	}
}

func TestListSessions_HeartbeatKeysOnContentChangeNotMtime(t *testing.T) {
	agentSessionID := "agent-1"
	sess, chats := implementingSessionWithChat(agentSessionID)
	tracker := status.NewTracker()

	// First real output at t1 (content changed).
	t1 := time.Now().Add(-10 * time.Second)
	tracker.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_WORKING, t1)
	// A later heartbeat with a FRESH ReceivedAt but the SAME LastOutputAt — this
	// is exactly what the poller reports on pane-mtime-only churn (content
	// unchanged: it passes prev.at forward). The heartbeat must NOT advance.
	tracker.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_IDLE, t1)

	got := listOneSession(t, sess, chats, tracker)

	if got.GetLastAgentActivityAt() == nil {
		t.Fatal("last_agent_activity_at = nil, want set")
	}
	if !got.GetLastAgentActivityAt().AsTime().Equal(t1) {
		t.Errorf("last_agent_activity_at = %v, want %v (content-change time, not heartbeat mtime)",
			got.GetLastAgentActivityAt().AsTime(), t1)
	}
}

func TestListSessions_HeartbeatIgnoresStoppedChat(t *testing.T) {
	agentSessionID := "agent-1"
	sess, chats := implementingSessionWithChat(agentSessionID)
	tracker := status.NewTracker()

	// The poller stamps a STOPPED chat's LastOutputAt to `now` when its tmux
	// session disappears (a display-label heartbeat, not genuine agent output).
	// Counting it would make a dead/crashed chat read as freshly active on every
	// poll and defeat the stale-vs-live signal — so a STOPPED chat must NOT set
	// last_agent_activity_at, even with a brand-new timestamp.
	tracker.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_STOPPED, time.Now())

	got := listOneSession(t, sess, chats, tracker)

	if got.GetLastAgentActivityAt() != nil {
		t.Errorf("last_agent_activity_at = %v, want nil (stopped chat is not live activity)",
			got.GetLastAgentActivityAt().AsTime())
	}
}

func TestListSessions_HeartbeatAbsentWhenNoChatActivity(t *testing.T) {
	agentSessionID := "agent-1"
	sess, chats := implementingSessionWithChat(agentSessionID)
	// Empty tracker: no observed output for this chat.
	got := listOneSession(t, sess, chats, status.NewTracker())

	if got.GetLastAgentActivityAt() != nil {
		t.Errorf("last_agent_activity_at = %v, want nil (no observed activity)", got.GetLastAgentActivityAt())
	}
}

func TestListSessions_AuthFailedOverlaySetsAttention(t *testing.T) {
	agentSessionID := "agent-1"
	sess, chats := implementingSessionWithChat(agentSessionID)
	tracker := status.NewTracker()
	setPersistentAuthFailed(tracker, agentSessionID)

	got := listOneSessionWithRotationEvents(t, sess, chats, tracker, []db.RotationEvent{
		confirmedAuthRotationEvent(sess.ID, agentSessionID),
	})

	if got.GetAttentionStatus() == nil {
		t.Fatal("attention_status = nil, want AGENT_AUTH_FAILED")
	}
	if got.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Errorf("attention reason = %v, want AGENT_AUTH_FAILED", got.GetAttentionStatus().GetReason())
	}
	if !got.GetAttentionStatus().GetNeedsAttention() {
		t.Error("needs_attention = false, want true")
	}
	if got.GetBlockedReason() != agentAuthFailedBlockedReason {
		t.Errorf("blocked_reason = %q, want %q", got.GetBlockedReason(), agentAuthFailedBlockedReason)
	}
}

func TestListSessions_NoAuthFlagOnNormalOutput(t *testing.T) {
	agentSessionID := "agent-1"
	sess, chats := implementingSessionWithChat(agentSessionID)
	tracker := status.NewTracker()
	tracker.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	// No auth marker set.

	got := listOneSession(t, sess, chats, tracker)

	if got.GetAttentionStatus() != nil {
		t.Errorf("attention_status = %v, want nil (no auth marker)", got.GetAttentionStatus())
	}
	if got.GetBlockedReason() != "" {
		t.Errorf("blocked_reason = %q, want empty", got.GetBlockedReason())
	}
}

func TestHydrateAgentObservability_AuthFailedRequiresRotationCorroboration(t *testing.T) {
	agentSessionID := "agent-1"
	chats := []*models.AgentChat{{ID: "chat-1", SessionID: "sess-1", AgentSessionID: agentSessionID}}
	tests := []struct {
		name           string
		events         func() []*pb.RotationEvent
		wantReason     pb.AttentionReason
		wantBlockedRsn string
	}{
		{
			name:       "missing rotation event suppresses overlay",
			wantReason: pb.AttentionReason_ATTENTION_REASON_UNSPECIFIED,
		},
		{
			name:       "unconfirmed auth probe suppresses overlay",
			events:     func() []*pb.RotationEvent { return []*pb.RotationEvent{unconfirmedAuthRotationProto()} },
			wantReason: pb.AttentionReason_ATTENTION_REASON_UNSPECIFIED,
		},
		{
			name:           "confirmed auth invalidation applies overlay",
			events:         func() []*pb.RotationEvent { return []*pb.RotationEvent{confirmedAuthRotationProto()} },
			wantReason:     pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED,
			wantBlockedRsn: agentAuthFailedBlockedReason,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := status.NewTracker()
			setPersistentAuthFailed(tracker, agentSessionID)

			var events []*pb.RotationEvent
			if tt.events != nil {
				events = tt.events()
			}
			p := &pb.Session{Id: "sess-1", UpdatedAt: timestamppb.New(time.Now()), RotationEvents: events}
			HydrateAgentObservability(tracker, p, chats)

			if got := p.GetAttentionStatus().GetReason(); got != tt.wantReason {
				t.Errorf("attention reason = %v, want %v", got, tt.wantReason)
			}
			if got := p.GetBlockedReason(); got != tt.wantBlockedRsn {
				t.Errorf("blocked_reason = %q, want %q", got, tt.wantBlockedRsn)
			}
		})
	}
}

func TestHydrateAgentObservability_AuthCorroborationMustMatchFailedChat(t *testing.T) {
	currentAgentSessionID := "agent-current"
	otherAgentSessionID := "agent-old"
	chats := []*models.AgentChat{
		{ID: "chat-current", SessionID: "sess-1", AgentSessionID: currentAgentSessionID},
		{ID: "chat-old", SessionID: "sess-1", AgentSessionID: otherAgentSessionID},
	}
	tracker := status.NewTracker()
	setPersistentAuthFailed(tracker, currentAgentSessionID)

	p := &pb.Session{
		Id:        "sess-1",
		UpdatedAt: timestamppb.New(time.Now()),
		RotationEvents: []*pb.RotationEvent{
			confirmedAuthRotationProto(otherAgentSessionID),
			unconfirmedAuthRotationProto(currentAgentSessionID),
		},
	}
	HydrateAgentObservability(tracker, p, chats)
	if p.GetAttentionStatus() != nil {
		t.Fatalf("attention_status = %+v, want nil when only another chat has confirmed auth audit", p.GetAttentionStatus())
	}

	p.RotationEvents = append([]*pb.RotationEvent{confirmedAuthRotationProto(currentAgentSessionID)}, p.GetRotationEvents()...)
	HydrateAgentObservability(tracker, p, chats)
	if got := p.GetAttentionStatus().GetReason(); got != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Fatalf("attention reason = %v, want AGENT_AUTH_FAILED after current chat is corroborated", got)
	}
}

func TestHydrateAgentObservability_AuthCorroborationIgnoresEarlierEpisode(t *testing.T) {
	agentSessionID := "agent-current"
	chats := []*models.AgentChat{{ID: "chat-current", SessionID: "sess-1", AgentSessionID: agentSessionID}}
	tracker := status.NewTracker()
	setPersistentAuthFailed(tracker, agentSessionID)
	since, ok := tracker.AuthFailedSince(agentSessionID)
	if !ok {
		t.Fatal("AuthFailedSince = false, want current auth episode")
	}

	p := &pb.Session{
		Id:        "sess-1",
		UpdatedAt: timestamppb.New(time.Now()),
		RotationEvents: []*pb.RotationEvent{
			confirmedAuthRotationProtoAt(agentSessionID, since.Add(-time.Second)),
		},
	}
	HydrateAgentObservability(tracker, p, chats)
	if p.GetAttentionStatus() != nil {
		t.Fatalf("attention_status = %+v, want nil for audit before current auth episode", p.GetAttentionStatus())
	}

	p.RotationEvents = append([]*pb.RotationEvent{confirmedAuthRotationProtoAt(agentSessionID, since.Add(time.Second))}, p.GetRotationEvents()...)
	HydrateAgentObservability(tracker, p, chats)
	if got := p.GetAttentionStatus().GetReason(); got != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Fatalf("attention reason = %v, want AGENT_AUTH_FAILED after current episode is corroborated", got)
	}
}

func TestHydrateAgentObservability_AuthCorroborationComparesAtRotationEventPrecision(t *testing.T) {
	agentSessionID := "agent-current"
	chats := []*models.AgentChat{{ID: "chat-current", SessionID: "sess-1", AgentSessionID: agentSessionID}}
	var (
		tracker   *status.Tracker
		since     time.Time
		createdAt time.Time
		ok        bool
	)
	for i := 0; i < 100; i++ {
		tracker = status.NewTracker()
		setPersistentAuthFailed(tracker, agentSessionID)
		since, ok = tracker.AuthFailedSince(agentSessionID)
		if !ok {
			t.Fatal("AuthFailedSince = false, want current auth episode")
		}
		createdAt = rotationEventComparableTime(since)
		if createdAt.Before(since) {
			break
		}
		time.Sleep(time.Microsecond)
	}
	if !createdAt.Before(since) {
		t.Fatalf("could not create fractional-millisecond auth episode timestamp after retries; since=%v", since)
	}

	p := &pb.Session{
		Id:        "sess-1",
		UpdatedAt: timestamppb.New(time.Now()),
		RotationEvents: []*pb.RotationEvent{
			confirmedAuthRotationProtoAt(agentSessionID, createdAt),
		},
	}
	HydrateAgentObservability(tracker, p, chats)
	if got := p.GetAttentionStatus().GetReason(); got != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Fatalf("attention reason = %v, want AGENT_AUTH_FAILED for same-millisecond corroboration", got)
	}
}

func TestHydrateAgentObservability_AuthCorroborationChecksNewestEventPerFailedChat(t *testing.T) {
	chatA := "agent-a"
	chatB := "agent-b"
	chats := []*models.AgentChat{
		{ID: "chat-a", SessionID: "sess-1", AgentSessionID: chatA},
		{ID: "chat-b", SessionID: "sess-1", AgentSessionID: chatB},
	}
	tracker := status.NewTracker()
	setPersistentAuthFailed(tracker, chatA)
	setPersistentAuthFailed(tracker, chatB)
	sinceA, ok := tracker.AuthFailedSince(chatA)
	if !ok {
		t.Fatal("AuthFailedSince(chatA) = false, want current auth episode")
	}
	sinceB, ok := tracker.AuthFailedSince(chatB)
	if !ok {
		t.Fatal("AuthFailedSince(chatB) = false, want current auth episode")
	}

	p := &pb.Session{
		Id:        "sess-1",
		UpdatedAt: timestamppb.New(time.Now()),
		RotationEvents: []*pb.RotationEvent{
			unconfirmedAuthRotationProtoAt(chatA, sinceA.Add(2*time.Second)),
			confirmedAuthRotationProtoAt(chatB, sinceB.Add(time.Second)),
		},
	}
	HydrateAgentObservability(tracker, p, chats)
	if got := p.GetAttentionStatus().GetReason(); got != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Fatalf("attention reason = %v, want AGENT_AUTH_FAILED from chat B despite newer unconfirmed chat A event", got)
	}
}

func TestListSessions_AuthCorroborationSurvivesRotationHistoryCap(t *testing.T) {
	agentSessionID := "agent-current"
	sess, chats := implementingSessionWithChat(agentSessionID)
	tracker := status.NewTracker()
	setPersistentAuthFailed(tracker, agentSessionID)
	since, ok := tracker.AuthFailedSince(agentSessionID)
	if !ok {
		t.Fatal("AuthFailedSince = false, want current auth episode")
	}

	events := make([]db.RotationEvent, 0, rotationEventsCap+1)
	for i := 0; i < rotationEventsCap; i++ {
		events = append(events, db.RotationEvent{
			ID:        "newer-unrelated",
			SessionID: sess.ID,
			ChatID:    "agent-other",
			Trigger:   "ROTATION_TRIGGER_USAGE_LIMITED",
			Outcome:   "ROTATION_OUTCOME_ROTATED",
			CreatedAt: since.Add(time.Duration(rotationEventsCap-i) * time.Second),
		})
	}
	events = append(events, db.RotationEvent{
		ID:        "older-current-auth",
		SessionID: sess.ID,
		ChatID:    agentSessionID,
		Trigger:   "ROTATION_TRIGGER_AUTH_INVALIDATED",
		Outcome:   "ROTATION_OUTCOME_ROTATED",
		CreatedAt: since.Add(time.Millisecond),
	})

	got := listOneSessionWithRotationEvents(t, sess, chats, tracker, events)
	if gotEvents := got.GetRotationEvents(); len(gotEvents) != rotationEventsCap {
		t.Fatalf("rotation events = %d, want capped history only", len(gotEvents))
	}
	if got := got.GetAttentionStatus().GetReason(); got != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Fatalf("attention reason = %v, want AGENT_AUTH_FAILED from uncapped corroboration lookup", got)
	}
}

// TestHydrateAgentObservability_StandaloneAppliesOverlay locks the reverse-stream
// contract: the standalone overlay (used by cmd/main.go for snapshot + session
// deltas) applies AGENT_AUTH_FAILED and last_agent_activity_at to a bare Session
// proto, independent of the ListSessions display pipeline. Without this the
// cloud/web read model — fed solely by the reverse stream — never sees them.
func TestHydrateAgentObservability_StandaloneAppliesOverlay(t *testing.T) {
	agentSessionID := "agent-1"
	chats := []*models.AgentChat{{ID: "chat-1", SessionID: "sess-1", AgentSessionID: agentSessionID}}
	tracker := status.NewTracker()
	lastOutput := time.Now().Add(-2 * time.Second)
	tracker.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_WORKING, lastOutput)
	setPersistentAuthFailed(tracker, agentSessionID)

	p := &pb.Session{Id: "sess-1", UpdatedAt: timestamppb.New(time.Now()), RotationEvents: []*pb.RotationEvent{confirmedAuthRotationProto()}}
	HydrateAgentObservability(tracker, p, chats)

	if p.GetLastAgentActivityAt() == nil || !p.GetLastAgentActivityAt().AsTime().Equal(lastOutput) {
		t.Errorf("last_agent_activity_at = %v, want %v", p.GetLastAgentActivityAt().AsTime(), lastOutput)
	}
	if p.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Errorf("attention reason = %v, want AGENT_AUTH_FAILED", p.GetAttentionStatus().GetReason())
	}
	if p.GetBlockedReason() != agentAuthFailedBlockedReason {
		t.Errorf("blocked_reason = %q, want %q", p.GetBlockedReason(), agentAuthFailedBlockedReason)
	}
}

// TestHydrateBaseAttention_PreservesBlockedReasonUnderAuthOverlay locks the
// reverse-stream fix: a Blocked session that also has an auth-failed chat must
// keep its real blocked_reason. cmd/main.go computes HydrateBaseAttention on the
// bare SessionToProto output (which carries blocked_reason but no
// attention_status) before HydrateAgentObservability — mirroring the local
// GetSession/ListSessions ordering. Without the base attention step the auth
// overlay would overwrite blocked_reason and bosso's full-replacement read model
// would lose the actionable failure reason.
func TestHydrateBaseAttention_PreservesBlockedReasonUnderAuthOverlay(t *testing.T) {
	agentSessionID := "agent-1"
	blockedReason := "blocked — max attempts reached"
	sess := &models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		State:         machine.Blocked,
		BlockedReason: &blockedReason,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	repo := &models.Repo{ID: "repo-1"}
	chats := []*models.AgentChat{{ID: "chat-1", SessionID: sess.ID, AgentSessionID: agentSessionID}}
	tracker := status.NewTracker()
	setPersistentAuthFailed(tracker, agentSessionID) // auth marker present, but session is Blocked

	// Reverse-stream projection order: bare proto -> base attention -> overlay.
	p := SessionToProto(sess)
	HydrateBaseAttention(p, sess, repo)
	HydrateAgentObservability(tracker, p, chats)

	if p.GetAttentionStatus() == nil {
		t.Fatal("attention_status = nil, want BLOCKED_MAX_ATTEMPTS preserved")
	}
	if p.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS {
		t.Errorf("attention reason = %v, want BLOCKED_MAX_ATTEMPTS (auth must not override)", p.GetAttentionStatus().GetReason())
	}
	if p.GetBlockedReason() != blockedReason {
		t.Errorf("blocked_reason = %q, want %q (real reason preserved, not clobbered by auth overlay)", p.GetBlockedReason(), blockedReason)
	}
}

// TestHydrateBaseAttention_AuthOverlayStillAppliesWhenNoBaseAttention confirms
// the fix does not regress the normal case: a non-blocked session with an
// auth-failed chat still gets the AGENT_AUTH_FAILED overlay, because
// HydrateBaseAttention leaves attention_status nil when there is no VCS-derived
// reason.
func TestHydrateBaseAttention_AuthOverlayStillAppliesWhenNoBaseAttention(t *testing.T) {
	agentSessionID := "agent-1"
	sess := &models.Session{
		ID:        "sess-1",
		RepoID:    "repo-1",
		State:     machine.ImplementingPlan,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	repo := &models.Repo{ID: "repo-1"}
	chats := []*models.AgentChat{{ID: "chat-1", SessionID: sess.ID, AgentSessionID: agentSessionID}}
	tracker := status.NewTracker()
	setPersistentAuthFailed(tracker, agentSessionID)

	p := SessionToProto(sess)
	p.RotationEvents = []*pb.RotationEvent{confirmedAuthRotationProto()}
	HydrateBaseAttention(p, sess, repo)
	HydrateAgentObservability(tracker, p, chats)

	if p.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Errorf("attention reason = %v, want AGENT_AUTH_FAILED", p.GetAttentionStatus().GetReason())
	}
	if p.GetBlockedReason() != agentAuthFailedBlockedReason {
		t.Errorf("blocked_reason = %q, want %q", p.GetBlockedReason(), agentAuthFailedBlockedReason)
	}
}

// TestHydrateBaseAttention_FixingChecksNoAutoRepairDoesNotResurrectStaleConflict
// locks the P2 follow-up: HydrateBaseAttention runs on bare SessionToProto protos
// with no hydrated display status, so it must NOT emit the FixingChecks/
// CanAutoRepair=false MERGE_CONFLICT_UNRESOLVABLE reason. The local ListSessions
// path suppresses that reason once the display tracker moves out of
// DISPLAY_STATUS_CONFLICT; the reverse stream cannot run that suppression, so
// emitting it here would resurrect a stale conflict AND block the auth overlay.
// The session must instead receive AGENT_AUTH_FAILED when it has an auth-failed
// chat.
func TestHydrateBaseAttention_FixingChecksNoAutoRepairDoesNotResurrectStaleConflict(t *testing.T) {
	agentSessionID := "agent-1"
	sess := &models.Session{
		ID:        "sess-1",
		RepoID:    "repo-1",
		State:     machine.FixingChecks,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	repo := &models.Repo{ID: "repo-1", CanAutoRepair: false}
	chats := []*models.AgentChat{{ID: "chat-1", SessionID: sess.ID, AgentSessionID: agentSessionID}}
	tracker := status.NewTracker()
	setPersistentAuthFailed(tracker, agentSessionID)

	// Reverse-stream projection: bare proto (no display status) -> base attention
	// -> overlay. The display tracker has moved out of conflict (nothing hydrated
	// it here), so the conflict reason is stale and must not be resurrected.
	p := SessionToProto(sess)
	p.RotationEvents = []*pb.RotationEvent{confirmedAuthRotationProto()}
	HydrateBaseAttention(p, sess, repo)
	HydrateAgentObservability(tracker, p, chats)

	if p.GetAttentionStatus() == nil {
		t.Fatal("attention_status = nil, want AGENT_AUTH_FAILED overlay")
	}
	if got := p.GetAttentionStatus().GetReason(); got == pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE {
		t.Fatalf("attention reason = MERGE_CONFLICT_UNRESOLVABLE, stale conflict resurrected on reverse stream")
	}
	if got := p.GetAttentionStatus().GetReason(); got != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		t.Errorf("attention reason = %v, want AGENT_AUTH_FAILED", got)
	}
	if p.GetBlockedReason() != agentAuthFailedBlockedReason {
		t.Errorf("blocked_reason = %q, want %q", p.GetBlockedReason(), agentAuthFailedBlockedReason)
	}
}

// TestHydrateAgentObservability_NilTrackerIsNoop guards the poller/main.go seams
// that may run before the tracker is wired: no panic, no overlay.
func TestHydrateAgentObservability_NilTrackerIsNoop(t *testing.T) {
	chats := []*models.AgentChat{{ID: "chat-1", SessionID: "sess-1", AgentSessionID: "agent-1"}}
	p := &pb.Session{Id: "sess-1"}
	HydrateAgentObservability(nil, p, chats)
	if p.GetAttentionStatus() != nil || p.GetLastAgentActivityAt() != nil {
		t.Error("nil tracker must leave the proto untouched")
	}
}

func TestListSessions_AuthFailedDoesNotOverrideUnrelatedReason(t *testing.T) {
	agentSessionID := "agent-1"
	blockedReason := "blocked — max attempts reached"
	sess := &models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		Title:         "blocked session",
		State:         machine.Blocked,
		BlockedReason: &blockedReason,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	chats := map[string][]*models.AgentChat{
		sess.ID: {{ID: "chat-1", SessionID: sess.ID, AgentSessionID: agentSessionID}},
	}
	tracker := status.NewTracker()
	setPersistentAuthFailed(tracker, agentSessionID) // auth marker present, but session is already Blocked

	got := listOneSession(t, sess, chats, tracker)

	if got.GetAttentionStatus() == nil {
		t.Fatal("attention_status = nil, want BLOCKED_MAX_ATTEMPTS preserved")
	}
	if got.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS {
		t.Errorf("attention reason = %v, want BLOCKED_MAX_ATTEMPTS (auth must not override)", got.GetAttentionStatus().GetReason())
	}
	if got.GetBlockedReason() != blockedReason {
		t.Errorf("blocked_reason = %q, want %q (unrelated reason preserved)", got.GetBlockedReason(), blockedReason)
	}
}

// --- BOS-667: agent-stalled attention overlay ---

// TestHydrateAgentObservability_StalledOverlay is the table-driven contract for
// the AGENT_STALLED overlay: it fires only on a fresh stalled marker, never
// outranks a real VCS attention reason, and yields to AGENT_AUTH_FAILED when
// both markers are live (auth names the fix; "stalled" only names the symptom).
func TestHydrateAgentObservability_StalledOverlay(t *testing.T) {
	const agentSessionID = "agent-1"
	tests := []struct {
		name           string
		stalled        bool
		authFailed     bool
		preAttention   *pb.AttentionStatus
		wantReason     pb.AttentionReason
		wantBlockedRsn string
	}{
		{
			name:           "stalled marker raises AGENT_STALLED",
			stalled:        true,
			wantReason:     pb.AttentionReason_ATTENTION_REASON_AGENT_STALLED,
			wantBlockedRsn: agentStalledBlockedReason,
		},
		{
			name:       "no marker raises nothing",
			wantReason: pb.AttentionReason_ATTENTION_REASON_UNSPECIFIED,
		},
		{
			name:           "auth-failed outranks stalled",
			stalled:        true,
			authFailed:     true,
			wantReason:     pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED,
			wantBlockedRsn: agentAuthFailedBlockedReason,
		},
		{
			name:         "existing attention is preserved",
			stalled:      true,
			preAttention: &pb.AttentionStatus{NeedsAttention: true, Reason: pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS},
			wantReason:   pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chats := []*models.AgentChat{{ID: "chat-1", SessionID: "sess-1", AgentSessionID: agentSessionID}}
			tracker := status.NewTracker()
			tracker.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
			tracker.SetStalled(agentSessionID, tt.stalled)
			if tt.authFailed {
				setPersistentAuthFailed(tracker, agentSessionID)
			}

			p := &pb.Session{Id: "sess-1", UpdatedAt: timestamppb.New(time.Now()), AttentionStatus: tt.preAttention}
			if tt.authFailed {
				p.RotationEvents = []*pb.RotationEvent{confirmedAuthRotationProto()}
			}
			HydrateAgentObservability(tracker, p, chats)

			if got := p.GetAttentionStatus().GetReason(); got != tt.wantReason {
				t.Errorf("attention reason = %v, want %v", got, tt.wantReason)
			}
			if got := p.GetBlockedReason(); got != tt.wantBlockedRsn {
				t.Errorf("blocked_reason = %q, want %q", got, tt.wantBlockedRsn)
			}
		})
	}
}

// TestHydrateAgentObservability_StalledMarkerSelfHealsWhenStale locks the
// fail-open direction of the overlay: markers age out via status.StaleThreshold,
// so a poller that stopped ticking (crashed, or the chat went away) must NOT
// leave a permanent "your session is dead" banner behind.
func TestHydrateAgentObservability_StalledMarkerSelfHealsWhenStale(t *testing.T) {
	const agentSessionID = "agent-1"
	chats := []*models.AgentChat{{ID: "chat-1", SessionID: "sess-1", AgentSessionID: agentSessionID}}
	tracker := status.NewTracker()
	tracker.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	tracker.SetStalled(agentSessionID, true)
	tracker.SetStalled(agentSessionID, false)

	p := &pb.Session{Id: "sess-1", UpdatedAt: timestamppb.New(time.Now())}
	HydrateAgentObservability(tracker, p, chats)

	if p.GetAttentionStatus() != nil {
		t.Errorf("attention_status = %v, want nil once the stalled marker cleared", p.GetAttentionStatus())
	}
}
