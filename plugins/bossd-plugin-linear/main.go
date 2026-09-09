// Package main is the entry point for the Linear task source plugin.
// It launches a go-plugin gRPC server that implements TaskSourceService,
// allowing the bossd daemon to fetch Linear issues and match them to PRs.
package main

import (
	"os"

	"github.com/rs/zerolog"

	sharedplugin "github.com/recurser/bossalib/plugin"
)

func main() {
	logger := zerolog.New(os.Stderr).With().
		Timestamp().
		Str("plugin", "linear").
		Logger()

	logger.Info().Msg("starting Linear task source plugin")

	sharedplugin.ServePlugin(logger, sharedplugin.PluginTypeTaskSource, &taskSourcePlugin{logger: logger})
}
