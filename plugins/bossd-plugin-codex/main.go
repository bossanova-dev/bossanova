// Package main is the entry point for the Codex agent plugin.
// It launches a go-plugin gRPC server that implements AgentRunnerService,
// allowing the bossd daemon to spawn and manage codex CLI subprocesses.
package main

import (
	"os"

	"github.com/rs/zerolog"

	sharedplugin "github.com/recurser/bossalib/plugin"
)

func main() {
	logger := zerolog.New(os.Stderr).With().
		Timestamp().
		Str("plugin", "codex").
		Logger()

	logger.Info().Msg("starting Codex agent plugin")

	sharedplugin.ServePlugin(logger, sharedplugin.PluginTypeAgentRunner, &agentRunnerPlugin{
		logger:     logger,
		runnerOpts: runnerOptsFromEnv(),
	})
}

// runnerOptsFromEnv translates the bossd daemon's per-plugin settings (which
// arrive as BOSS_PLUGIN_<key> env vars set by plugin/host.go) into Option
// values. Mirrors the claude plugin's wiring: bossd's plugin host writes the
// daemon-side Plugins[codex].Config map into the subprocess environment, and
// each plugin reads what it cares about here.
func runnerOptsFromEnv() []Option {
	var opts []Option
	if mode := os.Getenv("BOSS_PLUGIN_sandbox"); mode != "" {
		opts = append(opts, WithSandbox(mode))
	}
	if pol := os.Getenv("BOSS_PLUGIN_approval"); pol != "" {
		opts = append(opts, WithApproval(pol))
	}
	if model := os.Getenv("BOSS_PLUGIN_model"); model != "" {
		opts = append(opts, WithModel(model))
	}
	if effort := os.Getenv("BOSS_PLUGIN_effort"); effort != "" {
		opts = append(opts, WithEffort(effort))
	}
	if shell := os.Getenv("BOSS_PLUGIN_login_shell"); shell != "" {
		opts = append(opts, WithLoginShell(shell))
	}
	if os.Getenv("BOSS_PLUGIN_dangerously_bypass_approvals_and_sandbox") == "true" {
		opts = append(opts, WithDangerouslyBypassApprovalsAndSandbox(true))
	}
	return opts
}
