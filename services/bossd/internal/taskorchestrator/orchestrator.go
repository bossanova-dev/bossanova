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
	"github.com/recurser/bossd/internal/plugin"
)

// DefaultPollInterval is the default interval between task source polls.
const DefaultPollInterval = 2 * time.Minute

// TaskSourceProvider returns the currently active task source plugins.
// This is typically backed by plugin.Host.GetTaskSources().
type TaskSourceProvider interface {
	GetTaskSources() []plugin.TaskSource
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
	sources        TaskSourceProvider
	repos          db.RepoStore
	taskMappings   db.TaskMappingStore
	sessionCreator SessionCreator
	provider       vcs.Provider
	interval       time.Duration
	logger         zerolog.Logger

	mu     sync.Mutex
	queues map[string][]queuedTask // keyed by repo ID
	active map[string]bool         // repo ID → true if a task is being processed
}

// New creates a new task orchestrator.
func New(
	sources TaskSourceProvider,
	repos db.RepoStore,
	taskMappings db.TaskMappingStore,
	sessionCreator SessionCreator,
	provider vcs.Provider,
	interval time.Duration,
	logger zerolog.Logger,
) *Orchestrator {
	return &Orchestrator{
		sources:        sources,
		repos:          repos,
		taskMappings:   taskMappings,
		sessionCreator: sessionCreator,
		provider:       provider,
		interval:       interval,
		logger:         logger.With().Str("component", "task-orchestrator").Logger(),
		queues:         make(map[string][]queuedTask),
		active:         make(map[string]bool),
	}
}

// Start begins the poll loop. It returns when the context is cancelled.
// Repos are staggered across the poll interval so that API calls are
// spread evenly (e.g. 5 repos with 60s interval → one repo every 12s).
func (o *Orchestrator) Start(ctx context.Context) {
	safego.Go(o.logger, func() {
		o.run(ctx)
	})
}

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
	// Retry any pending plugin updates from previous cycles.
	o.RetryPendingUpdates(ctx)

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

	o.logger.Info().
		Str("repo", repo.displayName).
		Str("plugin", pluginName).
		Int("tasks", len(tasks)).
		Msg("poll complete")

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

	// Dedup: skip if we've already seen this external ID, unless the
	// previous attempt failed — in that case delete the old mapping so
	// the task is retried.
	existing, err := o.taskMappings.GetByExternalID(ctx, externalID)
	if err == nil && existing != nil {
		if existing.Status == models.TaskMappingStatusFailed {
			o.logger.Info().
				Str("external_id", externalID).
				Msg("previous attempt failed, retrying task")
			if err := o.taskMappings.Delete(ctx, existing.ID); err != nil {
				o.logger.Error().Err(err).
					Str("external_id", externalID).
					Msg("delete failed task mapping")
				return
			}
		} else {
			o.logger.Info().
				Str("external_id", externalID).
				Int("status", int(existing.Status)).
				Msg("task already tracked, skipping")
			return
		}
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
func (o *Orchestrator) HandleSessionCompleted(ctx context.Context, sessionID string, outcome models.TaskMappingStatus) {
	mapping, err := o.taskMappings.GetBySessionID(ctx, sessionID)
	if err != nil {
		// Not all sessions are task-orchestrated; this is expected.
		return
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

	if err := o.provider.MergePR(ctx, repo.originURL, prNumber, repo.mergeStrategy); err != nil {
		o.logger.Error().Err(err).
			Int("pr", prNumber).
			Str("repo", repo.displayName).
			Msg("auto-merge failed")
		o.updateMappingStatus(ctx, mapping.ID, models.TaskMappingStatusFailed)
		return
	}

	o.logger.Info().
		Int("pr", prNumber).
		Str("repo", repo.displayName).
		Msg("PR auto-merged successfully")
	o.updateMappingStatus(ctx, mapping.ID, models.TaskMappingStatusCompleted)
}

// handleCreateSession creates a Claude Code session to fix a failing PR.
// The session runs asynchronously — HandleSessionCompleted dequeues the
// next task when it finishes. We only dequeue here on the error path so
// the queue advances if session creation fails.
func (o *Orchestrator) handleCreateSession(ctx context.Context, task *bossanovav1.TaskItem, repo repoInfo, mapping *models.TaskMapping) {
	baseBranch := task.GetBaseBranch()
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
	mergeStrategy string
}
