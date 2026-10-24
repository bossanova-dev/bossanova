package main

import (
	"context"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/session"
)

func (w *daemonWiring) wireLifecycle() {
	w.lifecycle = session.NewLifecycle(w.sessions, w.repos, w.agentChats, w.cronJobs, w.worktrees, w.agentRunner, w.tmuxClient, w.ghProvider, log.Logger)
	w.lifecycle.SetTelemetry(w.telemetryClient)
	// The lifecycle constructor defaults to a hermetic no-op proof env resolver
	// (keeps unit tests off the OS keyring); the daemon must inject the real
	// keyring-backed resolver so proof credentials reach managed session spawns.
	w.lifecycle.SetProofEnvResolver(w.proofEnv)
	w.lifecycle.SetAgentAccountEnvResolver(w.accountSpawnEnv)
	// Drop draft-PR in-flight markers left by a daemon that died mid-create
	// (BOS-540). The marker means "a goroutine in THIS process is opening a PR
	// right now", so any that exist before this process has started a single
	// session are provably stale; leaving them would have the session advertise
	// a PR forever. Must run here, before anything can start a session — never
	// on the periodic path, which would clear live markers. Fail-soft: a store
	// error is a cosmetic loss, not a reason to refuse to boot.
	func() {
		sweepCtx, sweepCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer sweepCancel()
		if stale, err := w.lifecycle.ClearStaleDraftPRInFlightReasons(sweepCtx); err != nil {
			log.Warn().Err(err).Msg("failed to sweep stale draft PR in-flight markers")
		} else if len(stale) > 0 {
			log.Info().Strs("sessions", stale).Msg("cleared stale draft PR in-flight markers left by a previous daemon")
		}
	}()
	w.lifecycle.SetSessionDeletedNotifier(func(_ context.Context, sessionID string) {
		w.streamBus.Publish(upstream.StreamEvent{
			Session: &upstream.SessionEvent{
				Kind:    bossanovav1.SessionDelta_KIND_DELETED,
				Session: &bossanovav1.Session{Id: sessionID},
			},
		})
	})
	w.lifecycle.SetDisplayTracker(w.displayTracker)
	if len(w.agentClients) > 0 {
		w.lifecycle.SetAgents(w.agentClients)
	}
	w.lifecycle.SetBranchResolver(w.worktrees)
	// Mirror HostServiceServer.SetAgentLogsDir so Lifecycle.StartTmuxChat
	// can pass a deterministic log path to BuildInteractiveCommand. Without
	// this, the extracted method would fail-closed with FailedPrecondition.
	w.lifecycle.SetAgentLogsDir(w.agentLogsDir)
	// A bossd that crashed mid-start can leave a first-input file behind.
	if n := w.lifecycle.SweepStaleInitialInputFiles(time.Now()); n > 0 {
		log.Info().Int("removed", n).Msg("removed stale initial input files")
	}
	w.lifecycle.SetAgentRunStore(w.agentRuns)
	w.lifecycle.SetChatStatus(w.chatStatusTracker)
	w.lifecycle.SetChatRunningProbe(w.chatStatusTracker.Running)
	// An auth-failed (login-required) chat looks idle-and-finished to the
	// stranded-cron sweep and the completion gate; this probe lets them block it
	// rather than finalize a PR for a build that died on auth (BOS-1530).
	w.lifecycle.SetChatAuthFailedProbe(w.chatStatusTracker.AuthFailed)
	// Manual account switch (BOS-171): the registry validates the target
	// account, the transcript probe drives resume-vs-fresh via the agent
	// plugins, and the mid-turn reader adapts the server-held status.Tracker so
	// a WORKING chat is not interrupted without --force. The session→account
	// binding defaults from the lifecycle's own session store.
	w.chatWorking = func(agentSessionID string) bool {
		e := w.chatStatusTracker.Get(agentSessionID)
		return e != nil && e.Status == bossanovav1.ChatStatus_CHAT_STATUS_WORKING
	}
	w.lifecycle.SetAgentAccountSwitchDeps(w.accounts, w.agentClients, w.chatWorking)

}
