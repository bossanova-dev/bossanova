package github

import (
	"context"
	"fmt"

	"github.com/recurser/bossalib/kit/vcs"
)

// PostPRComment posts one conversation comment through the uncached write path.
func (p *Provider) PostPRComment(ctx context.Context, repoPath string, prID int, body string) error {
	nwo := vcs.GitHubNWO(repoPath)
	if nwo == "" || prID <= 0 {
		return fmt.Errorf("post PR comment: invalid repository or PR")
	}
	ctx = withRoute(ctx, "PostPRComment", nwo)
	_, err := p.gh(ctx, "api", "--method", "POST", fmt.Sprintf("repos/%s/issues/%d/comments", nwo, prID), "-f", "body="+body)
	if err != nil {
		return fmt.Errorf("post PR comment: %w", err)
	}
	return nil
}
