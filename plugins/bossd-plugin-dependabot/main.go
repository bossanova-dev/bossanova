// Package main is the entry point for the dependabot task source plugin.
// It launches a go-plugin gRPC server that implements TaskSourceService,
// allowing the bossd daemon to poll for dependabot PRs and classify them.
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
		Str("plugin", "dependabot").
		Logger()

	logger.Info().Msg("starting dependabot task source plugin")

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: goplugin.HandshakeConfig{
			ProtocolVersion:  sharedplugin.ProtocolVersion,
			MagicCookieKey:   sharedplugin.MagicCookieKey,
			MagicCookieValue: sharedplugin.MagicCookieValue,
		},
		Plugins: goplugin.PluginSet{
			sharedplugin.PluginTypeTaskSource: &taskSourcePlugin{logger: logger},
		},
		GRPCServer: goplugin.DefaultGRPCServer,
	})
}
