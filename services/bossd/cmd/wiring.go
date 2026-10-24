package main

import (
	"context"
	"sync/atomic"
	"time"

	plugindoor "github.com/recurser/bossd/api/plugin"
	"github.com/recurser/bossd/api/proxy"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonstate"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"

	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/api/hooks"
	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/adapter/accountcred"
	gitpkg "github.com/recurser/bossd/internal/adapter/git"
	"github.com/recurser/bossd/internal/adapter/github"
	"github.com/recurser/bossd/internal/adapter/proofenvkeyring"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/broadcast"
	"github.com/recurser/bossd/internal/callback"
	"github.com/recurser/bossd/internal/chatupload"
	cronpkg "github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain/questionsignal"
	daemontelemetry "github.com/recurser/bossd/internal/domain/telemetry"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/notesync"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/eventbus"
	"github.com/recurser/bossd/internal/resume"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/taskorchestrator"
)

// daemonWiring holds the daemon-owned dependencies shared by startup stages.
// It is allocated once and never copied: callbacks capture the same slots that
// later stages populate, including the atomic server publications.
type daemonWiring struct {
	opts                           runOpts
	alreadyRunning                 bool
	cleanups                       []func()
	accountCredMaintenance         *accountwiring.CredentialMaintainer
	accountCreds                   *accountcred.Store
	accountMaterializations        server.AgentAccountMaterializations
	accountMaterializer            account.Materializer
	accountResolver                *account.Resolver
	accountSmokeHook               server.AgentAccountSmokeRunner
	accountSpawnEnv                *accountwiring.SpawnEnvResolver
	accountUsageProbe              server.UsageProbeRecorder
	accounts                       *db.SQLiteAgentAccountStore
	agentChats                     *upstream.NotifyingAgentChatStore
	agentClients                   map[string]agent.RunnerClient
	agentLogsDir                   string
	agentRunner                    agent.RoutingRunner
	agentRuns                      *db.SQLiteAgentRunStore
	appDataDir                     string
	archiveSeam                    atomic.Pointer[server.Server]
	authNotifier                   server.AuthNotifier
	authStateReporter              server.AuthStateReporter
	bossEnv                        string
	broadcastEgress                broadcast.EgressPublisher
	broadcastResolver              *broadcast.Resolver
	broadcastSubscriptionEvaluator *broadcast.SubscriptionEvaluator
	broadcastSubscriptions         *db.SQLiteBroadcastSubscriptionStore
	broadcasts                     *db.SQLiteBroadcastStore
	callbackEvaluator              *callback.Evaluator
	callbackRetirer                *callback.Retirer
	chatRotator                    *rotation.ChatRotator
	chatStatusTracker              *status.Tracker
	chatUploadManager              upstream.UploadManager
	chatUploadMgr                  *chatupload.Manager
	chatUploads                    *chatUploadSender
	chatWorking                    func(agentSessionID string) bool
	checkSnapshots                 *db.SQLiteCheckSnapshotStore
	pendingTrackerMerges           db.PendingTrackerMergeStore
	proxyTokens                    db.ProxyTokenStore
	triggerLaunches                db.TriggerLaunchStore
	cmdHandlerStream               *upstream.CommandHandlerAdapter
	creatorAdapter                 *upstream.SessionCreatorAdapter
	cronActivity                   *session.CronActivityChecker
	cronGate                       *session.CronCompletionGate
	cronJobs                       *db.SQLiteCronJobStore
	cronScheduler                  *cronpkg.Scheduler
	daemonMetadata                 daemonstate.Metadata
	dispatcher                     *session.Dispatcher
	displayComputer                *status.DisplayStatusComputer
	displayPoller                  *session.DisplayPoller
	displayTracker                 *status.DisplayTracker
	drainArchiveWorkers            func()
	errCh                          chan error
	evictionRecomputeHookInstalled bool
	evidenceReader                 *evidence.Reader
	executablePath                 string
	ghProvider                     *github.Provider
	githubCallbacks                *db.SQLiteGitHubCallbackStore
	goroutineBudget                time.Duration
	hookSrv                        *hooks.HookServer
	hostService                    *plugindoor.HostServiceServer
	hydrateSessionForStream        func(ctx context.Context, pbSess *bossanovav1.Session)
	lifecycle                      *session.Lifecycle
	livenessChecker                taskorchestrator.SessionLivenessChecker
	noteSyncStates                 *db.SQLiteNoteSyncStore
	noteSyncWorker                 *notesync.Worker
	notes                          *db.SQLiteNoteStore
	orchestrator                   *taskorchestrator.Orchestrator
	persistedDaemonID              string
	pluginBus                      *eventbus.Bus
	pluginErrorTrackingDSN         string
	pluginHost                     *plugin.Host
	poller                         *session.Poller
	pollerCancel                   context.CancelFunc
	pollerCtx                      context.Context
	prAssociationResolver          *session.PRAssociationResolver
	proofEnv                       *proofenvkeyring.Resolver
	proxySrv                       *proxy.ProxyServer
	publishStoreSessionUpdate      func(ctx context.Context, sess *models.Session)
	questionSignals                *questionsignal.Store
	rawSessions                    *db.SQLiteSessionStore
	repairLease                    *status.RepairLeaseManager
	repos                          *db.SQLiteRepoStore
	resumeAttempts                 *db.SQLiteResumeAttemptStore
	rotationEngine                 *rotation.Engine
	rotationEvents                 *db.SQLiteRotationEventStore
	rotationRecorder               *rotation.Recorder
	sessionLister                  *sessionListerAdapter
	sessions                       db.SessionStore
	settings                       config.Settings
	severedStreamIDs               []string
	shutdownWG                     shutdownTracker
	snapshotPublisher              func(context.Context)
	socketPath                     string
	srv                            *server.Server
	startupDiagnosticWorkerDone    []<-chan struct{}
	startupProtectedRoots          []string
	streamAttentionServer          atomic.Pointer[server.Server]
	streamBus                      *upstream.StreamBus
	streamCancel                   context.CancelFunc
	streamClient                   *upstream.StreamClient
	streamHydrator                 *streamSessionHydrator
	taskMappings                   *db.SQLiteTaskMappingStore
	telemetryClient                *daemontelemetry.Client
	terminalReapObserver           *status.TerminalReapObserver
	terminalStreamClient           *upstream.TerminalStreamClient
	tmuxClient                     *tmux.Client
	tmuxDaemonID                   string
	tmuxStatusPoller               *status.TmuxStatusPoller
	trackArchiveDone               func(sessionID string, done <-chan struct{})
	trackDone                      func(name string, done <-chan struct{})
	trackedGo                      func(name string, fn func())
	trackerLinker                  *session.TrackerLinker
	transientResumeHookInstalled   bool
	transientResumer               *resume.TransientResumer
	webhookEventCh                 chan session.Event
	webhookEvidence                server.WebhookEvidence
	wedgeActivityCache             *server.WedgeActivityCache
	worktrees                      *gitpkg.Manager
}

// deferCleanup keeps resource teardown at run scope instead of stage scope.
func (w *daemonWiring) deferCleanup(cleanup func()) { w.cleanups = append(w.cleanups, cleanup) }
func (w *daemonWiring) close() {
	for _, cleanup := range w.cleanups {
		defer cleanup()
	}
}
