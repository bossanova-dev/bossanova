package main

import (
	"fmt"

	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/contract/daemon/socketauth"
	"github.com/recurser/bossd/api/hooks"
)

func (w *daemonWiring) wireHooks() error {
	// --- Hook Server (loopback HTTP for Claude Stop-hook notifications) ---
	//
	// Created and bound BEFORE lifecycle.Bootstrap: Bootstrap re-issues
	// ConfigureFinalizeHook for surviving tmux chats with HookPort, and the
	// claude plugin's WriteHookConfig rejects port <= 0. Binding the port
	// first ensures the re-arm rewrites the worktree's hook config with this
	// daemon's port instead of leaving the previous (now-dead) port in place.
	hookCfg := hooks.HookServerConfig{
		Sessions:        w.sessions,
		Finalizer:       w.lifecycle,
		QuestionSignals: w.questionSignals,
		PromptReceipts:  w.lifecycle,
		// The turn-ended hook's background-agent count feeds the tmux poller
		// through the same tracker it reports into.
		BackgroundAgents: w.chatStatusTracker,
		Logger:           log.Logger,
	}
	// HostService is non-nil whenever any plugin is configured (it's
	// constructed alongside the plugin host). When no plugins are loaded
	// the completer stays nil-interface and the agent-run-complete
	// endpoint surfaces 500s — that's fine because no plugin is around
	// to register a run in the first place. Wrap the nil-pointer check
	// here so the HookServer can rely on `completer == nil` working.
	// The same HostService also authenticates the question hook's per-run
	// token via ValidateRunToken.
	if hs := w.hostService; hs != nil {
		hookCfg.Completer = hs
		hookCfg.QuestionAuth = hs
	}
	// Per-chat prompt-receipt bearer tokens are derived from the persistent
	// socket token (the same file srv.Listen loads below), so they survive a
	// daemon restart. Set before the hook server serves and before Bootstrap
	// re-launches any chat. Never log the token.
	socketToken, err := socketauth.LoadOrCreateToken(w.socketPath)
	if err != nil {
		return fmt.Errorf("load socket auth token for prompt receipts: %w", err)
	}
	w.lifecycle.SetPromptReceiptKey(socketToken)

	w.hookSrv = hooks.NewHookServer(hookCfg)
	w.hookSrv.SetCronCompletionNotifier(w.cronGate)
	if err := w.hookSrv.Listen(); err != nil {
		return fmt.Errorf("hook server listen: %w", err)
	}
	// Plumb the bound port into the lifecycle so cron-spawned sessions can
	// stamp it into settings.local.json without the lifecycle having to
	// read it back from a file written by the same process.
	w.lifecycle.SetHookPort(w.hookSrv.Port())
	log.Info().Int("port", w.hookSrv.Port()).Msg("hook server listening on 127.0.0.1")
	if w.opts.onHookPortSet != nil {
		w.opts.onHookPortSet()
	}

	return nil
}
