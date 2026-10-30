package main

import (
	"os"
	"path/filepath"

	"github.com/recurser/bossd/api/proxy"

	"github.com/rs/zerolog/log"

	"github.com/recurser/bossd/internal/inflight"
	"github.com/recurser/bossd/internal/session"
)

func (w *daemonWiring) wireProxy() {
	var (
		severedErr error
	)
	// S7 failover reverse proxy (BOS-320): a loopback server the Claude
	// subprocess is pointed at via ANTHROPIC_BASE_URL when the default-on
	// managed_accounts.enabled + managed_accounts.failover_proxy_enabled gate
	// is satisfied. Bind it next to the hook server so the flags can be flipped
	// live; ANTHROPIC_BASE_URL injection stays gated on both flags + liveness
	// (Lifecycle.proxyBaseURL), so an always-bound listener is harmless when the
	// feature is turned off.
	//
	// Binding is NON-FATAL (the plan's liveness gate): a construction/bind
	// failure must never take down bossd for a default-on feature. On failure
	// leave the proxy unbound — proxyPort stays 0 and no registrar is wired, so
	// injection is disabled and every session falls back to the direct
	// api.anthropic.com path.
	//
	// Read the PREVIOUS daemon's in-flight stream record before the new proxy
	// starts writing its own (BOS-890). The ordering is load-bearing in both
	// directions: reading later would race the fresh proxy's first Enter, and
	// the read CLEARS the file, so a later read would delete entries this
	// process had already recorded. The ids are held in memory and only acted on
	// after Bootstrap's pane-token re-adoption sweep, further below.
	inflightPath := inflight.Path(w.appDataDir)
	w.severedStreamIDs, severedErr = inflight.ReadAndClear(inflightPath)
	if severedErr != nil {
		log.Warn().Err(severedErr).Msg("in-flight stream record: unreadable; severed streams from the previous exit cannot be recovered")
	}
	// The read above DELETED the record, and the ids now live only in this local.
	// Every early return between here and the MarkSevered call below would
	// therefore lose them permanently — one such path already exists (the cron
	// scheduler's fatal Start error), and nothing stops another being added. Put
	// the ids back on disk instead, so the next startup recovers what this one
	// never got to act on. severedStreamIDs is cleared at the consumption point,
	// so on the normal path this defer is a no-op at daemon shutdown.
	w.deferCleanup(func() {
		if len(w.severedStreamIDs) == 0 {
			return
		}
		if err := inflight.Restore(inflightPath, w.severedStreamIDs); err != nil {
			log.Warn().Err(err).Int("streams", len(w.severedStreamIDs)).
				Msg("in-flight stream record: could not re-persist unconsumed severed streams; they are lost")
			return
		}
		log.Warn().Int("streams", len(w.severedStreamIDs)).
			Msg("in-flight stream record: startup ended before severed streams were queued; re-persisted for the next start")
	})
	streamRecorder := inflight.NewRecorder(inflightPath, log.Logger)

	var _ proxy.Failover = (*session.Lifecycle)(nil)

	proxyCfg := proxy.ProxyServerConfig{
		Failover: w.lifecycle,
		Logger:   log.Logger,
		// Bind a FIXED loopback port so a frozen ANTHROPIC_BASE_URL baked into a
		// live tmux pane survives a daemon restart (BOS-409). Falls back to an
		// ephemeral port on a collision; 0/unset defaults to 44127.
		Port: w.settings.ManagedAccounts.FailoverProxyPort(),
		// Durable path-token registry (BOS-979). Without this the proxy holds the
		// registry only in memory, so a restart 401s every surviving pane whose
		// token the tmux-env sweep cannot reconstruct. Rebuilt in Bootstrap,
		// before Serve.
		ProxyTokens: w.proxyTokens,
	}
	// Assigned only when non-nil, to keep the interface value honest. NewRecorder
	// yields a nil *Recorder when there is no app-data path, and assigning a nil
	// POINTER into the StreamRecorder INTERFACE field produces a typed nil —
	// non-nil as an interface value, so every `p.streams != nil` check inside the
	// proxy would pass.
	//
	// No observable behaviour changed when this guard was added, and that is worth
	// saying plainly: every method on *inflight.Recorder (Enter, Leave, Seal,
	// Snapshot) opens with its own `if r == nil` no-op, so the typed nil was
	// already harmless at the only call sites there are. The point of the guard is
	// that the proxy's own nil check now means what it says, leaving the Recorder's
	// nil-receiver guards as a SECOND line of defence rather than the only one.
	// Neither layer is redundant — do not delete the other one on the strength of
	// this comment.
	if streamRecorder != nil {
		proxyCfg.Streams = streamRecorder
	}

	if ps, perr := proxy.NewProxyServer(proxyCfg); perr != nil {
		log.Warn().Err(perr).Msg("failover proxy server construction failed; proxy unbound, sessions use the direct path")
	} else if lerr := ps.Listen(); lerr != nil {
		log.Warn().Err(lerr).Msg("failover proxy server listen failed; proxy unbound, sessions use the direct path")
	} else {
		w.proxySrv = ps
		w.lifecycle.SetProxyPort(ps.Port())
		w.lifecycle.SetProxyRegistrar(ps)
		w.lifecycle.SetCompanionProxyRegistrar(ps)
		// Expose the proxy's bounded pass-through error tally to RepairDoctor as
		// an informational check (BOS-483). server.New ran above before the proxy
		// existed, so wire the provider now via a setter.
		w.srv.SetPassthroughStatsProvider(ps)
		w.srv.SetFixedPortStatusProvider(ps)
		log.Info().Int("port", ps.Port()).Int("configured_port", w.settings.ManagedAccounts.FailoverProxyPort()).
			Msg("failover proxy listening on 127.0.0.1 fixed port (survives daemon restart; falls back to ephemeral on collision; injection gated on managed_accounts.enabled + managed_accounts.failover_proxy_enabled, both default true)")
	}

	// Pre-seed the interactive REPL's one-time approval for the sentinel
	// ANTHROPIC_API_KEY (BOS-326) so proxied Claude sessions never block on the
	// api-key approval prompt. Gated on the same flags that arm the proxy;
	// fail-soft (a seed failure only means the first session may prompt once).
	if w.settings.ManagedAccounts.ManagedAgentAccountsEnabled() && w.settings.ManagedAccounts.FailoverProxyEnabled() {
		if home, herr := os.UserHomeDir(); herr == nil {
			if serr := session.SeedAPIKeyApproval(filepath.Join(home, ".claude.json"), session.SentinelApprovalSuffix()); serr != nil {
				log.Warn().Err(serr).Msg("failover proxy: could not pre-seed API-key approval; interactive sessions may prompt once")
			}
		}
	}

}
