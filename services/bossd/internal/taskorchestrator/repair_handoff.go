package taskorchestrator

import (
	"context"

	"github.com/recurser/bossalib/kit/safego"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
)

// FactoryRepoChecker identifies repositories whose verify job owns merging.
type FactoryRepoChecker interface {
	IsFactoryRepo(context.Context, string) bool
}

// RepairActivity reports whether the session currently holds a repair lease.
type RepairActivity interface{ Active(string) bool }

// SetFactoryRepoChecker supplies the shared verify-job checker; nil preserves direct merging.
func (o *Orchestrator) SetFactoryRepoChecker(c FactoryRepoChecker) { o.factoryRepoChecker = c }

// SetRepairActivity supplies the daemon repair lease manager; nil means no repair in flight.
func (o *Orchestrator) SetRepairActivity(r RepairActivity) { o.repairActivity = r }

// HasFactoryRepoChecker reports whether startup wired the verify-job checker.
func (o *Orchestrator) HasFactoryRepoChecker() bool { return o.factoryRepoChecker != nil }

// UsesFactoryRepoChecker reports whether startup shared the same merge-owner cache.
func (o *Orchestrator) UsesFactoryRepoChecker(c FactoryRepoChecker) bool {
	return o.factoryRepoChecker == c
}

// HasRepairActivity reports whether startup wired the repair lease manager.
func (o *Orchestrator) HasRepairActivity() bool { return o.repairActivity != nil }

func (o *Orchestrator) releaseRepairSlot(ctx context.Context, repoID, mappingID string) {
	o.mu.Lock()
	if o.activeMapping[repoID] != mappingID {
		o.mu.Unlock()
		return
	}
	delete(o.activeMapping, repoID)
	o.logger.Info().Str("repo_id", repoID).Str("mapping_id", mappingID).Msg("green repair handed off")
	o.dequeueNextLocked(ctx, repoID)
}

// HandleSessionCompleted is called when a session finishes (merged,
// closed, or blocked). It looks up the task mapping by session ID,
// updates the plugin via UpdateTaskStatus, and dequeues the next task.
//
// This method is idempotent: duplicate calls for the same session are
// no-ops. An in-memory set (guarded by o.mu) prevents the TOCTOU race
// where two concurrent callers both read InProgress from the DB before
// either updates it. A secondary DB-level check handles the post-restart
// case where the in-memory set is empty but the mapping is already terminal.
func (o *Orchestrator) HandleSessionCompleted(ctx context.Context, sessionID string, outcome domain.TaskMappingStatus) {
	mapping, err := o.taskMappings.GetBySessionID(ctx, sessionID)
	if err != nil {
		// Not all sessions are task-orchestrated; this is expected.
		return
	}

	// Fast in-memory guard: serialise concurrent callers for the same session.
	// LoadOrStore is atomic — the first caller stores and proceeds, any
	// concurrent caller observes the existing entry and bails. This replaces
	// the previous map+mutex and keeps o.mu free for queue operations.
	if _, loaded := o.completedSessions.LoadOrStore(sessionID, struct{}{}); loaded {
		o.logger.Debug().
			Str("session", sessionID).
			Msg("session completion already processed (in-memory guard)")
		return
	}

	// Belt-and-suspenders DB guard for post-restart: the in-memory set is
	// empty after a daemon restart, but the mapping status is already terminal.
	switch mapping.Status {
	case domain.TaskMappingStatusCompleted, domain.TaskMappingStatusFailed, domain.TaskMappingStatusSkipped:
		o.logger.Debug().
			Str("session", sessionID).
			Str("external_id", mapping.ExternalID).
			Int("existing_status", int(mapping.Status)).
			Int("new_outcome", int(outcome)).
			Msg("session already completed, skipping duplicate notification")
		return
	default:
		// Pending or InProgress — proceed with completion.
	}

	o.logger.Info().
		Str("session", sessionID).
		Str("external_id", mapping.ExternalID).
		Int("outcome", int(outcome)).
		Msg("session completed, updating task status")

	// Update the mapping status.
	o.updateMappingStatus(ctx, mapping.ID, outcome)

	// Auto-archive dependabot *repair* sessions once their PR merges, so they
	// stop cluttering the session list (BOS-101). Gated to outcome Completed
	// (PR merged; closed-unmerged is Failed and excluded) and the dependabot
	// plugin. Runs inside this method's idempotency guard, so it fires at most
	// once per session. Async so a slow archive never blocks the task queue.
	if outcome == domain.TaskMappingStatusCompleted &&
		mapping.PluginName == "dependabot" &&
		mapping.SessionID != nil &&
		o.archiver != nil {
		archiveSessionID := *mapping.SessionID
		done := safego.Go(o.logger, func() {
			if err := o.archiver.ArchiveSession(context.Background(), archiveSessionID); err != nil {
				o.logger.Warn().Err(err).Str("session", archiveSessionID).
					Msg("auto-archive dependabot repair session failed")
				return
			}
			o.logger.Info().Str("session", archiveSessionID).
				Msg("auto-archived dependabot repair session after merge")
		})
		// Join the archive to daemon shutdown instead of dropping the handle
		// (BOS-923): the detached context above outlives the caller's ctx, so
		// without this the daemon could exit mid-archive.
		if o.archiveTracker != nil {
			o.archiveTracker(archiveSessionID, done)
		}
	}

	// Notify the plugin about the task outcome.
	pluginStatus := taskMappingStatusToProto(outcome)
	sources := o.sources.GetTaskSources()
	for _, src := range sources {
		if err := src.UpdateTaskStatus(ctx, mapping.ExternalID, pluginStatus, ""); err != nil {
			o.logger.Warn().Err(err).
				Str("external_id", mapping.ExternalID).
				Msg("failed to update plugin task status, storing as pending")

			// Store as pending for retry.
			pendingStatus := outcome
			pendingStatusPtr := &pendingStatus
			if _, err := o.taskMappings.Update(ctx, mapping.ID, db.UpdateTaskMappingParams{
				PendingUpdateStatus: &pendingStatusPtr,
			}); err != nil {
				o.logger.Error().Err(err).Str("mapping", mapping.ID).Msg("store pending update failed")
			}
			continue
		}
		break
	}

	// Clear active mapping before dequeuing so the recovery sweep
	// doesn't re-process this mapping. Guard against deleting a
	// newer mapping that replaced ours while we were completing.
	o.mu.Lock()
	ownsSlot := o.activeMapping[mapping.RepoID] == mapping.ID
	if ownsSlot {
		delete(o.activeMapping, mapping.RepoID)
	}
	if ownsSlot || !o.active[mapping.RepoID] {
		o.dequeueNextLocked(ctx, mapping.RepoID)
	} else {
		o.mu.Unlock()
	}
}
