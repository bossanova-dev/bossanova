package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/errortrack"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	bossalog "github.com/recurser/bossalib/log"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/rs/zerolog/log"

	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/api/upstream"
	gitpkg "github.com/recurser/bossd/internal/adapter/git"
	"github.com/recurser/bossd/internal/adapter/github"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/callback"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/session"
)

func (w *daemonWiring) wireSessionInfrastructure() error {
	// --- Lifecycle ---

	w.worktrees = gitpkg.NewManager(log.Logger)
	// Run repo setup scripts through the user's login shell so per-project
	// version-manager shims (nodenv/asdf/…) are on PATH — otherwise the daemon's
	// restricted PATH can't find pnpm and worktree dependency/hook install
	// silently skips, leaving cron worktrees dependency-free.
	w.worktrees.LoginShell = w.settings.LoginShell
	// Every session this client creates carries BOSS_DAEMON_ID, which is the
	// only thing that lets the reaper tell its own orphans from a peer
	// daemon's panes on a shared host.
	// The two composer-readiness budgets come from settings.json (BOS-893) via
	// accessors that supply the defaults and clamp the send path, so the tmux
	// package never has to import config to carry two integers. They are passed
	// unconditionally: an unset block yields the same defaults the package
	// already holds, which the drift guard in the tmux tests keeps in step.
	w.tmuxClient = tmux.NewClient(
		tmux.WithDaemonID(w.tmuxDaemonID),
		tmux.WithSessionStartReadyDeadline(w.settings.TmuxDelivery.SessionStartReadyDeadline()),
		tmux.WithSendReadyDeadline(w.settings.TmuxDelivery.SendReadyDeadline()),
	)
	w.ghProvider = github.New(log.Logger)
	// publishStoreSessionUpdate emits the update event for a session a daemon
	// pass wrote directly through the store. The PR-association reconcile
	// (rename to the PR title) and the tracker linker (tracker_id/tracker_url)
	// both bypass the UpdateSession RPC that would emit it, so without this
	// bosso/web keep showing stale fields until the next full snapshot.
	w.publishStoreSessionUpdate = func(ctx context.Context, sess *models.Session) {
		pbSess := server.SessionToProto(sess)
		// bosso applies session deltas as full replacements
		// (state.go applySessionDelta), so populate the joined repo
		// display name or the web UI would lose the Repo column.
		if sess.RepoID != "" {
			if r, err := w.repos.Get(ctx, sess.RepoID); err == nil && r != nil {
				pbSess.RepoDisplayName = r.DisplayName
				pbSess.RepoOriginUrl = server.CanonicalRepoOriginURL(r.OriginURL)
			}
		}
		w.hydrateSessionForStream(ctx, pbSess)
		w.streamBus.Publish(upstream.StreamEvent{
			Session: &upstream.SessionEvent{
				Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
				Session: pbSess,
			},
		})
	}
	w.prAssociationResolver = session.NewPRAssociationResolver(w.sessions, w.repos, w.ghProvider, log.Logger).
		WithBranchResolver(w.worktrees).
		WithCommitSubjects(w.worktrees).
		WithCronJobs(w.cronJobs).
		WithUpdateNotifier(w.publishStoreSessionUpdate)

	// Reconcile sessions that were created before their PR existed (or
	// where PR creation happened out-of-band). Uses live branch state.
	if n, err := w.prAssociationResolver.Reconcile(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to reconcile PR associations")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("reconciled sessions with existing PRs")
	}

	// --- Dispatcher + Poller ---
	// Note: FixLoop removed - repair functionality moved to plugin

	w.dispatcher = session.NewDispatcher(w.sessions, w.repos, w.ghProvider, log.Logger)
	// Let the dispatcher poke the display tracker the instant a PR merges/closes
	// so the STATUS column flips immediately instead of waiting for the display
	// poller's next cycle.
	w.dispatcher.SetDisplayStatusSetter(w.displayTracker)
	w.poller = session.NewPoller(w.sessions, w.repos, w.ghProvider, session.DefaultPollInterval, session.DefaultPollTimeout, log.Logger)

	// GitHub callback evaluator (BOS-468): verifies authoritative PR/check state
	// and fires durable callbacks. Wired into the webhook dispatcher below, run
	// at startup for reconciliation, and used as the delivery worker's periodic
	// reconcile safety net.
	w.callbackEvaluator = callback.NewEvaluator(w.githubCallbacks, w.ghProvider, time.Now, log.Logger)
	w.callbackEvaluator.SetRetirer(w.callbackRetirer)

	// --- Settings + Display Poller ---

	w.bossEnv = config.EnvOr("BOSS_ENV", "local")
	var errortrackClose = func() {}

	if w.settings.ErrorTrackingEnabled {
		errortrackDSN := config.EnvOr("BOSSD_SENTRY_DSN", "https://f8081ecc39984438b534485cb56a7391@o4511396716871680.ingest.de.sentry.io/4511396747608144")
		close, err := errortrack.Init(errortrack.Opts{
			DSN:         errortrackDSN,
			App:         "bossd",
			Environment: w.bossEnv,
			Release:     buildinfo.Version + "-" + buildinfo.Commit,
		})
		if err != nil {
			log.Warn().Err(err).Msg("errortrack disabled")
		} else {
			errortrackClose = close
			w.pluginErrorTrackingDSN = errortrackDSN
		}
	}
	w.deferCleanup(func() { errortrackClose() })

	// Bossd-owned log dir for agent runs. Lives outside the worktree so a
	// hostile/buggy plugin can't path-traverse via symlinks. Plugin opens
	// log files here with O_NOFOLLOW (Task 7).
	//
	// An unset worktree base directory yields an empty agent-logs dir, which is
	// tolerated rather than fatal: agent logging degrades, and every consumer
	// already guards the empty case (HostServiceServer.StartAgentRun and
	// StartChatRun, Lifecycle.StartTmuxChat, agentLogIdleFor). The settings RPC
	// rejects an empty worktree_base_dir, so only a hand-edited settings.json
	// reaches this state — narrow, but it must not brick the daemon.
	w.agentLogsDir = bossalog.AgentLogsDir(w.settings.WorktreeBaseDir)
	if w.agentLogsDir == "" {
		log.Warn().Msg("no worktree base directory configured: agent logging disabled")
	} else if err := os.MkdirAll(w.agentLogsDir, 0o700); err != nil {
		return fmt.Errorf("create agent-logs dir %s: %w", w.agentLogsDir, err)
	}

	w.displayPoller = session.NewDisplayPoller(
		w.sessions, w.repos, w.ghProvider, w.displayTracker,
		w.settings.DisplayPollInterval(), log.Logger,
	)
	// Persist every poll's check list so `boss session checks <id>` can show
	// the daemon's view of CI history. Disk volume is bounded by poll
	// interval × active sessions and is fine for ops debugging. The same
	// store is shared with the gRPC server below so reads and writes hit
	// one instance.
	w.displayPoller.SetSnapshotStore(w.checkSnapshots)
	// Carry the boss/build receipt forward when a session's own worktree
	// pushes on top of a receipted head (BOS-1452).
	w.displayPoller.SetReceiptCarrier(w.ghProvider, w.worktrees)
	// Keep each session's verify evidence summary (video, length, proven
	// counts) for its current PR head. The poller reports every poll; package
	// evidence documents when the reader actually reads PR comments.
	// The reader reads through the daemon's vcs.Provider, not the GitHub
	// type, so any provider's FindPRCommentsWithMarker feeds it; the contract
	// it relies on is pinned in internal/evidence/provider_contract_test.go.
	var evidenceComments vcs.Provider = w.ghProvider
	w.evidenceReader = evidence.NewReader(evidenceComments, log.Logger)
	w.displayPoller.SetEvidenceObserver(w.evidenceReader)
	if reconciled, err := w.agentRuns.MarkOpenStopped(context.Background(), time.Now(), liveTmuxAgentSessionIDs(context.Background(), w.agentChats, w.tmuxClient)); err != nil {
		log.Warn().Err(err).Msg("failed to reconcile open agent runs")
	} else if reconciled > 0 {
		log.Info().Int64("count", reconciled).Msg("reconciled open agent runs after daemon restart")
	}

	return nil
}
