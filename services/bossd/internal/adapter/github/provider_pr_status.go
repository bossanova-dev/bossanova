package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/recurser/bossalib/kit/vcs"
)

// fetchPRStatus reads the current status of a pull request from GitHub,
// bypassing the read cache (see GetPRStatus).
func (p *Provider) fetchPRStatus(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error) {
	out, err := p.gh(ctx,
		"pr", "view", strconv.Itoa(prID),
		"--repo", repoFlag(repoPath),
		"--json", "state,mergeable,mergeStateStatus,isDraft,title,body,headRefName,baseRefName,headRefOid,isCrossRepository,reviewDecision,reviews",
	)
	if err != nil {
		return nil, fmt.Errorf("get PR status: %w", err)
	}

	var raw struct {
		State             string `json:"state"`
		Mergeable         string `json:"mergeable"`
		MergeStateStatus  string `json:"mergeStateStatus"`
		IsDraft           bool   `json:"isDraft"`
		Title             string `json:"title"`
		Body              string `json:"body"`
		HeadRefName       string `json:"headRefName"`
		BaseRefName       string `json:"baseRefName"`
		HeadRefOid        string `json:"headRefOid"`
		IsCrossRepository bool   `json:"isCrossRepository"`
		ReviewDecision    string `json:"reviewDecision"`
		// Each review object already carries its author's association; a
		// top-level authorAssociation field would be the PR author's instead.
		Reviews []struct {
			// ID is the review's GraphQL node ID, the same value the REST
			// review list reports as node_id.
			ID     string `json:"id"`
			State  string `json:"state"`
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			AuthorAssociation string `json:"authorAssociation"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse PR status: %w", err)
	}

	status := &vcs.PRStatus{
		State:             parsePRState(raw.State),
		Draft:             raw.IsDraft,
		Title:             raw.Title,
		Body:              raw.Body,
		HeadBranch:        raw.HeadRefName,
		BaseBranch:        raw.BaseRefName,
		HeadSHA:           raw.HeadRefOid,
		IsCrossRepository: raw.IsCrossRepository,
	}
	status.MergeStateStatus = parseMergeStateStatus(raw.MergeStateStatus)

	if raw.Mergeable != "" && raw.Mergeable != "UNKNOWN" {
		m := raw.Mergeable == "MERGEABLE"
		status.Mergeable = &m
	}
	if status.State == vcs.PRStateOpen && !status.Draft && (status.Mergeable == nil || *status.Mergeable) {
		rest, err := p.getPRMergeability(ctx, repoPath, prID)
		if err != nil {
			p.logger.Warn().Err(err).Int("pr", prID).Msg("failed to fetch PR mergeability")
		} else {
			if rest.Mergeable != nil {
				status.Mergeable = rest.Mergeable
			}
			if rest.MergeStateStatus != "" {
				status.MergeStateStatus = parseMergeStateStatus(rest.MergeStateStatus)
			}
			status.Rebaseable = rest.Rebaseable
		}
	}
	status.ReviewDecisionState = parseReviewDecision(raw.ReviewDecision)
	status.LatestReviewState = status.ReviewDecisionState
	// Only a trusted author's review may move LatestReviewState: the session
	// poller turns it into a ReviewSubmitted event that can start the fix
	// loop. This GraphQL-shaped list reports bots under a bare login with
	// association NONE, so a review that fails the association check is
	// re-identified against the REST list (where bots carry the [bot] suffix)
	// by node ID; a review that cannot be matched there stays skipped.
	var restTrusted map[string]bool
	for _, review := range raw.Reviews {
		if !vcs.IsTrustedAuthorAssociation(review.AuthorAssociation) {
			if restTrusted == nil {
				restTrusted = p.trustedReviewNodeIDs(ctx, repoPath, prID)
			}
			if review.ID == "" || !restTrusted[review.ID] {
				p.logUntrustedReview(prID, review.Author.Login, review.AuthorAssociation, "pr status")
				continue
			}
		}
		state := parseReviewState(review.State)
		if state != vcs.ReviewStateUnspecified {
			status.LatestReviewState = state
		}
	}
	if status.ReviewDecisionState == vcs.ReviewStateApproved ||
		status.ReviewDecisionState == vcs.ReviewStateChangesRequested {
		status.LatestReviewState = status.ReviewDecisionState
	}

	return status, nil
}
