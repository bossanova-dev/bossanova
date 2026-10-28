package main

import (
	"context"
	"os"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/apiversion"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/timestamppb"

	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/adapter/proofenv"
	"github.com/recurser/bossd/internal/adapter/proofenvkeyring"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/broadcast"
	"github.com/recurser/bossd/internal/callback"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/notesync"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/verifyevent"
)

func (w *daemonWiring) wireUpstream() {

	// webhookEvidence stays a nil interface without an upstream, so ListRepos
	// never flags a repo on a local-only daemon.

	// noteSyncWorker drains the note sync outbox to Bosso (BOS-1435). Built
	// only inside the upstream block: a local-only daemon runs no worker and
	// its outbox rows stay pending.

	// authStateReporter stays nil in local-only mode; the GetAuthState handler
	// reads that as upstream_configured=false rather than an error.

	// Cross-daemon broadcasts (BOS-558). Both halves of the path are inert
	// without an upstream, and both are wired ONLY inside the block below:
	//
	//   - persistedDaemonID is hoisted out of that block because server.New (far
	//     below) needs the SAME resolved id the stream registers under. It is
	//     resolved exactly once; re-resolving it ad hoc would risk a second value
	//     and the ingress loop guard compares against it, so a mismatch would
	//     stop this daemon recognising its own echo.
	//   - broadcastEgress stays a nil INTERFACE with no upstream (not a typed nil
	//     pointer, which would read as non-nil at the server's `== nil` guard and
	//     turn every local send into a publish attempt).
	//
	// With no upstream both stay zero, which is exactly the fail-closed state the
	// egress predicate and the ingress loop guard were written for: an unknown
	// daemon id publishes nothing and originates nothing.

	w.webhookEventCh = make(chan session.Event, 64)
	emitter := session.NewSessionEventEmitter(&displayPollerSessionLookup{sessions: w.sessions, repos: w.repos}, w.webhookEventCh, log.Logger)
	_, upstreamURLExplicit := os.LookupEnv("BOSSD_ORCHESTRATOR_URL")
	if cfg := upstream.ConfigFromEnv(); cfg != nil {
		// Pin daemon_id to a UUID persisted under the data dir (not the
		// rotating hostname) so a hostname change doesn't orphan the old
		// id's rows in the orchestrator read model. BOSSD_DAEMON_ID still
		// wins when set; hostname remains the last-resort fallback. The same
		// call swaps cfg.Hostname for the display name config.DaemonDisplayName
		// and config.DefaultDisplayHostname derive (BOS-662, BOS-1226) —
		// presentation only, and applied after the real hostname has been
		// handed to identity resolution.
		if idErr := resolveDaemonIdentity(cfg, w.settings, os.Getenv, w.appDataDir); idErr != nil {
			log.Warn().Err(idErr).Str("daemon_id", cfg.DaemonID).Msg("stable daemon id unavailable; using fallback")
		}
		w.persistedDaemonID = cfg.DaemonID
		// EGRESS: a broadcast issued here whose audience reaches beyond this
		// daemon rides the reverse stream to bosso for routing. The bus is
		// drop-oldest, so this is best-effort by construction — the send path's
		// contract is that a publish failure never fails the RPC or the local
		// deliveries.
		w.broadcastEgress = upstream.NewBroadcastEgressPublisher(w.streamBus)

		// ConnectRPC bidi streams (DaemonStream) require HTTP/2, and the
		// daemon needs HTTP/2 keepalive so a half-open stream (laptop
		// sleep, network change) is detected and reconnected instead of
		// blocking stream.Receive() forever. Both concerns live in
		// upstream.BuildUpstreamHTTPClient, which also documents why no
		// client-level Timeout is set on these long-lived streams.
		httpClient := upstream.BuildUpstreamHTTPClient(cfg.OrchestratorURL)
		client := bossanovav1connect.NewOrchestratorServiceClient(
			httpClient,
			cfg.OrchestratorURL,
			// Stamp the API version this daemon was built against so bosso
			// keeps us on compatible behavior after the API advances.
			connect.WithInterceptors(apiversion.ClientInterceptor(apiversion.DefaultRegistry().Current())),
		)

		// Gather repo IDs for registration.
		allRepos, err := w.repos.List(context.Background())
		if err != nil {
			log.Warn().Err(err).Msg("failed to list repos for upstream registration")
		}
		repoIDs := make([]string, 0, len(allRepos))
		for _, r := range allRepos {
			repoIDs = append(repoIDs, r.ID)
		}

		// Prefer BOSSD_USER_JWT; fall back to whatever the keychain
		// holds. Empty is allowed — bosso will reject the handshake and
		// the outer Run loop will back off, but the daemon stays up in
		// local-only mode.
		tokenProvider := upstream.NewKeychainTokenProvider()
		// Give the provider the daemon logger so its credential-state
		// transition warning lands in bossd's log. Without this the provider
		// is silent by construction, which is how a daemon reloaded from an
		// already-marked record started up looking healthy (BOS-942).
		tokenProvider.SetLogger(log.Logger)
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
				// The logger rides on the context so the provider's in-window
				// replay warning is logged rather than dropped to zerolog's
				// disabled logger (BOS-941, see upstream.logRefreshReplay).
				//
				// The 10s deadline deliberately stays as it is, even though it
				// is smaller than the provider's 24s replay budget, because
				// startup blocks on this call: widening it would trade up to
				// 25s of extra daemon-startup latency for a recovery the outer
				// Run loop's reRegister and the periodic refresher — which do
				// get the whole budget — already provide within seconds.
				//
				// Truncating the budget is safe, but NOT because a failure here
				// cannot be ambiguous — a connected dispatch cut short by this
				// deadline is classified exactly as unknown. It is safe because
				// refresh() checks the caller's context before it spends a
				// replay or writes the durable sign-out marker (BOS-941), so a
				// deadline that expires mid-budget returns a plain, retryable
				// error and leaves the credentials unflagged. The only way this
				// path signs the daemon out is a genuinely exhausted budget —
				// three unconfirmed dispatches completed inside 10s — which is
				// the same verdict the full budget would have reached.
				refreshCtx, refreshCancel := context.WithTimeout(
					log.Logger.WithContext(context.Background()), 10*time.Second)
				if _, err := tokenProvider.Refresh(refreshCtx); err != nil {
					log.Warn().Err(err).Msg("proactive token refresh before register failed")
				}
				refreshCancel()
			}
			authToken = tokenProvider.Token()
		}

		// The machine identity key proves to bosso that this machine holds
		// the key pinned to cfg.DaemonID, so another account that learns the
		// id cannot re-register it (BOS-1580). Loaded once; a keyring failure
		// is logged and the daemon registers without a proof, which bosso
		// still accepts for an id that has no pinned key.
		identityKey, keyErr := upstream.LoadDaemonIdentityKey(cfg.DaemonID)
		if keyErr != nil {
			log.Warn().Err(keyErr).Str("daemon_id", cfg.DaemonID).
				Msg("daemon identity key unavailable; registering without a possession proof")
			identityKey = nil
		}

		regCtx, regCancel := context.WithTimeout(context.Background(), 10*time.Second)
		sessionToken, err := upstream.Register(regCtx, client, cfg.DaemonID, cfg.Hostname, authToken, repoIDs, identityKey)
		regCancel()
		if err != nil {
			// Non-fatal: the stream's outer Run loop sees CodeUnauthenticated
			// on its first attempt and calls reRegister, which retries with
			// whatever tokenProvider holds. After `boss login`, NotifyLogin
			// reloads the keychain so the next reRegister succeeds — no
			// daemon restart required.
			log.Warn().Err(err).Msg("upstream register failed; stream will retry via reRegister")

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
		}

		// Always wire up the stream pipeline, regardless of whether the
		// initial Register succeeded. When it failed, sessionToken is
		// empty and the stream's outer Run loop will see
		// CodeUnauthenticated on its first DaemonStream open, then call
		// reRegister to rotate in a fresh session_token. Combined with
		// streamAuthAdapter.NotifyLogin reloading the keychain, this
		// lets a fresh `boss login` recover bossd from a startup auth
		// failure without restarting the daemon.
		//
		// bosso expects BOTH credentials on the stream:
		//   Authorization: Bearer <WorkOS JWT>   — proves user identity
		//   X-Daemon-Token: <session_token>      — proves daemon identity
		// See services/bosso/internal/server/stream.go DaemonStream.

		// Snapshot readers pull from the bossd stores, projecting
		// to the slim pb types the snapshot expects.
		// Hydrate the observability overlay onto every snapshot session so the
		// cloud/web read model's initial state on (re)connect matches what the
		// local RPCs serve — a session already login-required at connect time
		// shows AGENT_AUTH_FAILED without waiting for the next auth transition.
		snapshotSessions := upstream.NewSessionSnapshotReader(protoSessionListerFunc(
			func(ctx context.Context) ([]*bossanovav1.Session, error) {
				pbSessions, err := w.sessionLister.ListSessions(ctx)
				if err != nil {
					return nil, err
				}
				// One shared wait for every restart-proof read the snapshot
				// starts; reads that outlast it republish when they land.
				wedgeCtx := w.wedgeActivityCache.ShareReadWait(ctx)
				for _, s := range pbSessions {
					w.hydrateSessionForStream(wedgeCtx, s)
				}
				return pbSessions, nil
			},
		))
		// Snapshot the canonical https://<host>/<owner>/<repo> form of each
		// repo's origin URL so it matches the identifier bosso's webhook
		// dispatcher routes by (the GitHub html_url / GitLab web_url, also
		// normalized on the receiving end). Internal DB IDs would never
		// match a webhook payload. Repos without a parseable origin
		// (local-only, malformed) drop out — they can't receive webhooks
		// anyway, so leaving them out of the snapshot's repo set is
		// strictly correct.
		snapshotRepos := upstream.NewRepoSnapshotReader(func(ctx context.Context) ([]string, error) {
			rs, err := w.repos.List(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]string, 0, len(rs))
			for _, r := range rs {
				canonical := vcs.NormalizeRepoURL(r.OriginURL)
				if canonical == "" {
					continue
				}
				out = append(out, canonical)
			}
			return out, nil
		})
		snapshotAgentAccounts := upstream.AgentAccountSnapshotReaderFunc(w.snapshotAgentAccounts)
		// Snapshot the daemon's live GitHub callback-interest set (distinct
		// repo_origin_url + pr_number over every non-terminal callback) so bosso
		// can reconcile which daemon to route a PR webhook to on every reconnect.
		// The repo_origin_url is the canonical https://github.com/<owner>/<repo>
		// form (callback.RepoOriginURL), matching the repo snapshot above and the
		// identifier bosso's webhook dispatcher routes by (vcs.GitHubNWO).
		snapshotInterests := upstream.NewCallbackInterestReader(func(ctx context.Context) ([]*bossanovav1.CallbackInterest, error) {
			return callback.DeriveInterests(ctx, w.githubCallbacks)
		})
		snapshotChats := upstream.NewChatSnapshotReader(func(ctx context.Context) ([]*bossanovav1.AgentChatMetadata, error) {
			// Routable (not tmux-only): headless runs have no tmux session
			// name, so the old ListWithTmuxSession snapshot dropped them and
			// bosso's FindDaemonForChat 404'd remote send/transcript calls
			// after a reconnect that missed the create delta.
			chats, err := w.agentChats.ListRoutableChats(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]*bossanovav1.AgentChatMetadata, 0, len(chats))
			for _, c := range chats {
				out = append(out, &bossanovav1.AgentChatMetadata{
					Id:             c.ID,
					SessionId:      c.SessionID,
					AgentSessionId: c.AgentSessionID,
					AgentName:      c.AgentName,
					Title:          c.Title,
					DaemonId:       c.DaemonID,
					CreatedAt:      timestamppb.New(c.CreatedAt),
				})
			}
			return out, nil
		})
		snapshotStatuses := upstream.NewStatusSnapshotReader(func(ctx context.Context) ([]*bossanovav1.ChatStatusEntry, error) {
			// Walk the tracker's current (non-stale) entries so a
			// freshly-connected orchestrator inherits per-chat
			// status without waiting for the next transition. The
			// Tracker.Update hook suppresses no-op heartbeats, so
			// long-lived "working" chats that haven't transitioned
			// since the daemon's last connect would otherwise be
			// invisible to bosso (and the web UI) until they next
			// change state.
			entries := w.chatStatusTracker.Snapshot()
			out := make([]*bossanovav1.ChatStatusEntry, 0, len(entries))
			for agentSessionID, e := range entries {
				st, reason := status.PromoteWaiting(e.Status, w.chatStatusTracker.Waiting(agentSessionID))
				out = append(out, &bossanovav1.ChatStatusEntry{
					AgentSessionId: agentSessionID,
					Status:         st,
					WaitingReason:  reason,
					LastOutputAt:   timestamppb.New(e.LastOutputAt),
				})
			}
			return out, nil
		})

		// Command adapters delegate back to the existing
		// lifecycle/store surfaces.
		cmdHandler := &upstream.CommandHandlerAdapter{
			AccountSync: w.accountSync,
			Lifecycle:   w.lifecycle,
			Sessions:    sessionGetterAdapter{sessions: w.sessions},
			Automation:  automationToggleAdapter{sessions: w.sessions},
			// INGRESS (BOS-558): a broadcast bosso routed here from another
			// daemon becomes LOCAL delivery rows. It goes through the SAME store,
			// resolver and Sender the SendBroadcast RPC uses — reusing the values
			// built above rather than constructing new ones is what makes "no
			// second delivery path" structural: the existing worker drains the
			// rows with no knowledge they arrived from elsewhere.
			//
			// cfg.DaemonID (the persisted id resolved just above) backs the loop
			// guard that drops a command this daemon originated. The Ingress
			// deliberately holds no egress publisher — see its file comment.
			Broadcasts: broadcast.NewIngress(
				w.broadcasts,
				broadcast.NewSender(w.broadcasts, w.broadcastResolver, log.Logger),
				w.persistedDaemonID,
				nil, // time.Now
				log.Logger,
			),
			// BOS-1418: inbound trigger launches. Creator is wired post-hoc
			// with the server (below), like Waker and Commands. The budget is
			// the worst legitimate create: a full wait for the per-target start
			// lock followed by a full bootstrap.
			Triggers: &upstream.TriggerSessionLauncher{
				Launches: w.triggerLaunches,
				Repos:    w.repos,
				Logger:   log.Logger,
				Budget:   session.TargetStartLockTimeout + session.BootstrapTimeout,
			},
			// BOS-1467: trigger gate scripts. Like the cron gate, a trigger
			// gate receives only the scoped proof model key.
			TriggerGates: &upstream.TriggerGateRunner{
				Repos:    w.repos,
				ProofEnv: w.proofEnv,
				GateEnv: func(r upstream.ProofResolver, repo *models.Repo) (string, []string) {
					return proofenv.GateEnvFor(r, repo)
				},
				Logger: log.Logger,
			},
			OnCompletion: func(ctx context.Context, sessionID string) {
				if w.orchestrator != nil {
					w.orchestrator.HandleSessionCompleted(ctx, sessionID, domain.TaskMappingStatusFailed)
				}
			},
		}
		// Surface the adapter to the outer scope so we can attach
		// the chat-waker once srv exists. Keeping the assignment here
		// (rather than inside StreamClient construction) keeps the
		// happy path uncluttered when no orchestrator is configured.
		w.cmdHandlerStream = cmdHandler

		// Attacher bridges to claude.Runner's subscribe/history.
		attacher := &upstream.SessionAttacherAdapter{
			Sessions: attachLookupAdapter{sessions: w.sessions},
			Agent:    claudeAttachAdapter{runner: w.agentRunner},
			Logger:   log.Logger,
		}

		var sessionTokenHolder *upstream.SessionTokenHolder
		var reRegisterMu sync.Mutex
		// reRegister self-heals from a stale or missing session_token
		// (another bossd with the same daemon_id rotated it via UPSERT,
		// bosso's daemons row was cleared, OR the initial Register at
		// startup failed). The Run loop calls this after a
		// CodeUnauthenticated handshake; we re-use the fresh JWT path
		// from startup (tokenProvider auto-refreshes inside the opener)
		// and gather repoIDs each call so a repo set that changed
		// since startup is reflected. The mutex serializes token issuance
		// across the stream and snapshot-publisher recovery paths because
		// bosso keeps one current session_token per daemon_id.
		reRegister := func(ctx context.Context) (string, error) {
			reRegisterMu.Lock()
			defer reRegisterMu.Unlock()

			currentRepos, err := w.repos.List(ctx)
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
			regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			tok, err := upstream.Register(regCtx, client, cfg.DaemonID, cfg.Hostname, jwt, ids, identityKey)
			if err != nil {
				return "", err
			}
			if tok != "" && sessionTokenHolder != nil {
				sessionTokenHolder.Set(tok)
			}
			return tok, nil
		}

		// Shared session_token holder: every opener that sends
		// X-Daemon-Token reads from this so a re-register-driven
		// rotation fans out to all of them. When initial Register
		// failed, sessionToken is "" — the first stream open will be
		// rejected, reRegister fires, and the holder is populated.
		sessionTokenHolder = upstream.NewSessionTokenHolder(sessionToken)
		// Shared auth state: when the stored refresh token can no longer
		// be exchanged — WorkOS rejected it, or the exchange outcome could
		// never be confirmed (BOS-659) — the opener flips this to
		// NeedsLogin and both Run loops pause until NotifyLogin clears it
		// after a fresh `boss login`. Without this, the daemon tight-loops
		// on an unusable credential indefinitely.
		authState := upstream.NewAuthState()
		// BOS-376: positive terminal-liveness signal shared by the
		// TerminalStream reader (sets it healthy on TerminalReady) and its
		// Run-loop watchdog (escalates to a forced paired re-register when a
		// run of ready-timeouts shows the stream is alive but bound to the
		// wrong bosso pod after a deploy).
		//
		// Deploy-ordering note: the readiness gate is unconditional here by
		// design — an old bosso and a new-bosso-but-wrongly-bound stream are
		// indistinguishable (both send no TerminalReady), so the daemon must
		// treat "no ready" as unhealthy for cross-pod-split detection to work
		// at all, and the ticket asks for unbounded self-heal ("instigate
		// restart until they know that they are connected"). The one cost is a
		// version skew where a NEWER daemon runs against an OLDER bosso that
		// predates WithTerminalLiveness: that bosso never sends TerminalReady,
		// so the daemon will churn its terminal stream (and periodically force
		// a DaemonStream re-register). bosso's liveness is server-side and
		// additive and ships from the same monorepo commit, so it deploys
		// ahead of any released daemon; the skew direction is a dogfooding-only
		// state the operator controls (run a matching-or-older daemon against
		// deployed bosso). See the plan's Open Questions.
		terminalHealth := upstream.NewTerminalHealth()
		// creatorAdapter drives the daemon's StreamCreateSession core for
		// reverse-stream CreateSessionCommands. Server is wired post-hoc
		// (after srv.New, below) — same pattern as cmdHandlerStream.Waker.
		w.creatorAdapter = &upstream.SessionCreatorAdapter{Logger: log.Logger}
		// webhookHealth lets the GitHub pollers drop to a slow safety-net
		// interval for repos whose webhooks reach this daemon over the stream.
		// It reads streamClient lazily: the closure runs only after the stream
		// is up, by which point the assignment below has happened.
		webhookHealth := upstream.NewWebhookHealth(func() (time.Time, bool) {
			if w.streamClient == nil {
				return time.Time{}, false
			}
			return w.streamClient.StreamOpenSince()
		})
		w.poller.SetWebhookHealth(webhookHealth)
		w.cronScheduler.SetWebhookHealth(webhookHealth)
		w.displayPoller.SetWebhookHealth(webhookHealth)
		w.callbackEvaluator.SetWebhookHealth(webhookHealth)
		w.webhookEvidence = webhookHealth
		w.streamClient = upstream.NewStreamClient(upstream.StreamClientConfig{
			Client:       client,
			AuthToken:    authToken,          // WorkOS JWT → Authorization header
			SessionToken: sessionTokenHolder, // daemon token → X-Daemon-Token header
			DaemonID:     cfg.DaemonID,
			Hostname:     cfg.Hostname,
			Stores: upstream.StreamStores{
				Sessions:      snapshotSessions,
				Chats:         snapshotChats,
				Repos:         snapshotRepos,
				Statuses:      snapshotStatuses,
				Interests:     snapshotInterests,
				AgentAccounts: snapshotAgentAccounts,
			},
			Events:         w.streamBus,
			TokenProvider:  tokenProvider,
			CommandHandler: cmdHandler,
			Webhooks: upstream.NewWebhookDispatcherWithEmitterAndReviewComments(w.displayPoller, emitter, w.ghProvider, log.Logger).
				WithEvaluator(w.callbackEvaluator).
				WithVerifyEvents(verifyevent.New(w.repos, w.cronJobs, w.callbackEvaluator, proofenvkeyring.New(log.Logger), log.Logger)).
				WithReadInvalidator(w.ghProvider).
				WithPRPoller(w.poller).
				WithWebhookHealth(webhookHealth),
			Attacher:   attacher,
			Creator:    w.creatorAdapter,
			ReRegister: reRegister,
			AuthState:  authState,
			// Every DaemonStream registration builds a fresh DaemonState
			// on bosso, stranding any terminal sender bound to the prior
			// state (2026-07-11 incident). Cycle the TerminalStream so its
			// sender rebinds to the current state. terminalStreamClient is
			// assigned just below; CycleStream is nil-safe and no
			// handshake can complete before the Run loops start.
			OnHandshake: func() { w.terminalStreamClient.CycleStream() },
			Logger:      log.Logger,
		})
		// Periodic read-model reconciliation. The bidi DaemonStream is the
		// primary feed, but delta delivery is best-effort (forwardEvent drops
		// deltas on reconnect) and not every delete path publishes, so a
		// long-lived stream's read model drifts — deleted sessions linger as
		// phantom rows in the web until the daemon reconnects and re-snapshots.
		// A periodic full-snapshot re-publish reconciles the read model via
		// ReplaceDaemonSessions. It is unary (transport-agnostic), so it works
		// even where the bidi stream is half-duplexed by an intermediary.
		if !snapshotReconcileDisabled(os.Getenv) {
			// Steady state: gentle cadence, and MUST NOT close the live
			// stream's idle connections (they share the HTTP client).
			interval := steadyStateSnapshotInterval
			closeIdle := func() {}
			if upstreamURLExplicit && snapshotFallbackEnabled(os.Getenv) {
				// Break-glass: the bidi stream can't be carried at all, so the
				// publisher is the SOLE feed — publish aggressively and reclaim
				// idle connections (no long-lived stream to disrupt).
				interval = snapshotFallbackInterval
				closeIdle = httpClient.CloseIdleConnections
			}
			w.snapshotPublisher = func(ctx context.Context) {
				runSnapshotPublisher(ctx, client, sessionTokenHolder, upstream.StreamStores{
					Sessions:      snapshotSessions,
					Chats:         snapshotChats,
					Repos:         snapshotRepos,
					Statuses:      snapshotStatuses,
					Interests:     snapshotInterests,
					AgentAccounts: snapshotAgentAccounts,
				}, cfg.DaemonID, cfg.Hostname, reRegister, closeIdle, interval, log.Logger)
			}
		}

		// Note cloud sync (BOS-1435): the same orchestrator client, daemon
		// session token holder and re-register self-heal the snapshot
		// publisher uses. Started on streamCtx below and nudged by the note
		// server after every successful write.
		w.noteSyncWorker = notesync.New(notesync.Config{
			Store:      w.noteSyncStates,
			Repos:      notesync.RepoStoreOrigins{Repos: w.repos},
			Client:     client,
			Tokens:     sessionTokenHolder,
			ReRegister: reRegister,
			Logger:     log.Logger.With().Str("component", "note-sync").Logger(),
		})

		authAdapter := &streamAuthAdapter{
			streamClient:  w.streamClient,
			tokenProvider: tokenProvider,
			authState:     authState,
			logger:        log.Logger,
			// Read-only sources for the GetAuthState diagnostic. The stream
			// client owns the wedge clock; the holder owns "when did this
			// process last successfully register".
			streamAuth:    w.streamClient,
			sessionTokens: sessionTokenHolder,
			// Proactive login-triggered register. Same closure the reactive
			// Run-loop path uses, so both serialize on reRegisterMu and write
			// the one shared sessionTokenHolder — login and auth-failure
			// recovery can never double-register or fight each other.
			reRegister: reRegister,
		}
		w.authNotifier = authAdapter
		// A nil *streamAuthAdapter stored in the AuthStateReporter INTERFACE is
		// non-nil AS an interface, so the handler's `s.authStateReporter == nil`
		// local-only check would wave it through and call straight into a nil
		// pointer — a panic inside the very diagnostic used to explain a wedged
		// daemon. The composite literal above can never be nil, so there is no
		// guard to write here; the defence that actually holds is AuthState's
		// own nil-receiver guard, which returns the zero state instead of
		// dereferencing. Keep that guard if this assignment ever moves behind a
		// constructor that can return nil.
		w.authStateReporter = authAdapter

		// TerminalStream is a sibling of DaemonStream — separate bidi
		// for keystroke / data-chunk traffic so it cannot starve
		// control-plane commands. Reuses the SAME orchestrator client,
		// AuthToken, sessionTokenHolder, TokenProvider, and AuthState so
		// a re-register-driven session_token rotation or a re-login
		// pause fans out to both streams. Idle until bosso pushes the
		// first attach.
		w.terminalStreamClient = upstream.NewTerminalStreamClient(upstream.TerminalStreamClientConfig{
			Client:        client,
			AuthToken:     authToken,
			SessionToken:  sessionTokenHolder,
			TokenProvider: tokenProvider,
			AuthState:     authState,
			TmuxClient:    w.tmuxClient,
			AttachFactory: func(ctx context.Context, cfg upstream.AttachConfig) (upstream.TerminalAttach, error) {
				return tmux.NewTerminalAttach(ctx, tmux.AttachConfig{AttachID: cfg.AttachID, SessionName: cfg.SessionName, Cols: cfg.Cols, Rows: cfg.Rows, Logger: cfg.Logger, TmuxClient: w.tmuxClient})
			},
			Chats: upstream.NewChatStoreLookup(w.agentChats),
			// BOS-661: the streamed chat-upload receiver. Nil when the
			// upload directory could not be created, which leaves every
			// upload frame answered with a permanent rejection.
			Uploads: w.chatUploadManager,
			// BOS-376 self-heal wiring. Health is the positive-liveness
			// signal; ReRegister + CloseIdle are the watchdog's paired
			// re-dial hooks. reRegister rotates the DaemonStream session
			// token and streamClient.Reconnect() wakes its Run loop, then
			// httpClient.CloseIdleConnections drops pooled HTTP/2
			// connections so the next dial of BOTH reverse streams lands on
			// one fresh connection and co-locates on a single bosso pod.
			Health: terminalHealth,
			ReRegister: func(ctx context.Context) {
				if _, err := reRegister(ctx); err != nil {
					log.Warn().Err(err).Msg("terminal watchdog: forced DaemonStream re-register failed")
				}
				w.streamClient.Reconnect()
			},
			CloseIdle: httpClient.CloseIdleConnections,
			Logger:    log.Logger,
		})
	}

}

func (w *daemonWiring) snapshotAgentAccounts(ctx context.Context) ([]*bossanovav1.AgentAccount, error) {
	accounts, err := w.accounts.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*bossanovav1.AgentAccount, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, server.AgentAccountMetadata(account))
	}
	return out, nil
}
