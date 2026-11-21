// Package main is the entry point for the Antigravity agent plugin. It serves
// AgentRunnerService over go-plugin gRPC so bossd can drive Google's `agy` CLI.
package main

import (
	"os"

	"github.com/rs/zerolog"

	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
)

func main() {
	logger := zerolog.New(os.Stderr).With().
		Timestamp().
		Str("plugin", pluginName).
		Logger()
	defer sharedplugin.InitErrorTracking(logger, "antigravity")()

	logger.Info().Msg("starting Antigravity agent plugin")

	sharedplugin.ServePlugin(logger, sharedplugin.PluginTypeAgentRunner, newAgentRunnerPlugin(logger, runnerOptsFromEnv(logger)...))
}

// runnerOptsFromEnv reads the per-plugin settings bossd's plugin host exports
// as BOSS_PLUGIN_<key>.
func runnerOptsFromEnv(logger zerolog.Logger) []Option {
	var opts []Option
	if os.Getenv("BOSS_PLUGIN_dangerously_skip_permissions") == "true" {
		opts = append(opts, WithDangerouslySkipPermissions(true))
	}
	if mode := os.Getenv("BOSS_PLUGIN_mode"); mode != "" {
		if validMode(mode) {
			opts = append(opts, WithMode(mode))
		} else {
			logger.Warn().Str("mode", mode).Msg("ignoring unknown antigravity mode setting")
		}
	}
	if model := os.Getenv("BOSS_PLUGIN_model"); model != "" {
		opts = append(opts, WithModel(model))
	}
	if shell := os.Getenv("BOSS_PLUGIN_login_shell"); shell != "" {
		opts = append(opts, WithLoginShell(shell))
	}
	return opts
}
