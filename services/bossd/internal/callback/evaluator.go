package callback

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	ObserveBaseline(ctx context.Context, id string, now time.Time) error
	TriggerGroup(ctx context.Context, id, event string, now time.Time) (*models.GithubCallback, error)
	CancelUnreachable(ctx context.Context, id, event string, now time.Time) error
}

// prStatusProvider is the subset of vcs.Provider the evaluator queries for
// authoritative PR/check state, plus the head SHA's workflow runs (which only
// the github provider offers; it is deliberately not on vcs.Provider).
type prStatusProvider interface {
	GetPRStatus(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error)
	GetCheckResults(ctx context.Context, repoPath string, prID int) ([]vcs.CheckResult, error)
	ListWorkflowRuns(ctx context.Context, repoPath, headSHA string) ([]vcs.WorkflowRun, error)
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
	health   webhookHealth // optional; nil reconciles every PR every pass

	evalMu    sync.Mutex
	lastEvals map[prKey]time.Time
}

// webhookHealth reports whether GitHub webhooks currently reach the daemon for
// a repo. upstream.WebhookHealth implements it.
type webhookHealth interface {
	WebhookDeliveryHealthy(repo string) bool
}

// reconcileSafetyNetInterval is how often ReconcileAll still re-reads a PR in a
// repo whose webhooks reach the daemon. Every change on such a PR already runs
// EvaluatePR from the webhook dispatcher; the periodic pass only has to catch a
// delivery bosso dropped.
const reconcileSafetyNetInterval = 10 * time.Minute

type prKey struct {
	owner string
	name  string
	pr    int
}

// SetWebhookHealth wires the webhook-delivery tracker ReconcileAll consults.
// Must be called before the evaluator is shared. nil-safe.
func (e *Evaluator) SetWebhookHealth(h webhookHealth) {
	e.health = h
}

func (e *Evaluator) markEvaluated(k prKey, at time.Time) {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	if e.lastEvals == nil {
		e.lastEvals = make(map[prKey]time.Time)
	}
	e.lastEvals[k] = at
}

// forgetEvaluatedExcept drops last-evaluation times for PRs that no longer have
// an active callback, so the map is bounded by the live callback set.
func (e *Evaluator) forgetEvaluatedExcept(live map[prKey]struct{}) {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	for k := range e.lastEvals {
		if _, ok := live[k]; !ok {
			delete(e.lastEvals, k)
		}
	}
}

// reconcileDue reports whether ReconcileAll should re-read k now.
func (e *Evaluator) reconcileDue(k prKey, now time.Time) bool {
	if e.health == nil || !e.health.WebhookDeliveryHealthy(repoPath(k.owner, k.name)) {
		return true
	}
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	last, ok := e.lastEvals[k]
	return !ok || now.Sub(last) >= reconcileSafetyNetInterval
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
	e.markEvaluated(prKey{cbs[0].RepoOwner, cbs[0].RepoName, prNumber}, e.now())

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

	// The runs read only matters when the attached check set is green, so it is
	// skipped otherwise to spare the API quota.
	runsSettled := false
	if vcs.EvaluateChecks("", checks, nil).IsGreen() {
		runsSettled = e.runsSettled(ctx, rp, prNumber, status)
	}
	satisfied := satisfiedTriggers(status, checks, runsSettled)
	checkState := vcs.EvaluateChecks("", checks, nil).State
	now := e.now()
	for _, cb := range cbs {
		if !satisfied[cb.Trigger] {
			if unreachableAfterMerge(status, checkState, cb.Trigger) {
				if err := e.store.CancelUnreachable(ctx, cb.ID, "unreachable: pr merged", now); err != nil {
					if errors.Is(err, db.ErrGithubCallbackTriggerConflict) || errors.Is(err, sql.ErrNoRows) {
						continue
					}
					e.logger.Warn().Err(err).Str("callback_id", cb.ID).
						Msg("callback evaluator: cancel unreachable callback failed")
					return fmt.Errorf("cancel unreachable callback %s: %w", cb.ID, err)
				}
				e.logger.Info().Str("callback_id", cb.ID).Str("trigger", string(cb.Trigger)).
					Str("repo", rp).Int("pr", prNumber).
					Msg("callback evaluator: canceled callback the merged PR can no longer satisfy")
				continue
			}
			if cb.ShouldRequireTransition && !cb.HasObservedBaseline {
				if err := e.store.ObserveBaseline(ctx, cb.ID, now); err != nil {
					if errors.Is(err, db.ErrGithubCallbackTriggerConflict) || errors.Is(err, sql.ErrNoRows) {
						continue
					}
					e.logger.Warn().Err(err).Str("callback_id", cb.ID).
						Msg("callback evaluator: observe baseline failed")
					return fmt.Errorf("observe callback baseline %s: %w", cb.ID, err)
				}
				e.logger.Info().Str("callback_id", cb.ID).Str("trigger", string(cb.Trigger)).
					Str("repo", rp).Int("pr", prNumber).Msg("callback evaluator: baseline observed")
			}
			continue
		}
		if cb.ShouldRequireTransition && !cb.HasObservedBaseline {
			if err := e.store.ObserveBaseline(ctx, cb.ID, now); err != nil {
				if errors.Is(err, db.ErrGithubCallbackTriggerConflict) || errors.Is(err, sql.ErrNoRows) {
					continue
				}
				e.logger.Warn().Err(err).Str("callback_id", cb.ID).
					Msg("callback evaluator: observe satisfied baseline failed")
				return fmt.Errorf("observe satisfied callback baseline %s: %w", cb.ID, err)
			}
			e.logger.Info().Str("callback_id", cb.ID).Str("trigger", string(cb.Trigger)).
				Str("repo", rp).Int("pr", prNumber).Msg("callback evaluator: satisfied baseline observed")
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
//
// It is a background pass, so its reads go through the provider's read cache.
// A PR in a repo whose webhooks reach the daemon is skipped until
// reconcileSafetyNetInterval has passed since it was last evaluated.
func (e *Evaluator) ReconcileAll(ctx context.Context) error {
	ctx = vcs.WithCachedReads(ctx)
	active := models.GithubCallbackStateActive
	cbs, err := e.store.List(ctx, db.ListGithubCallbacksFilter{State: &active})
	if err != nil {
		return fmt.Errorf("list active github callbacks: %w", err)
	}
	now := e.now()
	seen := make(map[prKey]struct{}, len(cbs))
	var errs []error
	for _, cb := range cbs {
		k := prKey{cb.RepoOwner, cb.RepoName, cb.PRNumber}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		if !e.reconcileDue(k, now) {
			continue
		}
		if err := e.EvaluatePR(ctx, cb.RepoOwner, cb.RepoName, cb.PRNumber); err != nil {
			errs = append(errs, err)
		}
	}
	e.forgetEvaluatedExcept(seen)
	return errors.Join(errs...)
}

// runsSettled reports whether every GitHub Actions workflow run for the PR's
// head SHA has completed. A check set read before the head's workflows have
// attached their jobs is green on the jobs it has so far, so checks_passed is
// only decidable once nothing is still queued or running. An empty head SHA or
// a failed runs read is not settled: it is logged and suppresses only the
// checks-passed pair, never the other triggers on the same evaluation.
func (e *Evaluator) runsSettled(ctx context.Context, rp string, prNumber int, status *vcs.PRStatus) bool {
	if status == nil || status.HeadSHA == "" {
		e.logger.Warn().Str("repo", rp).Int("pr", prNumber).
			Msg("callback evaluator: no head SHA; checks_passed held until workflow runs are readable")
		return false
	}
	runs, err := e.provider.ListWorkflowRuns(ctx, rp, status.HeadSHA)
	if err != nil {
		e.logger.Warn().Err(err).Str("repo", rp).Int("pr", prNumber).Str("head_sha", status.HeadSHA).
			Msg("callback evaluator: list workflow runs failed; checks_passed held")
		return false
	}
	for _, run := range runs {
		if run.HeadSHA != "" && run.HeadSHA != status.HeadSHA {
			continue
		}
		if !strings.EqualFold(run.Status, "completed") {
			return false
		}
	}
	return true
}

// unreachableAfterMerge reports whether an unsatisfied trigger can never fire
// because the PR has merged. Left active, such a callback is re-read from
// GitHub on every reconcile until its 24h expiry — a boss-build wait arms both
// checks_passed and checks_failed, and the loser of a green merge otherwise
// sits there for a day.
//
//   - closed, ready_for_review, checks_passed_ready need a PR that is not
//     merged (closed means closed-unmerged; the other two need an open PR).
//   - checks_failed is dead once the head's checks finished green.
//   - checks_passed is dead once a head check failed.
//
// Checks still pending (or unreadable) keep both checks triggers alive: the
// head's checks can still complete after a merge. A closed-unmerged PR is not
// final — it can be reopened — so nothing is retired for it.
func unreachableAfterMerge(status *vcs.PRStatus, checkState vcs.CheckVerdictState, trigger models.GithubCallbackTrigger) bool {
	if status == nil || status.State != vcs.PRStateMerged {
		return false
	}
	switch trigger {
	case models.GithubCallbackTriggerClosed,
		models.GithubCallbackTriggerReadyForReview,
		models.GithubCallbackTriggerChecksPassedReady:
		return true
	case models.GithubCallbackTriggerChecksFailed:
		return checkState == vcs.CheckVerdictGreen
	case models.GithubCallbackTriggerChecksPassed:
		return checkState == vcs.CheckVerdictFailing
	default:
		return false
	}
}

// satisfiedTriggers derives which triggers the authoritative state currently
// satisfies. A trigger absent from the map is not satisfied.
//
//   - merged:               PR is merged.
//   - closed:               PR is closed and NOT merged (PRStateMerged is distinct).
//   - checks_passed:        check verdict is green AND every head workflow run
//     has completed (runsSettled).
//   - checks_failed:        at least one completed check failed/timed_out/cancelled.
//   - ready_for_review:     PR is open and not a draft (the draft-to-ready flip).
//   - checks_passed_ready:  checks_passed AND the PR is open and not a draft.
//
// Pending checks satisfy neither passed nor failed; a completed failure still
// satisfies checks_failed even if other checks are still pending.
func satisfiedTriggers(status *vcs.PRStatus, checks []vcs.CheckResult, runsSettled bool) map[models.GithubCallbackTrigger]bool {
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

	checkVerdict := vcs.EvaluateChecks("", checks, nil)
	if checkVerdict.State == vcs.CheckVerdictFailing {
		out[models.GithubCallbackTriggerChecksFailed] = true
	}
	checksPassed := checkVerdict.IsGreen() && runsSettled
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
