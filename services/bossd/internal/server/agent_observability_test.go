package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
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
	tracker.SetAuthFailed(agentSessionID, true)

	got := listOneSession(t, sess, chats, tracker)

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
	tracker.SetAuthFailed(agentSessionID, true) // auth marker present, but session is already Blocked

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
