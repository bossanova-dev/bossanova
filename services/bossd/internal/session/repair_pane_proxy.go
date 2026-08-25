package session

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
)

// SetPaneRepairDispatcher wires the probe-skipping pane-repair dispatcher used
// by RepairProxyPane (BOS-982). Production wires it to
// ChatRotator.OnProxyTokenUnresolved. Leaving it unset (nil) is safe and makes
// the whole attribution path inert — the proxy's unknown-token 401 then behaves
// exactly as it did before this seam existed.
func (l *Lifecycle) SetPaneRepairDispatcher(dispatch func(agentSessionID string)) {
	l.paneRepair = dispatch
}

// RepairProxyPane routes a proxy-minted "unknown session token" 401 straight to
// a same-account respawn of the pane that owns that token, skipping the account
// probe (BOS-982).
//
// The caller is the failover proxy's unknown-token branch, which has already
// resolved the presented token's digest to a DURABLE registration (sessionID +
// agentSessionID) that its in-memory map no longer holds — the exact shape a
// daemon restart or a Deregister-while-live produces. That 401 is self-inflicted:
// the account behind the pane was never consulted, so probing it can only ever
// answer "healthy", which is why this path skips the probe.
//
// Because the presented token is attacker-supplied (any local process can hit
// the loopback proxy with an arbitrary /s/<token>), attribution is verified
// against the pane's own live state before anything is dispatched:
//
//  1. the daemon's proxy must be enabled and bound (otherwise no pane can hold a
//     legitimately baked URL at all);
//  2. the chat must still exist and still be routable to a LIVE tmux session;
//  3. that session's own baked ANTHROPIC_BASE_URL must parse as a canonical
//     proxy URL on the LIVE port, via the unchanged parseBakedProxyToken; and
//  4. the token it yields must equal the presented one, compared in constant
//     time so a near-miss cannot be used as an oracle for a real pane's token.
//
// Any failed check returns (false, …) and the proxy writes its unchanged 401 —
// never a guess. The two tmux calls are for ONE named pane and sit behind the
// caller's per-fingerprint rate limit and a durable-row hit, so this is not the
// "live tmux scan on the proxy hot path" the design rules out.
//
// token is a secret: it is never logged here, and never leaves this function.
func (l *Lifecycle) RepairProxyPane(ctx context.Context, sessionID, agentSessionID, token string) (bool, error) {
	if l == nil || l.paneRepair == nil {
		return false, nil
	}
	if agentSessionID == "" || token == "" {
		return false, nil
	}
	if !l.failoverProxyEnabled() || l.proxyPort == 0 {
		return false, nil
	}
	if l.agentChats == nil || l.tmux == nil {
		return false, nil
	}

	chat, err := l.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		if errors.Is(err, db.ErrAgentChatNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("repair proxy pane: get chat %s: %w", agentSessionID, err)
	}
	if chat == nil {
		return false, nil
	}
	// Only Claude panes ever receive an injected ANTHROPIC_BASE_URL, so only a
	// Claude pane can legitimately present a proxy token.
	if proxyAgentName(chat.AgentName) != string(models.AccountProviderClaude) {
		return false, nil
	}

	tmuxName := persistedChatPaneName(chat)
	if tmuxName == "" {
		// Mirror the adoption sweep's fallback: a chat whose tmux name was never
		// persisted still has a live pane holding a baked token.
		owner := sessionID
		if chat.SessionID != "" {
			owner = chat.SessionID
		}
		if owner == "" || l.sessions == nil {
			return false, nil
		}
		sess, serr := l.sessions.Get(ctx, owner)
		if serr != nil {
			return false, fmt.Errorf("repair proxy pane: get session %s: %w", owner, serr)
		}
		if sess == nil {
			return false, nil
		}
		tmuxName = tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
	}
	if !l.paneIsLive(ctx, tmuxName) {
		// Never attribute a token to a pane that is not live.
		return false, nil
	}

	baked, ok := l.paneBakedProxyURL(ctx, tmuxName)
	if !ok {
		return false, nil
	}
	paneToken, ok := parseBakedProxyToken(baked, l.proxyPort)
	if !ok {
		// Non-canonical or port-mismatched: the adoption sweep skips exactly this
		// shape, and so must attribution.
		return false, nil
	}
	if subtle.ConstantTimeCompare([]byte(paneToken), []byte(token)) != 1 {
		return false, nil
	}

	l.logger.Info().
		Str("agent_session_id", agentSessionID).
		Str("tmux_session", tmuxName).
		Msg("failover proxy: unknown token attributed to a live pane; dispatching respawn-in-place without an account probe")
	l.paneRepair(agentSessionID)
	return true, nil
}
