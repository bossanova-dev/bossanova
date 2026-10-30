package session

import (
	"context"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
)

// resolveAccountEnv returns the account env overlay for sess, or nil when no
// resolver is wired (older/test wiring) or the session is unbound. When the S7
// failover proxy is enabled and live, it also folds in the ANTHROPIC_BASE_URL
// overlay that points the Claude subprocess at the loopback proxy (BOS-320),
// but only when the bound account resolver produced a Claude OAuth bearer the
// proxy can substitute for the BOS-326 sentinel. Never logs values (the proxy
// token is a secret).
// An error means the binding could not be honoured; callers propagate it and
// refuse the spawn (BOS-1142).
func (l *Lifecycle) resolveAccountEnv(ctx context.Context, sess *models.Session) (map[string]string, error) {
	var base map[string]string
	if l.accountEnv != nil {
		var err error
		base, err = l.accountEnv.Resolve(ctx, sess)
		if err != nil {
			return nil, err
		}
	}
	base = l.ApplyFailoverProxyEnv(sess, base)
	return l.ApplyCompanionProxyEnv(ctx, sess, sess.AgentName, base), nil
}

// resolveAccountEnvForChat is the CHAT-authority sibling of resolveAccountEnv
// (BOS-1135). Use it at any spawn whose account authority is a chat binding
// rather than the session's own — a respawn after a chat-scoped account switch,
// an orphan resume, or a resurrect, all of which build their spawn view from
// agent_chats and not from sessions.account_id.
//
// The difference is ONLY which proxy target the failover overlay is keyed on.
// Credential materialization is identical: both resolve from spawnSess, whose
// AccountID the caller has already set to the chat's account, so the child gets
// the right CLAUDE_CODE_OAUTH_TOKEN either way. What resolveAccountEnv gets
// wrong for a chat-bound spawn is the layer above that: it mints
// TokenForSession(sess.ID), whose target resolves through
// Lifecycle.CurrentBearer(<sessionID>) → sessions.account_id — the binding a
// chat-scoped switch deliberately never writes (BOS-1386). The pane therefore
// comes back up billing the account the operator just switched AWAY from, with
// no CLAUDE_CODE_OAUTH_TOKEN of its own to fall back on (the overlay deletes
// it). Keying the token on the chat instead routes CurrentBearer through
// currentBearerForChat → agent_chats.account_id, which is the binding the
// switch DID write.
//
// chat is the account authority, not merely the chat being spawned: pass nil
// (or a chat with no account of its own) when the account came from the
// session seed, and the session-scoped overlay is applied exactly as before.
// It does not have to correspond to a persisted agent_chats row yet — a fresh
// respawn resolves its env before Step 7 creates the row. TokenForChat carries
// the supplied account id as the target's fallback, but that fallback is only
// reached once the row exists and carries no account of its own:
// chatProxyBinding returns no binding while the row is still absent, so the
// pre-persist gap itself is NOT covered by it.
//
// Never logs values (both the account bearer and the proxy token are secrets).
//
// Returns an error for the same reason resolveAccountEnv does (BOS-1142): a
// bound account whose credentials cannot be injected must refuse the spawn, not
// silently hand back an env without them and let the child come up on the agent
// CLI's ambient login. Every caller of this function is a spawn path, so the
// error is never advisory -- swallowing it here would reopen the silent degrade
// on exactly the chat-scoped paths BOS-1135 added.
func (l *Lifecycle) resolveAccountEnvForChat(ctx context.Context, spawnSess *models.Session, chat *domain.AgentChat) (map[string]string, error) {
	var base map[string]string
	if l.accountEnv != nil {
		var err error
		base, err = l.accountEnv.Resolve(ctx, spawnSess)
		if err != nil {
			return nil, err
		}
	}
	if chat == nil || chat.AgentSessionID == "" || chat.AccountID == nil {
		// No chat-scoped authority to key a target on: the account came from the
		// session seed. Degrade to the session-scoped overlay, which is
		// byte-identical to resolveAccountEnv.
		//
		// The discriminator is `chat.AccountID == nil` and nothing more, matching
		// Server.resolveChatAccountEnvForSpawn (account_binding.go) exactly — the
		// one place this shape is already proven. A non-nil pointer to "" is an
		// EXPLICIT account-0/unmanaged binding, not an absent one, and the
		// codebase honours that distinction elsewhere; testing the pointee here
		// too would be a third, wider definition of "chat-bound" that only this
		// function held. It would also be inert: proxyBaseURLForChat returns ""
		// for an empty accountID exactly as proxyBaseURL does for an empty
		// sessions.account_id, so both arms leave base untouched either way.
		base = l.ApplyFailoverProxyEnv(spawnSess, base)
		return l.ApplyCompanionProxyEnv(ctx, spawnSess, spawnSess.AgentName, base), nil
	}
	base = l.ApplyFailoverProxyEnvForChat(spawnSess, chat, *chat.AccountID, base)
	return l.ApplyCompanionProxyEnv(ctx, spawnSess, spawnSess.AgentName, base), nil
}
