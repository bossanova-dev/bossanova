package domain

type CreateSessionParams struct {
	RepoID       string
	Title        string
	Plan         string
	WorktreePath string
	BranchName   string
	BaseBranch   string
	AgentName    string // Agent plugin name; daemon callers should pass a resolved name. Empty falls back to "claude" for legacy callers.
	Model        string // Opaque agent model id; "" = plugin default.
	// EffectiveModel and EffectiveEffort are write-once runtime-attribution
	// values resolved at creation from request value over per-agent config.
	EffectiveModel  string
	EffectiveEffort string
	// AccountID binds the session to a rotation account; nil/empty = the
	// system-default account 0 (no injected env, D9).
	AccountID  *string
	PRNumber   *int
	PRURL      *string
	TrackerID  *string
	TrackerURL *string
}
