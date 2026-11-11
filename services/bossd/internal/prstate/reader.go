package prstate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/ghusage"
)

// Provider is the GitHub read surface Read needs. The daemon's shared GitHub
// provider satisfies it, so its webhook-invalidated read cache serves the
// reads.
type Provider interface {
	GetPRStatus(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error)
	GetCheckSet(ctx context.Context, repoPath string, prID int) (vcs.CheckSet, error)
	VerifyPassedAtHead(ctx context.Context, repoPath, headSHA string) (bool, error)
	ListWorkflowRuns(ctx context.Context, repoPath, headSHA string) ([]vcs.WorkflowRun, error)
	GetReviewComments(ctx context.Context, repoPath string, prID int) ([]vcs.ReviewComment, error)
}

// Options narrows one read.
type Options struct {
	// Fresh skips the read cache, so every read goes to GitHub and refreshes
	// the cache for later readers.
	Fresh bool
	// IncludeReviewComments adds the review comments read, the expensive one.
	IncludeReviewComments bool
	// IncludeWorkflowRuns reads the head's workflow runs whatever the verdict,
	// so a caller that judges the head by its own rules sees every run.
	IncludeWorkflowRuns bool
	// Label is the GitHub usage label the reads are attributed to; empty
	// means ghusage.LabelPRState.
	Label string
}

// Snapshot is one PR's state as read at ObservedAt.
type Snapshot struct {
	// Degraded marks a snapshot read through another quota while the primary refused.
	Degraded        bool
	Status          vcs.PRStatus
	Checks          []vcs.CheckResult
	HasBuildReceipt bool
	Verdict         vcs.CheckVerdict
	VerifyPhase     vcs.VerifyPhase
	// VerifyPassed confirms a candidate pass against statuses on Status.HeadSHA.
	VerifyPassed bool
	// RunsSettled is nil unless Verdict is green, so "not computed" differs
	// from "not settled"; without IncludeWorkflowRuns the runs read is skipped
	// for any other verdict.
	RunsSettled *bool
	// HeldRuns lists the head runs still in flight when RunsSettled is false.
	HeldRuns []HeldWorkflowRun
	// WorkflowRuns is every run the runs read returned for the head; RunsRead
	// says the read happened, so an empty list is a head with no run.
	WorkflowRuns []vcs.WorkflowRun
	RunsRead     bool
	// ReviewComments is read only with Options.IncludeReviewComments.
	ReviewComments []vcs.ReviewComment
	ObservedAt     time.Time
}

// ErrNoStatus is returned when the provider answered a PR status read with
// nothing.
var ErrNoStatus = errors.New("provider returned no PR status")

// RequiredChecksSource resolves a repo reference to the repo's configured
// required-check policy. checkpolicy.Resolver implements it.
type RequiredChecksSource interface {
	RequiredChecks(ctx context.Context, repoRef string) vcs.RequiredChecks
}

// Reader reads PR state through a Provider.
type Reader struct {
	provider Provider
	required RequiredChecksSource // optional; nil requires no named checks
	now      func() time.Time
}

// NewReader returns a Reader over provider. A nil now uses the wall clock.
func NewReader(provider Provider, now func() time.Time) *Reader {
	if now == nil {
		now = time.Now
	}
	return &Reader{provider: provider, now: now}
}

// SetRequiredChecks wires the repo required-check policy, so a head whose
// required checks have not run reads not-requested rather than green, as the
// daemon's own checks_passed gates do. Must be called before the reader is
// shared. nil-safe.
func (r *Reader) SetRequiredChecks(src RequiredChecksSource) { r.required = src }

// requiredFor returns the check names repo requires on a PR into base.
func (r *Reader) requiredFor(ctx context.Context, repo, base string) []string {
	if r.required == nil {
		return nil
	}
	return r.required.RequiredChecks(ctx, repo).For(base)
}

// Read returns the state of repo#pr. Every provider read is attributed to
// opts.Label (the pr-state usage label by default) and, unless opts.Fresh, may
// be served from the read cache or joined to an identical read already in
// flight. Required reads return errors. An unavailable verify confirmation
// leaves VerifyPassed false while retaining the available snapshot.
func (r *Reader) Read(ctx context.Context, repo string, pr int, opts Options) (Snapshot, error) {
	label := opts.Label
	if label == "" {
		label = ghusage.LabelPRState
	}
	ctx = ghusage.WithLabel(ctx, label)
	if !opts.Fresh {
		ctx = vcs.WithCachedReads(ctx)
	}
	status, err := r.provider.GetPRStatus(ctx, repo, pr)
	if err != nil {
		return Snapshot{}, fmt.Errorf("get PR status %s#%d: %w", repo, pr, err)
	}
	if status == nil {
		return Snapshot{}, fmt.Errorf("get PR status %s#%d: %w", repo, pr, ErrNoStatus)
	}
	set, err := r.provider.GetCheckSet(ctx, repo, pr)
	if err != nil {
		return Snapshot{}, fmt.Errorf("get check set %s#%d: %w", repo, pr, err)
	}
	snap := Snapshot{
		Status:          *status,
		Degraded:        status.Degraded || set.Degraded,
		Checks:          set.Checks,
		HasBuildReceipt: set.HasBuildReceipt,
		Verdict:         vcs.EvaluateChecksRequiring(status.HeadSHA, set.Checks, nil, r.requiredFor(ctx, repo, status.BaseBranch)),
		VerifyPhase:     vcs.ClassifyVerify(set.Checks),
	}
	if status.HeadSHA != "" && vcs.VerifyPassed(set.Checks) {
		confirmed, confirmErr := r.provider.VerifyPassedAtHead(ctx, repo, status.HeadSHA)
		snap.VerifyPassed = confirmErr == nil && confirmed
		if ctx.Err() != nil {
			return Snapshot{}, ctx.Err()
		}
	}
	if green := snap.Verdict.IsGreen(); green || opts.IncludeWorkflowRuns {
		runs, read, err := r.readRuns(ctx, repo, status.HeadSHA)
		if err != nil {
			return Snapshot{}, err
		}
		snap.WorkflowRuns, snap.RunsRead = runs, read
		if green {
			held := HeldHeadRuns(status.HeadSHA, runs)
			settled := read && len(held) == 0
			snap.RunsSettled, snap.HeldRuns = &settled, held
		}
	}
	if opts.IncludeReviewComments {
		comments, err := r.provider.GetReviewComments(ctx, repo, pr)
		if err != nil {
			return Snapshot{}, fmt.Errorf("get review comments %s#%d: %w", repo, pr, err)
		}
		snap.ReviewComments = comments
	}
	snap.ObservedAt = r.now()
	return snap, nil
}

// readRuns lists the head's workflow runs and reports whether the read
// happened. An empty head SHA cannot name its runs, so it is not read, and a
// green head with no read is never settled.
func (r *Reader) readRuns(ctx context.Context, repo, headSHA string) ([]vcs.WorkflowRun, bool, error) {
	if headSHA == "" {
		return nil, false, nil
	}
	runs, err := r.provider.ListWorkflowRuns(ctx, repo, headSHA)
	if err != nil {
		return nil, false, fmt.Errorf("list workflow runs %s@%s: %w", repo, headSHA, err)
	}
	return runs, true, nil
}
