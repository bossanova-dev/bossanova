// Package main is the entry point for the Claude agent plugin.
// It launches a go-plugin gRPC server that implements AgentRunnerService,
// allowing the bossd daemon to spawn and manage Claude Code subprocesses.
package main

import (
	"os"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"

	sharedplugin "github.com/recurser/bossalib/plugin"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
	"github.com/recurser/bossd-plugin-claude/skilldata"
)

func main() {
	logger := zerolog.New(os.Stderr).With().
		Timestamp().
		Str("plugin", "claude").
		Logger()

	logger.Info().Msg("starting Claude agent plugin")

	if err := ensureSkillsInstalled(); err != nil {
		logger.Warn().Err(err).Msg("failed to update boss skills")
	}

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: sharedplugin.NewHandshakeForPlugin(),
		VersionedPlugins: map[int]goplugin.PluginSet{
			sharedplugin.ProtocolVersion: {
				sharedplugin.PluginTypeAgentRunner: &agentRunnerPlugin{
					logger:     logger,
					runnerOpts: runnerOptsFromEnv(),
				},
			},
		},
		GRPCServer: goplugin.DefaultGRPCServer,
	})
}

// runnerOptsFromEnv translates the bossd daemon's per-plugin settings (which
// arrive as BOSS_PLUGIN_<key> env vars set by plugin/host.go) into RunnerOption
// values. Without this wiring, the daemon-side
// Plugins[claude].Config["dangerously_skip_permissions"] toggle never reached
// the Claude subprocess, which made repair runs exit immediately on the first
// permission prompt and produced 0-byte agent log files.
func runnerOptsFromEnv() []RunnerOption {
	var opts []RunnerOption
	if os.Getenv("BOSS_PLUGIN_dangerously_skip_permissions") == "true" {
		opts = append(opts, WithDangerouslySkipPermissions(true))
	}
	return opts
}

// ensureSkillsInstalled re-extracts the embedded boss skills into
// ~/.claude/skills/* if they were previously installed there. No-op if
// the user never installed boss skills via the CLI.
func ensureSkillsInstalled() error {
	skillsDir, err := libskillinstall.DefaultDir()
	if err != nil {
		return err
	}
	if !libskillinstall.IsInstalled(skillsDir) {
		return nil
	}
	return libskillinstall.Extract(skillsDir, skilldata.SkillsFS)
}
