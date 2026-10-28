package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog/log"
)

func (w *daemonWiring) wireShutdown() error {
	// --- Wait for shutdown trigger ---

	select {
	case sig := <-w.opts.stopSig:
		log.Info().Str("signal", sig.String()).Msg("shutting down")
	case err := <-w.errCh:
		// Server exited unexpectedly.
		return fmt.Errorf("server: %w", err)
	}

	// Stop poller, dispatcher, and task orchestrator (all use pollerCtx).
	// Must cancel before stopping plugin host, since the orchestrator
	// calls into plugins.
	w.pollerCancel()

	// Stop upstream StreamClient (if running).
	w.streamCancel()

	// Stop credential maintenance before the plugin host goes away (BOS-1141):
	// a live verification drives an agent runner plugin, so it must be given a
	// bounded chance to finish rather than have its transport pulled. Stop()
	// cancels future scheduling and waits, bounded, for owned work.
	w.accountSync.Stop()
	w.accountCredMaintenance.Stop()

	// Stop the cron scheduler and wait for in-flight fires to finish. Bound
	// the wait so a stuck CreateSession cannot delay overall shutdown.
	cronStopCtx, cronStopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := w.cronScheduler.Stop(cronStopCtx); err != nil {
		log.Warn().Err(err).Msg("cron scheduler stop timed out")
	}
	cronStopCancel()

	// Drain the failover proxy, THEN stop the plugin host (BOS-888). Agents reach
	// the model only through the proxy, so stopping plugins first tears down the
	// agent processes while their model streams are still open. The proxy gets its
	// own budget rather than the shared 5s ctx created below — an ordinary Claude
	// turn outlives 5s by minutes. A nil interface, not a typed nil, when the
	// proxy never came up: it is constructed and bound unconditionally above and
	// only its injection is gated on managed accounts, so proxySrv is nil exactly
	// when construction or Listen failed.
	//
	// Known cost: srv.Shutdown is now up to the drain budget later, so the gRPC
	// socket keeps accepting for that window in a daemon whose poller,
	// dispatcher and orchestrator are already cancelled above. The drain bails
	// out early once the in-flight count has stopped falling (BOS-1219), which
	// only ever SHORTENS this leg — the budget below is still the ceiling the
	// accounting has to cover. A `boss` command
	// that connects mid-drain reaches a half-shut-down daemon rather than
	// failing fast. The window was already a few seconds before BOS-888 and the
	// drain budget is deliberately sized (config.defaultProxyDrainTimeout) to
	// stay well inside daemon.LifecycleShutdownTimeout, which is what bounds it.
	// Closing the gRPC listener first would narrow the window but would cut
	// in-flight CreateSession calls earlier than today, so it is not done here.
	var drainer proxyDrainer
	if w.proxySrv != nil {
		drainer = w.proxySrv
	}
	drainProxyThenStopPlugins(log.Logger, drainer, w.pluginHost, w.settings.ManagedAccounts.ProxyDrainTimeout())
	w.pluginBus.Close()

	// Graceful shutdown with 5-second timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := w.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// Shut down the hook server and remove its port file.
	if err := w.hookSrv.Shutdown(ctx); err != nil {
		log.Warn().Err(err).Msg("hook server shutdown error")
	}

	// Join any background draft-PR creates StartSession spawned (BOS-540).
	// They run on a context deliberately detached from the request that started
	// them, so nothing else cancels them; this is what keeps an interrupted
	// create from being stranded *silently*. It runs AFTER srv.Shutdown so the
	// set it snapshots is final — draining earlier would miss a create spawned
	// by a CreateSession still in flight — and after the other Shutdown calls
	// because it owns its own budget: srv and hookSrv share the single 5s ctx
	// above, and a drain wedged in between would hand them an already-expired
	// one, turning every graceful shutdown into an abrupt listener close. The
	// failover proxy's drain is the one long wait that is deliberately NOT in
	// that group (BOS-888): it runs further up, before the plugin host stops and
	// before the 5s ctx is created at all, on a budget of its own, so it cannot
	// eat anyone else's. The bound is short
	// because a create can legitimately take ~a minute: the point is not to
	// finish it but to name the sessions we are walking away from, whose branch
	// and placeholder commit survive on the remote for the next reconcile pass
	// to attach. Stragglers are then cancelled — asynchronously, so this asks
	// them to stop rather than waiting; the deferred database.Close may still
	// beat one, which surfaces as a logged store error, not a crash.
	draftPRCtx, draftPRCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if abandoned := w.lifecycle.WaitForBackgroundDraftPRs(draftPRCtx); len(abandoned) > 0 {
		log.Warn().
			Strs("sessions", abandoned).
			Msg("shutting down with draft PR creation still in flight; branches are preserved and the PR may still be created remotely")
	}
	draftPRCancel()
	w.lifecycle.CancelBackgroundDraftPRs()

	// A failover during the proxy drain above can leave a chat row rewrite
	// running. Cancel it and wait for it here, before the database closes.
	relabelCtx, relabelCancel := context.WithTimeout(context.Background(), 2*time.Second)
	if !w.lifecycle.StopBackgroundRelabels(relabelCtx) {
		log.Warn().Msg("shutting down while a chat row rewrite is still running; some chat rows may still show the previous account")
	}
	relabelCancel()

	// Clean up socket file.
	_ = os.Remove(w.socketPath)

	// Wait for all tracked daemon goroutines to exit, with a hard 10-second
	// upper bound. Logs a warning on timeout — we still exit cleanly but
	// some goroutines may have been abandoned (e.g. a plugin RPC hang).
	waitForTrackedGoroutines(log.Logger, w.shutdownWG.wait, w.shutdownWG.stillRunning, w.goroutineBudget)

	// Only now join the auto-archive workers (BOS-923). The wait above is what
	// proves their producers — poller, dispatcher, reconcile sweep, task
	// orchestrator — have stopped, and srv.Shutdown above drained the handler
	// goroutine that MergeSession's post-merge refresh runs on, so closing
	// registration here cannot refuse an archive any live producer is about to
	// start. On the forced-exit branch a producer may still be alive, and then
	// a refusal is possible and says so in the log.
	//
	// Called explicitly here, rather than left to its defer, so the join lands
	// inside the shutdown sequence and ahead of the "daemon stopped" log line
	// instead of after it. The defer is what covers the error returns above.
	w.drainArchiveWorkers()

	log.Info().Msg("daemon stopped")
	return nil
}
