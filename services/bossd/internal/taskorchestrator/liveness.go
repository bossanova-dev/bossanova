package taskorchestrator

import (
	"context"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
)

// SessionLivenessChecker reports whether a task-orchestrated session
// is still making progress. Used by the recovery sweep to detect stuck tasks.
type SessionLivenessChecker interface {
	IsSessionAlive(ctx context.Context, sessionID string) bool
}

// defaultLivenessChecker checks liveness by looking at the session state
// and whether the agent process or tmux session is still running.
type defaultLivenessChecker struct {
	sessions        db.SessionStore
	chats           db.AgentChatStore
	agentForSession func(*models.Session) agent.AgentRunner
	tmux            *tmux.Client
}

// NewLivenessChecker creates a SessionLivenessChecker backed by the
// session store, chat store, a per-session agent runner resolver, and
// tmux client. agentForSession returns the AgentRunner that should be
// queried for a given session — typically a Dispatcher that does its
// own internal routing — and may return nil for sessions whose agent
// plugin isn't loaded; IsSessionAlive treats nil as "skip the runner
// check, fall through to tmux/chat liveness signals" rather than as
// a fatal error.
func NewLivenessChecker(sessions db.SessionStore, chats db.AgentChatStore, agentForSession func(*models.Session) agent.AgentRunner, tmux *tmux.Client) SessionLivenessChecker {
	return &defaultLivenessChecker{sessions: sessions, chats: chats, agentForSession: agentForSession, tmux: tmux}
}

func (c *defaultLivenessChecker) IsSessionAlive(ctx context.Context, sessionID string) bool {
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return false
	}

	// If the session has advanced past ImplementingPlan, VCS events
	// (checks passed/failed, PR merged/closed) handle completion.
	// The task is not stuck.
	switch sess.State {
	case machine.CreatingWorktree, machine.StartingAgent, machine.ImplementingPlan:
		// Early states -- fall through to agent liveness check below.
	default:
		// All post-ImplementingPlan states (PushingBranch, AwaitingChecks, etc.)
		// are driven by VCS events, not the agent. The task is not stuck.
		return true
	}

	hasAgentID := sess.AgentSessionID != nil && *sess.AgentSessionID != ""

	// Check headless agent runner. agentForSession may return nil for
	// sessions whose agent plugin isn't loaded — fall through to the
	// tmux/chat checks rather than treating a missing runner as fatal.
	if hasAgentID {
		runner := c.agentForSession(sess)
		if runner != nil && runner.IsRunning(*sess.AgentSessionID) {
			return true
		}
	}

	// Check per-chat tmux sessions (interactive agent).
	if c.tmux != nil && c.chats != nil {
		chats, chatErr := c.chats.ListBySession(ctx, sessionID)
		if chatErr == nil {
			for _, chat := range chats {
				if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" &&
					c.tmux.HasSession(ctx, *chat.TmuxSessionName) {
					return true
				}
			}
		}
	}

	// Legacy: check session-level tmux name.
	hasTmuxName := sess.TmuxSessionName != nil && *sess.TmuxSessionName != ""
	if hasTmuxName && c.tmux != nil && c.tmux.HasSession(ctx, *sess.TmuxSessionName) {
		return true
	}

	// If neither process identifier is set, the session is still initializing
	// (e.g. quick chat waiting for first user attach). Don't mark it as dead.
	if !hasAgentID && !hasTmuxName {
		if c.chats != nil {
			// Check if any chats exist with tmux names.
			chats, chatErr := c.chats.ListBySession(ctx, sessionID)
			if chatErr == nil && len(chats) == 0 {
				return true
			}
		} else {
			return true
		}
	}

	return false
}
