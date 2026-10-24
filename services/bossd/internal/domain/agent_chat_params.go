package domain

type CreateAgentChatParams struct {
	SessionID         string
	AgentSessionID    string
	ProviderSessionID *string
	AgentName         string // Agent plugin name; empty falls back to "claude".
	Title             string
	// AccountID binds the chat to a rotation account; nil/empty = the
	// system-default account 0 (no injected env, D9).
	AccountID *string
	// Model is the per-chat agent model id; empty inherits the agent CLI
	// default (BOS-381). bossd never enumerates valid models.
	Model string
	// Role is the purpose the chat is created for; the unknown role is
	// stored as NULL.
	Role AgentChatRole
}

type UpdateResumedChatParams struct {
	// NewAgentSessionID re-keys the row onto a fresh agent_session_id.
	NewAgentSessionID *string
	// SessionID moves the chat to a different parent session.
	SessionID *string
	// Title replaces the chat title.
	Title *string
	// AgentName replaces the agent (provider) that owns the chat.
	AgentName *string
	// Model replaces the per-chat model id.
	Model *string
	// AccountID replaces the bound account (nullable).
	AccountID **string
	// ProviderSessionID replaces the provider's own session id (nullable).
	ProviderSessionID **string
	// ClearStartError clears start_error so a previously failed chat is
	// no longer badged as failed after a successful resume. The old
	// delete+create resume path cleared it implicitly; an in-place
	// rebind has to say so.
	ClearStartError bool
}
