package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	plugindoor "github.com/recurser/bossd/api/plugin"
	"github.com/recurser/bossd/internal/adapter/dotenv"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/contract/daemon/daemonbin"
	"github.com/recurser/bossalib/daemonstate"
	"github.com/rs/zerolog/log"

	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/internal/adapter/proofenvkeyring"
	"github.com/recurser/bossd/internal/adapter/tccprobe"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/eventbus"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
)

func (w *daemonWiring) wirePlugins() error {
	// --- Plugin Host ---

	w.pluginBus = eventbus.New(log.Logger)
	w.proofEnv = proofenvkeyring.New(log.Logger)
	w.hostService = plugindoor.NewHostServiceServer(plugindoor.Config{
		Provider: w.ghProvider, Repos: w.repos, Sessions: w.sessions, Chats: w.agentChats,
		DisplayTracker: w.displayTracker, ChatTracker: w.chatStatusTracker, AgentRuns: w.agentRuns,
		RepairLease: w.repairLease, PanePIDResolver: w.tmuxClient.PanePID, ProofEnv: w.proofEnv,
		EnvOverlay: dotenv.OverlayWithRepo,
	})
	w.pluginHost = plugin.New(w.pluginBus, w.hostService, log.Logger)
	if w.pluginErrorTrackingDSN != "" {
		w.pluginHost.SetErrorTracking(w.pluginErrorTrackingDSN, w.bossEnv)
	}
	// The tracker linker links active, unlinked sessions to the tracker issue
	// their PR title, branch or title names, confirmed against the repo's own
	// tracker. It runs on the periodic reconcile tick below, never at startup:
	// the startup reconcile runs before plugins load.
	w.trackerLinker = session.NewTrackerLinker(w.sessions, w.repos,
		server.NewTrackerResolver(w.pluginHost),
		w.prAssociationResolver.OpenPRs,
		w.publishStoreSessionUpdate,
		log.Logger)

	// Register DisplayTracker onChange callback to notify plugins of status changes
	w.displayTracker.SetOnChange(func(sessionID string, oldEntry, newEntry *status.DisplayEntry) {
		if newEntry != nil {
			w.pluginHost.NotifyStatusChange(context.Background(), sessionID, newEntry.Status, newEntry.HasFailures)
		}
	})

	pluginCfgs := w.settings.Plugins
	logPluginRejections := func(rejected []config.PluginRejection) {
		for _, r := range rejected {
			log.Error().Str("plugin", r.Name).Str("path", r.Path).Str("reason", r.Reason).
				Msg("SECURITY: refusing to load unverified plugin binary before exec")
		}
	}
	if w.opts.plugins != nil {
		pluginCfgs = w.opts.plugins
	} else {
		// Drop any non-discoverable plugin (e.g. the E2E-only stub-runner) that an
		// older daemon persisted before the binary was excluded from discovery.
		// The explicit --plugins path above intentionally bypasses this so the
		// E2E harness can still load the stub.
		if filtered, dropped := config.FilterNonDiscoverablePlugins(pluginCfgs); len(dropped) > 0 {
			log.Info().Strs("dropped", dropped).Msg("removing non-discoverable plugins from config")
			pluginCfgs = filtered
			w.settings.Plugins = filtered
			if err := config.Save(w.settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist filtered plugin list to settings")
			} else {
				log.Info().Msg("persisted filtered plugin list to settings")
			}
		}
	}
	if w.opts.plugins == nil && len(pluginCfgs) == 0 {
		var rejected []config.PluginRejection
		pluginCfgs, rejected = config.DiscoverPluginsVerified()
		logPluginRejections(rejected)
		if len(pluginCfgs) > 0 {
			log.Info().Int("count", len(pluginCfgs)).Msg("auto-discovered plugins")
			w.settings.Plugins = pluginCfgs
			if err := config.Save(w.settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist discovered plugins to settings")
			} else {
				log.Info().Msg("persisted discovered plugins to settings")
			}
		}
	} else if w.opts.plugins == nil {
		// Reconcile the persisted plugin list against a fresh discovery scan so two
		// drift modes self-heal without a hand-edit: (1) a configured entry whose
		// stored path no longer resolves — e.g. a Homebrew upgrade moved the
		// binaries — is repaired to the discovered binary (HealPluginPaths), rather
		// than being rejected and leaving the daemon with zero agents; (2) a
		// freshly-built plugin not yet in settings.json is appended
		// (MergeDiscoveredPlugins). Both preserve existing entries' enabled/config.
		discovered, rejected := config.DiscoverPluginsVerified()
		logPluginRejections(rejected)
		if healed, names := config.HealPluginPaths(pluginCfgs, discovered); len(names) > 0 {
			log.Warn().Strs("healed", names).
				Msg("repaired plugin paths that no longer resolve (a package upgrade may have moved the binaries)")
			pluginCfgs = healed
			w.settings.Plugins = healed
			if err := config.Save(w.settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist healed plugin list to settings")
			} else {
				log.Info().Msg("persisted healed plugin list to settings")
			}
		}
		if merged, added := config.MergeDiscoveredPlugins(pluginCfgs, discovered); len(added) > 0 {
			log.Info().Strs("added", added).Msg("merged newly-discovered plugins into config")
			pluginCfgs = merged
			w.settings.Plugins = merged
			if err := config.Save(w.settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist merged plugin list to settings")
			} else {
				log.Info().Msg("persisted merged plugin list to settings")
			}
		}
	}

	// Dedup, gate, and verify the resolved list, in that order. The three steps
	// are sequenced inside finalizePluginConfigs rather than inline here because
	// their order is load-bearing and this function is far too long and
	// boot-heavy to assert an order within: dedup must run first so the gate sees
	// one entry per plugin, and verification must run last so it vets the list
	// the daemon will actually load.
	pluginCfgs = finalizePluginConfigs(pluginCfgs, &w.settings, w.opts.plugins != nil,
		defaultPluginPipeline(logPluginRejections))

	if runtime.GOOS == "darwin" {
		workingPaths := []string{w.settings.WorktreeBaseDir}
		registeredRepos, err := w.repos.List(context.Background())
		if err != nil {
			log.Warn().Err(err).Msg("startup diagnostics could not list repositories; checking the worktree base only")
		} else {
			for _, repo := range registeredRepos {
				workingPaths = append(workingPaths, repo.LocalPath, repo.WorktreeBaseDir)
			}
		}

		home, err := os.UserHomeDir()
		if err != nil {
			log.Warn().Err(err).Msg("startup diagnostics could not resolve the user home; skipping protected-folder checks")
		} else {
			protectedRoots, resolutionDiagnostics := protectedRootsForResolvedWithTracker(home, workingPaths, protectedRootResolutionTimeout, filepath.EvalSymlinks, func(done <-chan struct{}) {
				w.startupDiagnosticWorkerDone = append(w.startupDiagnosticWorkerDone, done)
			})
			w.startupProtectedRoots = protectedRoots
			probeResults := tccprobe.ProbeWithTracker(context.Background(), protectedRoots, tccprobe.DefaultTimeout, func(done <-chan struct{}) {
				w.startupDiagnosticWorkerDone = append(w.startupDiagnosticWorkerDone, done)
			})
			persistTCCProbeResults(log.Logger, w.appDataDir, w.daemonMetadata, append(probeResults, resolutionDiagnostics...), daemonstate.Write)
			for _, result := range resolutionDiagnostics {
				message := "startup protected-folder diagnostic: symlink resolution blocked before probe"
				switch result.Status {
				case tccprobe.StatusOK, tccprobe.StatusAbsent:
					continue
				case tccprobe.StatusBlocked:
					// Keep the default blocked-resolution diagnostic.
				case tccprobe.StatusDenied:
					message = "startup protected-folder diagnostic: symlink resolution denied before probe"
				case tccprobe.StatusError:
					message = "startup protected-folder diagnostic: symlink resolution failed before probe"
				}
				log.Error().Err(result.Err).
					Str("candidate", result.Path).
					Msg(message)
			}
			logProtectedRootProbeResults(log.Logger, probeResults, w.executablePath)
		}

		stagedAppDataDir, err := config.DefaultAppDataDir()
		if err != nil {
			log.Warn().Err(err).Msg("startup diagnostics could not resolve the stable bossd staging directory")
		} else if sourcePath, err := exec.LookPath(daemonbin.BossdName); err == nil {
			stagedPath := daemonbin.StagedPath(stagedAppDataDir)
			stale, compareErr := stagedBinaryStale(w.executablePath, stagedPath, sourcePath)
			if compareErr != nil {
				log.Warn().Err(compareErr).Str("staged", stagedPath).Msg("startup diagnostics could not compare the staged bossd with the binary on PATH")
			} else if stale {
				log.Warn().
					Str("running", w.executablePath).
					Str("source", sourcePath).
					Msg("staged bossd is stale; run 'boss daemon restart' to pick up the newer binary on PATH")
			}
		}
	}

	if err := w.pluginHost.Start(context.Background(), pluginCfgs, w.settings); err != nil {
		w.pluginBus.Close()
		return fmt.Errorf("plugin host: %w", err)
	}

	loadedAgents := w.pluginHost.AgentRunners()
	w.agentClients = map[string]agent.RunnerClient{}
	pluginRunners := map[string]agent.Runner{}

	if len(loadedAgents) == 0 {
		// No agent plugin loaded: daemon stays healthy but session creation
		// will fail. Operators install bossd-plugin-claude (or another
		// AgentRunner plugin) and restart.
		log.Warn().Msg("no AgentRunner plugin loaded; sessions cannot be started until an agent plugin is installed")
		w.agentRunner = agent.NoopRunner{}
	} else {
		// Build per-agent registries: agent.RunnerClient (for
		// ConfigureFinalizeHook etc.) and agent.Runner (for the
		// Dispatcher's per-session routing). The dispensed plugin client
		// satisfies both interfaces — plugin.AgentRunner is a superset of
		// agent.RunnerClient (it adds GetInfo) — so a type assertion
		// is enough to bridge the package boundary.
		tailer := agent.NewTailer(log.Logger)
		for name, raw := range loadedAgents {
			client, ok := raw.(agent.RunnerClient)
			if !ok {
				log.Warn().Str("plugin", name).Msg("agent plugin does not satisfy AgentRunnerClient; skipping")
				continue
			}
			w.agentClients[name] = client
			runner := agent.NewPluginRunner(client, tailer, w.agentLogsDir, log.Logger)
			runner.SetAgentName(name)
			runner.SetAgentRunStore(w.agentRuns)
			pluginRunners[name] = runner
		}
		w.hostService.SetAgentClients(w.agentClients)
		w.hostService.SetAgentLogsDir(w.agentLogsDir)

		// Dispatcher routes Start/Stop/IsRunning by reading AgentName from
		// SQLite via the lookup closure built below.
		lookup := newDispatcherLookup(w.sessions, w.agentChats)
		w.agentRunner = agent.NewDispatcher(pluginRunners, lookup, w.settings.DefaultAgent, log.Logger)
	}

	return nil
}
