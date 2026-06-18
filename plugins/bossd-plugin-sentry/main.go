// Package main is the entry point for the Sentry task source plugin.
// It launches a go-plugin gRPC server that implements TaskSourceService,
// allowing the bossd daemon to fetch unresolved Sentry issues and seed
// sessions with them. It mirrors bossd-plugin-linear.
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
		Str("plugin", "sentry").
		Logger()

	logger.Info().Msg("starting Sentry task source plugin")

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: sharedplugin.NewHandshakeForPlugin(),
		VersionedPlugins: map[int]goplugin.PluginSet{
			sharedplugin.ProtocolVersion: {
				sharedplugin.PluginTypeTaskSource: &taskSourcePlugin{logger: logger},
			},
		},
		GRPCServer: goplugin.DefaultGRPCServer,
	})
}
