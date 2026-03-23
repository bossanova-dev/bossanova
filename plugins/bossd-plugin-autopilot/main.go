// Package main is the entry point for the autopilot workflow plugin.
// It launches a go-plugin gRPC server that implements WorkflowService,
// allowing the bossd daemon to orchestrate end-to-end plan execution.
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
		Str("plugin", "autopilot").
		Logger()

	logger.Info().Msg("starting autopilot workflow plugin")

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: goplugin.HandshakeConfig{
			ProtocolVersion:  sharedplugin.ProtocolVersion,
			MagicCookieKey:   sharedplugin.MagicCookieKey,
			MagicCookieValue: sharedplugin.MagicCookieValue,
		},
		Plugins: goplugin.PluginSet{
			sharedplugin.PluginTypeWorkflow: &workflowPlugin{logger: logger},
		},
		GRPCServer: goplugin.DefaultGRPCServer,
	})
}
