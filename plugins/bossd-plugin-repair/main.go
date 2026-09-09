// Package main is the entry point for the repair workflow plugin.
// It launches a go-plugin gRPC server that implements WorkflowService,
// allowing the bossd daemon to automatically repair red-status PRs.
package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	sharedplugin "github.com/recurser/bossalib/plugin"
)

// shutdownTimeout caps how long we wait for in-flight repair goroutines to
// drain when the plugin receives SIGTERM. The host's Kill path gives plugins
// ~2s before SIGKILL, so keep this comfortably under that.
const shutdownTimeout = 1500 * time.Millisecond

func main() {
	logger := zerolog.New(os.Stderr).With().
		Timestamp().
		Str("plugin", "repair").
		Logger()

	logger.Info().Msg("starting repair workflow plugin")

	plugin := &repairPlugin{logger: logger}

	// SIGTERM = graceful shutdown request (from the host's Kill path OR any
	// external signal — e.g. the 2026-07-11 incident's machine-wide plugin
	// kill). go-plugin's server eats SIGINT internally but leaves SIGTERM
	// alone. We drain in-flight repairs (self-bounded by shutdownTimeout)
	// and then EXIT: staying alive after Shutdown leaves the workflow
	// permanently CANCELLED while the process still answers Ping, which the
	// host health loop reads as "healthy" — the zombie state behind BOS-346.
	// Exiting instead turns any SIGTERM into an ordinary plugin restart,
	// after which the host re-arms the workflow from its desired state.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	go handleSigterm(sigCh, logger, plugin.Shutdown, os.Exit)

	// No drain on the parent-death path, unlike handleSigterm above: by the
	// time the watchdog fires the host bossd is already gone, so there is
	// nothing left to report a drained workflow to and the host rebuilds
	// desired state from its own store on restart. Draining anyway would only
	// hold an already-orphaned process open for shutdownTimeout.
	sharedplugin.ServePlugin(logger, sharedplugin.PluginTypeWorkflow, plugin)
}

// handleSigterm blocks for one SIGTERM, drains in-flight repair goroutines
// (bounded inside shutdown by its timeout argument), then exits the process
// so the host health loop restarts it and re-arms the workflow. A second
// SIGTERM during the drain is simply absorbed — the process is already on
// its way out within shutdownTimeout. Exit code 0: the host restart path
// keys off process exit, not the code.
func handleSigterm(sigCh <-chan os.Signal, logger zerolog.Logger, shutdown func(time.Duration), exit func(int)) {
	<-sigCh
	logger.Info().Msg("received SIGTERM, draining repair goroutines")
	shutdown(shutdownTimeout)
	logger.Info().Msg("drained; exiting so the host restarts the plugin and re-arms the workflow")
	exit(0)
}
