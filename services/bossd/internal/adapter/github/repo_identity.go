package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/recurser/bossalib/kit/vcs"
)

// GetRepositoryIdentity reads immutable identity and visibility with local credentials.
// This optional capability deliberately bypasses the provider's time-based read cache:
// the identity verifier owns the absolute lifetime of every successful response.
func (p *Provider) GetRepositoryIdentity(ctx context.Context, repoPath string) (int64, string, error) {
	canonical := vcs.CanonicalRepoURL(repoPath)
	parsed, err := url.Parse(canonical)
	if err != nil || canonical == "" || parsed.Host != "github.com" {
		return 0, "", fmt.Errorf("repository identity requires github.com origin")
	}
	ctx = withRoute(ctx, "GetRepositoryIdentity", vcs.GitHubNWO(canonical))
	out, err := p.ghConditional(ctx, "repos/"+repoFlag(canonical))
	if err != nil {
		return 0, "", fmt.Errorf("read repository identity: %w", err)
	}
	var response struct {
		ID         int64  `json:"id"`
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return 0, "", fmt.Errorf("parse repository identity: %w", err)
	}
	if response.ID <= 0 {
		return 0, "", fmt.Errorf("repository identity missing positive id")
	}
	return response.ID, response.Visibility, nil
}
