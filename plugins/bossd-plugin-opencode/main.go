// Package main is the entry point for the OpenCode agent plugin.
// It launches a go-plugin gRPC server that implements AgentRunnerService,
// allowing the bossd daemon to spawn and manage opencode CLI subprocesses.
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
		Str("plugin", "opencode").
		Logger()

	logger.Info().Msg("starting OpenCode agent plugin")

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: sharedplugin.NewHandshakeForPlugin(),
		VersionedPlugins: map[int]goplugin.PluginSet{
			sharedplugin.ProtocolVersion: {
				sharedplugin.PluginTypeAgentRunner: &agentRunnerPlugin{
					logger:     logger,
					runnerOpts: resolveRunnerOpts(),
				},
			},
		},
		GRPCServer: goplugin.DefaultGRPCServer,
	})
}

// runnerOptsFromEnv translates the bossd daemon's per-plugin settings (which
// arrive as BOSS_PLUGIN_<key> env vars set by plugin/host.go) into Option
// values. bossd's plugin host writes the daemon-side Plugins[opencode].Config
// map into the subprocess environment, and each plugin reads what it cares
// about here.
//
// Reads the model selection, the optional login-shell wrapper, and the
// permission escape hatch. The headless run path always passes a permission flag
// (default --auto); BOSS_PLUGIN_dangerously_skip_permissions=true swaps in the
// undocumented --dangerously-skip-permissions instead (see
// WithDangerouslySkipPermissions). The codex-specific sandbox/approval/bypass
// toggles are intentionally not consumed.
func runnerOptsFromEnv() []Option {
	var opts []Option
	if model := os.Getenv("BOSS_PLUGIN_model"); model != "" {
		opts = append(opts, WithModel(model))
	}
	if shell := os.Getenv("BOSS_PLUGIN_login_shell"); shell != "" {
		opts = append(opts, WithLoginShell(shell))
	}
	if os.Getenv("BOSS_PLUGIN_dangerously_skip_permissions") == "true" {
		opts = append(opts, WithDangerouslySkipPermissions(true))
	}
	return opts
}

// resolveRunnerOpts is the production Option set: the deterministic env-derived
// options plus a best-effort one-shot `opencode --version` probe so buildArgv can
// pick a permission flag the installed binary actually accepts (--auto only
// exists on opencode >= 1.18; older/unknown falls back to
// --dangerously-skip-permissions). Kept separate from runnerOptsFromEnv so the
// env translation stays deterministic and unit-testable without the binary.
func resolveRunnerOpts() []Option {
	opts := runnerOptsFromEnv()
	if version := probeOpencodeVersion(os.Getenv("BOSS_PLUGIN_login_shell")); version != "" {
		opts = append(opts, WithCLIVersion(version))
	}
	return opts
}
