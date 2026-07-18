package session

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/tmux"
)

// stalePaneProxyPort parses the port out of a baked ANTHROPIC_BASE_URL
// (http://127.0.0.1:<port>/s/<token>) and reports whether it is stale relative
// to the live failover-proxy port. A pane is stale only when the baked URL
// carries a real non-zero port that differs from the live port. Any parse
// failure, a missing port, an empty URL, or an unbound live port (0) yields
// (0, false) so the caller stays inert rather than surfacing a false positive.
func stalePaneProxyPort(bakedURL string, livePort int) (int, bool) {
	if bakedURL == "" || livePort == 0 {
		return 0, false
	}
	u, err := url.Parse(bakedURL)
	if err != nil {
		return 0, false
	}
	portStr := u.Port()
	if portStr == "" {
		return 0, false
	}
	bakedPort, err := strconv.Atoi(portStr)
	if err != nil || bakedPort == 0 {
		return 0, false
	}
	return bakedPort, bakedPort != livePort
}

// detectStalePaneProxyPorts is a best-effort startup sweep (BOS-409) that flags
// live managed tmux panes whose baked ANTHROPIC_BASE_URL points at a proxy port
// other than the one this (post-restart) daemon actually bound. Such a pane's
// already-running Claude REPL froze the old URL in its own process environment,
// so it retries a dead port forever — a permanent ConnectionRefused wedge that a
// pane restart is the only in-place cure for. Re-pointing a running REPL's env
// is infeasible (tmux set-environment only affects newly-spawned processes), so
// the repair is to SURFACE: a loud Warn plus a durable rotation audit marker the
// operator can see. The pane is NEVER auto-killed — that would destroy the live
// REPL/scrollback.
//
// It mirrors reArmSurvivingTmuxChats: iterate agent_chats rows that carry a tmux
// session, resolve each one's tmux session name, and skip any whose pane is not
// live. A single bad row (DB miss, parse error, dead pane) never blocks the
// rest. The whole sweep is inert when the proxy is off or unbound, and after the
// root fix ships (a fixed port that survives restarts) it only fires on the
// transitional edges — a changed port config, or a pane baked a pre-upgrade
// ephemeral port.
func (l *Lifecycle) detectStalePaneProxyPorts(ctx context.Context) {
	if l.agentChats == nil {
		return
	}
	// Inert when the proxy is disabled or has not bound: there is no live port
	// to compare against, so there is nothing to flag.
	if !l.failoverProxyEnabled() || l.proxyPort == 0 {
		return
	}
	chats, err := l.agentChats.ListWithTmuxSession(ctx)
	if err != nil {
		l.logger.Warn().Err(err).Msg("bootstrap stale-proxy sweep: failed to list chats with tmux session")
		return
	}
	for _, chat := range chats {
		if chat == nil || chat.AgentSessionID == "" {
			continue
		}
		// Only Claude sessions ever get an injected ANTHROPIC_BASE_URL.
		agentName := chat.AgentName
		if agentName == "" {
			agentName = string(models.AccountProviderClaude)
		}
		if agentName != string(models.AccountProviderClaude) {
			continue
		}

		tmuxName := ""
		if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
			tmuxName = *chat.TmuxSessionName
		} else {
			sess, serr := l.sessions.Get(ctx, chat.SessionID)
			if serr != nil {
				l.logger.Warn().Err(serr).
					Str("agent_session", chat.AgentSessionID).
					Str("session", chat.SessionID).
					Msg("bootstrap stale-proxy sweep: failed to load session; skipping")
				continue
			}
			tmuxName = tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
		}
		if tmuxName == "" || l.tmux == nil || !l.tmux.HasSession(ctx, tmuxName) {
			continue
		}

		baked, ok := l.tmux.ShowEnv(ctx, tmuxName, "ANTHROPIC_BASE_URL")
		if !ok || baked == "" {
			continue
		}
		bakedPort, stale := stalePaneProxyPort(baked, l.proxyPort)
		if !stale {
			continue
		}

		l.logger.Warn().
			Str("chat_id", chat.ID).
			Str("agent_session", chat.AgentSessionID).
			Str("session", chat.SessionID).
			Str("tmux_session", tmuxName).
			Int("baked_port", bakedPort).
			Int("live_port", l.proxyPort).
			Msg("failover proxy: tmux pane baked to a stale proxy port after a daemon restart; restart this pane to reconnect")

		// Durable needs-attention marker via the existing rotation audit sink
		// (Recorder is nil-safe). Trigger/Outcome are left empty — this is not a
		// rotation decision; the human-readable explanation lives in Detail so no
		// new proto/db enum value is invented for this ticket.
		l.rotationRecorder.Record(ctx, rotation.AuditEvent{
			SessionID: chat.SessionID,
			ChatID:    chat.ID,
			Provider:  string(models.AccountProviderClaude),
			Detail: fmt.Sprintf(
				"stale failover-proxy port: pane baked %d, live proxy on %d — restart this pane to reconnect (BOS-409)",
				bakedPort, l.proxyPort),
		})
	}
}
