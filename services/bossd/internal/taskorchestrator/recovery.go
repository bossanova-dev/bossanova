package taskorchestrator

import (
	"context"

	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
)

// recoverStaleTasks checks for CREATE_SESSION tasks that are stuck
// (dead Claude process, orphaned mapping) and forces queue advancement.
// This is called at the start of each poll cycle as a safety net.
func (o *Orchestrator) recoverStaleTasks(ctx context.Context) {
	o.mu.Lock()
	// Snapshot the active mappings so we don't hold the lock during DB calls.
	activeMappings := make(map[string]string, len(o.activeMapping))
	for repoID, mappingID := range o.activeMapping {
		activeMappings[repoID] = mappingID
	}
	o.mu.Unlock()

	for repoID, mappingID := range activeMappings {
		// Re-check under lock that the mapping hasn't changed since we
		// took the snapshot. A concurrent HandleSessionCompleted may have
		// already completed this mapping and started a new task, which
		// would set a different mapping ID for the same repo.
		o.mu.Lock()
		currentMappingID, stillActive := o.activeMapping[repoID]
		o.mu.Unlock()
		if !stillActive || currentMappingID != mappingID {
			// A concurrent completion already handled this mapping.
			continue
		}

		mapping, err := o.taskMappings.Get(ctx, mappingID)
		if err != nil {
			// Mapping not found — it was deleted or the DB is inconsistent.
			// Clear the active state and advance the queue.
			o.logger.Warn().
				Str("repo", repoID).
				Str("mapping", mappingID).
				Msg("active mapping not found, clearing stale state")
			o.mu.Lock()
			// Double-check: only clear if the mapping hasn't been replaced.
			if o.activeMapping[repoID] == mappingID {
				delete(o.activeMapping, repoID)
			}
			o.mu.Unlock()
			o.dequeueNext(ctx, repoID)
			continue
		}

		switch mapping.Status {
		case domain.TaskMappingStatusCompleted, domain.TaskMappingStatusFailed, domain.TaskMappingStatusSkipped, domain.TaskMappingStatusOrphaned:
			// Terminal-from-this-loop's-perspective: clear active state and
			// advance. Orphaned shouldn't normally be in activeMapping (the
			// status is set by MarkOrphaned at boot, before this map
			// is populated), but if it ever is, treat the slot as freed —
			// the next poll will re-pick the task via processTask's
			// orphan-aware path.
			o.logger.Info().
				Str("repo", repoID).
				Str("mapping", mappingID).
				Int("status", int(mapping.Status)).
				Msg("active mapping already terminal, advancing queue")
			o.mu.Lock()
			if o.activeMapping[repoID] == mappingID {
				delete(o.activeMapping, repoID)
			}
			o.mu.Unlock()
			o.dequeueNext(ctx, repoID)

		case domain.TaskMappingStatusInProgress:
			if mapping.SessionID != nil {
				// Has a session — check if it's still alive.
				// Only a DEAD session recovers the task. A parked session
				// (its pane deliberately reaped, chat row kept) is not a
				// failure — failing it here would be exactly the wrong
				// inference BOS-884 exists to prevent — so the mapping is
				// left in place for a later sweep to re-evaluate.
				switch o.sessionLiveness(ctx, *mapping.SessionID) {
				case session.LivenessDead:
					o.logger.Warn().
						Str("repo", repoID).
						Str("mapping", mappingID).
						Str("session", *mapping.SessionID).
						Msg("session dead, recovering stuck task")
					// Reuse the existing idempotent completion flow.
					o.HandleSessionCompleted(ctx, *mapping.SessionID, domain.TaskMappingStatusFailed)
				case session.LivenessParked:
					o.logger.Info().
						Str("repo", repoID).
						Str("mapping", mappingID).
						Str("session", *mapping.SessionID).
						Msg("session parked (tmux pane deliberately reaped), leaving task in progress")
				case session.LivenessAlive:
					// Still working; the sweep has nothing to recover.
				}
			} else {
				// InProgress with no session — orphaned. Mark failed and advance.
				o.logger.Warn().
					Str("repo", repoID).
					Str("mapping", mappingID).
					Msg("in-progress mapping with no session, marking failed")
				o.updateMappingStatus(ctx, mapping.ID, domain.TaskMappingStatusFailed)
				o.mu.Lock()
				if o.activeMapping[repoID] == mappingID {
					delete(o.activeMapping, repoID)
				}
				o.mu.Unlock()
				o.dequeueNext(ctx, repoID)
			}

		default:
			// Pending — shouldn't be in activeMapping. Mark failed and advance.
			o.logger.Warn().
				Str("repo", repoID).
				Str("mapping", mappingID).
				Msg("pending mapping in active state, marking failed")
			o.updateMappingStatus(ctx, mapping.ID, domain.TaskMappingStatusFailed)
			o.mu.Lock()
			if o.activeMapping[repoID] == mappingID {
				delete(o.activeMapping, repoID)
			}
			o.mu.Unlock()
			o.dequeueNext(ctx, repoID)
		}
	}
}
