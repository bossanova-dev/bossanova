package taskorchestrator

import (
	"context"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
)

// SessionLivenessChecker reports whether a task-orchestrated session
// is still making progress. Used by the recovery sweep to detect stuck tasks.
type SessionLivenessChecker interface {
	IsSessionAlive(ctx context.Context, sessionID string) bool
}

// defaultLivenessChecker checks liveness by looking at the session state
// and whether the Claude process is still running.
type defaultLivenessChecker struct {
	sessions db.SessionStore
	claude   claude.ClaudeRunner
}

// NewLivenessChecker creates a SessionLivenessChecker backed by the
// session store and Claude runner.
func NewLivenessChecker(sessions db.SessionStore, claude claude.ClaudeRunner) SessionLivenessChecker {
	return &defaultLivenessChecker{sessions: sessions, claude: claude}
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
		// Early states — fall through to Claude liveness check below.
	default:
		// All post-ImplementingPlan states (PushingBranch, AwaitingChecks, etc.)
		// are driven by VCS events, not Claude. The task is not stuck.
		return true
	}

	// If there's no Claude session ID, the process was never started
	// or has already been cleaned up.
	if sess.ClaudeSessionID == nil {
		return false
	}

	return c.claude.IsRunning(*sess.ClaudeSessionID)
}
