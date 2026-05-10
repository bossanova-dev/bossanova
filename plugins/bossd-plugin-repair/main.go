// Package main is the entry point for the repair workflow plugin.
// It launches a go-plugin gRPC server that implements WorkflowService,
// allowing the bossd daemon to automatically repair red-status PRs.
package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
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

	// SIGTERM = host requesting graceful shutdown. go-plugin's server eats
	// SIGINT internally but leaves SIGTERM alone, so this hook lets us
	// drain in-flight repairs before the process is reaped.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info().Msg("received SIGTERM, draining repair goroutines")
		plugin.Shutdown(shutdownTimeout)
	}()

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: sharedplugin.NewHandshakeForPlugin(),
		VersionedPlugins: map[int]goplugin.PluginSet{
			sharedplugin.ProtocolVersion: {
				sharedplugin.PluginTypeWorkflow: plugin,
			},
		},
		GRPCServer: goplugin.DefaultGRPCServer,
	})
}
