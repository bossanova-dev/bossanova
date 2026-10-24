package main

import (
	"github.com/recurser/bossalib/config"
	"github.com/rs/zerolog/log"

	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
)

func (w *daemonWiring) wireAccounts() {
	// Account binding (BOS-170): the resolver decides which registry account a
	// session runs under and materializes its spawn env via the provider plugin.
	// Degrade-safe — an empty registry, an unbound session, or a plugin without
	// rotation all collapse to account 0 (no per-account env). Wired into the
	// server (creation-time default policy) and both spawn seams below.
	w.accountMaterializer = accountwiring.NewMaterializer(w.agentClients, w.accounts, w.accountCreds, log.Logger)
	w.accountUsageProbe, _ = w.accountMaterializer.(server.UsageProbeRecorder)
	// Optional too: without it RemoveAgentAccount cannot purge the on-disk credential
	// materialization, so it keeps its pre-purge behavior (keyring + row only).
	w.accountMaterializations, _ = w.accountMaterializer.(server.AgentAccountMaterializations)
	w.accountResolver = account.NewResolver(
		accountwiring.NewRegistry(w.accounts),
		w.accountMaterializer,
		log.Logger,
		account.WithUsageStalenessWindow(w.settings.ManagedAccounts.UsageStalenessWindow()),
	)
	w.accountSpawnEnv = accountwiring.NewSpawnEnvResolver(w.accountResolver, log.Logger)
	w.hostService.SetAgentAccountEnvResolver(w.accountSpawnEnv)
	accountSmoke, err := accountwiring.NewSmokeRunner(w.agentClients, w.accountCreds, log.Logger)
	if err != nil {
		log.Warn().Err(err).Msg("account provider verification unavailable; account test will validate credential shape only")
	}
	// BOS-1141: the daemon-owned credential-maintenance coordinator wraps the
	// smoke runner so scheduled sweeps and explicit TestAgentAccount RPCs share one
	// single-flight verification path and one generation-guarded durable write.
	// Constructed even when managed accounts are disabled — the RPC hook still
	// benefits from de-duplication — and Start()ed unconditionally, because the
	// scheduled path now consults the kill switch live (WithAuthCheckEnabled
	// below) rather than at boot.

	if accountSmoke != nil {
		// Staleness is deliberately left at the package default (hours). It is
		// NOT wired to ManagedAccounts.UsageStalenessWindow(): that knob sizes
		// the age of a cached USAGE SNAPSHOT, gathered by a cheap HTTP probe,
		// and defaults to 30 minutes. A credential check spawns a real agent
		// process, so borrowing that window made nearly every account stale on
		// every restart and turned the boot sweep into one live agent run per
		// account.
		// The managed-accounts kill switch is re-read from disk per sweep, not
		// captured here: `boss settings --no-managed-accounts` rewrites
		// settings.json without restarting the daemon, so a boot-time snapshot
		// would keep spawning real Codex verification runs after the operator
		// turned rotation off. Fails closed on a load error, matching
		// Lifecycle.currentRotationConfig. Declared inline rather than reusing
		// rotationConfigLoader because that closure is built later in run().
		w.accountCredMaintenance, err = accountwiring.NewCredentialMaintainer(
			accountSmoke, w.accounts, w.accountCreds, log.Logger,
			accountwiring.WithAuthCheckEnabled(func() bool {
				loaded, err := config.Load()
				if err != nil {
					log.Warn().Err(err).
						Msg("account: credential maintenance settings load failed; treating managed accounts as disabled")
					return false
				}
				return loaded.ManagedAccounts.ManagedAgentAccountsEnabled()
			}),
		)
		if err != nil {
			log.Warn().Err(err).Msg("account credential maintenance unavailable; verification stays on-demand only")
			w.accountCredMaintenance = nil
		}
	}
	// The server's verification hook: the coordinator when it exists, else the
	// bare smoke runner. Never both — a parallel smoke/persist path would
	// escape the single-flight and the generation guard.

	if w.accountCredMaintenance != nil {
		w.accountSmokeHook = w.accountCredMaintenance
	} else if accountSmoke != nil {
		w.accountSmokeHook = accountSmoke
	}

}
