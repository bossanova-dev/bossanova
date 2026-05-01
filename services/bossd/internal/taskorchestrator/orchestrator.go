package taskorchestrator

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/mergepolicy"
	"github.com/recurser/bossd/internal/plugin"
)

// DefaultPollInterval is the default interval between task source polls.
const DefaultPollInterval = 2 * time.Minute

// TaskSourceProvider returns the currently active task source plugins.
// This is typically backed by plugin.Host.GetTaskSources().
type TaskSourceProvider interface {
	GetTaskSources() []plugin.TaskSource
}

// BaseBranchSyncer fast-forwards the local base branch in a main repo to
// match origin/<base>. Implemented by git.Manager; kept as a narrow
// interface here to avoid an internal/git import from the orchestrator and
// to simplify test doubles.
//
// EnsureBaseBranchReadyForSync, FetchBase, and IsAncestor participate in
// auto-merge safety: the pre-check rejects merges against a diverged local
// base, and the fetch+ancestor pair verify the PR's merge commit actually
// landed on origin/<base> before the mapping is marked complete.
type BaseBranchSyncer interface {
	SyncBaseBranch(ctx context.Context, localPath, base string) error
	EnsureBaseBranchReadyForSync(ctx context.Context, localPath, base string) error
	FetchBase(ctx context.Context, localPath, base string) error
	IsAncestor(ctx context.Context, localPath, ref, target string) (bool, error)
}

// queuedTask pairs a task item with its repo info for the FIFO queue.
type queuedTask struct {
	task       *bossanovav1.TaskItem
	repo       repoInfo
	pluginName string
}

// Orchestrator polls task source plugins for new tasks and routes
// them to the appropriate action (auto-merge, create session, etc.).
// Each repo has an in-memory FIFO queue that ensures only one task
// is processed at a time per repo.
type Orchestrator struct {
	sources         TaskSourceProvider
	repos           db.RepoStore
	taskMappings    db.TaskMappingStore
	sessionCreator  SessionCreator
	provider        vcs.Provider
	baseSyncer      BaseBranchSyncer       // optional; nil-safe
	livenessChecker SessionLivenessChecker // optional; nil-safe
	interval        time.Duration
	logger          zerolog.Logger

	mu            sync.Mutex
	queues        map[string][]queuedTask // keyed by repo ID
	active        map[string]bool         // repo ID → true if a task is being processed
	activeMapping map[string]string       // repo ID → mapping ID (for CREATE_SESSION tasks only)

	// completedSessions guards against concurrent HandleSessionCompleted for
	// the same session (sessionID → struct{}). sync.Map.LoadOrStore is
	// atomic, so callers cannot observe the intermediate "empty slot" state
	// that would enable a duplicate completion. Entries are cleared each
	// poll cycle; the DB-level terminal-status check backstops late arrivals.
	completedSessions sync.Map

	done chan struct{} // closed when Start's goroutine exits
}

// New creates a new task orchestrator. baseSyncer may be nil; when nil,
// auto-merged PRs will not refresh the main repo's local base branch.
func New(
	sources TaskSourceProvider,
	repos db.RepoStore,
	taskMappings db.TaskMappingStore,
	sessionCreator SessionCreator,
	provider vcs.Provider,
	baseSyncer BaseBranchSyncer,
	livenessChecker SessionLivenessChecker,
	interval time.Duration,
	logger zerolog.Logger,
) *Orchestrator {
	return &Orchestrator{
		sources:         sources,
		repos:           repos,
		taskMappings:    taskMappings,
		sessionCreator:  sessionCreator,
		provider:        provider,
		baseSyncer:      baseSyncer,
		livenessChecker: livenessChecker,
		interval:        interval,
		logger:          logger.With().Str("component", "task-orchestrator").Logger(),
		queues:          make(map[string][]queuedTask),
		active:          make(map[string]bool),
		activeMapping:   make(map[string]string),
		done:            make(chan struct{}),
	}
}

// Start begins the poll loop. It returns when the context is cancelled.
// Repos are staggered across the poll interval so that API calls are
// spread evenly (e.g. 5 repos with 60s interval → one repo every 12s).
func (o *Orchestrator) Start(ctx context.Context) {
	safego.Go(o.logger, func() {
		defer close(o.done)
		o.run(ctx)
	})
}

// Done returns a channel closed when Start's goroutine exits.
func (o *Orchestrator) Done() <-chan struct{} { return o.done }

func (o *Orchestrator) run(ctx context.Context) {
	ticker := time.NewTicker(o.interval)
	defer ticker.Stop()

	// Poll immediately on start.
	o.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			o.logger.Info().Msg("task orchestrator stopped")
			return
		case <-ticker.C:
			o.poll(ctx)
		}
	}
}

// poll iterates repos that have CanAutoMergeDependabot enabled and
// calls each task source plugin's PollTasks for each repo, staggering
// the calls across the interval to reduce API burst.
func (o *Orchestrator) poll(ctx context.Context) {
	// Evict stale completion guards. The concurrent window for
	// HandleSessionCompleted is milliseconds; by the next poll cycle the
	// DB status is terminal, so the DB-level guard catches late arrivals.
	o.completedSessions.Clear()

	// Retry any pending plugin updates from previous cycles.
	o.RetryPendingUpdates(ctx)

	// Recover any stuck tasks (dead Claude processes, orphaned mappings).
	o.recoverStaleTasks(ctx)

	repos, err := o.repos.List(ctx)
	if err != nil {
		o.logger.Error().Err(err).Msg("list repos")
		return
	}

	sources := o.sources.GetTaskSources()
	if len(sources) == 0 {
		return
	}

	// Filter to repos that have dependabot auto-merge enabled.
	var eligibleRepos []repoInfo
	for _, repo := range repos {
		if repo.CanAutoMergeDependabot {
			eligibleRepos = append(eligibleRepos, repoInfo{
				id:            repo.ID,
				displayName:   repo.DisplayName,
				originURL:     repo.OriginURL,
				localPath:     repo.LocalPath,
				baseBranch:    repo.DefaultBaseBranch,
				mergeStrategy: string(repo.MergeStrategy),
			})
		}
	}

	if len(eligibleRepos) == 0 {
		return
	}

	// Calculate stagger delay: spread repos evenly across the interval.
	stagger := o.interval / time.Duration(len(eligibleRepos))

	for i, repo := range eligibleRepos {
		if ctx.Err() != nil {
			return
		}

		// Stagger after the first repo.
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(stagger):
			}
		}

		for _, src := range sources {
			o.pollSource(ctx, src, repo)
		}
	}
}

// pollSource calls PollTasks on a single source for a single repo
// and processes the returned tasks.
func (o *Orchestrator) pollSource(ctx context.Context, src plugin.TaskSource, repo repoInfo) {
	info, err := src.GetInfo(ctx)
	if err != nil {
		o.logger.Warn().Err(err).
			Str("repo", repo.displayName).
			Msg("get plugin info failed")
		return
	}
	pluginName := info.GetName()

	tasks, err := src.PollTasks(ctx, repo.originURL)
	if err != nil {
		o.logger.Warn().Err(err).
			Str("repo", repo.displayName).
			Msg("poll tasks failed")
		return
	}

	for _, task := range tasks {
		o.processTask(ctx, task, repo, pluginName)
	}
}

// processTask handles a single task from a plugin. It checks for
// duplicates via the task mapping store and enqueues new tasks into
// the per-repo FIFO queue. If no task is currently active for this
// repo, the task is processed immediately.
func (o *Orchestrator) processTask(ctx context.Context, task *bossanovav1.TaskItem, repo repoInfo, pluginName string) {
	externalID := task.GetExternalId()

	// Dedup: skip if we've already seen this external ID. All statuses
	// (including Failed) are terminal — a failed task is not retried
	// automatically. This prevents FailOrphanedMappings (which marks
	// all in-progress mappings as Failed on daemon restart) from
	// triggering infinite session re-creation.
	existing, err := o.taskMappings.GetByExternalID(ctx, externalID)
	if err == nil && existing != nil {
		o.logger.Info().
			Str("external_id", externalID).
			Int("status", int(existing.Status)).
			Msg("task already tracked, skipping")
		return
	}

	o.enqueue(ctx, task, repo, pluginName)
}

// enqueue adds a task to the per-repo FIFO queue and processes it
// immediately if no other task is active for that repo.
func (o *Orchestrator) enqueue(ctx context.Context, task *bossanovav1.TaskItem, repo repoInfo, pluginName string) {
	o.mu.Lock()
	if o.active[repo.id] {
		// Deduplicate: skip if a task with the same external ID is already queued.
		for _, q := range o.queues[repo.id] {
			if q.task.GetExternalId() == task.GetExternalId() {
				o.logger.Debug().
					Str("repo", repo.displayName).
					Str("external_id", task.GetExternalId()).
					Msg("task already queued, skipping duplicate")
				o.mu.Unlock()
				return
			}
		}
		// Another task is being processed for this repo — queue it.
		o.queues[repo.id] = append(o.queues[repo.id], queuedTask{task: task, repo: repo, pluginName: pluginName})
		o.logger.Debug().
			Str("repo", repo.displayName).
			Str("external_id", task.GetExternalId()).
			Int("queue_depth", len(o.queues[repo.id])).
			Msg("task queued (repo busy)")
		o.mu.Unlock()
		return
	}
	o.active[repo.id] = true
	o.mu.Unlock()

	o.routeTask(ctx, task, repo, pluginName)
}

// dequeueNext processes the next queued task for a repo, if any.
// Called after a task completes (either immediately or via callback).
func (o *Orchestrator) dequeueNext(ctx context.Context, repoID string) {
	o.mu.Lock()
	queue := o.queues[repoID]
	if len(queue) == 0 {
		o.active[repoID] = false
		o.mu.Unlock()
		return
	}
	next := queue[0]
	o.queues[repoID] = queue[1:]
	o.mu.Unlock()

	o.routeTask(ctx, next.task, next.repo, next.pluginName)
}

// HandleSessionCompleted is called when a session finishes (merged,
// closed, or blocked). It looks up the task mapping by session ID,
// updates the plugin via UpdateTaskStatus, and dequeues the next task.
//
// This method is idempotent: duplicate calls for the same session are
// no-ops. An in-memory set (guarded by o.mu) prevents the TOCTOU race
// where two concurrent callers both read InProgress from the DB before
// either updates it. A secondary DB-level check handles the post-restart
// case where the in-memory set is empty but the mapping is already terminal.
func (o *Orchestrator) HandleSessionCompleted(ctx context.Context, sessionID string, outcome models.TaskMappingStatus) {
	mapping, err := o.taskMappings.GetBySessionID(ctx, sessionID)
	if err != nil {
		// Not all sessions are task-orchestrated; this is expected.
		return
	}

	// Fast in-memory guard: serialise concurrent callers for the same session.
	// LoadOrStore is atomic — the first caller stores and proceeds, any
	// concurrent caller observes the existing entry and bails. This replaces
	// the previous map+mutex and keeps o.mu free for queue operations.
	if _, loaded := o.completedSessions.LoadOrStore(sessionID, struct{}{}); loaded {
		o.logger.Debug().
			Str("session", sessionID).
			Msg("session completion already processed (in-memory guard)")
		return
	}

	// Belt-and-suspenders DB guard for post-restart: the in-memory set is
	// empty after a daemon restart, but the mapping status is already terminal.
	switch mapping.Status {
	case models.TaskMappingStatusCompleted, models.TaskMappingStatusFailed, models.TaskMappingStatusSkipped:
		o.logger.Debug().
			Str("session", sessionID).
			Str("external_id", mapping.ExternalID).
			Int("existing_status", int(mapping.Status)).
			Int("new_outcome", int(outcome)).
			Msg("session already completed, skipping duplicate notification")
		return
	default:
		// Pending or InProgress — proceed with completion.
	}

	o.logger.Info().
		Str("session", sessionID).
		Str("external_id", mapping.ExternalID).
		Int("outcome", int(outcome)).
		Msg("session completed, updating task status")

	// Update the mapping status.
	o.updateMappingStatus(ctx, mapping.ID, outcome)

	// Notify the plugin about the task outcome.
	pluginStatus := taskMappingStatusToProto(outcome)
	sources := o.sources.GetTaskSources()
	for _, src := range sources {
		if err := src.UpdateTaskStatus(ctx, mapping.ExternalID, pluginStatus, ""); err != nil {
			o.logger.Warn().Err(err).
				Str("external_id", mapping.ExternalID).
				Msg("failed to update plugin task status, storing as pending")

			// Store as pending for retry.
			pendingStatus := outcome
			pendingStatusPtr := &pendingStatus
			if _, err := o.taskMappings.Update(ctx, mapping.ID, db.UpdateTaskMappingParams{
				PendingUpdateStatus: &pendingStatusPtr,
			}); err != nil {
				o.logger.Error().Err(err).Str("mapping", mapping.ID).Msg("store pending update failed")
			}
			continue
		}
		break
	}

	// Clear active mapping before dequeuing so the recovery sweep
	// doesn't re-process this mapping. Guard against deleting a
	// newer mapping that replaced ours while we were completing.
	o.mu.Lock()
	if o.activeMapping[mapping.RepoID] == mapping.ID {
		delete(o.activeMapping, mapping.RepoID)
	}
	o.mu.Unlock()

	// Process the next queued task for this repo.
	o.dequeueNext(ctx, mapping.RepoID)
}

// RetryPendingUpdates retries any task mapping updates that failed
// on the previous attempt (e.g. due to plugin crash).
func (o *Orchestrator) RetryPendingUpdates(ctx context.Context) {
	pending, err := o.taskMappings.ListPending(ctx)
	if err != nil {
		o.logger.Error().Err(err).Msg("list pending task mappings")
		return
	}

	sources := o.sources.GetTaskSources()
	for _, mapping := range pending {
		if mapping.PendingUpdateStatus == nil {
			continue
		}

		pluginStatus := taskMappingStatusToProto(*mapping.PendingUpdateStatus)
		details := ""
		if mapping.PendingUpdateDetails != nil {
			details = *mapping.PendingUpdateDetails
		}

		for _, src := range sources {
			if err := src.UpdateTaskStatus(ctx, mapping.ExternalID, pluginStatus, details); err != nil {
				o.logger.Warn().Err(err).
					Str("external_id", mapping.ExternalID).
					Msg("retry pending update still failing")
				continue
			}

			// Success — clear pending fields.
			var nilStatus *models.TaskMappingStatus
			var nilDetails *string
			if _, err := o.taskMappings.Update(ctx, mapping.ID, db.UpdateTaskMappingParams{
				Status:               mapping.PendingUpdateStatus,
				PendingUpdateStatus:  &nilStatus,
				PendingUpdateDetails: &nilDetails,
			}); err != nil {
				o.logger.Error().Err(err).Str("mapping", mapping.ID).Msg("clear pending update failed")
			}
			break
		}
	}
}

// recoverStaleTasks checks for CREATE_SESSION tasks that are stuck
// (dead Claude process, orphaned mapping) and forces queue advancement.
// This is called at the start of each poll cycle as a safety net.
func (o *Orchestrator) recoverStaleTasks(ctx context.Context) {
	o.mu.Lock()
	// Snapshot the active mappings so we don't hold the lock during DB calls.
	activeMappings := make(map[string]string, len(o.activeMapping))
	for repoID, mappingID := range o.activeMapping {
		activeMappings[repoID] = mappingID
	}
	o.mu.Unlock()

	for repoID, mappingID := range activeMappings {
		// Re-check under lock that the mapping hasn't changed since we
		// took the snapshot. A concurrent HandleSessionCompleted may have
		// already completed this mapping and started a new task, which
		// would set a different mapping ID for the same repo.
		o.mu.Lock()
		currentMappingID, stillActive := o.activeMapping[repoID]
		o.mu.Unlock()
		if !stillActive || currentMappingID != mappingID {
			// A concurrent completion already handled this mapping.
			continue
		}

		mapping, err := o.taskMappings.Get(ctx, mappingID)
		if err != nil {
			// Mapping not found — it was deleted or the DB is inconsistent.
			// Clear the active state and advance the queue.
			o.logger.Warn().
				Str("repo", repoID).
				Str("mapping", mappingID).
				Msg("active mapping not found, clearing stale state")
			o.mu.Lock()
			// Double-check: only clear if the mapping hasn't been replaced.
			if o.activeMapping[repoID] == mappingID {
				delete(o.activeMapping, repoID)
			}
			o.mu.Unlock()
			o.dequeueNext(ctx, repoID)
			continue
		}

		switch mapping.Status {
		case models.TaskMappingStatusCompleted, models.TaskMappingStatusFailed, models.TaskMappingStatusSkipped:
			// Already terminal — clear active state and advance.
			o.logger.Info().
				Str("repo", repoID).
				Str("mapping", mappingID).
				Int("status", int(mapping.Status)).
				Msg("active mapping already terminal, advancing queue")
			o.mu.Lock()
			if o.activeMapping[repoID] == mappingID {
				delete(o.activeMapping, repoID)
			}
			o.mu.Unlock()
			o.dequeueNext(ctx, repoID)

		case models.TaskMappingStatusInProgress:
			if mapping.SessionID != nil {
				// Has a session — check if it's still alive.
				if o.livenessChecker != nil && !o.livenessChecker.IsSessionAlive(ctx, *mapping.SessionID) {
					o.logger.Warn().
						Str("repo", repoID).
						Str("mapping", mappingID).
						Str("session", *mapping.SessionID).
						Msg("session dead, recovering stuck task")
					// Reuse the existing idempotent completion flow.
					o.HandleSessionCompleted(ctx, *mapping.SessionID, models.TaskMappingStatusFailed)
				}
			} else {
				// InProgress with no session — orphaned. Mark failed and advance.
				o.logger.Warn().
					Str("repo", repoID).
					Str("mapping", mappingID).
					Msg("in-progress mapping with no session, marking failed")
				o.updateMappingStatus(ctx, mapping.ID, models.TaskMappingStatusFailed)
				o.mu.Lock()
				if o.activeMapping[repoID] == mappingID {
					delete(o.activeMapping, repoID)
				}
				o.mu.Unlock()
				o.dequeueNext(ctx, repoID)
			}

		default:
			// Pending — shouldn't be in activeMapping. Mark failed and advance.
			o.logger.Warn().
				Str("repo", repoID).
				Str("mapping", mappingID).
				Msg("pending mapping in active state, marking failed")
			o.updateMappingStatus(ctx, mapping.ID, models.TaskMappingStatusFailed)
			o.mu.Lock()
			if o.activeMapping[repoID] == mappingID {
				delete(o.activeMapping, repoID)
			}
			o.mu.Unlock()
			o.dequeueNext(ctx, repoID)
		}
	}
}

// taskMappingStatusToProto converts an internal task mapping status
// to a proto TaskItemStatus.
func taskMappingStatusToProto(s models.TaskMappingStatus) bossanovav1.TaskItemStatus {
	switch s {
	case models.TaskMappingStatusCompleted:
		return bossanovav1.TaskItemStatus_TASK_ITEM_STATUS_COMPLETED
	case models.TaskMappingStatusFailed:
		return bossanovav1.TaskItemStatus_TASK_ITEM_STATUS_FAILED
	case models.TaskMappingStatusInProgress:
		return bossanovav1.TaskItemStatus_TASK_ITEM_STATUS_IN_PROGRESS
	default:
		return bossanovav1.TaskItemStatus_TASK_ITEM_STATUS_UNSPECIFIED
	}
}

// routeTask dispatches a task based on its action, creates a task
// mapping for dedup, and performs the action.
func (o *Orchestrator) routeTask(ctx context.Context, task *bossanovav1.TaskItem, repo repoInfo, pluginName string) {
	action := task.GetAction()
	externalID := task.GetExternalId()

	// Treat UNSPECIFIED as CREATE_SESSION per proto spec.
	if action == bossanovav1.TaskAction_TASK_ACTION_UNSPECIFIED {
		action = bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION
	}

	o.logger.Info().
		Str("external_id", externalID).
		Str("action", action.String()).
		Str("title", task.GetTitle()).
		Str("repo", repo.displayName).
		Msg("routing task")

	// Create task mapping to prevent duplicate processing.
	mapping, err := o.taskMappings.Create(ctx, db.CreateTaskMappingParams{
		ExternalID: externalID,
		PluginName: pluginName,
		RepoID:     repo.id,
	})
	if err != nil {
		o.logger.Error().Err(err).
			Str("external_id", externalID).
			Msg("create task mapping failed")
		o.dequeueNext(ctx, repo.id)
		return
	}

	switch action {
	case bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE:
		o.handleAutoMerge(ctx, task, repo, mapping)

	case bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION:
		o.handleCreateSession(ctx, task, repo, mapping)

	case bossanovav1.TaskAction_TASK_ACTION_NOTIFY_USER:
		o.handleNotifyUser(ctx, task, repo, mapping)

	case bossanovav1.TaskAction_TASK_ACTION_UNSPECIFIED:
		// Unreachable: UNSPECIFIED is converted to CREATE_SESSION above.
	}
}

// handleAutoMerge merges a PR directly without creating a Claude session.
// Auto-merge completes synchronously, so dequeue the next task immediately.
func (o *Orchestrator) handleAutoMerge(ctx context.Context, task *bossanovav1.TaskItem, repo repoInfo, mapping *models.TaskMapping) {
	defer o.dequeueNext(ctx, repo.id)

	prNumber, err := parsePRNumberFromExternalID(task.GetExternalId())
	if err != nil {
		o.logger.Error().Err(err).
			Str("external_id", task.GetExternalId()).
			Msg("cannot parse PR number for auto-merge")
		o.updateMappingStatus(ctx, mapping.ID, models.TaskMappingStatusFailed)
		return
	}

	o.logger.Info().
		Int("pr", prNumber).
		Str("repo", repo.displayName).
		Msg("auto-merging PR")

	// Mirror MergeSession's pre-merge invariant: refuse to auto-merge when
	// the local base has diverged from origin. Dependabot runs unattended,
	// so surprises here compound silently across many PRs — fail loud and
	// early instead.
	if o.baseSyncer != nil && repo.localPath != "" && repo.baseBranch != "" {
		if err := o.baseSyncer.EnsureBaseBranchReadyForSync(ctx, repo.localPath, repo.baseBranch); err != nil {
			o.logger.Error().Err(err).
				Int("pr", prNumber).
				Str("repo", repo.displayName).
				Str("local_path", repo.localPath).
				Str("base", repo.baseBranch).
				Msg("auto-merge aborted: local base branch not ready for sync")
			o.updateMappingStatus(ctx, mapping.ID, models.TaskMappingStatusFailed)
			return
		}
	}

	strategy, err := mergepolicy.ResolveStrategy(ctx, o.provider, repo.originURL, repo.mergeStrategy)
	if err != nil {
		o.logger.Error().Err(err).
			Int("pr", prNumber).
			Str("repo", repo.displayName).
			Msg("auto-merge aborted: no merge strategy available")
		o.updateMappingStatus(ctx, mapping.ID, models.TaskMappingStatusFailed)
		return
	}

	if err := o.provider.MergePR(ctx, repo.originURL, prNumber, strategy); err != nil {
		o.logger.Error().Err(err).
			Int("pr", prNumber).
			Str("repo", repo.displayName).
			Msg("auto-merge failed")
		o.updateMappingStatus(ctx, mapping.ID, models.TaskMappingStatusFailed)
		return
	}

	// Verify the merge commit is on origin/<base>. If it isn't, gh lied or
	// something rewrote history — mark the mapping failed so a human looks.
	if o.baseSyncer != nil && repo.localPath != "" && repo.baseBranch != "" {
		if err := mergepolicy.VerifyOnBase(ctx, o.provider, o.baseSyncer, repo.localPath, repo.originURL, repo.baseBranch, prNumber); err != nil {
			o.logger.Error().Err(err).
				Int("pr", prNumber).
				Str("repo", repo.displayName).
				Str("base", repo.baseBranch).
				Msg("auto-merge verification failed: commit not on base after merge")
			o.updateMappingStatus(ctx, mapping.ID, models.TaskMappingStatusFailed)
			return
		}
	}

	// Refresh the user's local main repo so subsequent worktrees and
	// manual operations on <base> start from the post-merge tip.
	// Auto-merge runs unattended, so a sync failure logs a warning
	// rather than failing the task — the server-side merge is already done.
	if o.baseSyncer != nil && repo.localPath != "" && repo.baseBranch != "" {
		if err := o.baseSyncer.SyncBaseBranch(ctx, repo.localPath, repo.baseBranch); err != nil {
			o.logger.Warn().Err(err).
				Int("pr", prNumber).
				Str("repo", repo.displayName).
				Str("local_path", repo.localPath).
				Str("base", repo.baseBranch).
				Msg("post-merge sync of local base branch failed; user can run `git fetch` manually")
		}
	}

	o.logger.Info().
		Int("pr", prNumber).
		Str("repo", repo.displayName).
		Str("strategy", strategy).
		Msg("PR auto-merged successfully")
	o.updateMappingStatus(ctx, mapping.ID, models.TaskMappingStatusCompleted)
}

// handleCreateSession creates a Claude Code session to fix a failing PR.
// The session runs asynchronously — HandleSessionCompleted dequeues the
// next task when it finishes. We only dequeue here on the error path so
// the queue advances if session creation fails.
func (o *Orchestrator) handleCreateSession(ctx context.Context, task *bossanovav1.TaskItem, repo repoInfo, mapping *models.TaskMapping) {
	// The task's base_branch is advisory: plugins that don't know the repo
	// configuration set the repo's default branch (or "main" as a historical
	// fallback). Always prefer the repo's configured default so repos with
	// non-"main" defaults (master, develop, trunk) aren't silently wrong.
	baseBranch := repo.baseBranch
	if baseBranch == "" {
		baseBranch = task.GetBaseBranch()
	}
	if baseBranch == "" {
		baseBranch = "main"
	}

	opts := CreateSessionOpts{
		RepoID:          repo.id,
		Title:           task.GetTitle(),
		Plan:            task.GetPlan(),
		BaseBranch:      baseBranch,
		HeadBranch:      task.GetExistingBranch(),
		SkipSetupScript: slices.Contains(task.GetLabels(), "dependabot"),
	}

	// Extract PR number from the external ID so the session displays
	// a clickable PR link in the TUI session list.
	if prNumber, err := parsePRNumberFromExternalID(task.GetExternalId()); err == nil {
		opts.PRNumber = &prNumber
		if nwo := vcs.GitHubNWO(task.GetRepoOriginUrl()); nwo != "" {
			prURL := fmt.Sprintf("https://github.com/%s/pull/%d", nwo, prNumber)
			opts.PRURL = &prURL
		}
	}

	sess, err := o.sessionCreator.CreateSession(ctx, opts)
	if err != nil {
		o.logger.Error().Err(err).
			Str("external_id", task.GetExternalId()).
			Str("title", task.GetTitle()).
			Msg("create session failed")
		o.updateMappingStatus(ctx, mapping.ID, models.TaskMappingStatusFailed)
		o.dequeueNext(ctx, repo.id)
		return
	}

	// Link the task mapping to the session.
	sessionID := sess.ID
	sessionIDPtr := &sessionID
	status := models.TaskMappingStatusInProgress
	if _, err := o.taskMappings.Update(ctx, mapping.ID, db.UpdateTaskMappingParams{
		SessionID: &sessionIDPtr,
		Status:    &status,
	}); err != nil {
		o.logger.Error().Err(err).
			Str("mapping", mapping.ID).
			Msg("update task mapping with session ID failed")
	}

	// Track the active mapping so the recovery sweep can detect stuck tasks.
	o.mu.Lock()
	o.activeMapping[repo.id] = mapping.ID
	o.mu.Unlock()

	o.logger.Info().
		Str("session", sess.ID).
		Str("external_id", task.GetExternalId()).
		Str("title", task.GetTitle()).
		Msg("session created for task")
}

// handleNotifyUser logs the task for user notification. In the future
// this could send a notification via the TUI or an external channel.
// Notify completes synchronously, so dequeue the next task immediately.
func (o *Orchestrator) handleNotifyUser(ctx context.Context, task *bossanovav1.TaskItem, repo repoInfo, mapping *models.TaskMapping) {
	defer o.dequeueNext(ctx, repo.id)

	o.logger.Info().
		Str("external_id", task.GetExternalId()).
		Str("title", task.GetTitle()).
		Str("repo", repo.displayName).
		Msg("task requires user attention (skipping)")
	o.updateMappingStatus(ctx, mapping.ID, models.TaskMappingStatusSkipped)
}

// updateMappingStatus is a helper to update a task mapping's status.
func (o *Orchestrator) updateMappingStatus(ctx context.Context, mappingID string, status models.TaskMappingStatus) {
	if _, err := o.taskMappings.Update(ctx, mappingID, db.UpdateTaskMappingParams{
		Status: &status,
	}); err != nil {
		o.logger.Error().Err(err).
			Str("mapping", mappingID).
			Msg("update task mapping status failed")
	}
}

// parsePRNumberFromExternalID extracts the PR number from an external
// ID formatted as "plugin:pr:<repoURL>:<prNumber>".
func parsePRNumberFromExternalID(externalID string) (int, error) {
	parts := strings.Split(externalID, ":")
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid external ID format: %s", externalID)
	}
	last := parts[len(parts)-1]
	n, err := strconv.Atoi(last)
	if err != nil {
		return 0, fmt.Errorf("cannot parse PR number from %q: %w", externalID, err)
	}
	return n, nil
}

// repoInfo is a lightweight struct carrying the info needed to poll
// a repo, avoiding passing the full models.Repo around.
type repoInfo struct {
	id            string
	displayName   string
	originURL     string
	localPath     string
	baseBranch    string
	mergeStrategy string
}
