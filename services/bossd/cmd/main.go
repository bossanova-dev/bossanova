// Package main is the entry point for the bossd daemon.
package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/http2"

	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	bossalog "github.com/recurser/bossalib/log"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/skilldata"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/eventbus"
	"github.com/recurser/bossd/internal/server"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/taskorchestrator"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/recurser/bossd/internal/upstream"
	"github.com/recurser/bossd/internal/vcs/github"
	"github.com/recurser/bossd/migrations"
)

// sessionListerAdapter adapts SessionStore to upstream.SessionLister.
type sessionListerAdapter struct {
	sessions db.SessionStore
}

// ListSessions returns every session (active and archived) as protobuf,
// populated with each session's repo display name via a single JOIN query.
// Archived sessions are included so the orchestrator sees the archive
// transition — filtering to active only would make an archived session
// look indistinguishable from a deleted one at the receiver.
func (a *sessionListerAdapter) ListSessions(ctx context.Context) ([]*bossanovav1.Session, error) {
	rows, err := a.sessions.ListWithRepo(ctx, "")
	if err != nil {
		return nil, err
	}
	pbSessions := make([]*bossanovav1.Session, 0, len(rows))
	for _, r := range rows {
		pbSess := server.SessionToProto(r.Session)
		pbSess.RepoDisplayName = r.RepoDisplayName
		pbSessions = append(pbSessions, pbSess)
	}
	return pbSessions, nil
}

func main() {
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
}

func run(opts runOpts) error {
	// Human-friendly console logging plus rotated file at $XDG_STATE_HOME/bossanova/logs/bossd.log.
	logCloser := bossalog.Setup("bossd")
	defer func() { _ = logCloser.Close() }()

	// --- Database ---

	dbPath := opts.dbPath
	if dbPath == "" {
		p, err := db.DefaultDBPath()
		if err != nil {
			return fmt.Errorf("db path: %w", err)
		}
		dbPath = p
	}

	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = database.Close() }()

	log.Info().Str("path", dbPath).Msg("database opened")

	// --- Migrations ---

	if err := migrate.Run(database, migrations.FS); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	log.Info().Msg("migrations complete")

	// --- Stores ---

	repos := db.NewRepoStore(database)
	rawSessions := db.NewSessionStore(database)
	attempts := db.NewAttemptStore(database)
	claudeChats := db.NewClaudeChatStore(database)
	taskMappings := db.NewTaskMappingStore(database)
	rawWorkflows := db.NewWorkflowStore(database)

	// The display-status computer needs to read the bare stores; wrap them
	// after construction so the computer's own writes don't recurse through
	// the recompute hooks (the wrapper short-circuits on display-only writes,
	// but reading via the unwrapped store is also free of side effects).
	chatStatusTracker := status.NewTracker()
	displayTracker := status.NewDisplayTracker()
	displayComputer := status.NewDisplayStatusComputer(
		rawSessions, displayTracker, chatStatusTracker, claudeChats, rawWorkflows, log.Logger,
	)
	var sessions db.SessionStore = db.NewRecomputingSessionStore(rawSessions, displayComputer)
	var workflows db.WorkflowStore = db.NewRecomputingWorkflowStore(rawWorkflows, displayComputer)

	// Wire the display tracker so its mutations recompute synchronously.
	displayTracker.SetRecomputer(displayComputer)
	// Wire the chat-status tracker similarly. It is keyed by claude_id, so
	// resolve to a session before calling Recompute.
	chatStatusTracker.SetOnUpdate(func(claudeID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		chat, err := claudeChats.GetByClaudeID(ctx, claudeID)
		if err != nil || chat == nil {
			return
		}
		_ = displayComputer.Recompute(ctx, chat.SessionID)
	})

	// Create session lister for upstream sync
	sessionLister := &sessionListerAdapter{sessions: sessions}

	// Fail any workflows left in running/pending state from a previous daemon
	// instance. Their driving goroutines no longer exist after a restart.
	if n, err := workflows.FailOrphaned(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to clean up orphaned workflows")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("failed orphaned workflows from previous run")
	}

	// Advance sessions stuck in ImplementingPlan whose driving workflows are
	// no longer running. Must run after FailOrphaned so the subquery sees
	// the updated workflow statuses.
	if n, err := sessions.AdvanceOrphanedSessions(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to advance orphaned sessions")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("advanced orphaned sessions to awaiting_checks")
	}

	// Fail any task mappings left in Pending/InProgress from a previous
	// daemon instance. Their driving goroutines no longer exist.
	if n, err := taskMappings.FailOrphanedMappings(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to clean up orphaned task mappings")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("failed orphaned task mappings from previous run")
	}

	// Backfill the display-status composite for every active session. After
	// a daemon restart the in-memory inputs (chat, display tracker) are
	// empty, so the persisted display_label may not match the stored state.
	// Recomputing once at boot ensures the row matches what Compute would
	// produce given current inputs (typically "stopped" or PR-axis label),
	// so clients reading via the bosso DB-fallback path don't see stale
	// "running 2/4" labels from the previous daemon's last write.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		all, err := rawSessions.ListActive(ctx, "")
		if err != nil {
			log.Warn().Err(err).Msg("display backfill: list active sessions failed")
		} else {
			var updated int
			for _, s := range all {
				if err := displayComputer.Recompute(ctx, s.ID); err != nil {
					log.Debug().Err(err).Str("session_id", s.ID).Msg("display backfill: recompute failed")
					continue
				}
				updated++
			}
			if updated > 0 {
				log.Info().Int("count", updated).Msg("display backfill: recomputed active sessions")
			}
		}
		cancel()
	}

	// --- Lifecycle ---

	worktrees := gitpkg.NewManager(log.Logger)
	claudeRunner := claude.NewRunner(log.Logger)
	tmuxClient := tmux.NewClient()
	ghProvider := github.New(log.Logger)
	lifecycle := session.NewLifecycle(sessions, repos, claudeChats, worktrees, claudeRunner, tmuxClient, ghProvider, log.Logger)

	// Reconcile sessions that were created before their PR existed (or
	// where PR creation happened out-of-band). Matches by branch name.
	if n, err := session.ReconcilePRAssociations(
		context.Background(), sessions, repos, ghProvider, log.Logger,
	); err != nil {
		log.Warn().Err(err).Msg("failed to reconcile PR associations")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("reconciled sessions with existing PRs")
	}

	// --- Dispatcher + Poller ---
	// Note: FixLoop removed - repair functionality moved to plugin

	dispatcher := session.NewDispatcher(sessions, repos, ghProvider, nil, log.Logger)
	poller := session.NewPoller(sessions, repos, ghProvider, session.DefaultPollInterval, session.DefaultPollTimeout, log.Logger)

	// --- Settings + Display Poller ---

	settings, _ := config.Load()

	// Update boss skills if they were previously installed by the CLI.
	if skillsDir, err := skilldata.DefaultSkillsDir(); err == nil && skilldata.BossSkillsInstalled(skillsDir) {
		if err := skilldata.ExtractSkills(skillsDir, skilldata.SkillsFS); err != nil {
			log.Warn().Err(err).Msg("failed to update skills")
		}
	}

	displayPoller := session.NewDisplayPoller(
		sessions, repos, ghProvider, displayTracker,
		settings.DisplayPollInterval(), log.Logger,
	)

	// --- Plugin Host ---

	pluginBus := eventbus.New(log.Logger)
	pluginHost := plugin.New(pluginBus, ghProvider, log.Logger)
	autopilotRunner := claude.NewTmuxRunner(tmuxClient, log.Logger)
	pluginHost.SetWorkflowDeps(workflows, sessions, claudeChats, autopilotRunner)
	pluginHost.SetSessionDeps(repos, sessions, displayTracker, chatStatusTracker)

	// Register DisplayTracker onChange callback to notify plugins of status changes
	displayTracker.SetOnChange(func(sessionID string, oldEntry, newEntry *status.DisplayEntry) {
		if newEntry != nil {
			pluginHost.NotifyStatusChange(context.Background(), sessionID, newEntry.Status, newEntry.HasFailures)
		}
	})

	pluginCfgs := settings.Plugins
	if opts.plugins != nil {
		pluginCfgs = opts.plugins
	} else if len(pluginCfgs) == 0 {
		pluginCfgs = config.DiscoverPlugins()
		if len(pluginCfgs) > 0 {
			log.Info().Int("count", len(pluginCfgs)).Msg("auto-discovered plugins")
			settings.Plugins = pluginCfgs
			if err := config.Save(settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist discovered plugins to settings")
			} else {
				log.Info().Msg("persisted discovered plugins to settings")
			}
		}
	}

	// Self-heal a settings file that accumulated duplicate plugin entries —
	// e.g. a user added a plugin the discovery loop also wrote. Duplicates
	// would otherwise spawn parallel plugin subprocesses with independent
	// in-memory dedup state (see bossd-plugin-repair).
	if deduped, dropped := config.DedupPluginConfigs(pluginCfgs); dropped {
		log.Warn().Int("before", len(pluginCfgs)).Int("after", len(deduped)).Msg("removing duplicate plugin entries")
		pluginCfgs = deduped
		if opts.plugins == nil {
			settings.Plugins = deduped
			if err := config.Save(settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist deduped plugin list to settings")
			}
		}
	}

	if err := pluginHost.Start(context.Background(), pluginCfgs); err != nil {
		pluginBus.Close()
		return fmt.Errorf("plugin host: %w", err)
	}

	// shutdownWG tracks daemon goroutines so we can wait for them to exit cleanly.
	// Subsystems that manage their own goroutines (poller, dispatcher, orchestrator,
	// display poller, tmux poller) expose a Done() channel; goroutines spawned
	// directly below use wg.Add/wg.Done via trackedGo below.
	var shutdownWG sync.WaitGroup

	// trackedGo spawns fn via safego.Go and registers it with shutdownWG.
	trackedGo := func(fn func()) {
		shutdownWG.Add(1)
		safego.Go(log.Logger, func() {
			defer shutdownWG.Done()
			fn()
		})
	}

	// trackDone registers a subsystem's Done() channel with shutdownWG.
	trackDone := func(done <-chan struct{}) {
		shutdownWG.Add(1)
		go func() {
			defer shutdownWG.Done()
			<-done
		}()
	}

	// Auto-start repair plugin workflows if available
	trackedGo(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		for _, svc := range pluginHost.GetWorkflowServices() {
			infoResp, err := svc.GetInfo(ctx)
			if err != nil {
				log.Warn().Err(err).Msg("failed to get plugin info for auto-start")
				continue
			}
			if infoResp == nil {
				log.Warn().Msg("plugin returned nil info for auto-start")
				continue
			}

			// Auto-start repair plugin
			if infoResp.Name == "repair" {
				log.Info().Str("plugin_name", infoResp.Name).Msg("auto-starting repair plugin")
				repairCfgJSON, _ := json.Marshal(settings.Repair)
				_, err := svc.StartWorkflow(ctx, &bossanovav1.StartWorkflowRequest{
					ConfigJson: string(repairCfgJSON),
				})
				if err != nil {
					log.Warn().Err(err).Str("plugin_name", infoResp.Name).Msg("failed to auto-start repair plugin")
				}
			}
		}
	})

	// --- Task Orchestrator ---

	sessionCreator := taskorchestrator.NewSessionCreator(sessions, lifecycle, log.Logger)
	// Warn if tmux is not available — interactive sessions will fail at attach time.
	if !tmuxClient.Available(context.Background()) {
		log.Warn().Msg("tmux is not installed or not in PATH; interactive sessions will not work")
	}

	livenessChecker := taskorchestrator.NewLivenessChecker(sessions, claudeChats, claudeRunner, tmuxClient)
	orchestrator := taskorchestrator.New(
		pluginHost, repos, taskMappings, sessionCreator, ghProvider,
		worktrees, livenessChecker, taskorchestrator.DefaultPollInterval, log.Logger,
	)

	// Wire the orchestrator as the completion notifier for the dispatcher
	// and server so that terminal session states unblock the per-repo task queue.
	dispatcher.SetCompletionNotifier(orchestrator)

	// --- Tmux Status Poller ---

	tmuxStatusPoller := status.NewTmuxStatusPoller(chatStatusTracker, claudeChats, sessions, tmuxClient, log.Logger)

	// --- Server ---

	socketPath := opts.socketPath
	if socketPath == "" {
		p, err := server.DefaultSocketPath()
		if err != nil {
			return fmt.Errorf("socket path: %w", err)
		}
		socketPath = p
	}

	// --- Upstream (optional, cloud mode) ---
	//
	// The legacy upstream.Manager (heartbeat + SyncSessions loops) was
	// replaced in T3.7 by upstream.StreamClient, which opens a single
	// long-lived DaemonStream and receives commands the orchestrator
	// pushes. Bootstrap sequence:
	//   1. Build the Connect client against BOSSD_ORCHESTRATOR_URL.
	//   2. Call RegisterDaemon with the WorkOS JWT to obtain a
	//      session_token (bosso persists the daemon's identity).
	//   3. Construct StreamClient with adapters that wrap the existing
	//      stores/lifecycle/tmux reader — no new subsystems needed.
	//   4. Launch Run(ctx) in a tracked goroutine. It owns reconnects,
	//      token refresh, snapshot, delta forwarding, and command
	//      dispatch on its own.
	streamBus := upstream.NewStreamBus(log.Logger)
	defer streamBus.Close()

	// Wire the display-status computer's post-write hook into the stream
	// bus so every Recompute that actually writes a new (label, intent,
	// spinner) trio fans out a SessionDelta{UPDATED} on the reverse
	// stream. Without this, bosso only ever sees the initial
	// DaemonSnapshot — labels recomputed after startup (PR check
	// results, chat status, workflow transitions) never reach the web UI
	// and every session shows whatever it computed to before the gh
	// poller had run, which is uniformly "idle" for sessions whose chat
	// status is IDLE and whose PR check state is UNSPECIFIED.
	displayComputer.SetOnUpdate(func(ctx context.Context, sessionID string) {
		row, err := rawSessions.Get(ctx, sessionID)
		if err != nil {
			log.Debug().Err(err).Str("session_id", sessionID).Msg("display update: session lookup failed")
			return
		}
		pbSess := server.SessionToProto(row)
		// Populate the joined repo display name. bosso applies session
		// deltas as full replacements (state.go applySessionDelta), so
		// omitting this would clobber the populated value the initial
		// DaemonSnapshot set and the web UI would lose the Repo column.
		if row.RepoID != "" {
			if r, err := repos.Get(ctx, row.RepoID); err == nil && r != nil {
				pbSess.RepoDisplayName = r.DisplayName
			}
		}
		streamBus.Publish(upstream.StreamEvent{
			Session: &upstream.SessionEvent{
				Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
				Session: pbSess,
			},
		})
	})

	var streamClient *upstream.StreamClient
	var authNotifier server.AuthNotifier
	if cfg := upstream.ConfigFromEnv(); cfg != nil {
		// ConnectRPC bidi streams (DaemonStream) require HTTP/2. Over
		// TLS that's handled via ALPN automatically; over plain HTTP
		// (local dev against http://localhost:8080) Go's default
		// transport stays on HTTP/1.1 and bosso's h2c handler rejects
		// bidi with HTTP 505. When the orchestrator URL is cleartext,
		// use an h2c-capable Transport so the daemon speaks HTTP/2
		// directly.
		httpClient := &http.Client{Timeout: 30 * time.Second}
		if strings.HasPrefix(cfg.OrchestratorURL, "http://") {
			httpClient.Transport = &http2.Transport{
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, network, addr)
				},
			}
			httpClient.Timeout = 0 // streams must not time out
		}
		client := bossanovav1connect.NewOrchestratorServiceClient(httpClient, cfg.OrchestratorURL)

		// Gather repo IDs for registration.
		allRepos, err := repos.List(context.Background())
		if err != nil {
			log.Warn().Err(err).Msg("failed to list repos for upstream registration")
		}
		var repoIDs []string
		for _, r := range allRepos {
			repoIDs = append(repoIDs, r.ID)
		}

		// Prefer BOSSD_USER_JWT; fall back to whatever the keychain
		// holds. Empty is allowed — bosso will reject the handshake and
		// the outer Run loop will back off, but the daemon stays up in
		// local-only mode.
		tokenProvider := upstream.NewKeychainTokenProvider()
		authToken := cfg.UserJWT
		if authToken == "" {
			// If the cached keychain token is expired (or about to be),
			// proactively refresh via the WorkOS refresh_token before
			// RegisterDaemon. The periodic refresh loop only runs after
			// the stream is alive, which is too late — bosso rejects the
			// initial register with an expired JWT and the whole startup
			// falls back to local-only mode.
			exp := tokenProvider.ExpiresAt()
			if !exp.IsZero() && time.Until(exp) < 60*time.Second {
				refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 10*time.Second)
				if _, err := tokenProvider.Refresh(refreshCtx); err != nil {
					log.Warn().Err(err).Msg("proactive token refresh before register failed")
				}
				refreshCancel()
			}
			authToken = tokenProvider.Token()
		}

		regCtx, regCancel := context.WithTimeout(context.Background(), 10*time.Second)
		sessionToken, err := upstream.Register(regCtx, client, cfg.DaemonID, cfg.Hostname, authToken, repoIDs)
		regCancel()
		if err != nil {
			// Non-fatal: daemon works in local mode without orchestrator.
			log.Warn().Err(err).Msg("upstream register failed, running in local-only mode")

			// Diagnostic dump: print the register inputs and the JWT
			// claims (unverified) so it's obvious when the daemon is
			// sending an expired or wrong-client token. Access token
			// itself is not logged — just the claims.
			iss, sub, aud, expStr, jwtErr := decodeJWTClaimsForLog(authToken)
			log.Warn().
				Str("orchestrator_url", cfg.OrchestratorURL).
				Str("daemon_id", cfg.DaemonID).
				Str("hostname", cfg.Hostname).
				Bool("bossd_user_jwt_set", cfg.UserJWT != "").
				Int("token_len", len(authToken)).
				Str("boss_workos_client_id", os.Getenv("BOSS_WORKOS_CLIENT_ID")).
				Str("bosso_workos_client_id", os.Getenv("BOSSO_WORKOS_CLIENT_ID")).
				Str("jwt_iss", iss).
				Str("jwt_sub", sub).
				Str("jwt_aud", aud).
				Str("jwt_exp", expStr).
				AnErr("jwt_decode_err", jwtErr).
				Msg("upstream register diagnostic")
		} else {
			log.Info().Str("daemon_id", cfg.DaemonID).Msg("registered with orchestrator")

			// bosso expects BOTH credentials on the stream:
			//   Authorization: Bearer <WorkOS JWT>   — proves user identity
			//   X-Daemon-Token: <session_token>      — proves daemon identity
			// See services/bosso/internal/server/stream.go DaemonStream.

			// Snapshot readers pull from the bossd stores, projecting
			// to the slim pb types the snapshot expects.
			snapshotSessions := upstream.NewSessionSnapshotReader(sessionLister)
			snapshotRepos := upstream.NewRepoSnapshotReader(func(ctx context.Context) ([]string, error) {
				rs, err := repos.List(ctx)
				if err != nil {
					return nil, err
				}
				out := make([]string, len(rs))
				for i, r := range rs {
					out[i] = r.ID
				}
				return out, nil
			})
			snapshotChats := upstream.NewChatSnapshotReader(func(ctx context.Context) ([]*bossanovav1.ClaudeChatMetadata, error) {
				chats, err := claudeChats.ListWithTmuxSession(ctx)
				if err != nil {
					return nil, err
				}
				out := make([]*bossanovav1.ClaudeChatMetadata, 0, len(chats))
				for _, c := range chats {
					out = append(out, &bossanovav1.ClaudeChatMetadata{
						Id:        c.ID,
						SessionId: c.SessionID,
						ClaudeId:  c.ClaudeID,
						Title:     c.Title,
						DaemonId:  c.DaemonID,
						CreatedAt: timestamppb.New(c.CreatedAt),
					})
				}
				return out, nil
			})
			snapshotStatuses := upstream.NewStatusSnapshotReader(func(ctx context.Context) ([]*bossanovav1.ChatStatusEntry, error) {
				// No bulk accessor today — return empty. The chat-status
				// tracker's public surface is per-claude-id; expanding
				// it is an independent change, and the coalescer will
				// fill the orchestrator's map with live deltas shortly
				// after reconnect.
				return nil, nil
			})

			// Command adapters delegate back to the existing
			// lifecycle/store surfaces.
			cmdHandler := &upstream.CommandHandlerAdapter{
				Lifecycle:  lifecycle,
				Sessions:   sessionGetterAdapter{sessions: sessions},
				Automation: automationToggleAdapter{sessions: sessions},
				OnCompletion: func(ctx context.Context, sessionID string) {
					if orchestrator != nil {
						orchestrator.HandleSessionCompleted(ctx, sessionID, models.TaskMappingStatusFailed)
					}
				},
			}

			// Attacher bridges to claude.Runner's subscribe/history.
			attacher := &upstream.SessionAttacherAdapter{
				Sessions: attachLookupAdapter{sessions: sessions},
				Claude:   claudeAttachAdapter{runner: claudeRunner},
				Logger:   log.Logger,
			}

			// reRegister self-heals from a stale session_token (e.g. another
			// bossd with the same daemon_id rotated it via UPSERT, or
			// bosso's daemons row was cleared). The Run loop calls this
			// after a CodeUnauthenticated handshake; we re-use the fresh
			// JWT path from startup (tokenProvider auto-refreshes inside
			// the opener) and gather repoIDs each call so a repo set that
			// changed since startup is reflected.
			reRegister := func(ctx context.Context) (string, error) {
				currentRepos, err := repos.List(ctx)
				if err != nil {
					log.Warn().Err(err).Msg("reRegister: repos.List failed; proceeding with empty set")
					currentRepos = nil
				}
				ids := make([]string, 0, len(currentRepos))
				for _, r := range currentRepos {
					ids = append(ids, r.ID)
				}
				jwt := tokenProvider.Token()
				if jwt == "" {
					jwt = cfg.UserJWT
				}
				return upstream.Register(ctx, client, cfg.DaemonID, cfg.Hostname, jwt, ids)
			}

			streamClient = upstream.NewStreamClient(upstream.StreamClientConfig{
				Client:       client,
				AuthToken:    authToken,    // WorkOS JWT → Authorization header
				SessionToken: sessionToken, // daemon token → X-Daemon-Token header
				DaemonID:     cfg.DaemonID,
				Hostname:     cfg.Hostname,
				Stores: upstream.StreamStores{
					Sessions: snapshotSessions,
					Chats:    snapshotChats,
					Repos:    snapshotRepos,
					Statuses: snapshotStatuses,
				},
				Events:         streamBus,
				TokenProvider:  tokenProvider,
				CommandHandler: cmdHandler,
				Webhooks:       &upstream.NoopWebhookDispatcher{Logger: log.Logger},
				Attacher:       attacher,
				ReRegister:     reRegister,
				Logger:         log.Logger,
			})

			authNotifier = &streamAuthAdapter{
				streamClient:  streamClient,
				tokenProvider: tokenProvider,
				logger:        log.Logger,
			}
		}
	}

	srv := server.New(server.Config{
		Repos:              repos,
		Sessions:           sessions,
		Attempts:           attempts,
		ClaudeChats:        claudeChats,
		Workflows:          workflows,
		ChatStatus:         chatStatusTracker,
		DisplayTracker:     displayTracker,
		TmuxPoller:         tmuxStatusPoller,
		Lifecycle:          lifecycle,
		Claude:             claudeRunner,
		Worktrees:          worktrees,
		Provider:           ghProvider,
		PluginHost:         pluginHost,
		CompletionNotifier: orchestrator,
		AuthNotifier:       authNotifier,
		Logger:             log.Logger,
	})

	// Start poller and dispatcher.
	pollerCtx, pollerCancel := context.WithCancel(context.Background())
	defer pollerCancel()
	events := poller.Run(pollerCtx)
	trackDone(poller.Done())
	dispatcherDone := safego.Go(log.Logger, func() { dispatcher.Run(pollerCtx, events) })
	trackDone(dispatcherDone)

	// Start task orchestrator (polls plugin task sources).
	orchestrator.Start(pollerCtx)
	trackDone(orchestrator.Done())

	// Start display status poller.
	displayPoller.Run(pollerCtx)
	trackDone(displayPoller.Done())

	// Bootstrap tmux status poller with pre-existing sessions before starting
	// the polling loop, so sessions from before a daemon restart show correct
	// status (idle/question) instead of defaulting to unknown.
	tmuxStatusPoller.Bootstrap(context.Background())

	// Start tmux status poller (captures pane content to detect question/idle/working).
	tmuxStatusPoller.Run(pollerCtx)
	trackDone(tmuxStatusPoller.Done())

	// Start chat status cleanup goroutine (GC stale entries every 30s).
	trackedGo(func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				chatStatusTracker.Cleanup()
			}
		}
	})

	// Bind the socket and initialize the http.Server synchronously so
	// Shutdown below cannot race with the serving goroutine's write to
	// the internal server field.
	if err := srv.Listen(socketPath); err != nil {
		return fmt.Errorf("server listen: %w", err)
	}

	// Start serving in a goroutine.
	errCh := make(chan error, 1)
	trackedGo(func() {
		log.Info().Str("socket", socketPath).Msg("starting server")
		errCh <- srv.Serve()
	})

	// Start the upstream StreamClient (no-op in local-only mode).
	// streamCtx is separate from pollerCtx so we can stop the stream
	// before the plugin host is torn down, letting orchestrator commands
	// that ride on it drain cleanly.
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	if streamClient != nil {
		trackedGo(func() {
			streamClient.Run(streamCtx)
		})
	}

	// --- Ready hook (tests) ---

	if opts.onReady != nil {
		safego.Go(log.Logger, opts.onReady)
	}

	// --- Wait for shutdown trigger ---

	select {
	case sig := <-opts.stopSig:
		log.Info().Str("signal", sig.String()).Msg("shutting down")
	case err := <-errCh:
		// Server exited unexpectedly.
		return fmt.Errorf("server: %w", err)
	}

	// Stop poller, dispatcher, and task orchestrator (all use pollerCtx).
	// Must cancel before stopping plugin host, since the orchestrator
	// calls into plugins.
	pollerCancel()

	// Stop upstream StreamClient (if running).
	streamCancel()

	// Stop plugin host.
	if err := pluginHost.Stop(); err != nil {
		log.Warn().Err(err).Msg("plugin host stop error")
	}
	pluginBus.Close()

	// Graceful shutdown with 5-second timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// Clean up socket file.
	_ = os.Remove(socketPath)

	// Wait for all tracked daemon goroutines to exit, with a hard 10-second
	// upper bound. Logs a warning on timeout — we still exit cleanly but
	// some goroutines may have been abandoned (e.g. a plugin RPC hang).
	waitCh := make(chan struct{})
	go func() {
		shutdownWG.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
		log.Info().Msg("all daemon goroutines exited cleanly")
	case <-time.After(10 * time.Second):
		log.Warn().Msg("forced exit: daemon goroutines did not stop within 10s")
	}

	log.Info().Msg("daemon stopped")
	return nil
}

// sessionGetterAdapter wires db.SessionStore.Get into the
// upstream.SessionReader interface used by the command handler adapter.
type sessionGetterAdapter struct {
	sessions db.SessionStore
}

func (a sessionGetterAdapter) GetSession(ctx context.Context, id string) (*bossanovav1.Session, error) {
	sess, err := a.sessions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return server.SessionToProto(sess), nil
}

// automationToggleAdapter exposes db.SessionStore.Update's
// AutomationEnabled field as a narrow interface so the pause/resume
// command path doesn't need the full update surface.
type automationToggleAdapter struct {
	sessions db.SessionStore
}

func (a automationToggleAdapter) SetAutomationEnabled(ctx context.Context, sessionID string, enabled bool) error {
	_, err := a.sessions.Update(ctx, sessionID, db.UpdateSessionParams{AutomationEnabled: &enabled})
	return err
}

// attachLookupAdapter resolves a session ID to its current claude
// session ID and state — the two bits the attacher needs to decide
// whether to tail or bounce straight to SessionEnded.
type attachLookupAdapter struct {
	sessions db.SessionStore
}

func (a attachLookupAdapter) LookupAttachTarget(ctx context.Context, sessionID string) (string, int32, error) {
	sess, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return "", 0, err
	}
	claudeID := ""
	if sess.ClaudeSessionID != nil {
		claudeID = *sess.ClaudeSessionID
	}
	return claudeID, int32(sess.State), nil
}

// claudeAttachAdapter converts claude.Runner's OutputLine channel into
// the upstream-package AttachOutputLine shape so the attacher's
// interface stays free of the claude package.
type claudeAttachAdapter struct {
	runner claude.ClaudeRunner
}

func (a claudeAttachAdapter) IsRunning(claudeSessionID string) bool {
	return a.runner.IsRunning(claudeSessionID)
}

func (a claudeAttachAdapter) History(claudeSessionID string) []upstream.AttachOutputLine {
	lines := a.runner.History(claudeSessionID)
	out := make([]upstream.AttachOutputLine, len(lines))
	for i, l := range lines {
		out[i] = upstream.AttachOutputLine{Text: l.Text, Timestamp: l.Timestamp}
	}
	return out
}

func (a claudeAttachAdapter) Subscribe(ctx context.Context, claudeSessionID string) (<-chan upstream.AttachOutputLine, error) {
	ch, err := a.runner.Subscribe(ctx, claudeSessionID)
	if err != nil {
		return nil, err
	}
	out := make(chan upstream.AttachOutputLine, 64)
	go func() {
		defer close(out)
		for line := range ch {
			select {
			case out <- upstream.AttachOutputLine{Text: line.Text, Timestamp: line.Timestamp}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// streamAuthAdapter implements server.AuthNotifier by reloading
// credentials from the keychain and signalling the StreamClient to
// reconnect. The stream's own Run loop picks up the refreshed token on
// the next handshake, so the adapter only needs to refresh the
// tokenProvider cache — cancellation of the current stream attempt is
// implicit in the reconnect-on-error path.
type streamAuthAdapter struct {
	streamClient  *upstream.StreamClient
	tokenProvider *upstream.KeychainTokenProvider
	logger        zerolog.Logger
}

// NotifyLogin reloads keychain credentials and nudges the stream to
// reconnect with the fresh token. The StreamClient's outer Run loop
// treats any stream error as a reconnect trigger, so forcing a single
// token-refresh event is sufficient; a full tear-down is overkill.
func (a *streamAuthAdapter) NotifyLogin(_ context.Context, _ []string) error {
	if a.tokenProvider != nil {
		// Best-effort: the stream's outer Run loop reconnects on any
		// error, so a refresh failure here is not actionable.
		_, _ = a.tokenProvider.Refresh(context.Background())
	}
	return nil
}

// NotifyLogout is a soft signal today: the stream stays up until the
// server shuts it down via streamCancel. A future iteration can add an
// explicit "drop stream" hook; for now the JWT staleness is handled by
// the refresh loop closing the stream when bosso rejects it.
func (a *streamAuthAdapter) NotifyLogout() {}

// decodeJWTClaimsForLog extracts iss/sub/aud/exp from an unverified JWT
// for diagnostic logging. It deliberately does not validate the signature
// — it's just pulling fields out of the base64url-encoded payload so the
// log line tells us whether the token is expired, for the wrong client,
// or malformed. Returns empty strings + err on any parse failure.
func decodeJWTClaimsForLog(token string) (iss, sub, aud, exp string, err error) {
	if token == "" {
		return "", "", "", "", fmt.Errorf("empty token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", "", "", fmt.Errorf("not a JWT (%d parts)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", "", "", fmt.Errorf("base64 decode payload: %w", err)
	}
	var claims struct {
		Iss string          `json:"iss"`
		Sub string          `json:"sub"`
		Aud json.RawMessage `json:"aud"`
		Exp int64           `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", "", "", fmt.Errorf("unmarshal claims: %w", err)
	}
	expStr := ""
	if claims.Exp > 0 {
		t := time.Unix(claims.Exp, 0)
		expStr = fmt.Sprintf("%s (in %s)", t.Format(time.RFC3339), time.Until(t).Round(time.Second))
	}
	return claims.Iss, claims.Sub, string(claims.Aud), expStr, nil
}
