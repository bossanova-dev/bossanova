package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// OpenIssueURL returns the URL of an open issue, excluding pull requests.
func (p *Provider) OpenIssueURL(ctx context.Context, host, owner, repo string, number int) (string, bool, error) {
	out, err := p.gh(ctx, "api", fmt.Sprintf("repos/%s/%s/issues/%d", owner, repo, number), "--hostname", host)
	if err != nil {
		if strings.Contains(ghResponseText(err), "HTTP 404") {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get GitHub issue: %w", err)
	}
	var issue struct {
		HTMLURL     string          `json:"html_url"`
		State       string          `json:"state"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal([]byte(out), &issue); err != nil {
		return "", false, fmt.Errorf("parse GitHub issue: %w", err)
	}
	if issue.State != "open" || len(issue.PullRequest) != 0 || issue.HTMLURL == "" {
		return "", false, nil
	}
	return issue.HTMLURL, true, nil
}
