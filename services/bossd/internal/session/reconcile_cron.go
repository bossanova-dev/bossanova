package session

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/recurser/bossalib/contract/plugin/statusdetect"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossd/internal/db"
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
// A parked session (its tmux pane deliberately reaped, chat row kept) is
// neither alive nor dead — see Liveness — so every consumer below has to say
// what it does with that third answer rather than folding it into a boolean.
type sessionLiveness interface {
	SessionLiveness(context.Context, string) Liveness
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
	if sess == nil {
		return false
	}
	if isPreAgentState(sess.State) {
		if c.liveness != nil && c.liveness.SessionLiveness(context.Background(), sess.ID) == LivenessAlive {
			return true
		}
		return time.Since(sess.CreatedAt) < preAgentReapGrace
	}
	if sess.AgentSessionID == nil || *sess.AgentSessionID == "" {
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
			// A parked session's pane was reaped because it had gone idle, so
			// it is not producing output and must not suppress the next cron
			// fire. Unlike the reap/finalize consumers, treating parked like
			// dead here is safe: RunActive only gates overlap, it never
			// finalizes a session or fails a task.
			return c.liveness.SessionLiveness(context.Background(), sess.ID) == LivenessAlive
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
// state-set queries (ListByStates / UpdateStateConditionalFrom). This is the
// STARTUP (full) set — it includes the pre-agent transitions a daemon restart
// can freeze a run in.
func strandedReapStateInts() []int {
	out := make([]int, len(strandedReapStates))
	for i, s := range strandedReapStates {
		out[i] = int(s)
	}
	return out
}

// isPreAgentState reports whether s is one of the pre-agent transitions
// (CreatingWorktree/StartingAgent) that a live create path owns during normal
// operation. These have no AgentSessionID and no spawned pane yet, so liveness
// reports them not-alive even while a session is legitimately mid-creation —
// which is exactly why the periodic sweep must never reap them.
func isPreAgentState(s machine.State) bool {
	return s == machine.CreatingWorktree || s == machine.StartingAgent
}

// periodicReapStates is the post-agent subset of strandedReapStates, DERIVED
// from that single source of truth (filtering out pre-agent states) so the two
// sets can't silently drift. The periodic (running-daemon) sweep reaps only
// these: a running daemon owns CreatingWorktree/StartingAgent through the live
// create path, so the periodic sweep must leave them alone (BOS-426).
func periodicReapStates() []machine.State {
	out := make([]machine.State, 0, len(strandedReapStates))
	for _, s := range strandedReapStates {
		if isPreAgentState(s) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// zeroOutputReapStates are the states a zero-output unattended session
// (isZeroOutputSession) is reaped from beyond the shared set. Such a session runs
// in the repository checkout and never has a PR, so a PR-lifecycle state is never
// legitimate for it — yet one can land there (a stray PlanComplete), and outside
// the shared set it would sit idle forever. Blocked is deliberately absent: a
// failed hard-delete blocks the session, and reaping it would retry that failure
// every sweep while hiding it.
var zeroOutputReapStates = []machine.State{
	machine.AwaitingChecks,
	machine.FixingChecks,
	machine.GreenDraft,
	machine.ReadyForReview,
}

// isZeroOutputSession reports an unattended session that runs in the
// repository checkout with no worktree, branch or PR: finalize hard-deletes it.
func isZeroOutputSession(sess *models.Session) bool {
	return sess != nil && sess.IsQuickChat && isUnattendedSession(sess)
}

// withZeroOutputReapStates returns reapInts plus zeroOutputReapStates, the
// state set a zero-output session is finalized from.
func withZeroOutputReapStates(reapInts []int) []int {
	out := append([]int(nil), reapInts...)
	for _, s := range zeroOutputReapStates {
		out = append(out, int(s))
	}
	return out
}

// periodicReapStateInts returns periodicReapStates as ints for the store's
// state-set queries. This is the PERIODIC (post-agent) set.
func periodicReapStateInts() []int {
	states := periodicReapStates()
	out := make([]int, len(states))
	for i, s := range states {
		out[i] = int(s)
	}
	return out
}

// preAgentReapGrace is a defensive, periodic-only, should-be-unreachable second
// line of defense. periodicReapStateInts already excludes pre-agent states, so
// the periodic ListByStates query never returns a mid-creation row and this
// grace never fires in production today. It exists so that if a future edit
// re-adds a pre-agent state to the periodic set, a still-live (seconds-old)
// creation is still protected while a restart-frozen (old) strand is not.
const preAgentReapGrace = 3 * time.Minute

// preAgentReapAllowed is a pure helper for the defensive periodic-only grace.
// A non-pre-agent state is always allowed (age-independent). A pre-agent state
// is allowed only once it is older than preAgentReapGrace, so a live creation
// still within the grace window is never reaped.
func preAgentReapAllowed(state machine.State, createdAt, now time.Time) bool {
	if !isPreAgentState(state) {
		return true
	}
	return now.Sub(createdAt) >= preAgentReapGrace
}

// reapPhase distinguishes the one-shot startup recovery pass from the periodic
// running-daemon sweep. The two select different reap state-sets (BOS-426).
type reapPhase int

const (
	// reapStartup is the one-shot post-restart recovery: full reap set including
	// pre-agent states, which only a daemon restart can strand.
	reapStartup reapPhase = iota
	// reapPeriodic is the running-daemon 2-min sweep: post-agent states only,
	// plus a defensive age grace, so it can never reap a live creation.
	reapPeriodic
)

// strandedRunIsDead reports whether a stranded unattended run is genuinely dead
// and safe to reap. It mirrors cronRunIsOver's fail-safe posture: any ambiguity
// leaves the session for the next sweep rather than risking a premature finalize
// of a live run.
//
//   - Post-agent strands (ImplementingPlan/PushingBranch/OpeningDraftPR have an
//     AgentSessionID) defer to cronRunIsOver unchanged.
//   - Pre-agent strands (CreatingWorktree/StartingAgent have no agent log yet, so
//     AgentSessionID is nil/empty) have no durable idle evidence. At startup a
//     dead liveness result proves the creator did not survive the daemon restart;
//     if liveness is unwired or reports alive, they are conservatively left.
func (l *Lifecycle) strandedRunCompletionEvidence(sess *models.Session) (bool, string) {
	if sess == nil {
		return false, "session_unknown"
	}
	if sess.AgentSessionID == nil || *sess.AgentSessionID == "" {
		// A tmux chat gets an agent ID only after its pane and chat row have been
		// created. Thus in a pre-agent state, a dead liveness result at startup
		// reaps a row the restart froze before it could create a live pane. Once
		// an agent ID exists, false liveness is never enough for tmux completion.
		// Only a DEAD verdict reaps. A parked session was torn down on purpose
		// and reaping it here would finalize a session the reaper meant to
		// keep — the exact failure BOS-884 prevents.
		if isPreAgentState(sess.State) && l.liveness != nil &&
			l.liveness.SessionLiveness(context.Background(), sess.ID) == LivenessDead {
			return true, "pre_agent_liveness_dead"
		}
		return false, "pre_agent_liveness_unknown_or_alive"
	}
	return l.cronRunCompletionEvidence(sess)
}

func (l *Lifecycle) strandedRunIsDead(sess *models.Session) bool {
	over, _ := l.strandedRunCompletionEvidence(sess)
	return over
}

// RecoverStrandedCronSessionsAtStartup runs the one-shot post-restart recovery
// sweep. It reaps the FULL set (strandedReapStates) — ImplementingPlan plus the
// pre-agent transitions (CreatingWorktree/StartingAgent) and post-agent
// transitions (PushingBranch/OpeningDraftPR) — because a daemon restart is the
// only thing that can strand a session in a pre-agent state, and startup is the
// one-shot pass that follows a restart. No age grace is applied: a restart can
// legitimately leave a young-but-dead pre-agent strand that must still be
// reaped.
//
// ORDERING: the daemon runs ReapStrandedBootstrapSessionsAtStartup to completion
// BEFORE this pass, in the same goroutine (BOS-717). Both select the pre-agent
// states and both claim a row with a conditional transition, so run concurrently
// they do not corrupt anything but the winner is arbitrary — and the outcomes
// differ (Blocked + artifact cleanup vs. finalized as a completed run, which for
// a row whose worktree may never have existed can report worktree_gone/pr_failed
// and skip the cleanup). By the time this runs, rows the reaper claimed are in
// Blocked and therefore outside the reap set below. This pass still owns every
// pre-agent row the reaper declines — notably one carrying a tmux pane but no
// agent id, which the reaper defers to the sweep that understands liveness — so
// the pre-agent states must stay in this set.
//
// The wrapper keeps the func(context.Context) (int, error) signature so the
// opts.startupStrandedCronRecovery seam type is unchanged (BOS-426).
func (l *Lifecycle) RecoverStrandedCronSessionsAtStartup(ctx context.Context) (int, error) {
	return l.recoverStrandedCronSessions(ctx, reapStartup)
}

// RecoverStrandedCronSessionsPeriodic runs the running-daemon 2-min sweep. It
// reaps the POST-AGENT set only (periodicReapStates) — a running daemon owns
// CreatingWorktree/StartingAgent through the live create path, so the periodic
// sweep must never load or reap a mid-creation row (that race deleted the row
// out from under an in-flight `start session`, failing it with `update worktree
// path: sql: no rows in result set`, BOS-426). A defensive periodic-only age
// grace (preAgentReapGrace) is additionally applied as a should-be-unreachable
// second line of defense.
func (l *Lifecycle) RecoverStrandedCronSessionsPeriodic(ctx context.Context) (int, error) {
	return l.recoverStrandedCronSessions(ctx, reapPeriodic)
}

// recoverStrandedCronSessions finalizes unattended sessions — cron-scheduled or
// tmux_unattended (e.g. /boss-epic) — whose agent run has ended but whose Stop-hook
// finalize signal never reached the daemon — e.g. the daemon restarted as the run
// finished, so the loopback hook server's ephemeral port (baked into the
// worktree's settings.local.json at session start) was stale and the Stop-hook
// curl got connection-refused. The session is then stranded with no completion
// trigger: RecoverFinalizingSessions only rescues sessions already in Finalizing,
// and the hookless tmux-completion poll is both Claude-disabled and lost on restart.
//
// The reap state-set is phase-scoped (BOS-426): the startup phase reaps the full
// strandedReapStates (incl. pre-agent CreatingWorktree/StartingAgent that a
// restart can freeze), while the periodic phase reaps only the post-agent subset
// (periodicReapStates) so a running daemon's live create path is never raced. The
// same set is used for both the ListByStates query AND the finalize expected-
// states argument. Orphaned is EXCLUDED (see strandedReapStates), and the
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
// An auth-failed chat (SetChatAuthFailedProbe) is the one dead-looking run that
// is not finalized: it is blocked with the agent-auth-failed reason instead and
// is not counted as routed (BOS-1530).
//
// Modal prompts and live background work defer recovery; distinct prompts are
// surfaced once in Warn logs while the session remains in its reap state.
//
// Returns the number of sessions routed to finalization. A finalize error is
// logged at Warn and the loop continues (the session still counts as routed); a
// failure to list sessions aborts the sweep.
func (l *Lifecycle) recoverStrandedCronSessions(ctx context.Context, phase reapPhase) (int, error) {
	if l.cronCompletionNotifier == nil {
		// No gate wired (legacy/partial wiring): nothing to route to.
		return 0, nil
	}

	// Select the reap state-set by phase; use the same set for the ListByStates
	// query and the finalize expected-states argument so they can't drift.
	reapInts := strandedReapStateInts()
	if phase == reapPeriodic {
		reapInts = periodicReapStateInts()
	}

	zeroOutputInts := withZeroOutputReapStates(reapInts)
	sharedStates := make(map[machine.State]bool, len(reapInts))
	for _, s := range reapInts {
		sharedStates[machine.State(s)] = true
	}

	stranded, err := l.sessions.ListByStates(ctx, zeroOutputInts)
	if err != nil {
		return 0, fmt.Errorf("list stranded unattended sessions: %w", err)
	}

	// nil reapFinalizer means use the real pipeline; tests inject a fake.
	finalize := l.reapFinalizer
	if finalize == nil {
		finalize = l.finalizeSessionFrom
	}

	now := time.Now()
	routed := 0
	authBlocked := 0
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
		// The widened states are a zero-output session's only: any other
		// session in AwaitingChecks and friends is on its PR's lifecycle.
		expected := reapInts
		if isZeroOutputSession(sess) {
			expected = zeroOutputInts
		} else if !sharedStates[sess.State] {
			continue
		}
		// Periodic-only defensive age grace (belt-and-suspenders): periodicReapStateInts
		// already excludes pre-agent states so ListByStates won't return them, but this
		// guard protects a future re-add of a pre-agent state to the periodic set — a
		// live (seconds-old) creation stays protected while a restart-frozen (old) strand
		// is not. Never applied to startup, which must reap young-but-dead pre-agent
		// strands (BOS-426).
		if phase == reapPeriodic && !preAgentReapAllowed(sess.State, sess.CreatedAt, now) {
			continue
		}
		if l.DeferDroppedRunFinalization(sess) {
			continue
		}
		over, evidence := l.strandedRunCompletionEvidence(sess)
		l.logStrandedPrompt(ctx, sess, evidence)
		if !over {
			l.logger.Debug().
				Str("session", sess.ID).
				Str("completion_evidence", evidence).
				Msg("stranded unattended session recovery deferred")
			continue
		}

		// An auth-failed chat is idle with no pending turn, so the evidence
		// above reads it as over — but the run died on a login-required error,
		// it did not finish. Block it instead of finalizing (BOS-1530); it is
		// counted apart from routed and its PR is left exactly as it is.
		//
		// The startup pass runs before the status poller has observed any pane,
		// so the in-memory probe is still empty there; it re-derives the signal
		// from the session's live panes instead (startupPaneShowsLoginRequired).
		if l.SessionChatAuthFailed(sess) || (phase == reapStartup && l.startupPaneShowsLoginRequired(ctx, sess)) {
			if l.blockAuthFailedStrandedSession(ctx, sess, expected, evidence) {
				authBlocked++
			}
			continue
		}

		evt := l.logger.Warn().
			Str("session", sess.ID).
			Str("state", sess.State.String()).
			Bool("tmuxUnattended", sess.IsTmuxUnattended).
			Str("completion_evidence", evidence)
		if sess.CronJobID != nil && *sess.CronJobID != "" {
			evt = evt.Str("cronJob", *sess.CronJobID)
		}
		evt.Msg("recovering stranded unattended session: agent idle but finalize signal lost")

		// Route through the broadened finalize entry directly (not the gate),
		// synchronously and bounded by a per-call timeout. cancel is called
		// explicitly each iteration rather than deferred inside the loop.
		fctx, cancel := context.WithTimeout(ctx, defaultCronFinalizeTimeout)
		if _, ferr := finalize(fctx, sess.ID, expected, finalizeTriggerStrandedRecovery); ferr != nil {
			l.logger.Warn().Err(ferr).
				Str("session", sess.ID).
				Msg("stranded-cron reap: finalize failed")
		}
		cancel()
		routed++
	}

	if authBlocked > 0 {
		l.logger.Warn().
			Int("auth_blocked", authBlocked).
			Int("routed", routed).
			Msg("stranded unattended session sweep blocked auth-failed runs instead of finalizing them")
	}
	return routed, nil
}

// startupPaneShowsLoginRequired re-derives the auth-failed signal straight from
// the session's persisted tmux panes for the one-shot startup sweep. The
// in-memory probe (SessionChatAuthFailed) is empty after a restart — the status
// poller has not bootstrapped yet, and its marker needs consecutive fresh
// observations — so without this a login-required run a previous daemon left
// behind reads as idle-and-over and is finalized (BOS-1530). Each persisted chat
// pane is captured, because an account switch can leave the parent session's
// agent id pointing at a cleared chat. A pane that cannot be captured (gone,
// probe error) or missing wiring fails toward false, matching the probe's own
// posture: a false auth-failed is worse than a missed one.
func (l *Lifecycle) startupPaneShowsLoginRequired(ctx context.Context, sess *models.Session) bool {
	return l.anyChatPane(ctx, sess, statusdetect.IsLoginRequired)
}

// anyChatPane shares the persisted-pane walk across startup auth and idle-run probes.
// Missing wiring, failed reads and vanished panes provide no positive evidence.
func (l *Lifecycle) anyChatPane(ctx context.Context, sess *models.Session, pred func([]byte) bool) bool {
	if sess == nil || l.agentChats == nil || l.tmux == nil {
		return false
	}
	chats, err := l.agentChats.ListBySession(ctx, sess.ID)
	if err != nil {
		return false
	}
	for _, chat := range chats {
		if chat == nil || chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			continue
		}
		content, err := l.tmux.CapturePane(ctx, *chat.TmuxSessionName)
		if err != nil {
			continue
		}
		if pred([]byte(content)) {
			return true
		}
	}
	return false
}

// logStrandedPrompt deduplicates warnings while a session remains parked on the
// same prompt. Re-capture only deferred prompts; disappearance stays a deferral
// for this sweep and clears the warning on the next non-prompt observation.
func (l *Lifecycle) logStrandedPrompt(ctx context.Context, sess *models.Session, evidence string) {
	if evidence != "tmux_pane_awaiting_input" {
		l.strandedPromptMu.Lock()
		delete(l.strandedPrompts, sess.ID)
		l.strandedPromptMu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var prompt string
	l.anyChatPane(ctx, sess, func(pane []byte) bool {
		if !statusdetect.HasModalPrompt(pane) {
			return false
		}
		prompt = strandedPromptExcerpt(pane)
		return true
	})
	l.strandedPromptMu.Lock()
	defer l.strandedPromptMu.Unlock()
	if prompt == "" || l.strandedPrompts[sess.ID] == prompt {
		return
	}
	if l.strandedPrompts == nil {
		l.strandedPrompts = make(map[string]string)
	}
	l.strandedPrompts[sess.ID] = prompt
	l.logger.Warn().Str("session", sess.ID).Str("state", sess.State.String()).Str("prompt", prompt).
		Msg("stranded unattended session is waiting on an agent prompt; not finalizing")
}

// strandedPromptExcerpt keeps the current card readable without ANSI escapes,
// box-drawing chrome, multiline output or unbounded log fields.
func strandedPromptExcerpt(pane []byte) string {
	lines := strings.Split(string(statusdetect.StripANSI(pane)), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	text := strings.Map(func(r rune) rune {
		if r >= '─' && r <= '╿' {
			return ' '
		}
		return r
	}, strings.Join(lines, " "))
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > 300 {
		return string(runes[:299]) + "…"
	}
	return text
}

// blockAuthFailedStrandedSession moves a stranded unattended session whose chat
// is auth-failed to Blocked with the shared agent-auth-failed reason, and never
// finalizes it (BOS-1530). Finalizing would push, open and ready a PR for a
// build that died before its own finish step.
//
// The move is a persisted conditional transition from the session's reap set
// straight to Blocked — the shape the finalize-failure path and the
// stranded-bootstrap reaper use — never via Finalizing. The state machine's
// Block event is not permitted from every reap state (pre-agent and
// PushingBranch/OpeningDraftPR lack it), so it is deliberately not fired; the
// conditional write is the idempotency gate, and a session that moved since the
// listing loses it and is left alone. Returns true only when this call blocked
// the session.
func (l *Lifecycle) blockAuthFailedStrandedSession(ctx context.Context, sess *models.Session, expected []int, evidence string) bool {
	moved, err := l.sessions.UpdateStateConditionalFrom(ctx, sess.ID, int(machine.Blocked), expected)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sess.ID).
			Msg("stranded-cron reap: mark auth-failed session blocked failed")
		return false
	}
	if !moved {
		return false
	}
	reason := sessionreason.AgentAuthFailed()
	reasonPtr := &reason
	if _, err := l.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{BlockedReason: &reasonPtr}); err != nil {
		l.logger.Warn().Err(err).Str("session", sess.ID).
			Msg("stranded-cron reap: persist agent-auth-failed blocked reason failed")
	}

	evt := l.logger.Warn().
		Str("session", sess.ID).
		Str("state", sess.State.String()).
		Bool("tmuxUnattended", sess.IsTmuxUnattended).
		Str("completion_evidence", evidence).
		Bool("auth_failed", true)
	if sess.CronJobID != nil && *sess.CronJobID != "" {
		evt = evt.Str("cronJob", *sess.CronJobID)
	}
	evt.Msg("blocking stranded unattended session: agent chat is auth-failed; not finalizing")
	return true
}

// cronRunIsOver reports whether a cron session's agent run has finished. Cron
// Claude runs interactively in tmux and stays alive idle after its turn, so a
// merely-alive tmux can't decide this; the durable signal is the agent log
// going quiet for cronAgentIdleThreshold. Fail-safe: when the evidence is
// ambiguous we return false (treat as still running) so a live run is never
// finalized.
//
// A running agent runner is always active. Tmux-hosted unattended runs require
// a known idle log with no modal prompt or working indicator in any captured
// chat pane, except that a missing log may be finalized only with direct
// proof that its persisted tmux pane exited. Liveness alone can be false while
// their chat is still working. Non-tmux/headless runs retain the fast path where
// a wired liveness checker confirms the session is dead, even when their log is
// fresh or absent. Remaining non-tmux runs use the durable log-idle threshold.
func (l *Lifecycle) cronRunCompletionEvidence(sess *models.Session) (bool, string) {
	if sess == nil || sess.AgentSessionID == nil || *sess.AgentSessionID == "" {
		return false, "agent_session_unknown"
	}
	if l.agentRunner != nil && l.agentRunner.IsRunningByAgent(sess.AgentName, *sess.AgentSessionID) {
		return false, "agent_runner_running"
	}
	idle, logKnown := agentLogIdleFor(l.agentLogsDir, *sess.AgentSessionID, time.Now())
	if isUnattendedSession(sess) {
		if !logKnown {
			return l.loglessTmuxCompletionEvidence(sess)
		}
		if idle < cronAgentIdleThreshold {
			return false, "tmux_log_active"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		working := false
		modal := l.anyChatPane(ctx, sess, func(pane []byte) bool {
			working = working || statusdetect.HasWorkingIndicator(pane)
			return statusdetect.HasModalPrompt(pane)
		})
		if modal {
			return false, "tmux_pane_awaiting_input"
		}
		if working {
			return false, "tmux_pane_background_work"
		}
		return true, "tmux_log_idle"
	}
	// Only a DEAD verdict is completion evidence; a parked session's pane was
	// reaped on purpose and finalizing it would end a run nobody stopped.
	if l.liveness != nil && l.liveness.SessionLiveness(context.Background(), sess.ID) == LivenessDead {
		return true, "liveness_dead"
	}
	if !logKnown {
		// No durable log evidence, and liveness is either unwired or reports the
		// session alive: conservatively treat as still running.
		return false, "log_unknown"
	}
	if idle < cronAgentIdleThreshold {
		return false, "log_active"
	}
	return true, "log_idle"
}

// loglessTmuxCompletionEvidence recovers the degraded pipe-pane path without
// trusting the aggregate session-liveness result. A missing log alone is
// ambiguous: a live tmux agent may be working after pipe-pane failed. It is
// completion evidence only when every persisted tmux pane for the session has
// disappeared or exited. An account switch can clear the old chat's tmux name
// and create a replacement chat with a fresh agent-session ID while the parent
// session retains the old ID, so the persisted chats—not that stale ID—define
// the candidate panes. Any missing dependency, database error, no candidate
// pane, live pane, or tmux probe failure defers finalization.
func (l *Lifecycle) loglessTmuxCompletionEvidence(sess *models.Session) (bool, string) {
	if sess == nil || sess.AgentSessionID == nil || *sess.AgentSessionID == "" {
		return false, "tmux_log_unknown"
	}
	if l.agentChats == nil || l.tmux == nil {
		return false, "tmux_pane_probe_unavailable"
	}

	chats, err := l.agentChats.ListBySession(context.Background(), sess.ID)
	if err != nil {
		return false, "tmux_pane_probe_error"
	}
	foundPane := false
	missingPane := false
	deadPane := false
	for _, chat := range chats {
		if chat == nil || chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			continue
		}
		foundPane = true

		alive, err := l.tmux.HasSessionStatus(context.Background(), *chat.TmuxSessionName)
		if err != nil {
			return false, "tmux_pane_probe_error"
		}
		if !alive {
			missingPane = true
			continue
		}

		dead, err := l.tmux.PaneDead(context.Background(), *chat.TmuxSessionName)
		if err != nil {
			return false, "tmux_pane_probe_error"
		}
		if dead {
			deadPane = true
			continue
		}
		return false, "tmux_pane_running"
	}

	if foundPane {
		if missingPane {
			return true, "tmux_pane_missing"
		}
		if deadPane {
			return true, "tmux_pane_dead"
		}
	}
	return false, "tmux_pane_unknown"
}

func (l *Lifecycle) cronRunIsOver(sess *models.Session) bool {
	over, _ := l.cronRunCompletionEvidence(sess)
	return over
}

// CronRunIsOver is the exported wrapper the CronCompletionGate is wired with so
// the Stop-hook finalize path enforces the same completion criterion as the
// stranded-cron sweep. Keeping it a thin pass-through means the gate and the
// sweep can never drift onto different definitions of "the cron run is over".
func (l *Lifecycle) CronRunIsOver(sess *models.Session) bool {
	return l.cronRunIsOver(sess)
}

// CronRunCompletionEvidence is the production completion predicate used by both
// the Stop-hook gate and recovery sweep. The evidence value is suitable for
// structured logs and deliberately remains a stable, private-detail string.
func (l *Lifecycle) CronRunCompletionEvidence(sess *models.Session) (bool, string) {
	return l.cronRunCompletionEvidence(sess)
}
