package main

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossd/internal/callback"
	"github.com/recurser/bossd/internal/inflight"
	"github.com/recurser/bossd/internal/status"
)

func (w *daemonWiring) wireBootstrap() error {
	// Start the cron scheduler now that the hook port is set, so cron-spawned
	// sessions (which carry a HookToken) pass StartSession's hookPort guard
	// instead of being recorded as fire_failed during the startup window.
	if err := w.cronScheduler.Start(context.Background()); err != nil {
		return fmt.Errorf("cron scheduler: %w", err)
	}

	// Re-arm the poll fallback for hookless agent runs that survived a
	// daemon restart. For agents with a finalize hook (claude), the poll
	// re-arm is skipped (cached IsSupported=true short-circuits the loop) but
	// ConfigureFinalizeHook still re-writes the worktree hook config with this
	// daemon's port — which is why SetHookPort must run first (above). For
	// codex (and future hookless agents), this re-attaches the daemon to
	// in-flight runs so their eventual completion still signals through.
	if w.opts.onBootstrapStart != nil {
		w.opts.onBootstrapStart()
	}
	w.lifecycle.Bootstrap(w.pollerCtx)

	// Now — and only now — act on the streams the previous daemon's death
	// severed (BOS-890). Bootstrap has just re-adopted every surviving pane's
	// frozen proxy token (BOS-481), so a pane that can reconnect on its own has
	// already been handed back the means to do it; marking earlier would start
	// the settle window against chats that were never really stuck. Marking is
	// only a trigger: each chat still has to pass the resumer's full gate ladder
	// — settle window, auth deference, session-resumable, IDLE, attempt budget —
	// a settle window from now, against live state.
	//
	// This placement is NOT an ordering contract against the tmux poller's
	// Bootstrap further below, and deliberately so. It used to be one implicitly:
	// the resumer compared the tracker's last-output stamp against the severance
	// instant, and Bootstrap seeds every restored pane with `now - IdleThreshold
	// - 1s` — a margin of ~6 seconds that the startup work between here and there
	// (orphan advancement, headless resume, a display backfill that recomputes
	// every active session under a 30s timeout) blows through on any busy
	// workspace, at which point the seed lands after the severance and every
	// severed chat reads as recovered. The resumer now gates on
	// Tracker.Liveness's seeded flag instead, which says "the poller has observed
	// nothing real here yet" regardless of what the placeholder timestamp says.
	// The dependency is removed rather than documented-and-untested, which is why
	// there is no ordering assertion here to keep honest.
	if w.transientResumer != nil {
		inflight.MarkSevered(log.Logger, w.severedStreamIDs, w.transientResumer.OnStreamSevered)
	}
	// Consumed (or unconsumable — with no resumer wired nothing will ever act on
	// them, and re-persisting would hand the next daemon an ever-growing backlog
	// of stale claims, which is exactly what ReadAndClear's one-shot rule
	// forbids). Either way the fail-safe re-persist above must not fire.
	w.severedStreamIDs = nil

	// Advance sessions stuck in ImplementingPlan whose driving workflows are no
	// longer running. Must run after MarkFailed (above) so the subquery sees
	// the updated workflow statuses, AND after lifecycle.Bootstrap's headless
	// orphan sweep so a restart-killed `boss new --detach` run has already been
	// marked ORPHANED and is no longer in ImplementingPlan — otherwise this bulk
	// advance (which matches any workflow-less ImplementingPlan row) would move it
	// to AwaitingChecks and its bootstrap-only PR would read as green (BOS-229).
	if n, err := w.sessions.MarkOrphansArchived(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to advance orphaned sessions")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("advanced orphaned sessions to awaiting_checks")
	}

	// Resume only after the startup bulk advancement. Bootstrap's orphan sweep
	// stamps restart-killed detached runs Orphaned; resuming earlier would put
	// them back in ImplementingPlan where MarkOrphansArchived mistakes them
	// for a completed workflow-less run and advances them to AwaitingChecks.
	if n := w.lifecycle.ResumeOrphanedHeadlessRuns(w.pollerCtx); n > 0 {
		log.Info().Int("count", n).Msg("orphan-resume sweep: resumed orphaned headless runs")
	}

	// Backfill the display-status composite for every active session. After
	// a daemon restart the in-memory inputs (chat, display tracker) are
	// empty, so the persisted display_label may not match the stored state.
	// Recomputing once at boot ensures the row matches what Compute would
	// produce given current inputs (typically "stopped" or PR-axis label),
	// so clients reading via the bosso DB-fallback path don't see stale
	// "running 2/4" labels from the previous daemon's last write. Runs after the
	// orphan sweep + MarkOrphansArchived so it reflects the final states.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		all, err := w.rawSessions.ListActive(ctx, "")
		if err != nil {
			log.Warn().Err(err).Msg("display backfill: list active sessions failed")
		} else {
			var updated int
			for _, s := range all {
				if err := w.displayComputer.Recompute(ctx, s.ID); err != nil {
					log.Debug().Err(err).Str("session_id", s.ID).Msg("display backfill: recompute failed")
					continue
				}
				updated++
			}
			if updated > 0 {
				log.Info().Int("count", updated).Msg("display backfill: recomputed active sessions")
			}
		}
		cancel()
	}

	// Bootstrap tmux status poller with pre-existing sessions before starting
	// the polling loop, so sessions from before a daemon restart show correct
	// status (idle/question) instead of defaulting to unknown.
	w.tmuxStatusPoller.Bootstrap(context.Background())
	if w.opts.onBootstrapComplete != nil {
		w.opts.onBootstrapComplete()
	}

	// Start tmux status poller (captures pane content to detect question/idle/working).
	w.tmuxStatusPoller.Run(w.pollerCtx)
	w.trackDone("tmux-status-poller", w.tmuxStatusPoller.Done())

	// Start the tmux reaper. It carries two independently-knobbed paths:
	//
	//   - the orphan path (BOS-846), which reconciles live panes against the DB
	//     and kills the ones no row can account for. It ships disabled and
	//     dry-run, because an orphan reap is unrecoverable;
	//   - the idle path (BOS-886), which kills the pane of a chat nobody has
	//     touched for hours and clears that chat's pane pointer. It ships
	//     ENABLED, because the chat row survives and attaching wakes it.
	//
	// With neither enabled Run returns immediately. The idle path's three
	// dependencies all fail closed if they are ever left nil here: no tracker
	// means no evidence, no oracle means no provable transcript, and no kill
	// seam means candidates are logged and left alive.
	tmuxReaper := status.NewTmuxReaper(w.agentChats, w.sessions, w.repos, w.tmuxClient,
		w.tmuxDaemonID, w.settings.TmuxReaper, status.IdleReapDeps{
			Config: w.settings.TmuxIdleReap,
			DeliveryPending: func(ctx context.Context, id string) (bool, error) {
				return callback.DeliveryPendingForChat(ctx, w.githubCallbacks, id, time.Now())
			},
			ProgressWait: func(ctx context.Context, chat *domain.AgentChat, sess *models.Session) (bool, error) {
				return w.progressWait(ctx, chat, sess, time.Time{})
			},
			SessionAttention: w.srv.SessionAttention,
			Tracker:          w.chatStatusTracker,
			Transcripts:      status.NewAgentTranscriptOracle(w.agentClients),
			// The canonical clear-then-kill teardown, reached through a
			// function value rather than an import: session already imports
			// status, so importing it back would be a cycle.
			KillChat: w.lifecycle.ReapIdleChatTmuxSession,
		}, log.Logger)
	w.terminalReapObserver.Bind(tmuxReaper)
	tmuxReaper.Run(w.pollerCtx)
	w.trackDone("tmux-reaper", tmuxReaper.Done())

	return nil
}
