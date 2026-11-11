package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/recurser/bossalib/kit/vcs"
)

// prReview is one review in either PR status source: the `gh pr view --json
// reviews` list or the open-PR snapshot's review page.
type prReview struct {
	// ID is the review's GraphQL node ID, the same value the REST review list
	// reports as node_id.
	ID     string `json:"id"`
	State  string `json:"state"`
	Author struct {
		Login string `json:"login"`
		// Typename is only queried by the snapshot; `gh pr view` leaves it
		// empty and reports bots under a bare login.
		Typename string `json:"__typename"`
	} `json:"author"`
	// Each review carries its own author's association; a top-level
	// authorAssociation field would be the PR author's instead.
	AuthorAssociation string `json:"authorAssociation"`
}

// prStatusFields are the GraphQL pull request fields a vcs.PRStatus is built
// from. `gh pr view --json` and the open-PR snapshot both decode into it.
type prStatusFields struct {
	Additions         int        `json:"additions"`
	Deletions         int        `json:"deletions"`
	ChangedFiles      int        `json:"changedFiles"`
	State             string     `json:"state"`
	Mergeable         string     `json:"mergeable"`
	MergeStateStatus  string     `json:"mergeStateStatus"`
	IsDraft           bool       `json:"isDraft"`
	Title             string     `json:"title"`
	Body              string     `json:"body"`
	HeadRefName       string     `json:"headRefName"`
	BaseRefName       string     `json:"baseRefName"`
	HeadRefOid        string     `json:"headRefOid"`
	IsCrossRepository bool       `json:"isCrossRepository"`
	ReviewDecision    string     `json:"reviewDecision"`
	Reviews           []prReview `json:"reviews"`
}

// needsMergeability reports whether a PR in this state gets its mergeability
// (and rebaseability) from the REST pull request: open, ready for review and
// not already known to conflict.
func (raw prStatusFields) needsMergeability() bool {
	knownConflict := raw.Mergeable != "" && raw.Mergeable != "UNKNOWN" && raw.Mergeable != "MERGEABLE"
	return parsePRState(raw.State) == vcs.PRStateOpen && !raw.IsDraft && !knownConflict
}

// fetchPRStatus reads the current status of a pull request from GitHub,
// bypassing the read cache (see GetPRStatus).
func (p *Provider) fetchPRStatus(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error) {
	out, err := p.gh(ctx,
		"pr", "view", strconv.Itoa(prID),
		"--repo", repoFlag(repoPath),
		"--json", "state,mergeable,mergeStateStatus,isDraft,title,body,headRefName,baseRefName,headRefOid,isCrossRepository,reviewDecision,reviews,additions,deletions,changedFiles",
	)
	if err != nil {
		if quota, ok := refusedQuota(err); ok && quota == quotaGraphQL {
			status, restErr := p.fetchPRStatusREST(ctx, repoPath, prID)
			if restErr == nil {
				return status, nil
			}
			err = fmt.Errorf("GraphQL read refused (%v); REST fallback: %w", err, restErr)
		}
		return nil, fmt.Errorf("get PR status: %w", err)
	}

	var raw prStatusFields
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse PR status: %w", err)
	}

	var mergeability *pullRequestMergeability
	if raw.needsMergeability() {
		rest, err := p.getPRMergeability(ctx, repoPath, prID)
		if err != nil {
			p.logger.WithLevel(p.rateLimitLog.Level("PR mergeability", err)).Err(err).Int("pr", prID).Msg("failed to fetch PR mergeability")
		} else {
			mergeability = rest
		}
	}
	// This GraphQL-shaped list reports bots under a bare login with
	// association NONE, so a review that fails the association check is
	// re-identified against the REST list (where bots carry the [bot] suffix)
	// by node ID; a review that cannot be matched there stays skipped.
	var restTrusted map[string]bool
	trusted := func(review prReview) bool {
		if vcs.IsTrustedAuthorAssociation(review.AuthorAssociation) {
			return true
		}
		if restTrusted == nil {
			restTrusted = p.trustedReviewNodeIDs(ctx, repoPath, prID)
		}
		if review.ID != "" && restTrusted[review.ID] {
			return true
		}
		p.logUntrustedReview(prID, review.Author.Login, review.AuthorAssociation, "pr status")
		return false
	}
	return buildPRStatus(raw, mergeability, trusted), nil
}

// buildPRStatus is the one decision path from a PR's GraphQL fields to a
// vcs.PRStatus, shared by the per-PR read and the open-PR snapshot so the two
// can never disagree about a PR. mergeability, when non-nil, overrides the
// GraphQL mergeable and merge state and supplies rebaseability. trusted decides
// whether a review may move LatestReviewState, and logs the ones it rejects.
func buildPRStatus(raw prStatusFields, mergeability *pullRequestMergeability, trusted func(prReview) bool) *vcs.PRStatus {
	status := &vcs.PRStatus{
		State:             parsePRState(raw.State),
		AddedLineCount:    raw.Additions,
		DeletedLineCount:  raw.Deletions,
		ChangedFileCount:  raw.ChangedFiles,
		Draft:             raw.IsDraft,
		Title:             raw.Title,
		Body:              raw.Body,
		HeadBranch:        raw.HeadRefName,
		BaseBranch:        raw.BaseRefName,
		HeadSHA:           raw.HeadRefOid,
		IsCrossRepository: raw.IsCrossRepository,
		MergeStateStatus:  parseMergeStateStatus(raw.MergeStateStatus),
	}
	if raw.Mergeable != "" && raw.Mergeable != "UNKNOWN" {
		m := raw.Mergeable == "MERGEABLE"
		status.Mergeable = &m
	}
	if mergeability != nil {
		if mergeability.Mergeable != nil {
			status.Mergeable = mergeability.Mergeable
		}
		if mergeability.MergeStateStatus != "" {
			status.MergeStateStatus = parseMergeStateStatus(mergeability.MergeStateStatus)
		}
		status.Rebaseable = mergeability.Rebaseable
	}
	status.ReviewDecisionState = parseReviewDecision(raw.ReviewDecision)
	status.LatestReviewState = status.ReviewDecisionState
	// Only a trusted author's review may move LatestReviewState: the session
	// poller turns it into a ReviewSubmitted event that can start the fix
	// loop.
	for _, review := range raw.Reviews {
		if !trusted(review) {
			continue
		}
		if state := parseReviewState(review.State); state != vcs.ReviewStateUnspecified {
			status.LatestReviewState = state
		}
	}
	if status.ReviewDecisionState == vcs.ReviewStateApproved ||
		status.ReviewDecisionState == vcs.ReviewStateChangesRequested {
		status.LatestReviewState = status.ReviewDecisionState
	}
	return status
}

// checkRow is one check in `gh pr checks --json name,state,workflow,description`
// form. The open-PR snapshot converts its status-check rollup into the same rows.
type checkRow struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Workflow    string `json:"workflow"`
	Description string `json:"description"`
}

// VerifyPassedAtHead confirms a candidate verify pass using commit statuses for
// the reported SHA. PR status and check rollups have independent cache entries.
func (p *Provider) VerifyPassedAtHead(ctx context.Context, repoPath, sha string) (bool, error) {
	nwo := repoFlag(repoPath)
	if _, _, ok := splitNWO(nwo); !ok || !isFullLowerHexSHA(sha) {
		return false, fmt.Errorf("confirm verify pass: invalid repository or commit SHA")
	}
	ctx = withRoute(ctx, "VerifyPassedAtHead", nwo)
	out, err := p.gh(ctx, "api", fmt.Sprintf("repos/%s/commits/%s/status?per_page=100", nwo, sha), "--paginate", "--jq", ".statuses")
	if err != nil {
		return false, fmt.Errorf("get commit statuses: %w", err)
	}
	type statusRow struct {
		Context string `json:"context"`
		checkRow
	}
	rows, err := decodePaginatedPages[statusRow](out, "commit statuses")
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.Context == vcs.VerifyStatusContext {
			row.Name = row.Context
			return vcs.VerifyPassed(p.buildCheckSet([]checkRow{row.checkRow}).Checks), nil
		}
	}
	return false, nil
}

// buildCheckSet is the one decision path from check rows to a vcs.CheckSet,
// shared by the per-PR `gh pr checks` read and the open-PR snapshot.
//
// The gh CLI's "pr checks" command combines status and conclusion into a single
// "state" field (SUCCESS, FAILURE, PENDING, STARTUP_FAILURE, etc.) rather than
// exposing them separately. We map these combined states back to our Status +
// Conclusion model.
func (p *Provider) buildCheckSet(rows []checkRow) vcs.CheckSet {
	set := vcs.CheckSet{Checks: make([]vcs.CheckResult, 0, len(rows))}
	for _, r := range rows {
		status, conclusion, recognized := parseCheckState(r.State)
		if r.Workflow == "" && provenanceContexts[r.Name] {
			if r.Name == vcs.BuildReceiptContext {
				// Any boss/build status, whatever its state, means a
				// receipt carrier must not post over it.
				set.BuildReceiptSeen = true
			}
			if r.Name == vcs.BuildReceiptContext && status == vcs.CheckStatusCompleted && conclusion != nil && *conclusion == vcs.CheckConclusionSuccess {
				set.HasBuildReceipt = true
			}
			// A provenance receipt (a commit status a boss skill posts to say
			// who produced the head) is not a gate. Counting it would turn a
			// head with no CI into a demonstrated pass; skills-toolbox/
			// pr-check-state.mjs drops the same contexts.
			continue
		}
		if !recognized {
			// An unrecognized state means GitHub introduced (or we missed)
			// a value we don't enumerate. Surface the gap so we can add it
			// to the switch; the aggregate verdict treats it as unknown.
			p.logger.Warn().
				Str("state", r.State).
				Str("name", r.Name).
				Str("workflow", r.Workflow).
				Msg("unknown gh pr checks state; treating check as unclassified")
		}
		set.Checks = append(set.Checks, vcs.CheckResult{
			ID:           r.Workflow + "/" + r.Name,
			Name:         r.Name,
			Status:       status,
			Unclassified: !recognized,
			Conclusion:   conclusion,
			// The description carries the boss/verify phase and park reason
			// (vcs.ClassifyVerify); for an Actions job it is informational.
			Description: r.Description,
			Workflow:    r.Workflow,
		})
	}
	return set
}
