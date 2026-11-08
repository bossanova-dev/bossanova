package main

import (
	"context"
	"time"

	"github.com/recurser/bossd/internal/session"
	"github.com/rs/zerolog/log"
)

func (w *daemonWiring) runWorktreeReconcile() {
	reconcile := w.opts.startupWorktreeReconcile
	if reconcile == nil {
		reconcile = w.lifecycle.ReconcileUnrecordedWorktrees
	}
	runDailyWorktreeReconcile(w.pollerCtx, reconcile, time.NewTicker(24*time.Hour))
}

func runDailyWorktreeReconcile(ctx context.Context, reconcile func(context.Context) (session.UnrecordedWorktreeReport, error), ticker *time.Ticker) {
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := reconcile(ctx); err != nil {
			log.Warn().Err(err).Msg("unrecorded worktree reconcile failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
