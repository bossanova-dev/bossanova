package main

import (
	"errors"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/recurser/bossd/internal/session"
)

func (w *daemonWiring) wireBackgroundWorkers() {
	// Start chat status cleanup goroutine (GC stale entries every 30s).
	w.trackedGo("chat-status-cleanup", func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-w.pollerCtx.Done():
				return
			case <-ticker.C:
				w.chatStatusTracker.Cleanup()
				sweepWaitingChats(w.pollerCtx, w.chatStatusTracker, w.agentChats, w.displayComputer)
			}
		}
	})

	// Relaunches may wait for agent readiness; keep them off the chat-status
	// cleanup lane while retaining the 30-second due-attempt cadence.
	w.trackedGo("dropped-run-resume-sweep", func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-w.pollerCtx.Done():
				return
			case <-ticker.C:
				w.lifecycle.ResumeDroppedRuns(w.pollerCtx)
				w.lifecycle.MarkWorkingBlockedRuns(w.pollerCtx)
			}
		}
	})

	// Periodically reconcile sessions that are missing a PR number against
	// existing PRs on the same branch. The same call also runs once at
	// startup (above), but a long-lived daemon needs to keep reconciling:
	// the cron-tmux finalize path can race or surface a PR via a path
	// that doesn't write back to the session row, leaving the UI showing
	// "no PR" for a session whose branch already has one. 60s is a
	// compromise — fast enough that the gap is barely visible, slow
	// enough that the GitHub list-PRs cost stays small (the inner branch
	// only fires when there ARE orphaned sessions).
	w.trackedGo("pr-association-reconcile", func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-w.pollerCtx.Done():
				return
			case <-ticker.C:
				if n, err := w.prAssociationResolver.Reconcile(w.pollerCtx); err != nil {
					log.Warn().Err(err).Msg("periodic reconcile: failed")
				} else if n > 0 {
					log.Info().Int64("count", n).Msg("periodic reconcile: linked sessions to existing PRs")
				}
				// After the PR reconcile, so a just-associated PR's title is
				// already a candidate source for this pass.
				if n, err := w.trackerLinker.LinkActive(w.pollerCtx); errors.Is(err, session.ErrTrackerLinkPassTimedOut) {
					// The pass spent its own budget; the next tick resumes it.
					log.Info().Err(err).Int64("count", n).Msg("periodic tracker link: pass budget spent")
				} else if err != nil {
					log.Warn().Err(err).Msg("periodic tracker link: failed")
				} else if n > 0 {
					log.Info().Int64("count", n).Msg("periodic tracker link: linked sessions to tracker issues")
				}
				// Same tick, deliberately AFTER the reconcile above (BOS-875):
				// a session that merely LOST its PR association is attached by
				// the reconcile, so by the time this runs the remaining PR-less
				// sessions are the ones whose create genuinely failed and should
				// be re-pushed rather than re-linked.
				if n, err := w.lifecycle.RetryFailedDraftPRsPeriodic(w.pollerCtx); err != nil {
					log.Warn().Err(err).Msg("periodic draft-PR retry: failed")
				} else if n > 0 {
					log.Info().Int("count", n).Msg("periodic draft-PR retry: re-attempted PR creation for failed sessions")
				}
			}
		}
	})

	// Periodically clear repair alerts whose condition has ended (BOS-1535):
	// a blocked/failed/stalled repair alert used to outlive the head, CI run and
	// PR state that raised it. 60s matches the PR-association reconcile; the
	// sweep reads only the DB and the in-memory display/lease state.
	w.trackedGo("resolved-alert-clear", func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-w.pollerCtx.Done():
				return
			case <-ticker.C:
				if n, err := w.lifecycle.ClearResolvedAlerts(w.pollerCtx); err != nil {
					log.Warn().Err(err).Msg("periodic resolved-alert clear: failed")
				} else if n > 0 {
					log.Info().Int("count", n).Msg("periodic resolved-alert clear: cleared resolved repair alerts")
				}
			}
		}
	})

	// Periodically recover cron sessions whose Stop-hook finalize signal was
	// lost (see the startup call above). The startup pass only fires on a
	// restart; this sweep catches a hook that failed to deliver on a daemon
	// that stayed up. 2 min matches the orchestrator's reconcile cadence — the
	// session is already minutes-late, so sub-minute latency buys nothing, and
	// the inner agent-log idle checks only run when sessions are stuck.
	w.trackedGo("periodic-stranded-session-recovery", func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-w.pollerCtx.Done():
				return
			case <-ticker.C:
				if n, err := w.lifecycle.ReapExpiredKeptWorktrees(w.pollerCtx); err != nil {
					log.Warn().Err(err).Msg("periodic kept-worktree reap failed")
				} else if n > 0 {
					log.Info().Int("count", n).Msg("reaped expired prepared worktrees")
				}
				if n, err := w.lifecycle.RecoverStrandedCronSessionsPeriodic(w.pollerCtx); err != nil {
					log.Warn().Err(err).Msg("periodic stranded-cron recovery: failed")
				} else if n > 0 {
					log.Info().Int("count", n).Msg("periodic stranded-cron recovery: finalized stranded cron sessions")
				}
				// Same cadence, same tick: reclaim bootstraps that outlived the
				// bootstrap deadline on a daemon that stayed up (BOS-717). Gated
				// on age > deadline + margin, so a live create is never touched.
				if n, err := w.lifecycle.ReapStrandedBootstrapSessionsPeriodic(w.pollerCtx); err != nil {
					log.Warn().Err(err).Msg("periodic stranded-bootstrap reap: failed")
				} else if n > 0 {
					log.Info().Int("count", n).Msg("periodic stranded-bootstrap reap: reclaimed stranded bootstraps")
				}
			}
		}
	})

	// Periodically re-dispatch headless runs parked mid-rotation once their
	// resume-at stamp comes due (BOS-174). Level-triggered off persisted
	// rotation_resume_at, so it re-arms every parked run across a daemon restart
	// (no in-memory timer). The kill switch (ManagedAgentAccountsEnabled) short-circuits the
	// sweep, and until BOS-170 wires the binding/materializer every candidate
	// stays parked. Cadence comes from the rotation config (defaulted when unset).
	w.trackedGo("parked-rotation-sweep", func() {
		ticker := time.NewTicker(w.settings.ManagedAccounts.ParkSweepInterval())
		defer ticker.Stop()
		for {
			select {
			case <-w.pollerCtx.Done():
				return
			case <-ticker.C:
				if n := w.lifecycle.SweepParkedRotations(w.pollerCtx); n > 0 {
					log.Info().Int("count", n).Msg("parked-rotation sweep: redispatched parked headless runs")
				}
			}
		}
	})

	// Periodically auto-resume headless runs a daemon restart orphaned (BOS-407).
	// Level-triggered off the persisted Orphaned state, so it re-arms every
	// still-orphaned run across a daemon restart (no in-memory timer) and retries
	// any resume that could not happen at Bootstrap (agent plugin not yet ready,
	// transient StartByAgent error). Default OFF — ResumeOrphanedHeadlessRuns is a
	// no-op unless AutoResumeOrphans is opted in. Reuses the park-sweep cadence.
	w.trackedGo("orphaned-run-resume-sweep", func() {
		ticker := time.NewTicker(w.settings.ManagedAccounts.ParkSweepInterval())
		defer ticker.Stop()
		for {
			select {
			case <-w.pollerCtx.Done():
				return
			case <-ticker.C:
				if n := w.lifecycle.ResumeOrphanedHeadlessRuns(w.pollerCtx); n > 0 {
					log.Info().Int("count", n).Msg("orphan-resume sweep: resumed orphaned headless runs")
				}
			}
		}
	})

	// Proactive pre-cap rotation sweep (BOS-318). Periodically pre-empts a cap:
	// rotates IDLE chats off soon-to-cap accounts onto materially-idler ones.
	// Default OFF — SweepProactive is a no-op unless ProactiveRotation is set.
	w.trackedGo("proactive-rotation-sweep", func() {
		ticker := time.NewTicker(w.settings.ManagedAccounts.ProactiveSweepInterval())
		defer ticker.Stop()
		for {
			select {
			case <-w.pollerCtx.Done():
				return
			case <-ticker.C:
				if n := w.chatRotator.SweepProactive(w.pollerCtx); n > 0 {
					log.Info().Int("count", n).Msg("proactive pre-cap sweep: rotated idle chats")
				}
			}
		}
	})

	// Periodic usage refresh (BOS-320 hardening). Keeps active accounts' cached
	// usage snapshots inside the staleness window so the util-aware
	// default-account selector sidelines capped accounts instead of degrading to
	// LRU and binding a new session to an exhausted account. Benign vs the
	// proactive sweep — it re-probes and caches usage and, since BOS-584, clears
	// a cooldown its own probe contradicts, but never mutates a session — so it
	// runs whenever rotation is not the explicit kill switch, with an immediate
	// pass at boot (snapshots are otherwise stale until the first rotation event,
	// e.g. after a daemon restart).
	if w.settings.ManagedAccounts.ManagedAgentAccountsEnabled() {
		w.trackedGo("account-usage-refresh", func() {
			// Owned by this goroutine alone, so the boot pass and every ticker
			// pass share one view of which accounts are backing off with no
			// mutex: single writer, single reader, never escaping this closure.
			// Deliberately in-memory — a daemon restart forgets the backoff,
			// which is acceptable against the endpoint's hour-long trailing
			// window (a restart partly ages out of it anyway) and is far
			// cheaper than persisting polling state. Only this periodic loop
			// honours it; on-demand rotation/lifecycle/failover probes pass nil
			// because they need a fresh answer to decide correctly.
			throttleUntil := map[string]time.Time{}
			if n := refreshActiveAccountUsage(w.pollerCtx, log.Logger, w.accountMaterializer, w.accounts, throttleUntil); n > 0 {
				log.Info().Int("count", n).Msg("usage-refresh: seeded account usage at boot")
			}
			ticker := time.NewTicker(w.settings.ManagedAccounts.UsageRefreshInterval())
			defer ticker.Stop()
			for {
				select {
				case <-w.pollerCtx.Done():
					return
				case <-ticker.C:
					refreshActiveAccountUsage(w.pollerCtx, log.Logger, w.accountMaterializer, w.accounts, throttleUntil)
				}
			}
		})
	}

	// Proactive Codex credential maintenance (BOS-1141). One stale-at-boot
	// sweep after daemon wiring settles, then a jittered periodic sweep, so a
	// credential that died while the daemon was down is discovered before a
	// session tries to use it rather than by that session failing. Gated on
	// managed accounts for the same reason the usage refresh is: with rotation
	// off there is no registry to maintain. Stopped explicitly during shutdown.
	//
	// That gate is deliberately NOT repeated here. It lives inside the sweep
	// (WithAuthCheckEnabled, wired at construction) so it is re-read live: the
	// kill switch is a settings.json value the operator can flip while the
	// daemon runs, and a startup-only conjunct would both keep sweeping after a
	// disable and never start after an enable. With rotation off the loop parks
	// on its timer and each wake returns immediately.
	if w.accountCredMaintenance != nil {
		w.accountCredMaintenance.Start(w.pollerCtx)
		// The explicit shutdown Stop preserves ordering; run-scoped cleanup
		// also covers error returns. Stop is idempotent, so the pair is safe.
		w.deferCleanup(w.accountCredMaintenance.Stop)
	}

}
