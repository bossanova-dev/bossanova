package main

import (
	"context"
	"fmt"
	"net/http"

	libtelemetry "github.com/recurser/bossalib/kit/telemetry"
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/kit/safego"
)

func (w *daemonWiring) wireServing() error {
	var (
		streamCtx context.Context
	)
	// Bind the socket and initialize the http.Server synchronously so
	// Shutdown below cannot race with the serving goroutine's write to
	// the internal server field.
	if err := w.srv.Listen(w.socketPath); err != nil {
		return fmt.Errorf("server listen: %w", err)
	}

	// Start serving in a goroutine.
	w.errCh = make(chan error, 1)
	w.trackedGo("grpc-server", func() {
		log.Info().Str("socket", w.socketPath).Msg("starting server")
		if w.opts.onServeStart != nil {
			w.opts.onServeStart()
		}
		w.errCh <- w.srv.Serve()
	})
	w.trackedGo("hook-server", func() {
		if err := w.hookSrv.Serve(); err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Msg("hook server exited unexpectedly")
		}
	})
	if w.proxySrv != nil {
		w.trackedGo("failover-proxy-server", func() {
			if err := w.proxySrv.Serve(); err != nil && err != http.ErrServerClosed {
				log.Error().Err(err).Msg("failover proxy exited unexpectedly")
			}
		})
	}

	// Start the upstream StreamClient (no-op in local-only mode).
	// streamCtx is separate from pollerCtx so we can stop the stream
	// before the plugin host is torn down, letting orchestrator commands
	// that ride on it drain cleanly.
	streamCtx, w.streamCancel = context.WithCancel(context.Background())
	w.deferCleanup(func() { w.streamCancel() })
	if w.streamClient != nil {
		w.trackedGo("reverse-stream-client", func() {
			w.streamClient.Run(streamCtx)
		})
	}
	if w.snapshotPublisher != nil {
		w.trackedGo("snapshot-publisher", func() {
			w.snapshotPublisher(streamCtx)
		})
	}
	// The note sync worker rides streamCtx like the snapshot publisher, so it
	// stops with the upstream feeds and shutdownWG joins it.
	if w.noteSyncWorker != nil {
		w.trackedGo("note-sync-worker", func() {
			w.noteSyncWorker.Run(streamCtx)
		})
	}
	// Run the TerminalStream client alongside the DaemonStream client.
	// Each owns its own connect/reconnect loop so a transient bosso
	// outage on one bidi can't bring the other down — both sit in their
	// own backoff. Cancellation via streamCtx returns nil from Run on
	// graceful shutdown; a non-nil return on streamCtx-still-live is a
	// fatal opener misconfiguration (e.g. nil opener) and is logged
	// rather than restarted.
	if w.terminalStreamClient != nil {
		w.trackedGo("terminal-stream-client", func() {
			if err := w.terminalStreamClient.Run(streamCtx); err != nil && streamCtx.Err() == nil {
				log.Error().Err(err).Msg("terminal stream client exited unexpectedly")
			}
		})
	}

	// --- Ready hook (tests) ---

	w.telemetryClient.Capture(context.Background(), libtelemetry.EventDaemonStarted, daemonDistinctID(), nil)

	if w.opts.onReady != nil {
		safego.Go(log.Logger, w.opts.onReady)
	}

	return nil
}
