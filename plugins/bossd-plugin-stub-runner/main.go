// Package main is the entry point for the stub agent-runner plugin.
// It provides a deterministic AgentRunnerService implementation that starts
// sessions without launching any real agent subprocess. Intended for E2E tests
// that need a real bossd daemon without a real coding agent.
package main

import (
	"os"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"

	sharedplugin "github.com/recurser/bossalib/plugin"
)

func main() {
	logger := zerolog.New(os.Stderr).With().
		Timestamp().
		Str("plugin", "stub-runner").
		Logger()

	logger.Info().Msg("starting stub agent-runner plugin")

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: sharedplugin.NewHandshakeForPlugin(),
		VersionedPlugins: map[int]goplugin.PluginSet{
			sharedplugin.ProtocolVersion: {
				sharedplugin.PluginTypeAgentRunner: &agentRunnerPlugin{
					logger: logger,
				},
			},
		},
		GRPCServer: goplugin.DefaultGRPCServer,
	})
}
