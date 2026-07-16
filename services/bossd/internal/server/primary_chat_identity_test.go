package server

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// primaryChatIdentityStoreFake is a minimal db.AgentChatStore serving chats from
// a map by agent_session_id, for the BOS-381 primary-chat re-source tests.
type primaryChatIdentityStoreFake struct {
	db.AgentChatStore
	byAgentSession map[string]*models.AgentChat
}

func (f *primaryChatIdentityStoreFake) GetByAgentSessionID(_ context.Context, id string) (*models.AgentChat, error) {
	return f.byAgentSession[id], nil
}

// TestApplyPrimaryChatIdentity_OverridesProviderAndAccount asserts the pure
// applier re-sources agent_name/account_id from the primary chat and leaves the
// proto untouched when the chat inherits (nil account / empty agent).
func TestApplyPrimaryChatIdentity_OverridesProviderAndAccount(t *testing.T) {
	acct := "acct-codex-7"
	p := &pb.Session{AgentName: "claude", AccountId: strPtrLocal("acct-claude-1")}
	applyPrimaryChatIdentity(p, &models.AgentChat{AgentName: "codex", AccountID: &acct})
	if p.GetAgentName() != "codex" {
		t.Errorf("agent_name = %q, want codex (re-sourced from chat)", p.GetAgentName())
	}
	if p.GetAccountId() != acct {
		t.Errorf("account_id = %q, want %q (re-sourced from chat)", p.GetAccountId(), acct)
	}

	// A chat that never bound its own account/agent leaves the mirror in place.
	p2 := &pb.Session{AgentName: "claude", AccountId: strPtrLocal("acct-claude-1")}
	applyPrimaryChatIdentity(p2, &models.AgentChat{AgentName: "", AccountID: nil})
	if p2.GetAgentName() != "claude" {
		t.Errorf("agent_name = %q, want claude (inherited)", p2.GetAgentName())
	}
	if p2.GetAccountId() != "acct-claude-1" {
		t.Errorf("account_id = %q, want acct-claude-1 (inherited)", p2.GetAccountId())
	}
}

// TestWithPrimaryChatIdentity_ReSourcesFromPrimaryChat asserts the Server helper
// projects the primary chat's provider/account onto the Session proto (BOS-381):
// a claude-seeded session whose primary chat switched to codex/acct-2 reports the
// chat's values on the wire while the session columns stay a derived mirror.
func TestWithPrimaryChatIdentity_ReSourcesFromPrimaryChat(t *testing.T) {
	agentSessionID := "agent-primary-1"
	chatAcct := "acct-2"
	store := &primaryChatIdentityStoreFake{byAgentSession: map[string]*models.AgentChat{
		agentSessionID: {AgentSessionID: agentSessionID, AgentName: "codex", AccountID: &chatAcct},
	}}
	s := &Server{agentChats: store}

	session := &models.Session{
		ID:             "sess-1",
		AgentName:      "claude",
		AgentSessionID: &agentSessionID,
	}
	p := SessionToProto(session)
	if p.GetAgentName() != "claude" {
		t.Fatalf("precondition: SessionToProto agent_name = %q, want claude", p.GetAgentName())
	}

	s.withPrimaryChatIdentity(context.Background(), p, session)

	if p.GetAgentName() != "codex" {
		t.Errorf("agent_name = %q, want codex (from primary chat)", p.GetAgentName())
	}
	if p.GetAccountId() != chatAcct {
		t.Errorf("account_id = %q, want %q (from primary chat)", p.GetAccountId(), chatAcct)
	}
}

// TestPrimaryChatFromSlice_MatchesAgentSessionID asserts the list-path helper
// selects the primary chat (the one whose agent_session_id matches the session's)
// from a session's chat slice.
func TestPrimaryChatFromSlice_MatchesAgentSessionID(t *testing.T) {
	primary := &models.AgentChat{AgentSessionID: "agent-1", AgentName: "codex"}
	chats := []*models.AgentChat{
		{AgentSessionID: "agent-sibling", AgentName: "claude"},
		primary,
	}
	if got := primaryChatFromSlice(chats, "agent-1"); got != primary {
		t.Errorf("primaryChatFromSlice = %v, want the agent-1 chat", got)
	}
	if got := primaryChatFromSlice(chats, ""); got != nil {
		t.Errorf("primaryChatFromSlice(\"\") = %v, want nil", got)
	}
	if got := primaryChatFromSlice(chats, "missing"); got != nil {
		t.Errorf("primaryChatFromSlice(missing) = %v, want nil", got)
	}
}

func strPtrLocal(s string) *string { return &s }
