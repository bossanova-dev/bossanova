package main

import (
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/config"
)

// pluginPipelineDeps holds the three steps finalizePluginConfigs runs. They are
// injectable ONLY so their ORDER can be asserted: the order is load-bearing and
// the daemon's boot function is far too long and side-effect-heavy for a test to
// drive, so without this seam the sequence could only be checked by a human
// re-reading main.go — exactly the check that rots.
type pluginPipelineDeps struct {
	dedup  func(cfgs []config.PluginConfig, settings *config.Settings, explicitOverride bool) []config.PluginConfig
	gate   func(cfgs []config.PluginConfig, settings *config.Settings, explicitOverride bool) []config.PluginConfig
	verify func(cfgs []config.PluginConfig, explicitOverride bool) []config.PluginConfig
}

// defaultPluginPipeline wires the production steps. logRejections is bossd's
// own SECURITY-level rejection logger, passed in because it is a closure over
// run()'s logger.
func defaultPluginPipeline(logRejections func([]config.PluginRejection)) pluginPipelineDeps {
	return pluginPipelineDeps{
		dedup:  dedupPluginConfigs,
		gate:   applyExperimentalPluginGate,
		verify: verifyPluginConfigs(logRejections),
	}
}

// finalizePluginConfigs turns the resolved plugin list into the one the plugin
// host launches: dedup, then the experimental-plugin gate, then verification.
//
// The sequence is the point. Dedup runs first so the gate sees exactly one
// entry per plugin — gating a duplicated entry twice would report the same
// plugin twice and leave the second copy's flag to chance. Verification runs
// last so it vets the list the daemon will actually load rather than one the
// gate is about to change.
func finalizePluginConfigs(cfgs []config.PluginConfig, settings *config.Settings, explicitOverride bool, deps pluginPipelineDeps) []config.PluginConfig {
	cfgs = deps.dedup(cfgs, settings, explicitOverride)
	cfgs = deps.gate(cfgs, settings, explicitOverride)
	return deps.verify(cfgs, explicitOverride)
}

// dedupPluginConfigs self-heals a settings file that accumulated duplicate
// plugin entries — e.g. a user added a plugin the discovery loop also wrote.
// Duplicates would otherwise spawn parallel plugin subprocesses with
// independent in-memory dedup state (see bossd-plugin-repair). Unlike the gate
// this DOES persist, because the cleaned-up list is what the user should keep.
func dedupPluginConfigs(cfgs []config.PluginConfig, settings *config.Settings, explicitOverride bool) []config.PluginConfig {
	deduped, dropped := config.DedupPluginConfigs(cfgs)
	if !dropped {
		return cfgs
	}
	log.Warn().Int("before", len(cfgs)).Int("after", len(deduped)).Msg("removing duplicate plugin entries")
	if !explicitOverride {
		settings.Plugins = deduped
		if err := config.Save(*settings); err != nil {
			log.Warn().Err(err).Msg("failed to persist deduped plugin list to settings")
		}
	}
	return deduped
}

// verifyPluginConfigs re-applies the discovery policy's safety checks to the
// final list. Configured plugins (settings.Plugins, e.g. persisted by `boss
// config init --plugin-dir`, which the official installer runs) are exec'd by
// their stored path: auto-discovery vets the binaries it finds, but these
// explicit entries bypass that scan, so on a release build a plugin binary
// swapped after config init would run without a plugins.sum check. Any binary
// that fails is dropped (fail closed). The explicit --plugins E2E override
// loads unverified test stubs by design, so it is exempt.
func verifyPluginConfigs(logRejections func([]config.PluginRejection)) func([]config.PluginConfig, bool) []config.PluginConfig {
	return func(cfgs []config.PluginConfig, explicitOverride bool) []config.PluginConfig {
		if explicitOverride {
			return cfgs
		}
		verified, rejected := config.VerifyConfiguredPlugins(cfgs)
		logRejections(rejected)
		return verified
	}
}

// experimentalPluginsSettingsKey is the settings.json key that opts into an
// experimental plugin. Named here so the log line tells the user exactly what
// to add rather than making them find it in the docs.
const experimentalPluginsSettingsKey = "experimental_plugins"

// applyExperimentalPluginGate forces every experimental plugin's Enabled flag
// to match the user's `experimental_plugins` opt-in, returning the gated list.
// It is the only place in the daemon that holds both settings.ExperimentalPlugins
// and the resolved plugin slice, which is why the gate lives here: a
// discovery-side default can only affect a fresh install, while this overrides
// the `"enabled": true` a previous daemon already persisted for every existing
// user (BOS-1145).
//
// explicitOverride is true on the `--plugins` E2E path, which loads an explicit
// list of test stubs and deliberately bypasses the filter chain — the same
// carve-out FilterNonDiscoverablePlugins gets. The gate is then a no-op.
//
// It deliberately does NOT persist plugins[].enabled: under the authoritative
// reading the gate ignores that flag for a registry member, so writing the
// forced value would persist a field nothing reads. The one thing it does
// persist is settings.DefaultAgent — see below — which is a different key and a
// real availability fix, not a decorative one.
func applyExperimentalPluginGate(cfgs []config.PluginConfig, settings *config.Settings, explicitOverride bool) []config.PluginConfig {
	if explicitOverride {
		return cfgs
	}

	gated, disabled := config.ApplyExperimentalPluginGate(cfgs, settings.ExperimentalPlugins)
	for _, name := range disabled {
		log.Info().
			Str("plugin", name).
			Str("settings_key", experimentalPluginsSettingsKey).
			Msgf("experimental plugin %q is disabled; add it to %q in settings.json to enable it",
				name, experimentalPluginsSettingsKey)
	}

	// A user who set default_agent while the plugin was auto-enabled does not
	// merely lose a preference: resolveDefaultAgentName prefers a loaded runner
	// only when exactly ONE is loaded, so with claude and codex both present it
	// returns the stale name verbatim and validateExplicitAgentName then rejects
	// it as "agent %q is not loaded" — every session request carrying no explicit
	// agent fails. Clear it and let config.Load's backfill and the
	// single-loaded-runner rule take over.
	//
	// The clear has to reach DISK, not just this struct. Every reader that
	// decides a session's agent re-reads settings.json itself: Server.resolveAgentName
	// (services/bossd/internal/server/agent_resolution.go) calls config.Load on
	// each CreateSession and CreateCronJob, and the orchestrated-session resolver
	// in main.go does the same, falling back to this in-memory value only when
	// that Load errors. config.Load's backfill only fires when the key is EMPTY
	// on disk, so an unsaved clear leaves both resolving the stale name and the
	// availability fix above is inert. Only the in-process dispatcher default
	// would see it.
	for _, c := range gated {
		if c.Enabled || c.Name != settings.DefaultAgent || !config.IsExperimentalPlugin(c.Name) {
			continue
		}
		log.Warn().
			Str("plugin", c.Name).
			Str("settings_key", experimentalPluginsSettingsKey).
			Msgf("clearing default_agent %q: the plugin is not enabled, and leaving it set would fail every session request that carries no explicit agent",
				c.Name)
		settings.DefaultAgent = ""
		// Persisting here writes settings.Plugins as it stands on entry: the gate
		// returns a new slice and never assigns to it, so this is the pre-gate
		// list already on disk. The no-persist decision covers plugins[].enabled
		// and is not reopened by a default_agent write.
		if err := config.Save(*settings); err != nil {
			log.Warn().Err(err).
				Msg("failed to persist the cleared default_agent; session creation re-reads settings.json and will still resolve the gated-off agent")
		}
	}

	return gated
}
