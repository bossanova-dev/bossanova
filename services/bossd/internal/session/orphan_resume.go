package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dotenv"
)

// OrphanedHeadlessRunReason is the blocked_reason stamped on a headless run that
// a daemon restart killed mid-flight (set by sweepOrphanedHeadlessRuns). It is
// the single source of truth shared by detection and the resume sweep below: the
// resume sweep treats an Orphaned row carrying this exact reason as its
// claim/idempotency candidate, so the bytes must stay identical on both sides.
const OrphanedHeadlessRunReason = "headless run orphaned: killed by daemon restart (no recorded exit, no live agent process)"

// orphanResumeSteeringNotice is the verbatim single-line prefix prepended to the
// resumed plan when a headless run orphaned by a daemon restart is auto-resumed.
// It is a NAMED acceptance criterion (BOS-407): the exact wording tells the
// resumed agent that its previous turn was interrupted by a restart, so it must
// re-check the workspace before repeating side effects (commits, pushes, and
// comments may already exist). Mirrors steeringNotice (the rotation sibling).
// Do NOT reword.
const orphanResumeSteeringNotice = "Your previous turn was interrupted by a daemon restart. Verify current workspace/git/PR state before repeating any action (commits, pushes, comments may already exist)."

// ResumeOrphanedHeadlessRuns is the level-triggered sweep that auto-resumes
// headless runs a daemon restart orphaned. It mirrors SweepParkedRotations: it
// is driven entirely off the persisted Orphaned state (never an in-memory
// timer), so a daemon restart mid-resume re-arms every still-orphaned candidate
// on the next tick — a fresh Lifecycle over the same store resumes them.
//
// Gated OFF by default (AutoResumeOrphansEnabled, opt-in) because auto-resume
// reverses the deliberate "a one-shot's prompt may have side effects — the human
// decides" default; with the knob disabled the row stays Orphaned exactly as
// before. Independent of the rotation kill switch: the resume restarts on the
// SAME account, never rotating. Returns the number of runs resumed this pass.
func (l *Lifecycle) ResumeOrphanedHeadlessRuns(ctx context.Context) (resumed int) {
	cfg, ok := l.currentRotationConfig()
	if !ok || !cfg.AutoResumeOrphansEnabled() {
		return 0
	}
	if l.sessions == nil {
		return 0
	}
	claimStore, ok := l.sessions.(db.OrphanResumeStore)
	if !ok {
		l.logger.Error().Msg("orphan-resume sweep: session store does not support atomic orphan resume handoff")
		return 0
	}

	orphaned, err := l.sessions.ListByState(ctx, int(machine.Orphaned))
	if err != nil {
		l.logger.Warn().Err(err).Msg("orphan-resume sweep: list orphaned sessions failed")
		return 0
	}

	for _, session := range orphaned {
		if session == nil {
			continue
		}
		// ListByState intentionally includes archived rows for startup recovery,
		// but auto-resume must not resurrect a session the user discarded.
		if session.ArchivedAt != nil {
			continue
		}
		// Only rows carrying the daemon-restart orphan marker are candidates: an
		// ordinary/manual Orphaned row (a different mechanism, or a genuinely
		// finished run) is never resumed.
		if session.BlockedReason == nil || *session.BlockedReason != OrphanedHeadlessRunReason {
			continue
		}
		// No agent session id means there is no prior run to resume — leave it.
		if session.AgentSessionID == nil || *session.AgentSessionID == "" {
			continue
		}
		if l.resumeOrphanedRun(ctx, claimStore, session) {
			resumed++
		}
	}
	return resumed
}

// resumeOrphanedRun restarts one orphaned headless run on the SAME account (no
// rotation, no RotationAttemptCount bump), resuming the prior agent session with
// a verify-first steering notice prefixed to the plan. It is the orphan sibling
// of rotateAndRestart. The CAS Orphaned→ImplementingPlan is the idempotency/
// claim gate: a concurrent completion signal or a duplicate sweep pass that
// already advanced this row loses the CAS and this pass no-ops. Any failure
// leaves (or restores) the row Orphaned so the next sweep retries — the loud
// fallback is preserved on every unhappy path. Returns true only when the run
// was actually resumed.
func (l *Lifecycle) resumeOrphanedRun(ctx context.Context, claimStore db.OrphanResumeStore, session *models.Session) bool {
	// A missing agent runner (nil test/legacy wiring) leaves the run Orphaned
	// (fail-safe — a human nudges).
	if l.agentRunner == nil {
		return false
	}
	orphanReason := *session.BlockedReason

	// Claim the row atomically before doing any restart work. Losing the CAS
	// means a concurrent signal changed it, archived it, or cleared its orphan
	// marker after this sweep listed the candidate.
	advanced, err := claimStore.ClaimUnarchivedOrphan(ctx, session.ID, orphanReason)
	if err != nil {
		l.logger.Error().Err(err).Str("session", session.ID).
			Msg("orphan-resume sweep: transition to implementing failed")
		return false
	}
	if !advanced {
		return false
	}

	// Resolve the spawn view before materializing its account env: the primary
	// chat is the runtime authority for provider, model, and account binding.
	spawnSess := l.effectiveSpawnSession(ctx, session)

	// Materialize the CURRENT account's env exactly as the initial headless start
	// does (account > proof, then the worktree .env with the repo's stored
	// LINEAR_API_KEY / SENTRY_* beneath it). Same account — no rotation. Never
	// logged. A repo lookup failure is non-fatal (OverlayWithRepo still
	// guarantees LINEAR_API_KEY).
	repo := RepoForSessionEnv(ctx, l.repos, session.RepoID, session.ID, "orphan resume", l.logger)
	mergedEnv := dotenv.OverlayWithRepo(mergeEnv(l.resolveAccountEnv(ctx, spawnSess), l.resolveProofEnv()), session.WorktreePath, repo)

	resumedPrompt := orphanResumeSteeringNotice + "\n\n" + session.Plan

	var resume *string
	var priorAgentSession *string
	var priorAgentSessionID string
	if session.AgentSessionID != nil {
		resume = session.AgentSessionID
		priorAgentSession = session.AgentSessionID
		priorAgentSessionID = *session.AgentSessionID
	}
	// BOS-381: dispatch the restart under the primary chat's provider/model (the
	// runtime authority), not the session's stale seed.
	newID, err := l.startHeadlessReplacementRun(ctx, spawnSess, session.WorktreePath, resumedPrompt, resume, mergedEnv)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", session.ID).
			Msg("orphan-resume sweep: restart failed; re-parking Orphaned")
		// Restart failed after claiming the row: revert to Orphaned so the next
		// sweep retries. The orphan marker/reason is untouched, so the row stays
		// a candidate.
		l.releaseOrphanResumeClaim(ctx, claimStore, session.ID, orphanReason, priorAgentSession)
		return false
	}

	// Commit the replacement only while the orphan marker, original agent ID,
	// state, and unarchived status still match our claim. This protects the
	// entire spawn-to-persist handoff from a delayed completion or archive.
	committed, err := claimStore.CommitOrphanResume(ctx, session.ID, orphanReason, priorAgentSession, newID)
	if err != nil {
		l.logger.Error().Err(err).Str("session", session.ID).
			Msg("orphan-resume sweep: commit restart failed; releasing claim")
		l.stopOrphanedRestart(ctx, spawnSess.AgentName, newID, session.ID)
		l.releaseOrphanResumeClaim(ctx, claimStore, session.ID, orphanReason, priorAgentSession)
		return false
	}
	if !committed {
		l.logger.Warn().Str("session", session.ID).
			Msg("orphan-resume sweep: restart handoff lost ownership; stopping replacement")
		l.stopOrphanedRestart(ctx, spawnSess.AgentName, newID, session.ID)
		// RetrySession can clear the marker after this sweep has claimed the row
		// without starting a replacement itself. Release that markerless claim
		// (while retaining Retry's cleared marker) so the dead prior agent is
		// never left as an active ImplementingPlan run. The conditional release
		// still leaves a completion, archive, or agent-ID replacement untouched.
		l.releaseOrphanResumeClaim(ctx, claimStore, session.ID, orphanReason, priorAgentSession)
		return false
	}
	if err := l.syncResumedPrimaryChat(ctx, session, priorAgentSessionID, newID); err != nil {
		l.logger.Error().Err(err).Str("session", session.ID).
			Msg("orphan-resume sweep: persist resumed primary chat failed; re-parking Orphaned")
		l.stopOrphanedRestart(ctx, spawnSess.AgentName, newID, session.ID)
		// Restore the complete orphan shape only while this handoff still owns
		// the new agent ID. A later completion, archive, or manual intervention
		// wins the conditional rollback and remains untouched.
		if restoreErr := l.reparkOrphanResume(ctx, claimStore, session.ID, orphanReason, priorAgentSession, newID); restoreErr != nil {
			l.logger.Error().Err(restoreErr).Str("session", session.ID).
				Msg("orphan-resume sweep: rollback after primary chat persistence failed")
		}
		return false
	}

	// Re-arm completion for the NEW run only. The original orphaned run's exit
	// map was lost to the restart; the new StartByAgent process is tracked by the
	// plugin, so arming poll fallback drives its completion back through the
	// finalize/block fan-out. Without it the resumed run strands in
	// ImplementingPlan — this step is load-bearing.
	l.rearmCompletionForRotatedRun(session.ID, newID, spawnSess)
	if l.chatStatus != nil {
		agentName := spawnSess.AgentName
		runID := newID
		safego.Go(l.logger, func() {
			l.watchHeadlessRunStatus(agentName, runID)
		})
	}

	l.logger.Info().
		Str("session", session.ID).
		Str("resume_id", newID).
		Str("prior_agent_session", priorAgentSessionID).
		Msg("orphan-resume sweep: Orphaned→ImplementingPlan; resumed prior agent session on same account")
	return true
}

// syncResumedPrimaryChat preserves the primary chat row while moving it from
// the prior headless run id to the resumed run id. Old sessions may predate
// headless chat-row creation, so create the missing row rather than leaving a
// resumed run invisible to chat/status/routing APIs.
func (l *Lifecycle) syncResumedPrimaryChat(ctx context.Context, session *models.Session, priorAgentSession, newID string) error {
	if l.agentChats == nil || priorAgentSession == "" {
		return nil
	}
	chat, err := l.agentChats.GetByAgentSessionID(ctx, priorAgentSession)
	if err == nil && chat != nil {
		return l.agentChats.UpdateAgentSessionID(ctx, chat.ID, priorAgentSession, newID)
	}
	if err != nil && !errors.Is(err, db.ErrAgentChatNotFound) {
		return fmt.Errorf("get primary chat: %w", err)
	}
	spawnSess := l.effectiveSpawnSession(ctx, session)
	if _, err := l.agentChats.Create(ctx, db.CreateAgentChatParams{
		SessionID:      session.ID,
		AgentSessionID: newID,
		AgentName:      spawnSess.AgentName,
		Title:          session.Title,
		AccountID:      spawnSess.AccountID,
		Model:          spawnSess.Model,
	}); err != nil {
		return fmt.Errorf("create primary chat: %w", err)
	}
	return nil
}

func (l *Lifecycle) stopOrphanedRestart(ctx context.Context, agentName, newID, sessionID string) {
	if l.agentRunner == nil || !l.agentRunner.IsRunningByAgent(agentName, newID) {
		return
	}
	if err := l.agentRunner.StopByAgent(ctx, agentName, newID); err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).Str("resume_id", newID).
			Msg("orphan-resume sweep: stop after persistence failure failed")
	}
}

func (l *Lifecycle) releaseOrphanResumeClaim(ctx context.Context, store db.OrphanResumeStore, sessionID, reason string, priorAgentSession *string) {
	if _, err := store.ReleaseOrphanResumeClaim(ctx, sessionID, reason, priorAgentSession); err != nil {
		l.logger.Error().Err(err).Str("session", sessionID).
			Msg("orphan-resume sweep: release claim failed")
	}
}

func (l *Lifecycle) reparkOrphanResume(ctx context.Context, store db.OrphanResumeStore, sessionID, reason string, priorAgentSession *string, newAgentSessionID string) error {
	_, err := store.ReparkOrphanResume(ctx, sessionID, reason, priorAgentSession, newAgentSessionID)
	return err
}
