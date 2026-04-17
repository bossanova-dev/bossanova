package taskorchestrator

import (
	"context"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
)

// SessionLivenessChecker reports whether a task-orchestrated session
// is still making progress. Used by the recovery sweep to detect stuck tasks.
type SessionLivenessChecker interface {
	IsSessionAlive(ctx context.Context, sessionID string) bool
}

// defaultLivenessChecker checks liveness by looking at the session state
// and whether the Claude process or tmux session is still running.
type defaultLivenessChecker struct {
	sessions db.SessionStore
	chats    db.ClaudeChatStore
	claude   claude.ClaudeRunner
	tmux     *tmux.Client
}

// NewLivenessChecker creates a SessionLivenessChecker backed by the
// session store, chat store, Claude runner, and tmux client.
func NewLivenessChecker(sessions db.SessionStore, chats db.ClaudeChatStore, claude claude.ClaudeRunner, tmux *tmux.Client) SessionLivenessChecker {
	return &defaultLivenessChecker{sessions: sessions, chats: chats, claude: claude, tmux: tmux}
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
	case machine.CreatingWorktree, machine.StartingClaude, machine.ImplementingPlan:
		// Early states -- fall through to Claude liveness check below.
	default:
		// All post-ImplementingPlan states (PushingBranch, AwaitingChecks, etc.)
		// are driven by VCS events, not Claude. The task is not stuck.
		return true
	}

	hasClaudeID := sess.ClaudeSessionID != nil && *sess.ClaudeSessionID != ""

	// Check headless Claude runner.
	if hasClaudeID && c.claude.IsRunning(*sess.ClaudeSessionID) {
		return true
	}

	// Check per-chat tmux sessions (interactive Claude).
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
	if !hasClaudeID && !hasTmuxName {
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
