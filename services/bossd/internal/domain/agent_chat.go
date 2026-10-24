package domain

import "time"

// AgentChatRole is the purpose a chat was created for. It is identity, set
// once at creation, unlike Title, which agents and operators rewrite freely.
// The zero value is the unknown role of a legacy row created before roles
// existed; it is stored as NULL.
type AgentChatRole string

const (
	AgentChatRoleUnknown  AgentChatRole = ""
	AgentChatRoleBuild    AgentChatRole = "build"
	AgentChatRoleRepair   AgentChatRole = "repair"
	AgentChatRoleVerify   AgentChatRole = "verify"
	AgentChatRoleOperator AgentChatRole = "operator"
)

// ParseAgentChatRole maps a stored or wire spelling onto a role. Anything it
// does not recognise is the unknown role, so a value written by a newer binary
// is treated with the caution owed to a legacy row rather than rejected.
func ParseAgentChatRole(s string) AgentChatRole {
	switch r := AgentChatRole(s); r {
	case AgentChatRoleBuild, AgentChatRoleRepair, AgentChatRoleVerify, AgentChatRoleOperator:
		return r
	case AgentChatRoleUnknown:
		return AgentChatRoleUnknown
	}
	return AgentChatRoleUnknown
}

// IsAgentOwned reports whether automation owns the chat: no operator is
// expected to be reading or typing into it, so automation may replace it once
// it has gone idle. Operator and unknown chats are never agent-owned.
func (r AgentChatRole) IsAgentOwned() bool {
	switch r {
	case AgentChatRoleBuild, AgentChatRoleRepair, AgentChatRoleVerify:
		return true
	case AgentChatRoleOperator, AgentChatRoleUnknown:
		return false
	}
	return false
}

// AgentChat represents an agent conversation associated with a session.
type AgentChat struct {
	ID                string
	SessionID         string
	AgentSessionID    string  // Agent session UUID
	ProviderSessionID *string // Provider-owned resume/session UUID, nil when same as AgentSessionID or not discovered yet
	AgentName         string  // Agent plugin name (e.g. "claude", "opencode")
	Title             string
	DaemonID          string  // Originating daemon (empty = local)
	TmuxSessionName   *string // tmux session name for this chat (nil = no tmux)
	// StartError is set when the agent failed to start for this chat
	// (e.g. SendPlan timed out, ConfigureFinalizeHook returned error).
	// nil on the happy path; non-nil rows are preserved as historical
	// "attempted but never came up" entries so the chat list can show a
	// (failed to start) badge instead of silently swallowing the
	// attempt.
	StartError *string
	// AccountID binds the chat to a rotation account; nil/empty = the
	// system-default account 0 (no injected env, D9).
	AccountID *string
	// Model is the per-chat agent model id. Empty string means "inherit the
	// agent CLI default" (BOS-381 moves model authority from the session to the
	// chat). bossd never enumerates valid models; the agent CLI validates it.
	Model string
	// Role is the purpose the chat was created for; the zero value is a legacy
	// row whose purpose is unknown.
	Role AgentChatRole
	// ReclaimedAt and ReclaimReason are set when automation replaced this
	// agent-owned chat (repair reclaiming an idle one). Unlike StartError the
	// chat did run; nil means it was never replaced.
	ReclaimedAt   *time.Time
	ReclaimReason *string
	CreatedAt     time.Time
	// Seq is the store's insertion sequence (SQLite rowid). CreatedAt is stored
	// to the millisecond, so two chats can share it; Seq orders them. Zero for
	// a chat that was never read from the store.
	Seq int64
}

// IsRetired reports whether the chat's pane is gone for a reason bossd
// recorded and will not come back by itself: it never started, or automation
// replaced it. Such a row is history, not a live, parked or reachable chat.
func (c *AgentChat) IsRetired() bool {
	return (c.StartError != nil && *c.StartError != "") || c.ReclaimedAt != nil
}
