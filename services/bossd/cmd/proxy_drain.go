package main

import (
	"context"
	"errors"
	"time"

	"github.com/recurser/bossd/internal/server"
	"github.com/rs/zerolog"
)

// quietDrainThreshold is how long a drain may take and still be treated as the
// silent idle case. Anything slower waited on something real and is reported at
// info, even if the in-flight counter read zero when the drain began.
const quietDrainThreshold = time.Second

// proxyDrainer is the slice of *server.ProxyServer the shutdown sequence needs.
// Narrowing it to one method is what lets the ordering below be pinned by a test
// over fakes rather than by standing up a real daemon (BOS-888).
type proxyDrainer interface {
	Shutdown(ctx context.Context) (server.DrainOutcome, error)
}

// pluginStopper is the slice of the plugin host the shutdown sequence needs.
type pluginStopper interface {
	Stop() error
}

// drainProxyThenStopPlugins runs the two shutdown steps whose ORDER is the whole
// point of BOS-888: the loopback failover proxy drains first, the plugin host
// stops second.
//
// Agents reach the model only through the proxy, so the proxy's lifetime is the
// agent's connection lifetime. Stopping plugins first kills the agent processes
// while their model streams are still open, which is what surfaced as
// "Connection lost mid-response" on `boss daemon restart`. Draining first lets
// each in-flight request finish its bytes, so by the time plugins stop there is
// no half-written response left to sever.
//
// What this does NOT do: keep the whole agent turn alive. A turn is many
// /v1/messages requests (each tool round-trip is a new one), and
// http.Server.Shutdown closes the listener immediately — so an agent that
// issues its NEXT request during the drain gets connection-refused rather than
// a torn stream. That is a strictly better failure than a mid-response cut, and
// covering it is the recovery/resume work, not this.
//
// Both arguments are optional; callers pass a nil interface (never a typed nil)
// when there is nothing to act on. For the proxy that is NOT "managed accounts
// are off": run() constructs and binds the proxy unconditionally and gates only
// the injection of its address into sessions, so proxy is nil exactly when
// server.NewProxyServer failed or its Listen failed — the plan's non-fatal
// liveness gate. A drain is skipped because there is no listener, never because
// the feature is disabled.
func drainProxyThenStopPlugins(logger zerolog.Logger, proxy proxyDrainer, plugins pluginStopper, drainTimeout time.Duration) {
	if proxy != nil {
		drainFailoverProxy(logger, proxy, drainTimeout)
	}
	if plugins != nil {
		if err := plugins.Stop(); err != nil {
			logger.Warn().Err(err).Msg("plugin host stop error")
		}
	}
}

// drainFailoverProxy shuts the proxy down on its OWN budget and logs which of
// the two outcomes happened — a completed drain or an expired deadline — with
// the in-flight stream count on both sides of the wait.
//
// The budget is deliberately separate from the 5s context the gRPC and hook
// servers share: an ordinary Claude turn runs for minutes, so 5s would cut
// exactly the streams this drain exists to protect. It costs nothing when idle,
// because http.Server.Shutdown returns as soon as connections are idle, so a
// restart with no agent mid-turn is not measurably slower.
func drainFailoverProxy(logger zerolog.Logger, proxy proxyDrainer, drainTimeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	outcome, err := proxy.Shutdown(ctx)

	// An idle restart is the common case and says nothing worth a line at info.
	// The elapsed gate is load-bearing alongside the count: a drain that took
	// real time did real waiting and must not be reported as "nothing in
	// flight", whatever the counter happened to read at the instant it started.
	if err == nil && outcome.InFlightAtStart == 0 && outcome.Elapsed < quietDrainThreshold {
		logger.Debug().
			Dur("elapsed", outcome.Elapsed).
			Msg("failover proxy: shut down with no streams in flight")
		return
	}

	event := logger.Info()
	msg := "failover proxy: drained in-flight agent streams before stopping plugins"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		event = logger.Warn().Err(err)
		msg = "failover proxy: drain deadline expired; in-flight agent streams were cut"
	case err != nil:
		// http.Server.Shutdown also surfaces listener-close errors with the
		// drain fully complete. Reporting those as a severed stream would be
		// untrue, so they get their own line.
		event = logger.Warn().Err(err)
		msg = "failover proxy: drained, but shutting the listener down errored"
	}
	event.
		Bool("drained", outcome.Drained).
		Int("in_flight_at_start", outcome.InFlightAtStart).
		Int("in_flight_at_end", outcome.InFlightAtEnd).
		Dur("elapsed", outcome.Elapsed).
		Dur("budget", drainTimeout).
		Msg(msg)
}
