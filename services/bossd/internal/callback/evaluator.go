package callback

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// evaluatorStore is the subset of db.GithubCallbackStore the evaluator uses.
// db.GithubCallbackStore satisfies it.
type evaluatorStore interface {
	List(ctx context.Context, filter db.ListGithubCallbacksFilter) ([]*models.GithubCallback, error)
	TriggerGroup(ctx context.Context, id, event string, now time.Time) (*models.GithubCallback, error)
}

// prStatusProvider is the subset of vcs.Provider the evaluator queries for
// authoritative PR/check state.
type prStatusProvider interface {
	GetPRStatus(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error)
	GetCheckResults(ctx context.Context, repoPath string, prID int) ([]vcs.CheckResult, error)
}

// Evaluator verifies authoritative GitHub state for a PR and fires (triggers)
// every active callback whose requested event is currently satisfied. It never
// trusts the webhook payload alone: a signal only prompts a re-check, and only
// the store's atomic TriggerGroup advances a callback.
type Evaluator struct {
	store    evaluatorStore
	provider prStatusProvider
	now      func() time.Time
	logger   zerolog.Logger
}

// NewEvaluator constructs an Evaluator. now may be nil (defaults to time.Now).
func NewEvaluator(store evaluatorStore, provider prStatusProvider, now func() time.Time, logger zerolog.Logger) *Evaluator {
	if now == nil {
		now = time.Now
	}
	return &Evaluator{store: store, provider: provider, now: now, logger: logger}
}

// repoPath is the identifier the github provider expects (name-with-owner,
// "owner/name"). github.Provider.repoFlag runs vcs.GitHubNWO over it and falls
// back to the raw value when it carries no github.com host, so a bare
// "owner/name" is passed through to `gh --repo` unchanged.
func repoPath(owner, name string) string {
	return fmt.Sprintf("%s/%s", owner, name)
}

// EvaluatePR lists active callbacks for repoOwner/repoName#prNumber, queries the
// authoritative PR + check state once, and triggers every active callback whose
// requested trigger is currently satisfied. Returns quickly when no callbacks
// match. A provider error is returned to the caller (logged) and holds nothing
// open; a lost TriggerGroup race (ErrGithubCallbackTriggerConflict) or a raced
// delete (sql.ErrNoRows) is tolerated as non-error.
func (e *Evaluator) EvaluatePR(ctx context.Context, repoOwner, repoName string, prNumber int) error {
	if prNumber <= 0 || repoOwner == "" || repoName == "" {
		return nil
	}
	active := models.GithubCallbackStateActive
	cbs, err := e.store.List(ctx, db.ListGithubCallbacksFilter{
		RepoOwner: &repoOwner,
		RepoName:  &repoName,
		PRNumber:  &prNumber,
		State:     &active,
	})
	if err != nil {
		return fmt.Errorf("list active github callbacks for %s/%s#%d: %w", repoOwner, repoName, prNumber, err)
	}
	if len(cbs) == 0 {
		return nil
	}

	rp := repoPath(cbs[0].RepoOwner, cbs[0].RepoName)
	status, err := e.provider.GetPRStatus(ctx, rp, prNumber)
	if err != nil {
		e.logger.Warn().Err(err).Str("repo", rp).Int("pr", prNumber).
			Msg("callback evaluator: get PR status failed")
		return fmt.Errorf("get PR status %s#%d: %w", rp, prNumber, err)
	}
	checks, err := e.provider.GetCheckResults(ctx, rp, prNumber)
	if err != nil {
		e.logger.Warn().Err(err).Str("repo", rp).Int("pr", prNumber).
			Msg("callback evaluator: get check results failed")
		return fmt.Errorf("get check results %s#%d: %w", rp, prNumber, err)
	}

	satisfied := satisfiedTriggers(status, checks)
	now := e.now()
	for _, cb := range cbs {
		if !satisfied[cb.Trigger] {
			continue
		}
		if _, err := e.store.TriggerGroup(ctx, cb.ID, string(cb.Trigger), now); err != nil {
			if errors.Is(err, db.ErrGithubCallbackTriggerConflict) || errors.Is(err, sql.ErrNoRows) {
				continue
			}
			e.logger.Warn().Err(err).Str("callback_id", cb.ID).
				Msg("callback evaluator: trigger failed")
			return fmt.Errorf("trigger callback %s: %w", cb.ID, err)
		}
		e.logger.Info().Str("callback_id", cb.ID).Str("trigger", string(cb.Trigger)).
			Str("repo", rp).Int("pr", prNumber).Msg("callback evaluator: triggered")
	}
	return nil
}

// ReconcileAll re-evaluates every distinct PR that has an active callback. Used
// at startup and after upstream (re)registration to catch enduring states
// (merged/closed/checks) that were reached while the daemon was disconnected and
// whose webhook was therefore never delivered. Errors on individual PRs are
// collected and joined; evaluation of the remaining PRs continues.
func (e *Evaluator) ReconcileAll(ctx context.Context) error {
	active := models.GithubCallbackStateActive
	cbs, err := e.store.List(ctx, db.ListGithubCallbacksFilter{State: &active})
	if err != nil {
		return fmt.Errorf("list active github callbacks: %w", err)
	}
	type prKey struct {
		owner string
		name  string
		pr    int
	}
	seen := make(map[prKey]struct{}, len(cbs))
	var errs []error
	for _, cb := range cbs {
		k := prKey{cb.RepoOwner, cb.RepoName, cb.PRNumber}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		if err := e.EvaluatePR(ctx, cb.RepoOwner, cb.RepoName, cb.PRNumber); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// satisfiedTriggers derives which triggers the authoritative state currently
// satisfies. A trigger absent from the map is not satisfied.
//
//   - merged:               PR is merged.
//   - closed:               PR is closed and NOT merged (PRStateMerged is distinct).
//   - checks_passed:        at least one check, none pending, none failed.
//   - checks_failed:        at least one completed check failed/timed_out/cancelled.
//   - ready_for_review:     PR is open and not a draft (the draft-to-ready flip).
//   - checks_passed_ready:  checks_passed AND the PR is open and not a draft.
//
// Pending checks satisfy neither passed nor failed; a completed failure still
// satisfies checks_failed even if other checks are still pending.
func satisfiedTriggers(status *vcs.PRStatus, checks []vcs.CheckResult) map[models.GithubCallbackTrigger]bool {
	out := make(map[models.GithubCallbackTrigger]bool, 6)
	openAndReady := false
	if status != nil {
		switch status.State {
		case vcs.PRStateMerged:
			out[models.GithubCallbackTriggerMerged] = true
		case vcs.PRStateClosed:
			out[models.GithubCallbackTriggerClosed] = true
		case vcs.PRStateOpen:
			if !status.Draft {
				out[models.GithubCallbackTriggerReadyForReview] = true
				openAndReady = true
			}
		}
	}

	anyPending := false
	anyFailed := false
	for _, c := range checks {
		if c.Status != vcs.CheckStatusCompleted {
			anyPending = true
			continue
		}
		if c.Conclusion != nil && isFailingConclusion(*c.Conclusion) {
			anyFailed = true
		}
	}
	if anyFailed {
		out[models.GithubCallbackTriggerChecksFailed] = true
	}
	checksPassed := len(checks) > 0 && !anyPending && !anyFailed
	if checksPassed {
		out[models.GithubCallbackTriggerChecksPassed] = true
	}
	// checks_passed_ready additionally requires state == open (not just
	// !Draft): a merged or closed PR reports Draft == false too, so
	// draft-negation alone would let a closed/merged PR falsely satisfy this
	// trigger. Gating on openAndReady (which already implies State == Open)
	// keeps closed/merged PRs excluded.
	if checksPassed && openAndReady {
		out[models.GithubCallbackTriggerChecksPassedReady] = true
	}
	return out
}

// isFailingConclusion reports whether a completed check's conclusion counts as a
// failure for the checks_failed trigger.
func isFailingConclusion(c vcs.CheckConclusion) bool {
	switch c {
	case vcs.CheckConclusionFailure, vcs.CheckConclusionTimedOut, vcs.CheckConclusionCancelled:
		return true
	default:
		return false
	}
}
