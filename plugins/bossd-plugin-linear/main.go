// Package main is the entry point for the Linear task source plugin.
// It launches a go-plugin gRPC server that implements TaskSourceService,
// allowing the bossd daemon to fetch Linear issues and match them to PRs.
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
		Str("plugin", "linear").
		Logger()

	logger.Info().Msg("starting Linear task source plugin")

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
