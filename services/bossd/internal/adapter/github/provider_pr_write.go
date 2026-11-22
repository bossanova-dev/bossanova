package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/recurser/bossalib/kit/vcs"
)

// MergePR merges using opts.Strategy and, when supplied, pins GitHub's atomic
// merge to opts.ExpectedHeadSHA. Cleanup never turns a landed merge into a failure.
func (p *Provider) MergePR(ctx context.Context, repoPath string, prID int, opts vcs.MergePROpts) error {
	ctx = withRoute(ctx, "MergePR", vcs.GitHubNWO(repoPath))
	method := opts.Strategy
	if method != "rebase" && method != "squash" {
		method = "merge"
	}
	nwo := repoFlag(repoPath)
	if _, _, ok := splitNWO(nwo); !ok {
		return fmt.Errorf("merge PR: invalid GitHub repo: %s", nwo)
	}
	args := []string{"api", "-X", "PUT", fmt.Sprintf("repos/%s/pulls/%d/merge", nwo, prID), "-f", "merge_method=" + method}
	if opts.ExpectedHeadSHA != "" {
		pin, ok := vcs.NormalizeHeadSHA(opts.ExpectedHeadSHA)
		if !ok {
			return fmt.Errorf("merge PR: invalid expected head SHA %q: must be 40 hex characters", opts.ExpectedHeadSHA)
		}
		args = append(args, "-f", "sha="+pin)
	}
	if _, err := p.runGHWithTransientRetry(ctx, "merge PR", args...); err != nil {
		return fmt.Errorf("merge PR: %w", classifyMergeError(err, repoPath, prID))
	}
	p.deleteMergedHeadBranch(ctx, repoPath, prID)
	p.logger.Info().Int("number", prID).Str("strategy", opts.Strategy).Str("expected_head", opts.ExpectedHeadSHA).Msg("merged PR")
	return nil
}

func (p *Provider) deleteMergedHeadBranch(ctx context.Context, repoPath string, prID int) {
	pr, err := p.readRESTPullRequest(vcs.WithoutBackgroundReads(ctx), repoPath, prID)
	if err != nil {
		p.logger.Warn().Err(err).Int("number", prID).Msg("could not read merged PR for branch cleanup")
		return
	}
	if pr.Head.Repo == nil || pr.Base.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, pr.Base.Repo.FullName) || pr.Head.Ref == "" {
		return
	}
	_, err = p.gh(ctx, "api", "-X", "DELETE", fmt.Sprintf("repos/%s/git/refs/heads/%s", repoFlag(repoPath), escapedBranchRef(pr.Head.Ref)))
	if err == nil {
		return
	}
	text := ghResponseText(err)
	if strings.Contains(text, "HTTP 404") || (strings.Contains(text, "HTTP 422") && strings.Contains(strings.ToLower(text), "reference does not exist")) {
		p.logger.Debug().Int("number", prID).Str("branch", pr.Head.Ref).Msg("merged PR branch already deleted")
		return
	}
	p.logger.Warn().Err(err).Int("number", prID).Str("branch", pr.Head.Ref).Msg("could not delete merged PR branch")
}

func escapedBranchRef(branch string) string {
	segments := strings.Split(branch, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// MarkReadyForReview transitions an open draft PR to ready for review.
func (p *Provider) MarkReadyForReview(ctx context.Context, repoPath string, prID int) error {
	ctx = withRoute(ctx, "MarkReadyForReview", vcs.GitHubNWO(repoPath))
	pr, err := p.readRESTPullRequest(vcs.WithoutBackgroundReads(ctx), repoPath, prID)
	if err != nil {
		return fmt.Errorf("mark ready for review: %w", err)
	}
	if pr.State != "open" || pr.Merged {
		return fmt.Errorf("mark ready for review: PR #%d is closed", prID)
	}
	if !pr.Draft {
		p.logger.Info().Int("number", prID).Msg("PR already ready for review")
		return nil
	}
	if pr.NodeID == "" {
		return fmt.Errorf("mark ready for review: PR #%d has no node id", prID)
	}
	_, err = p.gh(ctx, "api", "graphql", "-f", "query=mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}", "-f", "id="+pr.NodeID)
	if err != nil {
		return fmt.Errorf("mark ready for review: %w", err)
	}
	p.logger.Info().Int("number", prID).Msg("marked PR ready for review")
	return nil
}
