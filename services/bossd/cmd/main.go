// Package main is the entry point for the bossd daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonstate"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	"github.com/rs/zerolog/log"
)

// achievedFileLimitSoft is the RLIMIT_NOFILE soft limit raiseFileLimit achieved
// at startup (0 when unknown/non-unix). Recorded into daemon state and surfaced
// by the RepairDoctor FD check (BOS-465).
var achievedFileLimitSoft uint64

func main() {
	// Raise RLIMIT_NOFILE before spawning anything so every child (setup
	// scripts, agent runners, git, codegen) inherits the higher limit and
	// FD-heavy steps don't die with EMFILE. Best-effort; never fails the daemon.
	achievedFileLimitSoft = raiseFileLimit()

	showVersion := flag.Bool("version", false, "Print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("bossd " + buildinfo.String())
		return
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	if err := run(runOpts{stopSig: sigCh}); err != nil {
		fmt.Fprintf(os.Stderr, "bossd: %v\n", err)
		os.Exit(1)
	}
}

type tmuxSessionStatusChecker interface {
	HasSessionStatus(ctx context.Context, sessionName string) (bool, error)
}

type tmuxAgentChatLister interface {
	ListWithTmuxSession(ctx context.Context) ([]*domain.AgentChat, error)
}

func liveTmuxAgentSessionIDs(ctx context.Context, chats tmuxAgentChatLister, checker tmuxSessionStatusChecker) []string {
	if chats == nil || checker == nil {
		return nil
	}
	rows, err := chats.ListWithTmuxSession(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("failed to list tmux-backed agent chats before run reconciliation")
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, chat := range rows {
		if chat == nil || chat.AgentSessionID == "" || chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			continue
		}
		alive, err := checker.HasSessionStatus(ctx, *chat.TmuxSessionName)
		if err != nil {
			log.Warn().Err(err).Str("tmux_session", *chat.TmuxSessionName).Str("agent_session", chat.AgentSessionID).Msg("failed to probe tmux run before reconciliation")
			out = append(out, chat.AgentSessionID)
			continue
		}
		if alive {
			out = append(out, chat.AgentSessionID)
		}
	}
	return out
}

// runOpts carries optional overrides for run. All fields are optional;
// zero values produce the production daemon defaults. Tests use this to
// inject a synthetic stop signal, isolate paths, and observe readiness.
type runOpts struct {
	// stopSig triggers graceful shutdown. Required for non-test callers.
	stopSig <-chan os.Signal

	// dbPath overrides db.DefaultDBPath() when non-empty.
	dbPath string

	// socketPath overrides server.DefaultSocketPath() when non-empty.
	socketPath string

	// plugins overrides discovered/configured plugins when non-nil.
	// Pass a non-nil empty slice to disable plugin discovery entirely.
	plugins []config.PluginConfig

	// onReady, if non-nil, is invoked once the daemon's server is
	// listening and all startup goroutines have been launched. Runs on a
	// separate goroutine so it cannot block shutdown.
	onReady func()

	// onBootstrapComplete, if non-nil, fires synchronously immediately
	// after TmuxStatusPoller.Bootstrap returns and before srv.Serve is
	// scheduled. Used by tests to pin the Bootstrap-before-Serve init
	// ordering invariant: a regression that started Serve first would be
	// caught by an OnServeStart firing while OnBootstrapComplete has not.
	onBootstrapComplete func()

	// onServeStart, if non-nil, fires synchronously inside the Serve
	// goroutine just before srv.Serve is invoked. Pairs with
	// onBootstrapComplete to assert init ordering.
	onServeStart func()

	// onHookPortSet, if non-nil, fires synchronously immediately after
	// lifecycle.SetHookPort. onBootstrapStart fires just before
	// lifecycle.Bootstrap. Together they pin the invariant that the hook
	// port is bound before bootstrap re-arms surviving tmux chats — otherwise
	// ConfigureFinalizeHook would be re-issued with port 0 and the worktree
	// would keep the previous daemon's stale hook URL.
	onHookPortSet    func()
	onBootstrapStart func()

	// onRotationSeamsWired, if non-nil, fires synchronously immediately after the
	// lifecycle's rotation binding and account materializer are installed.
	onRotationSeamsWired func(live bool)

	// onTransientResumeSeamsWired, if non-nil, fires synchronously immediately
	// after the BOS-518 auto-resume consumer is constructed. live reports that
	// BOTH halves of the lane exist — the resumer itself and the tracker
	// transition hook that is its only trigger — because either half alone is a
	// daemon that silently never auto-resumes.
	onTransientResumeSeamsWired func(live bool)

	// onEvictionRecomputeSeamsWired, if non-nil, fires synchronously immediately
	// after the BOS-1096 eviction recompute hook is installed. live reports that
	// the tracker actually holds the hook — a daemon that skipped the one
	// SetOnEntriesEvicted call is indistinguishable from one with the fix, in
	// every test but this one, right up until a chat goes quiet in production
	// and its session label freezes again.
	onEvictionRecomputeSeamsWired func(live bool)

	// onArchiveTrackerSeamsWired, if non-nil, fires synchronously immediately
	// after the four archive-after-merge archivers are wired. live reports that
	// ALL FOUR received a non-nil archive worker tracker, because an archiver
	// wired without one silently reopens the BOS-923 shutdown gap. The tracker
	// itself is passed too, so a test can register a handle it controls and
	// assert shutdown actually waits on it.
	onArchiveTrackerSeamsWired func(live bool, track func(string, <-chan struct{}))

	// onRelabelShutdownProbe, if non-nil, fires on the startup path with a probe
	// that reports whether the lifecycle's background chat row rewrites have
	// been stopped. A test reads it after run returns to pin that shutdown
	// calls StopBackgroundRelabels.
	onRelabelShutdownProbe func(stopped func() bool)

	// startupStrandedCronRecovery overrides the startup stranded-cron sweep for
	// shutdown-lifecycle tests. Nil uses Lifecycle.RecoverStrandedCronSessionsAtStartup.
	startupStrandedCronRecovery func(context.Context) (int, error)

	// shutdownGoroutineBudget overrides the tracked-goroutine join's 10s upper
	// bound when positive, so a test can reach its forced-exit branch in
	// milliseconds instead of wedging a real daemon for ten seconds.
	shutdownGoroutineBudget time.Duration

	// onShutdownTrackerReady, if non-nil, fires synchronously on the startup
	// path with run()'s own trackDone closure, letting a test register a handle
	// it controls and then assert what the forced-exit branch says about it.
	//
	// This is the one seam that hands registration out, and it is deliberately
	// narrow: the existing onX probes NOTIFY, and observing a moment is not
	// enough to reach a branch that only fires when a registration refuses to
	// complete. It widens nothing in production — nothing sets it there — and
	// the invariant it must not break is the caller's to keep: register on the
	// startup path, before the signal, exactly as run() itself does. See
	// shutdownTracker's doc comment for why an Add off the startup path panics.
	onShutdownTrackerReady func(trackDone func(name string, done <-chan struct{}))

	// archiveDrainBudget overrides the auto-archive join's 10s upper bound when
	// positive. Its only purpose is to let a test reach the forced-exit branch
	// of that join in milliseconds; nothing in production sets it, so the
	// budget the log line names is the budget production waits.
	archiveDrainBudget time.Duration

	// startupStrandedBootstrapReap overrides the startup stranded-bootstrap reap.
	// Nil uses Lifecycle.ReapStrandedBootstrapSessionsAtStartup. Its counterpart
	// above exists for shutdown-lifecycle tests; this one also lets a test pin the
	// ORDER of the two startup passes, which is load-bearing — see where they run.
	startupStrandedBootstrapReap func(context.Context) (int, error)
}

func run(opts runOpts) error {
	w := &daemonWiring{opts: opts}
	defer w.close()
	if err := w.wireStores(); err != nil {
		return err
	}
	if w.alreadyRunning {
		return nil
	}
	if err := w.wireSessionInfrastructure(); err != nil {
		return err
	}
	if err := w.wirePlugins(); err != nil {
		return err
	}
	w.wireAccounts()
	w.wireLifecycle()
	w.wireRotation()
	w.wireShutdownTracking()
	if err := w.wireRepairPlugin(); err != nil {
		return err
	}
	w.wireTaskWorkers()
	w.wireStreamObservers()
	w.wireChatUploads()
	w.wireUpstream()
	w.wireDaemon()
	w.wireDeliveryWorkers()
	if err := w.wireHooks(); err != nil {
		return err
	}
	w.wireProxy()
	if err := w.wireBootstrap(); err != nil {
		return err
	}
	w.wireBackgroundWorkers()
	if err := w.wireServing(); err != nil {
		return err
	}
	return w.wireShutdown()
}

// cleanupDaemonShutdownState removes this daemon's handoff metadata before
// releasing the singleton lock. A replacement that acquires the lock may write
// its own metadata immediately, so deleting after Close could remove the
// replacement's record.
func cleanupDaemonShutdownState(lockFile io.Closer, daemonMetadataWritten bool, daemonMetadataDir string) {
	if daemonMetadataWritten {
		_ = daemonstate.Remove(daemonMetadataDir)
	}
	_ = lockFile.Close()
}

func daemonDistinctID() string {
	hostname, err := os.Hostname()
	if err != nil {
		return libtelemetry.DaemonDistinctID("")
	}
	return daemonDistinctIDFromHostname(hostname)
}

func daemonDistinctIDFromHostname(hostname string) string {
	return libtelemetry.DaemonDistinctID(hostname)
}
