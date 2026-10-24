package main

import (
	"context"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/broadcast"
	"github.com/recurser/bossd/internal/callback"
	"github.com/recurser/bossd/internal/chatupload"
	"github.com/recurser/bossd/internal/resume"
)

func (w *daemonWiring) wireDeliveryWorkers() {
	// Start poller and dispatcher. pollerCtx is created above, next to the
	// bootstrap runner that also needs it.
	//
	// Hand the daemon-scoped context to the lifecycle so PollArmer.Arm
	// runs goroutines that outlive any single RPC handler.
	w.lifecycle.SetDaemonCtx(w.pollerCtx)

	// BOS-661 stale-upload janitor. Started here rather than beside the
	// manager because it needs the daemon-scoped pollerCtx (cancelled first
	// during shutdown, then joined via shutdownWG). It prunes completed
	// uploads past the retention TTL and abandoned in-flight ones, so a
	// client that disconnects mid-transfer cannot accumulate disk usage.
	if w.chatUploadMgr != nil {
		w.trackedGo("chat-upload-janitor", func() { w.chatUploadMgr.RunJanitor(w.pollerCtx, chatupload.DefaultJanitorInterval) })
	}

	pollerEvents := w.poller.Run(w.pollerCtx)
	merged := mergeSessionEvents(w.pollerCtx, pollerEvents, w.webhookEventCh)
	w.trackDone("poller", w.poller.Done())
	dispatcherDone := safego.Go(log.Logger, func() { w.dispatcher.Run(w.pollerCtx, merged) })
	w.trackDone("dispatcher", dispatcherDone)

	// The two startup recovery passes, run in ONE goroutine so they are
	// sequential. Both are tracked in the daemon lifecycle: either can enter the
	// full finalize pipeline or do git work, so shutdown must cancel and wait for
	// them before tearing down the process.
	//
	// Sequential is load-bearing, not tidiness. Both passes select pre-agent rows
	// (CreatingWorktree/StartingAgent) and both claim a row with a conditional
	// state transition, so concurrently exactly one wins and the other correctly
	// backs off — no corruption, but the WINNER decided the outcome, and the two
	// outcomes differ: reclaimed as a failed bootstrap (Blocked + worktree/branch
	// cleanup) versus finalized as a completed cron run. For a row whose worktree
	// may never have been created, finalize is the wrong answer — it can classify
	// the strand as worktree_gone/pr_failed and skip the artifact cleanup
	// entirely. Restart recovery must not be a coin flip between those.
	//
	// The bootstrap reaper goes FIRST because it is the pass that owns rows which
	// never reached an agent: it declines any row carrying an agent id or a tmux
	// pane, deferring those to the cron sweep that understands agent liveness.
	// Ordering it first enforces the ownership split that reaper already
	// documents, and narrows nothing — rows it declines fall straight through to
	// the cron sweep below, which finds the rows it did claim already in Blocked
	// (outside its reap set) and skips them.
	//
	// Also distinct in reach: the cron sweep only covers unattended runs, while
	// the bootstrap reaper also reclaims a plain `boss new` that died
	// mid-bootstrap, which nothing previously did (BOS-717). Its startup pass
	// only touches rows that predate this process, so it cannot race a create the
	// daemon has just accepted.
	startupRecovery := w.opts.startupStrandedCronRecovery
	if startupRecovery == nil {
		startupRecovery = w.lifecycle.RecoverStrandedCronSessionsAtStartup
	}
	startupBootstrapReap := w.opts.startupStrandedBootstrapReap
	if startupBootstrapReap == nil {
		startupBootstrapReap = w.lifecycle.ReapStrandedBootstrapSessionsAtStartup
	}
	w.trackedGo("startup-stranded-session-recovery", func() {
		if n, err := startupBootstrapReap(w.pollerCtx); err != nil {
			log.Warn().Err(err).Msg("failed to reap stranded bootstrap sessions")
		} else if n > 0 {
			log.Info().Int("count", n).Msg("reclaimed sessions stranded in early bootstrap")
		}

		if n, err := startupRecovery(w.pollerCtx); err != nil {
			log.Warn().Err(err).Msg("failed to recover stranded cron sessions")
		} else if n > 0 {
			log.Info().Int("count", n).Msg("recovered stranded cron sessions with lost finalize signal")
		}
	})

	// Start task orchestrator (polls plugin task sources).
	w.orchestrator.Start(w.pollerCtx)
	w.trackDone("task-orchestrator", w.orchestrator.Done())

	// Start display status poller.
	w.displayPoller.Run(w.pollerCtx)
	w.trackDone("display-poller", w.displayPoller.Done())

	// The archive deferrer (BOS-1380) fires archives deferred until every chat
	// is idle. It is an archive PRODUCER, so it is joined on shutdownWG like the
	// poller and dispatcher above: it must stop before drainArchiveWorkers
	// closes archive tracking, and the archives it fires are handed to
	// trackArchiveDone so that drain joins them.
	w.trackDone("archive-deferrer", w.srv.RunArchiveDeferrer(w.pollerCtx, w.trackArchiveDone))
	// The finish deferrer (BOS-1508) fires unattended-run finalizes requested
	// through FinishUnattendedRun once the session settles. A finalize can
	// hard-delete a worktree, so it is joined exactly like the archive
	// deferrer: stopped before the archive drain, its work handed to
	// trackArchiveDone.
	w.trackDone("finish-deferrer", w.srv.RunFinishDeferrer(w.pollerCtx, w.trackArchiveDone))
	// Post-merge tracker writes that did not land are retried from a durable
	// queue, so a restart does not drop them.
	w.trackDone("tracker-merge-retries", w.srv.RunTrackerMergeRetries(w.pollerCtx))

	// --- GitHub callback delivery worker (BOS-468) ---
	//
	// Leases triggered callbacks and delivers their registered message through
	// the verified wake+submit path (Server.SendChatMessage). ScheduleRetry with
	// capped backoff on failure; ExpireOverdue sweeps past-expiry rows. The
	// evaluator (wired into the webhook dispatcher) advances callbacks to
	// triggered; this worker owns triggered -> delivered.
	callbackDeliverer := callback.DelivererFunc(func(ctx context.Context, agentSessionID, message string) error {
		return deliverCallbackMessage(ctx, w.srv, agentSessionID, message)
	})

	// --- Transient-API-error auto-resume (BOS-518) ---
	//
	// Constructed here rather than beside chatRotator because it shares the
	// callback worker's delivery primitive (srv.SendChatMessage with wake+submit),
	// which only exists once srv does. It must exist before the status poller
	// starts below — the poller's first Bootstrap capture can already set the
	// marker and fire the tracker hook.
	//
	// SettleWindow/BackoffBase/Now/Schedule are deliberately left at their
	// package defaults: the tuning belongs to the policy, not to the wiring.
	w.transientResumer = resume.NewTransientResumer(resume.TransientResumerDeps{
		Logger:     log.Logger.With().Str("component", "transient-resume").Logger(),
		MarkerSet:  w.chatStatusTracker.TransientAPIError,
		AuthFailed: w.chatStatusTracker.AuthFailed,
		ChatState: func(agentSessionID string) (bossanovav1.ChatStatus, time.Time, bool) {
			entry := w.chatStatusTracker.Get(agentSessionID)
			if entry == nil {
				// No tracker evidence at all — the resumer treats unknown as "never
				// fire", which is why this reports ok=false rather than a zero status.
				return bossanovav1.ChatStatus_CHAT_STATUS_UNSPECIFIED, time.Time{}, false
			}
			return entry.Status, entry.LastOutputAt, true
		},
		ChatLiveness: func(agentSessionID string) (time.Time, bool, bool) {
			// The restart lane's stalled predicate needs a stamp that a spinner frame
			// does not move and a bootstrap seed does not fake. Entry.LastOutputAt is
			// neither: it advances on any repaint, and after a restart it is the
			// poller's synthetic seed. Tracker.Liveness (BOS-805) carries both the
			// spinner-insensitive stamp and the flag that discloses the seed, which is
			// exactly the pair the resumer gates on.
			//
			// spinnerPresent is discarded on purpose: it is a "working right now"
			// reading with its own staleness gate, and the resumer's question is about
			// the past ("has anything real happened since the severance?"), which the
			// substantive stamp answers on its own.
			_, lastSubstantiveAt, seeded := w.chatStatusTracker.Liveness(agentSessionID)
			if lastSubstantiveAt.IsZero() && !seeded {
				// No reading at all — the poller has never observed this pane. Report
				// ok=false so the resumer's unknown ⇒ never-fire rule applies.
				return time.Time{}, false, false
			}
			return lastSubstantiveAt, seeded, true
		},
		SessionResumable: func(ctx context.Context, agentSessionID string) bool {
			chat, err := w.agentChats.GetByAgentSessionID(ctx, agentSessionID)
			if err != nil || chat == nil {
				return false
			}
			sess, err := w.sessions.Get(ctx, chat.SessionID)
			if err != nil || sess == nil {
				return false
			}
			// Archived, merged and closed all mean a human deliberately ended the
			// work; Orphaned means the run was killed mid-flight and has no live
			// agent to deliver into. Merged/Closed mirror cron's isTerminalState
			// (unexported, hence not reused); Orphaned is added because it is
			// terminal by definition. Every other state — including Blocked and the
			// post-implement waiting states — keeps a live pane a resume can reach,
			// and the resumer's IDLE-only gate already filters the rest.
			return sess.ArchivedAt == nil &&
				sess.State != machine.Merged &&
				sess.State != machine.Closed &&
				sess.State != machine.Orphaned
		},
		Deliver: func(ctx context.Context, agentSessionID, message string) error {
			return deliverChatMessage(ctx, w.srv, agentSessionID, message)
		},
	})
	if w.opts.onTransientResumeSeamsWired != nil {
		w.opts.onTransientResumeSeamsWired(w.transientResumer != nil && w.transientResumeHookInstalled)
	}
	if w.opts.onEvictionRecomputeSeamsWired != nil {
		w.opts.onEvictionRecomputeSeamsWired(w.evictionRecomputeHookInstalled && w.chatStatusTracker.HasOnEntriesEvicted())
	}

	callbackWorker := callback.NewDeliveryWorker(callback.WorkerConfig{
		Store:      w.githubCallbacks,
		Deliverer:  callbackDeliverer,
		Reconciler: w.callbackEvaluator,
		Gate:       w.callbackRetirer,
		Logger:     log.Logger,
		Telemetry:  w.telemetryClient,
	})
	callbackWorkerDone := safego.Go(log.Logger, func() { callbackWorker.Run(w.pollerCtx) })
	w.trackDone("callback-delivery-worker", callbackWorkerDone)

	// --- Broadcast delivery worker (BOS-556) ---
	//
	// Leases the outstanding deliveries of resolved broadcasts and injects the
	// rendered prompt through the same verified wake+submit path the callback
	// worker uses (Server.SendChatMessage, never a direct tmux write), so a
	// broadcast reaches an asleep chat and is actually submitted rather than
	// left in the composer. SendBroadcast owns pending -> resolved; this worker
	// owns resolved -> completed, with capped backoff retry and a lazy expiry
	// sweep per tick.
	broadcastDeliverer := broadcast.DelivererFunc(func(ctx context.Context, agentSessionID, message string) error {
		return deliverChatMessage(ctx, w.srv, agentSessionID, message)
	})
	//
	// The worker's tick also carries the standing-subscription reconcile sweep
	// (BOS-557), the same way the callback worker carries the callback one: it
	// expires overdue subscriptions and fires any whose owning session already
	// sits in a trigger state, covering both a transition made while the daemon
	// was down and the startup bulk orphan advance that bypasses the hook.
	broadcastWorker := broadcast.NewDeliveryWorker(broadcast.WorkerConfig{
		Store:      w.broadcasts,
		Deliverer:  broadcastDeliverer,
		Reconciler: w.broadcastSubscriptionEvaluator,
		Logger:     log.Logger,
		Telemetry:  w.telemetryClient,
	})
	broadcastWorkerDone := safego.Go(log.Logger, func() { broadcastWorker.Run(w.pollerCtx) })
	w.trackDone("broadcast-delivery-worker", broadcastWorkerDone)

	// Advertises the daemon's live GitHub callback-interest set to bosso as a
	// steady-state delta on the reverse-stream bus whenever it changes (snapshot
	// semantics: the full set each publish, empty = withdraw all). The
	// connect/reconnect DaemonSnapshot carries the guaranteed-first full set; this
	// advertiser keeps bosso's routing table current between reconnects.
	interestAdvertiser := callback.NewInterestAdvertiser(callback.AdvertiserConfig{
		Store: w.githubCallbacks,
		Publisher: callback.InterestPublisherFunc(func(interests []*bossanovav1.CallbackInterest) {
			w.streamBus.Publish(upstream.StreamEvent{Interests: &upstream.InterestsEvent{Interests: interests}})
		}),
		Logger: log.Logger,
	})
	interestAdvertiserDone := safego.Go(log.Logger, func() { interestAdvertiser.Run(w.pollerCtx) })
	w.trackDone("callback-interest-advertiser", interestAdvertiserDone)

	// Startup reconciliation: fire callbacks whose enduring PR state
	// (merged/closed/checks) was reached while the daemon was disconnected, so
	// the triggering webhook was never delivered. Best-effort; non-fatal.
	w.trackedGo("startup-callback-reconcile", func() {
		if err := w.callbackEvaluator.ReconcileAll(w.pollerCtx); err != nil && w.pollerCtx.Err() == nil {
			log.Warn().Err(err).Msg("startup github callback reconciliation failed")
		}
		// Advertise the freshly-reconciled interest set promptly rather than
		// waiting for the advertiser's first tick (reconcile may have advanced
		// callbacks active -> triggered, changing the set).
		if _, err := interestAdvertiser.Publish(w.pollerCtx); err != nil && w.pollerCtx.Err() == nil {
			log.Warn().Err(err).Msg("startup github callback interest advertisement failed")
		}
	})

	// Startup reconciliation for standing broadcast subscriptions (BOS-557):
	// fire any whose owning session reached a trigger state while the daemon was
	// down, and retire the overdue. Waiting for the worker's first periodic sweep
	// would leave a coordinator waiting reconcileEveryTicks polls for news that
	// already happened. Best-effort; non-fatal.
	w.trackedGo("startup-broadcast-subscription-reconcile", func() {
		if err := w.broadcastSubscriptionEvaluator.ReconcileAll(w.pollerCtx); err != nil && w.pollerCtx.Err() == nil {
			log.Warn().Err(err).Msg("startup broadcast subscription reconciliation failed")
		}
	})

}
