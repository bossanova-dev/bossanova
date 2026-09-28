// Package main is the entry point for the Claude agent plugin.
// It launches a go-plugin gRPC server that implements AgentRunnerService,
// allowing the bossd daemon to spawn and manage Claude Code subprocesses.
package main

import (
	"os"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/buildinfo"
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

	if result, err := ensureSkillsInstalled(); err != nil {
		logger.Warn().Err(err).Msg("failed to update boss skills")
	} else if result.Held {
		logger.Warn().
			Str("installed", result.Decision.Installed.String()).
			Str("payload", pluginSkillPayloadRecord().String()).
			Str("reason", result.Decision.Reason).
			Msg("held boss skill refresh: this plugin's embedded payload is not provably newer than the installed one; run `boss skills install` to replace it explicitly")
	}

	sharedplugin.ServePlugin(logger, sharedplugin.PluginTypeAgentRunner, &agentRunnerPlugin{
		logger:     logger,
		runnerOpts: runnerOptsFromEnv(),
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
	if shell := os.Getenv("BOSS_PLUGIN_login_shell"); shell != "" {
		opts = append(opts, WithLoginShell(shell))
	}
	if model := os.Getenv("BOSS_PLUGIN_model"); model != "" {
		opts = append(opts, WithModel(model))
	}
	if effort := os.Getenv("BOSS_PLUGIN_effort"); effort != "" {
		opts = append(opts, WithEffort(effort))
	}
	return opts
}

// pluginSkillBuildInfo reads this plugin binary's stamped revision and
// version. A seam so tests can stand in for an older or newer plugin.
var pluginSkillBuildInfo = func() (commit, version string) {
	return buildinfo.Commit, buildinfo.Version
}

// pluginSkillPayloadRecord describes the plugin's embedded payload for the
// no-downgrade rule. The plugin writes unattended, at every daemon start.
func pluginSkillPayloadRecord() libskillinstall.PayloadRecord {
	commit, version := pluginSkillBuildInfo()
	return libskillinstall.EmbeddedPayloadRecord(commit, version, libskillinstall.WriterUnattended)
}

// ensureSkillsInstalled refreshes the boss skills under ~/.claude/skills/*
// only when the installed tree differs from the embedded payload and the
// no-downgrade rule allows it. No-op if the user never installed boss skills
// via the CLI, and no-op when the payload already matches — which prevents the
// plugin from clobbering an up-to-date install on every daemon restart and
// ping-ponging with the CLI's startup prompt.
//
// The plugin runs outside any checkout, so its only ordering evidence is a
// release-version comparison: over a tree an explicit install or a checkout
// wrote, a dev or unstamped plugin holds rather than restoring its older embed.
func ensureSkillsInstalled() (libskillinstall.GuardedResult, error) {
	skillsDir, err := libskillinstall.DefaultDir()
	if err != nil {
		return libskillinstall.GuardedResult{}, err
	}
	if !libskillinstall.IsInstalled(skillsDir) {
		return libskillinstall.GuardedResult{}, nil
	}
	return libskillinstall.EnsureUpdatedGuarded(skillsDir, skilldata.SkillsFS, pluginSkillPayloadRecord(), libskillinstall.ReleaseVersionOrder)
}
