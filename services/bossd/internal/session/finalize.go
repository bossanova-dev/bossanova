package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	libvcs "github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

// finalizeChatSkill is the agent command the finalize chat is launched with.
// boss-finalize handles end-of-run cleanup (review summary, push, status
// update) so the cron-spawned worktree closes cleanly.
const finalizeChatSkill = "boss-finalize"

// FinalizeResult is the outcome of a FinalizeSession call. Outcome maps 1:1
// to the cron_jobs.last_run_outcome column FinalizeSession writes. NoOp is
// true when the conditional state transition no-op'd (duplicate Stop event
// or a session that never reached ImplementingPlan) — the hook endpoint
// returns 200 either way, but the caller uses this to log the distinction.
type FinalizeResult struct {
	Outcome models.CronJobOutcome
	NoOp    bool
	// Err, when non-nil, is the underlying failure behind a *_failed outcome.
	// It is not returned as a top-level error so the hook endpoint still
	// reports success — the outcome column is already recorded.
	Err error
}

// FinalizeSession runs the Stop-hook finalize pipeline for a cron-spawned
// session. It is idempotent: duplicate Stop events no-op via a conditional
// state transition (ImplementingPlan → Finalizing) guarded by rows_affected.
//
// Outcome classification, in the order it's evaluated:
//   - pr_created               — empty git status but branch already has an open PR, or branch has committed work that needs a PR
//   - deleted_no_changes       — empty git status and no committed branch work → worktree + session removed
//   - cleanup_failed           — empty status but worktree removal errored
//   - pr_skipped_no_github     — changes present, origin is not GitHub
//   - pr_failed                — dirty output present but the PR could not be opened for the finalize chat
//   - chat_spawn_failed        — dirty output or PR-backed branch could not start /boss-finalize chat
//   - pr_created               — dirty output: a PR was opened (placeholder commit added when the branch had none) and the /boss-finalize chat started
//   - pr_failed                — clean committed branch could not open or attach a PR
//   - chat_spawn_failed        — clean committed branch opened/attached a PR but /boss-finalize chat start failed
//   - pr_created               — clean committed branch opened/attached a PR and started finalize chat
//
// After the outcome is classified, FinalizeSession writes
// cron_jobs.last_run_outcome (step 4) and, on the pr_created success path,
// clears session.hook_token so a replayed Stop event can no longer
// authenticate against this session (step 5). Failure outcomes also fire the
// Block state-machine event so the session shows up as attention-needed in
// the UI — the Finalizing state itself is intentionally silent per
// vcs/attention.go.
func (l *Lifecycle) FinalizeSession(ctx context.Context, sessionID string) (*FinalizeResult, error) {
	// Step 1: conditional state transition. The rows_affected guard is the
	// authoritative idempotency mechanism — a check-then-set Go path would
	// race with concurrent Stop events.
	advanced, err := l.sessions.UpdateStateConditional(
		ctx, sessionID, int(machine.Finalizing), int(machine.ImplementingPlan),
	)
	if err != nil {
		return nil, fmt.Errorf("advance to finalizing: %w", err)
	}
	if !advanced {
		return &FinalizeResult{NoOp: true}, nil
	}

	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("worktree", session.WorktreePath).
		Msg("finalizing session")

	// Step 2 + 3: classify outcome by examining the worktree.
	result := l.classifyFinalizeOutcome(ctx, session)

	// Step 4: record the outcome on the cron job row. Non-cron sessions
	// (hypothetical — the hook is only wired into cron-spawned worktrees)
	// are skipped rather than treated as errors.
	if session.CronJobID != nil && *session.CronJobID != "" && l.cronJobs != nil {
		ranAt := time.Now()

		// For deleted_no_changes, the session row was already deleted by
		// finalizeNoChanges. SQLite's ON DELETE SET NULL cascade has already
		// nulled out cron_jobs.last_run_session_id; passing the deleted ID to
		// UpdateLastRun would re-violate the FK constraint, so we leave the
		// session-ID field alone (nil = don't update).
		var recordedSessionIDPtr *string
		if result.Outcome != models.CronJobOutcomeDeletedNoChanges {
			recordedSessionID := sessionID
			recordedSessionIDPtr = &recordedSessionID
		}

		if err := l.cronJobs.UpdateLastRun(ctx, *session.CronJobID, db.UpdateCronJobLastRunParams{
			SessionID: recordedSessionIDPtr,
			RanAt:     ranAt,
			Outcome:   result.Outcome,
		}); err != nil {
			// Outcome classification already succeeded; log and continue.
			l.logger.Error().Err(err).
				Str("session", sessionID).
				Str("cronJob", *session.CronJobID).
				Str("outcome", string(result.Outcome)).
				Msg("failed to record cron job last-run outcome")
		}
	}

	// Step 5: clear hook_token on success only, so a replayed Stop event can
	// no longer authenticate as this session. Failure outcomes keep the token
	// so an operator can manually retry.
	if result.Outcome == models.CronJobOutcomePRCreated {
		var nilToken *string
		if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
			HookToken: &nilToken,
		}); err != nil {
			l.logger.Error().Err(err).
				Str("session", sessionID).
				Msg("failed to clear hook_token after finalize success")
		}
	}

	// Step 6: on failure outcomes (except deleted_no_changes, which removed
	// the session entirely), persist a descriptive blocked reason and then
	// transition to Blocked so the session surfaces as attention-needed in
	// the UI. Finalizing itself is suppressed in ComputeAttentionStatus, so
	// leaving a failed session there would hide the problem.
	if needsAttention(result.Outcome) {
		reason := sessionreason.FinalizeFailure(string(result.Outcome), result.Err)
		reasonPtr := &reason
		if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
			BlockedReason: &reasonPtr,
		}); err != nil {
			l.logger.Error().Err(err).Str("session", sessionID).
				Msg("failed to persist finalize blocked reason")
		}
		if _, err := l.sessions.UpdateStateConditional(
			ctx, sessionID, int(machine.Blocked), int(machine.Finalizing),
		); err != nil {
			l.logger.Error().Err(err).
				Str("session", sessionID).
				Str("outcome", string(result.Outcome)).
				Msg("failed to transition failed finalize to blocked")
		}
	}

	return result, nil
}

// StartFinalizeChat spawns a new agent chat running the boss-finalize command
// against the session's worktree. The prior implementing-plan chat is left
// running — the existing idle poller reaps it once it stops producing output —
// so the user can switch back to it if the finalize chat fails.
//
// StartTmuxChat records the new chat in agent_chats. The session's primary
// AgentSessionID is intentionally NOT overwritten: the implementing chat remains
// the canonical "main" chat for the session, and the finalize chat is a sibling.
func (l *Lifecycle) StartFinalizeChat(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	if session.WorktreePath == "" {
		return fmt.Errorf("session %s has no worktree path", sessionID)
	}

	agentSessionID, err := l.StartTmuxChat(ctx, sessionID, ChatInput{Command: finalizeChatSkill}, "Finalize", HookOpts{
		AllowSiblingChat: true,
	})
	if err != nil {
		return fmt.Errorf("start finalize chat: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("agentSessionID", agentSessionID).
		Msg("finalize chat started")

	return nil
}

// bossdManagedWorktreeFiles lists files bossd writes into the worktree
// during session setup. They appear in `git status --porcelain` (typically
// untracked) but are NOT Claude-authored changes and must not influence
// the finalize outcome. The Stop-hook config additionally contains a
// bearer token, so misclassifying it as a Claude change would risk
// pushing credentials to the remote via EnsurePR.
//
// A parallel global-gitignore effort is plumbing these paths through a
// shared excludesFile per worktree; this filter is the in-process
// belt-and-suspenders so a regression there can't re-introduce the
// pr_failed → Blocked failure observed for "do nothing" cron runs.
var bossdManagedWorktreeFiles = []string{
	".claude/settings.local.json",
	".claude/scheduled_tasks.lock",
}

// stripBossdManagedFilesWith removes porcelain entries for any of the given
// managedPaths before the empty-status check. Lines are "XY path" (two
// status chars, a space, then the pathspec); we slice past the 3-char
// prefix and trim trailing whitespace before comparing. Rename entries
// ("R  old -> new") are rare and never originate from bossd, so are left
// untouched.
//
// Callers should pass the union of bossd-owned paths and any agent-plugin-
// contributed paths returned by AgentRunnerService.ListIgnoredDirtyFiles.
func stripBossdManagedFilesWith(porcelain string, managedPaths []string) string {
	if porcelain == "" {
		return ""
	}
	managed := make(map[string]struct{}, len(managedPaths))
	for _, p := range managedPaths {
		managed[p] = struct{}{}
	}
	var kept []string
	for line := range strings.SplitSeq(porcelain, "\n") {
		if len(line) >= 4 {
			path := strings.TrimSpace(line[3:])
			if _, drop := managed[path]; drop {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// classifyFinalizeOutcome runs steps 2 and 3 of the finalize pipeline: it
// inspects the worktree and routes to the cleanup, no-github, PR-failed, or
// PR-created branch. It never returns an error — unrecoverable failures are
// folded into the Outcome column so the caller always records something.
func (l *Lifecycle) classifyFinalizeOutcome(ctx context.Context, session *models.Session) *FinalizeResult {
	status, err := l.worktrees.Status(ctx, session.WorktreePath)
	if err != nil {
		// Can't tell whether there were changes; treat as recoverable pr_failed
		// so the user can investigate the worktree manually.
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     fmt.Errorf("git status: %w", err),
		}
	}

	// Drop bossd-owned and agent-plugin-owned files from the porcelain
	// output before the empty-check. The agent plugin declares which paths
	// it manages (e.g. .claude/settings.local.json) via ListIgnoredDirtyFiles;
	// we fall back to the hardcoded list when no client is loaded for this
	// session's agent (e.g. legacy rows whose agent_name doesn't match a
	// loaded plugin).
	var managedPaths []string
	if client, err := l.agentClientFor(session); err == nil {
		resp, rpcErr := client.ListIgnoredDirtyFiles(ctx, &bossanovav1.ListIgnoredDirtyFilesRequest{
			WorkDir: session.WorktreePath,
		})
		if rpcErr != nil {
			l.logger.Warn().Err(rpcErr).Msg("ListIgnoredDirtyFiles failed; using empty list")
		} else if resp != nil {
			managedPaths = resp.Paths
		}
	} else {
		managedPaths = bossdManagedWorktreeFiles
	}
	status = stripBossdManagedFilesWith(status, managedPaths)

	if strings.TrimSpace(status) == "" {
		if result := l.attachExistingPRIfCleanBranchHasOne(ctx, session); result != nil {
			return result
		}
		if result := l.createPRIfCleanBranchHasCommittedWork(ctx, session); result != nil {
			return result
		}
		return l.finalizeNoChanges(ctx, session)
	}

	// Changes exist — route on GitHub linkage.
	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     fmt.Errorf("get repo: %w", err),
		}
	}
	if !libvcs.IsGitHubURL(repo.OriginURL) {
		l.logger.Info().
			Str("session", session.ID).
			Str("origin", repo.OriginURL).
			Msg("finalize: changes present but origin is not GitHub; preserving worktree")
		return &FinalizeResult{Outcome: models.CronJobOutcomePRSkippedNoGitHub}
	}

	// The /boss-finalize skill assumes a PR already exists — it reads
	// `gh pr view` for the base/number/URL and runs `gh pr ready`, with no
	// `gh pr create` step. Cron sessions defer PR creation at start
	// (opts.DeferPR), so dirty output can reach here with no PR, and the
	// branch may have no commits beyond base when the agent left only
	// uncommitted changes. EnsurePR can't open a PR for a zero-commit branch
	// (GitHub rejects it with "no commits between") and deliberately makes no
	// placeholder commit. Add one in that case — mirroring the session-start
	// draft PR path — so dirty-only runs still get a PR for the chat to
	// operate on; the chat commits the dirty changes on top and the
	// placeholder is squashed away during finalize.
	hasCommittedWork, err := l.cleanBranchHasCommittedWork(ctx, session, true)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Msg("finalize: committed-work check failed for dirty cron output; preserving worktree")
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     err,
		}
	}
	if !hasCommittedWork {
		if err := l.worktrees.EmptyCommit(ctx, session.WorktreePath, draftPRPlaceholderCommitSubject); err != nil {
			l.logger.Warn().Err(err).
				Str("session", session.ID).
				Msg("finalize: placeholder commit failed for dirty cron output; preserving worktree")
			return &FinalizeResult{
				Outcome: models.CronJobOutcomePRFailed,
				Err:     err,
			}
		}
	}

	// EnsurePR is idempotent: it no-ops when the session already has a PR,
	// and otherwise pushes the branch and opens a draft PR.
	if err := l.EnsurePR(ctx, session.ID); err != nil {
		if attached, attachErr := l.attachBranchPRAfterEnsurePRError(ctx, session, repo); attached {
			l.logger.Warn().Err(err).
				Str("session", session.ID).
				Msg("finalize: EnsurePR failed after branch PR became visible; continuing finalize chat")
		} else {
			if attachErr != nil {
				err = fmt.Errorf("%w; attach branch PR after EnsurePR failure: %v", err, attachErr)
			}
			l.logger.Warn().Err(err).
				Str("session", session.ID).
				Msg("finalize: EnsurePR failed for dirty cron output; preserving worktree")
			return &FinalizeResult{
				Outcome: models.CronJobOutcomePRFailed,
				Err:     err,
			}
		}
	}

	// Safety net: EnsurePR may have created the PR on GitHub but failed to
	// persist the number (network drop after create, race with a concurrent
	// opener, etc.). Re-fetch the session and, if PRNumber is still unset,
	// fall back to an active branch-PR lookup so the UI shows "#N" not "-".
	if cur, err := l.sessions.Get(ctx, session.ID); err == nil && cur.PRNumber == nil {
		if _, attachErr := l.attachOpenPRForBranch(ctx, session.ID, cur, repo); attachErr != nil {
			l.logger.Warn().Err(attachErr).Str("session", session.ID).
				Msg("finalize: post-EnsurePR PR association failed; poller will retry")
		}
	}

	if err := l.StartFinalizeChat(ctx, session.ID); err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Msg("finalize: chat spawn failed for dirty cron output; PR already created, preserving worktree")
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeChatSpawnFailed,
			Err:     err,
		}
	}

	return &FinalizeResult{Outcome: models.CronJobOutcomePRCreated}
}

func (l *Lifecycle) attachBranchPRAfterEnsurePRError(ctx context.Context, session *models.Session, repo *models.Repo) (bool, error) {
	cur, err := l.sessions.Get(ctx, session.ID)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Msg("finalize: failed to refresh session before post-EnsurePR branch PR lookup")
		cur = session
	}
	if cur.PRNumber != nil {
		return true, nil
	}

	return l.attachOpenPRForBranch(ctx, session.ID, cur, repo)
}

func (l *Lifecycle) attachExistingPRIfCleanBranchHasOne(ctx context.Context, session *models.Session) *FinalizeResult {
	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeCleanupFailed,
			Err:     fmt.Errorf("get repo: %w", err),
		}
	}
	if !libvcs.IsGitHubURL(repo.OriginURL) {
		return nil
	}

	found, err := l.attachOpenPRForBranch(ctx, session.ID, session, repo)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Str("branch", session.BranchName).
			Msg("finalize: branch PR lookup failed; preserving worktree")
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     err,
		}
	}
	if !found {
		return nil
	}

	l.logger.Info().
		Str("session", session.ID).
		Str("branch", session.BranchName).
		Msg("finalize: attached existing branch PR for clean worktree")
	if err := l.StartFinalizeChat(ctx, session.ID); err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Msg("finalize: chat spawn failed after attaching existing PR")
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeChatSpawnFailed,
			Err:     err,
		}
	}
	return &FinalizeResult{Outcome: models.CronJobOutcomePRCreated}
}

func (l *Lifecycle) createPRIfCleanBranchHasCommittedWork(ctx context.Context, session *models.Session) *FinalizeResult {
	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeCleanupFailed,
			Err:     fmt.Errorf("get repo: %w", err),
		}
	}
	originURL, err := l.originURLIfConfigured(ctx, repo)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Str("branch", session.BranchName).
			Msg("finalize: origin lookup failed for clean committed branch; preserving worktree")
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     err,
		}
	}
	isGitHubOrigin := libvcs.IsGitHubURL(originURL)
	hasCommittedWork, err := l.cleanBranchHasCommittedWork(ctx, session, isGitHubOrigin)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Str("branch", session.BranchName).
			Msg("finalize: committed work lookup failed; preserving worktree")
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     err,
		}
	}
	if !hasCommittedWork {
		return nil
	}

	if !isGitHubOrigin {
		l.logger.Info().
			Str("session", session.ID).
			Str("branch", session.BranchName).
			Str("origin", originURL).
			Msg("finalize: clean branch has committed work but origin is not GitHub; preserving worktree")
		return &FinalizeResult{Outcome: models.CronJobOutcomePRSkippedNoGitHub}
	}

	l.logger.Info().
		Str("session", session.ID).
		Str("branch", session.BranchName).
		Msg("finalize: clean branch has committed work; creating PR")
	if err := l.EnsurePR(ctx, session.ID); err != nil {
		if attached, attachErr := l.attachBranchPRAfterEnsurePRError(ctx, session, repo); attached {
			l.logger.Warn().Err(err).
				Str("session", session.ID).
				Msg("finalize: EnsurePR failed after branch PR became visible; continuing finalize chat")
		} else {
			if attachErr != nil {
				err = fmt.Errorf("%w; attach branch PR after EnsurePR failure: %v", err, attachErr)
			}
			l.logger.Warn().Err(err).
				Str("session", session.ID).
				Msg("finalize: EnsurePR failed for clean committed branch; preserving worktree")
			return &FinalizeResult{
				Outcome: models.CronJobOutcomePRFailed,
				Err:     err,
			}
		}
	}

	// Safety net: same race as the dirty-output path — EnsurePR may have
	// opened the PR on GitHub but the PRNumber write was dropped. Re-fetch
	// and attach if still unset so the UI always shows the PR number.
	if cur, err := l.sessions.Get(ctx, session.ID); err == nil && cur.PRNumber == nil {
		if _, attachErr := l.attachOpenPRForBranch(ctx, session.ID, cur, repo); attachErr != nil {
			l.logger.Warn().Err(attachErr).Str("session", session.ID).
				Msg("finalize: post-EnsurePR PR association failed; poller will retry")
		}
	}

	if err := l.StartFinalizeChat(ctx, session.ID); err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Msg("finalize: chat spawn failed for clean committed branch; PR already created, preserving worktree")
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeChatSpawnFailed,
			Err:     err,
		}
	}

	return &FinalizeResult{Outcome: models.CronJobOutcomePRCreated}
}

func (l *Lifecycle) cleanBranchHasCommittedWork(ctx context.Context, session *models.Session, fetchBase bool) (bool, error) {
	if session.BranchName == "" {
		return false, nil
	}
	if session.BaseBranch == "" {
		return false, fmt.Errorf("base branch is empty for clean branch %q", session.BranchName)
	}

	baseRef := session.BaseBranch
	if fetchBase {
		if err := l.worktrees.FetchBase(ctx, session.WorktreePath, session.BaseBranch); err != nil {
			return false, fmt.Errorf("fetch base branch: %w", err)
		}
		baseRef = "refs/remotes/origin/" + session.BaseBranch
	}
	headInBase, err := l.worktrees.IsAncestor(ctx, session.WorktreePath, "HEAD", baseRef)
	if err != nil {
		return false, fmt.Errorf("check branch commits against %s: %w", baseRef, err)
	}
	return !headInBase, nil
}

func (l *Lifecycle) originURLIfConfigured(ctx context.Context, repo *models.Repo) (string, error) {
	if repo.OriginURL != "" {
		return repo.OriginURL, nil
	}

	url, err := l.worktrees.DetectOriginURL(ctx, repo.LocalPath)
	if err != nil {
		return "", fmt.Errorf("detect origin URL: %w", err)
	}
	if url == "" {
		return "", nil
	}

	if _, err := l.repos.Update(ctx, repo.ID, db.UpdateRepoParams{
		OriginURL: &url,
	}); err != nil {
		return "", fmt.Errorf("persist origin URL: %w", err)
	}

	l.logger.Info().
		Str("repo", repo.ID).
		Str("originURL", url).
		Msg("re-detected and persisted origin URL")

	repo.OriginURL = url
	return url, nil
}

// finalizeNoChanges handles the deleted_no_changes branch: remove the
// worktree and delete the session row. Any error demotes the outcome to
// cleanup_failed and preserves the session row so the user can investigate.
func (l *Lifecycle) finalizeNoChanges(ctx context.Context, session *models.Session) *FinalizeResult {
	l.logger.Info().
		Str("session", session.ID).
		Msg("finalize: no changes; removing worktree and session")

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeCleanupFailed,
			Err:     fmt.Errorf("get repo: %w", err),
		}
	}

	if session.WorktreePath != "" && session.WorktreePath != repo.LocalPath {
		if err := l.worktrees.Archive(ctx, session.WorktreePath); err != nil {
			return &FinalizeResult{
				Outcome: models.CronJobOutcomeCleanupFailed,
				Err:     fmt.Errorf("archive worktree: %w", err),
			}
		}
	}

	// Tear down any per-chat tmux sessions BEFORE deleting the session row.
	// agent_chats.session_id has ON DELETE CASCADE, so once the row is gone
	// we lose the tmux_session_name needed to find and kill the tmux session
	// — leaving a stranded `claude` process with no DB pointer back to it.
	// Cron-spawned sessions always have a tmux-hosted chat in this branch
	// (per startCronTmuxChat); interactive sessions never reach finalize, so
	// this is effectively the cron cleanup path.
	l.killAllChatTmuxSessions(ctx, session.ID)

	if err := l.sessions.Delete(ctx, session.ID); err != nil {
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeCleanupFailed,
			Err:     fmt.Errorf("delete session: %w", err),
		}
	}

	return &FinalizeResult{Outcome: models.CronJobOutcomeDeletedNoChanges}
}

// RecoverFinalizingSessions handles daemon-startup recovery: any session left
// in Finalizing from a previous crash can't be safely re-driven (we don't
// know whether EnsurePR ran, or whether the finalize chat was spawned), so
// we record failed_recovered on the cron_job, transition the session to
// Blocked so it surfaces in the UI as needs-attention, and preserve the
// worktree for the operator to investigate.
//
// hook_token is intentionally NOT cleared — if the operator manually
// re-fires the cron job, the new run gets a fresh session row and the
// stranded Finalizing → Blocked one stays around as evidence.
//
// Returns the number of sessions recovered. Errors on individual sessions
// are logged but do not abort the loop — startup must complete even if a
// row's outcome write fails.
func (l *Lifecycle) RecoverFinalizingSessions(ctx context.Context) (int, error) {
	stuck, err := l.sessions.ListByState(ctx, int(machine.Finalizing))
	if err != nil {
		return 0, fmt.Errorf("list finalizing sessions: %w", err)
	}

	recovered := 0
	for _, sess := range stuck {
		if sess.CronJobID != nil && *sess.CronJobID != "" && l.cronJobs != nil {
			recordedID := sess.ID
			if err := l.cronJobs.UpdateLastRun(ctx, *sess.CronJobID, db.UpdateCronJobLastRunParams{
				SessionID: &recordedID,
				RanAt:     time.Now(),
				Outcome:   models.CronJobOutcomeFailedRecovered,
			}); err != nil {
				l.logger.Error().Err(err).
					Str("session", sess.ID).
					Str("cronJob", *sess.CronJobID).
					Msg("recover: failed to record failed_recovered outcome")
			}
		}

		transitioned, err := l.sessions.UpdateStateConditional(
			ctx, sess.ID, int(machine.Blocked), int(machine.Finalizing),
		)
		if err != nil {
			l.logger.Error().Err(err).
				Str("session", sess.ID).
				Msg("recover: failed to transition stuck Finalizing session to Blocked")
			continue
		}
		if !transitioned {
			continue
		}

		recoverReason := sessionreason.FinalizeRecovered()
		recoverReasonPtr := &recoverReason
		if _, err := l.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
			BlockedReason: &recoverReasonPtr,
		}); err != nil {
			l.logger.Error().Err(err).Str("session", sess.ID).
				Msg("recover: failed to persist blocked reason")
		}

		l.logger.Warn().
			Str("session", sess.ID).
			Msg("recovered session stuck in Finalizing from previous daemon run")
		recovered++
	}

	return recovered, nil
}

// needsAttention reports whether a finalize outcome should drop the session
// into the Blocked state so it surfaces as attention-needed in the UI. The
// happy path (pr_created) and the session-deleted path (deleted_no_changes)
// both return false — the former continues under the finalize chat, the
// latter has no row to transition. failed_recovered and fire_failed are
// recorded by other code paths (RecoverFinalizingSessions and the
// scheduler respectively) and never flow through FinalizeSession's
// needsAttention check, but they're listed here to keep the switch
// exhaustive.
func needsAttention(o models.CronJobOutcome) bool {
	switch o {
	case models.CronJobOutcomePRFailed,
		models.CronJobOutcomePRSkippedNoGitHub,
		models.CronJobOutcomeChatSpawnFailed,
		models.CronJobOutcomeCleanupFailed:
		return true
	case models.CronJobOutcomeDeletedNoChanges,
		models.CronJobOutcomePRCreated,
		models.CronJobOutcomeFailedRecovered,
		models.CronJobOutcomeFireFailed:
		return false
	}
	return false
}
