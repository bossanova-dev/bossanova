package main

import (
	"context"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/adapter/proofenvkeyring"
	cronpkg "github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/domain/questionsignal"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/taskorchestrator"
)

func (w *daemonWiring) wireTaskWorkers() {
	// --- Task Orchestrator ---

	// Warn if tmux is not available — interactive sessions will fail at attach
	// time, and cron fires will record fire_failed (cron-spawned sessions are
	// hosted in tmux with no headless fallback).
	if !w.tmuxClient.Available(context.Background()) {
		log.Warn().Msg("tmux is not installed or not in PATH; interactive sessions will not work, and cron fires will record fire_failed")
	}

	sessionCreator := taskorchestrator.NewSessionCreatorWithAgentAccountResolver(w.sessions, w.lifecycle, func() string {
		loaded, err := config.Load()
		if err != nil {
			log.Warn().Err(err).Msg("load config for orchestrated session default agent")
			return w.settings.DefaultAgent
		}
		return loaded.DefaultAgent
	}, w.livenessChecker, func(_ context.Context, sessionID string) {
		// Propagate cleanup of a half-started session so it doesn't linger
		// as a phantom row in the web read model until the daemon reconnects.
		w.streamBus.Publish(upstream.StreamEvent{
			Session: &upstream.SessionEvent{
				Kind:    bossanovav1.SessionDelta_KIND_DELETED,
				Session: &bossanovav1.Session{Id: sessionID},
			},
		})
	}, w.accountResolver, log.Logger)
	w.orchestrator = taskorchestrator.New(
		w.pluginHost, w.repos, w.taskMappings, sessionCreator, w.ghProvider,
		w.worktrees, w.livenessChecker, taskorchestrator.DefaultPollInterval, log.Logger,
	)

	w.orchestrator.SetFactoryRepoChecker(w.factoryRepoChecker)
	w.orchestrator.SetRepairActivity(w.repairLease)

	// --- Cron Scheduler ---

	// cronActivity is shared by BOTH the scheduler's overlap suppression and the
	// server's cron STATUS derivation (BOS-332), so the TUI STATUS column and the
	// scheduler agree on "is this run still active" from one source of truth.
	w.cronActivity = session.NewCronActivityChecker(w.agentLogsDir, w.livenessChecker)
	// The adapter uses the same lifecycle stop and queue notification as the RPC.
	cronCanceller := cronpkg.RunCancellerFunc(func(ctx context.Context, id string) error {
		if err := w.lifecycle.StopSession(ctx, id); err != nil {
			return err
		}
		w.orchestrator.HandleSessionCompleted(ctx, id, domain.TaskMappingStatusFailed)
		return nil
	})
	var _ cronpkg.RunCanceller = cronCanceller
	w.cronScheduler = cronpkg.New(cronpkg.Config{
		Store:     w.cronJobs,
		Sessions:  w.sessions,
		Repos:     w.repos,
		Creator:   sessionCreator,
		Activity:  w.cronActivity,
		Canceller: cronCanceller,
		// Cron gates receive only the scoped proof model key, never the upload token.
		GateProofEnv: proofenvkeyring.New(log.Logger),
		Logger:       log.Logger,
		Telemetry:    w.telemetryClient,
	})
	// NOTE: cronScheduler.Start is intentionally deferred until after the hook
	// server is bound and lifecycle.SetHookPort has run (below). A tick that
	// fires before the port is set would have its session rejected by
	// StartSession (hookPort == 0) and be recorded as fire_failed, which is most
	// likely right after a restart that lands near a scheduled tick.

	// Wire the orchestrator as the completion notifier for the dispatcher
	// display poller, and server so terminal session states unblock the per-repo task queue.
	w.dispatcher.SetCompletionNotifier(w.orchestrator)
	w.displayPoller.SetCompletionNotifier(w.orchestrator)

	// --- Tmux Status Poller ---

	// questionSignals is the structured "a question is pending" store (BOS-485),
	// shared between the poller (reads/clears it to drive CHAT_STATUS_QUESTION)
	// and the hook server (writes it when the agent's Notification hook fires).
	// One instance so both sides see the same records.
	w.questionSignals = questionsignal.NewStore(questionsignal.DefaultTTL)

	w.tmuxStatusPoller = status.NewTmuxStatusPoller(w.chatStatusTracker, w.agentChats, w.sessions, w.tmuxClient, w.agentClients, log.Logger)
	w.tmuxStatusPoller.SetQuestionSignals(w.questionSignals)
	w.tmuxStatusPoller.SetAgentRunStore(w.agentRuns)
	// BOS-667: per-phase stall thresholds come from settings.json so an operator
	// with an unusually slow tool step can widen them without a rebuild. Zero /
	// absent values fall back to the built-in defaults inside SetStallThresholds
	// rather than firing instantly, so an untouched settings.json is safe.
	w.tmuxStatusPoller.SetStallThresholds(
		w.settings.StallDetection.AwaitingModelThreshold(),
		w.settings.StallDetection.ExecutingToolThreshold(),
	)
	w.lifecycle.SetQuestionSignals(w.questionSignals)

}
