package taskorchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/domain/mergepolicy"
	"github.com/recurser/bossd/internal/ghusage"
)

// withMergeLabel attributes the GitHub calls of a task auto-merge to
// daemon:merge in the usage ledger.
func withMergeLabel(ctx context.Context) context.Context {
	return ghusage.WithLabel(ctx, ghusage.LabelMerge)
}

// mergeConfirmedHead re-reads the PR in the foreground and merges it pinned to
// the head that read saw. The plugin classified the PR from background reads
// that may be one open-PR snapshot old, so its AUTO_MERGE verdict alone never
// merges a head. It reports whether the merge landed; on false it has already
// settled the mapping (Failed, or handed to repairRebase for a rebase refusal).
func (o *Orchestrator) mergeConfirmedHead(ctx context.Context, repo repoInfo, mapping *domain.TaskMapping, prNumber int, strategy string, repairRebase func(error)) bool {
	opts, err := o.confirmAutoMergeHead(ctx, repo, prNumber, strategy)
	if err != nil {
		o.logger.Info().Err(err).
			Int("pr", prNumber).
			Str("repo", repo.displayName).
			Msg("auto-merge skipped: fresh read says the PR is not ready")
		o.updateMappingStatusWithDetails(ctx, mapping.ID, domain.TaskMappingStatusFailed, err.Error())
		return false
	}

	err = o.provider.MergePR(ctx, repo.originURL, prNumber, opts)
	switch {
	case err == nil:
		return true
	case errors.Is(err, vcs.ErrHeadMismatch):
		// The head moved after the fresh read: the new head is unverified, so
		// fail the mapping rather than repairing a commit nobody has checked.
		o.logger.Info().Err(err).
			Int("pr", prNumber).
			Str("repo", repo.displayName).
			Msg("auto-merge refused: PR head moved after the fresh read")
	case mergepolicy.IsRebaseRefusal(strategy, err):
		o.logger.Info().Err(err).
			Int("pr", prNumber).
			Str("repo", repo.displayName).
			Msg("auto-merge rebase failed; queueing repair session")
		repairRebase(err)
		return false
	default:
		o.logger.Error().Err(err).
			Int("pr", prNumber).
			Str("repo", repo.displayName).
			Msg("auto-merge failed")
	}
	o.updateMappingStatusWithDetails(ctx, mapping.ID, domain.TaskMappingStatusFailed, taskFailureDetails(err))
	return false
}

// confirmAutoMergeHead does one foreground read pair (PR status, then the
// head's checks) and returns merge options pinned to that head when its checks
// are green and GitHub does not report it unmergeable. The ctx is deliberately
// not marked for cached reads: this read is what makes the merge safe.
func (o *Orchestrator) confirmAutoMergeHead(ctx context.Context, repo repoInfo, prNumber int, strategy string) (vcs.MergePROpts, error) {
	status, err := o.provider.GetPRStatus(ctx, repo.originURL, prNumber)
	if err != nil {
		return vcs.MergePROpts{}, fmt.Errorf("fresh PR status read: %w", err)
	}
	if status == nil {
		return vcs.MergePROpts{}, errors.New("fresh PR status read returned no status")
	}
	checkSet, readErr := vcs.ReadCheckSet(ctx, o.provider, repo.originURL, prNumber)
	verdict := vcs.EvaluateChecks(status.HeadSHA, checkSet.Checks, readErr)
	if !verdict.IsGreen() {
		return vcs.MergePROpts{}, fmt.Errorf("fresh checks verdict on head %q is %s (%s)", status.HeadSHA, verdict.State, verdict.Reason)
	}
	if status.Mergeable != nil && !*status.Mergeable {
		return vcs.MergePROpts{}, fmt.Errorf("fresh PR status reports head %q not mergeable", status.HeadSHA)
	}

	opts := vcs.MergePROpts{Strategy: strategy}
	if sha, ok := vcs.NormalizeHeadSHA(status.HeadSHA); ok {
		opts.ExpectedHeadSHA = sha
	} else {
		o.logger.Warn().
			Int("pr", prNumber).
			Str("repo", repo.displayName).
			Str("head_sha", status.HeadSHA).
			Msg("auto-merge: fresh head SHA is not a full commit SHA; merging unpinned on a green verdict")
	}
	return opts, nil
}
