// Package main is the entry point for the Sentry task source plugin.
// It launches a go-plugin gRPC server that implements TaskSourceService,
// allowing the bossd daemon to fetch unresolved Sentry issues and seed
// sessions with them. It mirrors bossd-plugin-linear.
package main

import (
	"os"

	"github.com/rs/zerolog"

	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
)

func main() {
	logger := zerolog.New(os.Stderr).With().
		Timestamp().
		Str("plugin", "sentry").
		Logger()
	defer sharedplugin.InitErrorTracking(logger, "sentry")()

	logger.Info().Msg("starting Sentry task source plugin")

	sharedplugin.ServePlugin(logger, sharedplugin.PluginTypeTaskSource, newTaskSourcePlugin(logger))
}
