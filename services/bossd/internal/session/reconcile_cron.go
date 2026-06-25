package session

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// cronAgentIdleThreshold is how long a cron run's agent log must be unwritten
// before the run counts as finished. Cron Claude streams output while working
// and goes quiet when done; the window is generous enough that a mid-run pause
// (a slow tool call) won't be mistaken for completion.
const cronAgentIdleThreshold = 15 * time.Minute

// agentLogIdleFor returns how long the agent's tmux-mirrored log has been idle
// (now - mtime) and whether that could be determined. The cron tmux chat mirrors
// pane output to agent-logs/<agentSessionID>.log via `tmux pipe-pane`, so the
// mtime is the last-output time — and unlike the Stop hook or the in-memory
// status tracker, it survives daemon restarts. known=false (missing log, empty
// id/dir) means "no durable evidence"; callers decide the fail-safe direction.
func agentLogIdleFor(agentLogsDir, agentSessionID string, now time.Time) (time.Duration, bool) {
	if agentLogsDir == "" || agentSessionID == "" {
		return 0, false
	}
	st, err := os.Stat(agentLogPathFor(agentLogsDir, agentSessionID))
	if err != nil {
		return 0, false
	}
	return now.Sub(st.ModTime()), true
}

// sessionLiveness reports whether a boss session is still running. It includes
// both headless agent subprocesses and tmux-hosted chats, so cron overlap
// suppression still works when the tmux log pipe failed before writing a log.
type sessionLiveness interface {
	IsSessionAlive(context.Context, string) bool
}

// CronActivityChecker reports whether a cron session's agent is still actively
// producing output, from the durable agent-log mtime. It satisfies the cron
// package's ActivityChecker seam.
//
// When the log mtime is available it drives the decision (idle < threshold =>
// active). When it is missing — StartTmuxChat treats `tmux pipe-pane` failure as
// non-fatal, so a live agent can run with no <agentSessionID>.log — the checker
// falls back to process liveness rather than blindly reporting inactive: a
// still-running agent counts as active (fail closed) so a degraded log-capture
// path cannot silently disable overlap suppression. Only when liveness confirms
// the agent is gone, or no liveness checker is wired, is a logless run reported
// as NOT active so it never blocks the next scheduled fire.
type CronActivityChecker struct {
	agentLogsDir string
	liveness     sessionLiveness
}

func NewCronActivityChecker(agentLogsDir string, liveness sessionLiveness) *CronActivityChecker {
	return &CronActivityChecker{agentLogsDir: agentLogsDir, liveness: liveness}
}

func (c *CronActivityChecker) RunActive(sess *models.Session) bool {
	if sess == nil || sess.AgentSessionID == nil || *sess.AgentSessionID == "" {
		return false
	}
	// Only ImplementingPlan represents active agent work here; later states may
	// still be "alive" for task orchestration while waiting on PR/check events,
	// but they must not block future cron fires.
	if sess.State != machine.ImplementingPlan {
		return false
	}
	idle, known := agentLogIdleFor(c.agentLogsDir, *sess.AgentSessionID, time.Now())
	if !known {
		// No durable log evidence: fall back to full session liveness so a live
		// tmux-hosted agent with a failed pipe-pane still suppresses overlap.
		// Nil liveness keeps the conservative fail-open default (don't block the
		// next fire).
		if c.liveness != nil {
			return c.liveness.IsSessionAlive(context.Background(), sess.ID)
		}
		return false
	}
	return idle < cronAgentIdleThreshold
}

// RecoverStrandedCronSessions finalizes cron sessions whose agent run has ended
// but whose Stop-hook finalize signal never reached the daemon — e.g. the
// daemon restarted as the run finished, so the loopback hook server's ephemeral
// port (baked into the worktree's settings.local.json at session start) was
// stale and the Stop-hook curl got connection-refused. The session is then
// stranded in ImplementingPlan with no completion trigger:
// RecoverFinalizingSessions only rescues sessions already in Finalizing, and
// the hookless tmux-completion poll is both Claude-disabled and lost on restart.
//
// For each cron session in ImplementingPlan whose agent log has gone quiet and
// whose headless runner is not running, this routes the session
// through the same cron completion gate the Stop hook and hookless poll use
// (notifyCronCompletionIfCronSession → NotifyCronAgentStopped → FinalizeSession).
// FinalizeSession is idempotent via its conditional ImplementingPlan→Finalizing
// transition, so a late Stop hook racing the sweep is a safe no-op.
//
// Conservative by construction: only cron-linked sessions with durable idle
// evidence are touched; any ambiguity (missing log/id, running agent) leaves the
// session for the next sweep rather than risking premature finalize of a live
// run.
//
// Call once at daemon startup (alongside RecoverFinalizingSessions, after the
// cron completion notifier is wired) and periodically thereafter. Returns the
// number of sessions routed to finalization. Per-session errors are not
// possible here — the gate dispatch is fire-and-forget — but a failure to list
// sessions aborts the sweep.
func (l *Lifecycle) RecoverStrandedCronSessions(ctx context.Context) (int, error) {
	if l.cronCompletionNotifier == nil {
		// No gate wired (legacy/partial wiring): nothing to route to.
		return 0, nil
	}

	stranded, err := l.sessions.ListByState(ctx, int(machine.ImplementingPlan))
	if err != nil {
		return 0, fmt.Errorf("list implementing_plan sessions: %w", err)
	}

	routed := 0
	for _, sess := range stranded {
		if sess.CronJobID == nil || *sess.CronJobID == "" {
			continue
		}
		if !l.cronRunIsOver(sess) {
			continue
		}

		l.logger.Warn().
			Str("session", sess.ID).
			Str("cronJob", *sess.CronJobID).
			Msg("recovering stranded cron session: agent idle but finalize signal lost")
		l.notifyCronCompletionIfCronSession(ctx, sess.ID)
		routed++
	}

	return routed, nil
}

// cronRunIsOver reports whether a cron session's agent run has finished. Cron
// Claude runs interactively in tmux and stays alive idle after its turn, so
// tmux/process liveness can't decide this; the durable signal is the agent log
// going quiet. Fail-safe: any sign of life — or no durable idle evidence —
// returns false (treat as still running), so we never finalize a live run.
func (l *Lifecycle) cronRunIsOver(sess *models.Session) bool {
	if sess.AgentSessionID == nil || *sess.AgentSessionID == "" {
		return false
	}
	if l.agentRunner != nil && l.agentRunner.IsRunningByAgent(sess.AgentName, *sess.AgentSessionID) {
		return false
	}
	idle, known := agentLogIdleFor(l.agentLogsDir, *sess.AgentSessionID, time.Now())
	if !known {
		return false
	}
	return idle >= cronAgentIdleThreshold
}
