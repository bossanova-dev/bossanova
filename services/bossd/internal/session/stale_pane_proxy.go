package session

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/rs/zerolog"

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

// detectStalePaneProxyPorts is a best-effort startup sweep (BOS-409) that finds
// live managed tmux panes whose baked ANTHROPIC_BASE_URL points at a proxy port
// other than the one this (post-restart) daemon actually bound. Such a pane's
// already-running Claude REPL froze the old URL in its own process environment,
// so it can never reach this daemon again, and re-pointing a running REPL's env
// is infeasible (tmux set-environment only affects newly-spawned processes).
//
// Each such pane is KILLED (BOS-1563), reversing BOS-409's original "surface,
// never auto-kill" rule for this one case: while the pane lives it keeps
// sending its credentials and prompts to whatever binds the old port — another
// local user's listener can answer them with tool calls that run as us — and
// losing its scrollback is the smaller cost. The kill clears the chat row's
// pane pointer first (the idle-reap shape, ReapIdleChatTmuxSession) and leaves
// the row itself in place, so the existing wake path respawns the conversation
// on the live port. A durable rotation audit marker records why.
//
// Fail-closed: only a baked URL tmux actually returned can condemn a pane. An
// unset or unreadable value (paneBakedProxyURL declines both) and any parse
// failure leave the pane alone.
//
// It mirrors reArmSurvivingTmuxChats: iterate agent_chats rows that carry a tmux
// session, resolve each one's tmux session name, and skip any whose pane is not
// live. A single bad row (DB miss, parse error, dead pane, failed kill) never
// blocks the rest. The whole sweep is inert when the proxy is off or unbound,
// and with a fixed port that survives restarts it only fires on a changed port
// config, a pane baked a pre-upgrade ephemeral port, or a lost fixed port.
// The sweep detects that loss indirectly: l.proxyPort (the live port) differs
// from the baked fixed port. ProxyServer.FixedPortLost reports the lost state
// itself, surfaced on boss repair doctor.
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
			agentName = string(models.AgentAccountProviderClaude)
		}
		if agentName != string(models.AgentAccountProviderClaude) {
			continue
		}

		tmuxName := persistedChatPaneName(chat)
		if tmuxName == "" {
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
		if !l.paneIsLive(ctx, tmuxName) {
			continue
		}

		baked, ok := l.paneBakedProxyURL(ctx, tmuxName)
		if !ok {
			continue
		}
		bakedPort, stale := stalePaneProxyPort(baked, l.proxyPort)
		if !stale {
			continue
		}

		logEv := func() *zerolog.Event {
			return l.logger.Error().
				Str("chat_id", chat.ID).
				Str("agent_session", chat.AgentSessionID).
				Str("session", chat.SessionID).
				Str("tmux_session", tmuxName).
				Int("baked_port", bakedPort).
				Int("live_port", l.proxyPort)
		}
		var detail string
		if kerr := l.killStalePane(ctx, chat, tmuxName); kerr != nil {
			logEv().Err(kerr).
				Msg("failover proxy: could not kill a tmux pane baked to a stale proxy port; it may still send traffic to whatever holds that port — kill it by hand")
			detail = fmt.Sprintf(
				"stale failover-proxy port: pane baked %d, live proxy on %d — kill failed (%v); kill this pane by hand (BOS-1563)",
				bakedPort, l.proxyPort, kerr)
		} else {
			logEv().Msg("failover proxy: killed a tmux pane baked to a stale proxy port; its chat resumes on the live port when woken")
			detail = fmt.Sprintf(
				"stale failover-proxy port: pane baked %d, live proxy on %d — pane killed so it cannot send traffic to whatever holds the old port; the chat resumes on the live port when woken (BOS-1563)",
				bakedPort, l.proxyPort)
		}

		// Durable needs-attention marker via the existing rotation audit sink
		// (Recorder is nil-safe). Trigger/Outcome are left empty — this is not a
		// rotation decision; the human-readable explanation lives in Detail so no
		// new proto/db enum value is invented for this ticket.
		l.rotationRecorder.Record(ctx, rotation.AuditEvent{
			SessionID: chat.SessionID,
			ChatID:    chat.ID,
			Provider:  string(models.AgentAccountProviderClaude),
			Detail:    detail,
		})
	}
}

// killStalePane tears down a stale-port pane while keeping its chat row
// resumable. A row that points at the pane goes through
// ReapIdleChatTmuxSession, which clears the pointer before killing (and
// restores it if the kill fails): a POINTED row over a dead pane is what the
// session guard reads as the agent having exited, which would finalize the
// session instead of letting the wake path resume it. A row with no pointer
// (the name was recomputed) has nothing to clear, so the pane is killed
// directly.
func (l *Lifecycle) killStalePane(ctx context.Context, chat *models.AgentChat, tmuxName string) error {
	if persistedChatPaneName(chat) == tmuxName {
		return l.ReapIdleChatTmuxSession(ctx, chat.SessionID, chat.AgentSessionID, tmuxName)
	}
	return l.tmux.KillSession(ctx, tmuxName)
}
