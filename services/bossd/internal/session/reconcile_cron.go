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

// strandedReapStates is the set of interrupted states the stranded-cron sweep
// reaps: ImplementingPlan (the historical case) plus the pre-implementation
// transitions a daemon restart can freeze a run in. Orphaned is deliberately
// EXCLUDED — it is a terminal state whose bootstrap-only PR must never be
// auto-finalized into a green PR; recovery there is a deliberate human action
// (machine.go). The post-implementation waiting states (AwaitingChecks,
// FixingChecks, GreenDraft, ReadyForReview) are out of scope (BOS-332).
var strandedReapStates = []machine.State{
	machine.ImplementingPlan,
	machine.CreatingWorktree,
	machine.StartingAgent,
	machine.PushingBranch,
	machine.OpeningDraftPR,
}

// strandedReapStateInts returns strandedReapStates as ints for the store's
// state-set queries (ListByStates / UpdateStateConditionalFrom).
func strandedReapStateInts() []int {
	out := make([]int, len(strandedReapStates))
	for i, s := range strandedReapStates {
		out[i] = int(s)
	}
	return out
}

// strandedRunIsDead reports whether a stranded unattended run is genuinely dead
// and safe to reap. It mirrors cronRunIsOver's fail-safe posture: any ambiguity
// leaves the session for the next sweep rather than risking a premature finalize
// of a live run.
//
//   - Post-agent strands (ImplementingPlan/PushingBranch/OpeningDraftPR have an
//     AgentSessionID) defer to cronRunIsOver unchanged — durable agent-log idle
//     plus liveness — so the historical ImplementingPlan path is byte-identical.
//   - Pre-agent strands (CreatingWorktree/StartingAgent have no agent log yet, so
//     AgentSessionID is nil/empty) have no durable idle evidence. They are dead
//     only when a wired liveness checker positively reports the session NOT alive;
//     if liveness is unwired or reports alive, they are conservatively left.
func (l *Lifecycle) strandedRunIsDead(sess *models.Session) bool {
	if sess.AgentSessionID != nil && *sess.AgentSessionID != "" {
		return l.cronRunIsOver(sess)
	}
	return l.liveness != nil && !l.liveness.IsSessionAlive(context.Background(), sess.ID)
}

// RecoverStrandedCronSessions finalizes unattended sessions — cron-scheduled or
// tmux_unattended (e.g. /boss-epic) — whose agent run has ended but whose Stop-hook
// finalize signal never reached the daemon — e.g. the daemon restarted as the run
// finished, so the loopback hook server's ephemeral port (baked into the
// worktree's settings.local.json at session start) was stale and the Stop-hook
// curl got connection-refused. The session is then stranded with no completion
// trigger: RecoverFinalizingSessions only rescues sessions already in Finalizing,
// and the hookless tmux-completion poll is both Claude-disabled and lost on restart.
//
// It reaps the set of interrupted states in strandedReapStates —
// ImplementingPlan plus the pre-implementation transitions
// (CreatingWorktree/StartingAgent/PushingBranch/OpeningDraftPR) a restart can
// freeze a run in. Orphaned is EXCLUDED (see strandedReapStates), and the
// post-implementation waiting states are out of scope (BOS-332).
//
// For each unattended session whose run strandedRunIsDead confirms is dead, this
// routes the session through the broadened finalize entry directly
// (finalizeSessionFrom with the reap set), NOT through the completion gate: the
// gate re-checks cronRunIsOver and its FinalizeSession entry is ImplementingPlan-
// only, so it could not advance a PushingBranch/etc. strand. The finalize is
// idempotent via its conditional state-set→Finalizing transition, so a late Stop
// hook racing the sweep is a safe no-op. The isUnattendedSession eligibility here
// mirrors the completion gate exactly.
//
// Conservative by construction: only unattended sessions strandedRunIsDead
// confirms are dead are touched; any ambiguity (missing log/id, running agent,
// unwired liveness for a pre-agent state) leaves the session for the next sweep.
//
// Call once at daemon startup (alongside RecoverFinalizingSessions, after the
// cron completion notifier is wired) and periodically thereafter. Returns the
// number of sessions routed to finalization. A finalize error is logged at Warn
// and the loop continues (the session still counts as routed); a failure to list
// sessions aborts the sweep.
func (l *Lifecycle) RecoverStrandedCronSessions(ctx context.Context) (int, error) {
	if l.cronCompletionNotifier == nil {
		// No gate wired (legacy/partial wiring): nothing to route to.
		return 0, nil
	}

	stranded, err := l.sessions.ListByStates(ctx, strandedReapStateInts())
	if err != nil {
		return 0, fmt.Errorf("list stranded unattended sessions: %w", err)
	}

	// nil reapFinalizer means use the real pipeline; tests inject a fake.
	finalize := l.reapFinalizer
	if finalize == nil {
		finalize = l.finalizeSessionFrom
	}

	routed := 0
	for _, sess := range stranded {
		// Skip archived sessions. ArchiveSession removes the worktree but leaves
		// the row in an implementing state, and ListByStates returns archived
		// rows regardless of archived status (session_store.go), so finalizing
		// one here would run `git status` against a gone worktree path — the
		// exact spurious pr_failed BOS-384 kills. There is nothing to finalize.
		if sess.ArchivedAt != nil {
			continue
		}
		// Match the completion gate's eligibility exactly: recover both
		// cron-scheduled and tmux_unattended (e.g. /boss-epic) sessions. A
		// tmux_unattended session has no CronJobID, so filtering on it here would
		// leave those sessions stranded forever.
		if !isUnattendedSession(sess) {
			continue
		}
		if !l.strandedRunIsDead(sess) {
			continue
		}

		evt := l.logger.Warn().
			Str("session", sess.ID).
			Str("state", sess.State.String()).
			Bool("tmuxUnattended", sess.TmuxUnattended)
		if sess.CronJobID != nil && *sess.CronJobID != "" {
			evt = evt.Str("cronJob", *sess.CronJobID)
		}
		evt.Msg("recovering stranded unattended session: agent idle but finalize signal lost")

		// Route through the broadened finalize entry directly (not the gate),
		// synchronously and bounded by a per-call timeout. cancel is called
		// explicitly each iteration rather than deferred inside the loop.
		fctx, cancel := context.WithTimeout(ctx, defaultCronFinalizeTimeout)
		if _, ferr := finalize(fctx, sess.ID, strandedReapStateInts()); ferr != nil {
			l.logger.Warn().Err(ferr).
				Str("session", sess.ID).
				Msg("stranded-cron reap: finalize failed")
		}
		cancel()
		routed++
	}

	return routed, nil
}

// cronRunIsOver reports whether a cron session's agent run has finished. Cron
// Claude runs interactively in tmux and stays alive idle after its turn, so a
// merely-alive tmux can't decide this; the durable signal is the agent log
// going quiet for cronAgentIdleThreshold. Fail-safe: when the evidence is
// ambiguous we return false (treat as still running) so a live run is never
// finalized.
//
// Two liveness signals refine that durable signal:
//   - If the headless runner reports the agent running, the run is not over.
//   - If a wired liveness checker confirms the session is NOT alive — neither a
//     headless subprocess nor a tmux chat survives — the run is definitively
//     over, so we reap it immediately even when the agent log is fresh (the
//     agent died moments before a restart) or absent entirely (a logless Codex
//     run, or a failed tmux pipe-pane). A live session can't be declared over
//     from liveness alone — a finished cron agent sits idle but alive in tmux —
//     so that case falls through to the durable log-idle threshold below.
func (l *Lifecycle) cronRunIsOver(sess *models.Session) bool {
	if sess.AgentSessionID == nil || *sess.AgentSessionID == "" {
		return false
	}
	if l.agentRunner != nil && l.agentRunner.IsRunningByAgent(sess.AgentName, *sess.AgentSessionID) {
		return false
	}
	if l.liveness != nil && !l.liveness.IsSessionAlive(context.Background(), sess.ID) {
		return true
	}
	idle, known := agentLogIdleFor(l.agentLogsDir, *sess.AgentSessionID, time.Now())
	if !known {
		// No durable log evidence, and liveness is either unwired or reports the
		// session alive: conservatively treat as still running.
		return false
	}
	return idle >= cronAgentIdleThreshold
}

// CronRunIsOver is the exported wrapper the CronCompletionGate is wired with so
// the Stop-hook finalize path enforces the same completion criterion as the
// stranded-cron sweep. Keeping it a thin pass-through means the gate and the
// sweep can never drift onto different definitions of "the cron run is over".
func (l *Lifecycle) CronRunIsOver(sess *models.Session) bool {
	return l.cronRunIsOver(sess)
}
