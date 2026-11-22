package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/kit/vcs"
)

type prPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type prConnection[T any] struct {
	PageInfo prPageInfo `json:"pageInfo"`
	Nodes    []T        `json:"nodes"`
}

func (p *Provider) readPRPages(ctx context.Context, repoPath string, prID int, label string, build func(string, int, string) ([]string, bool), consume func(json.RawMessage) (prPageInfo, error)) error {
	cursor := ""
	for page := 0; page < openPRSnapshotMaxPages; page++ {
		args, ok := build(repoPath, prID, cursor)
		if !ok {
			return fmt.Errorf("%s: invalid GitHub repo %q", label, repoPath)
		}
		out, err := p.gh(ctx, args...)
		if err != nil {
			return err
		}
		node, err := decodePRNode(out)
		if err != nil {
			return fmt.Errorf("parse %s: %w", label, err)
		}
		info, err := consume(node)
		if err != nil {
			return fmt.Errorf("parse %s: %w", label, err)
		}
		if !info.HasNextPage {
			return nil
		}
		if info.EndCursor == "" {
			return fmt.Errorf("parse %s: next page has no cursor", label)
		}
		cursor = info.EndCursor
	}
	return fmt.Errorf("%s: more than %d pages", label, openPRSnapshotMaxPages)
}

func decodePRNode(out string) (json.RawMessage, error) {
	var page struct {
		Data struct {
			Repository *struct {
				PullRequest json.RawMessage `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		return nil, err
	}
	if len(page.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", page.Errors[0].Message)
	}
	if page.Data.Repository == nil || len(page.Data.Repository.PullRequest) == 0 || string(page.Data.Repository.PullRequest) == "null" {
		return nil, fmt.Errorf("no pull request node")
	}
	return page.Data.Repository.PullRequest, nil
}

func (p *Provider) readPRStatus(ctx context.Context, repoPath string, prID int) (prStatusFields, error) {
	var raw prStatusFields
	first := true
	err := p.readPRPages(ctx, repoPath, prID, "PR status", vcs.GitHubPRStatusArgs, func(node json.RawMessage) (prPageInfo, error) {
		var pr struct {
			prStatusFields
			ReviewPage prConnection[prReview] `json:"reviews"`
		}
		if err := json.Unmarshal(node, &pr); err != nil {
			return prPageInfo{}, err
		}
		if first {
			raw = pr.prStatusFields
			raw.Reviews = make([]prReview, 0)
			first = false
		}
		raw.Reviews = append(raw.Reviews, pr.ReviewPage.Nodes...)
		return pr.ReviewPage.PageInfo, nil
	})
	if err != nil {
		return prStatusFields{}, err
	}
	return raw, nil
}

func (p *Provider) fetchCheckSet(ctx context.Context, repoPath string, prID int) (vcs.CheckSet, error) {
	var contexts []rollupContext
	err := p.readPRPages(ctx, repoPath, prID, "check results", vcs.GitHubPRChecksArgs, func(node json.RawMessage) (prPageInfo, error) {
		var pr struct {
			Commits struct {
				Nodes []struct {
					Commit struct {
						StatusCheckRollup *struct {
							Contexts prConnection[rollupContext] `json:"contexts"`
						} `json:"statusCheckRollup"`
					} `json:"commit"`
				} `json:"nodes"`
			} `json:"commits"`
		}
		if err := json.Unmarshal(node, &pr); err != nil {
			return prPageInfo{}, err
		}
		for _, n := range pr.Commits.Nodes {
			if rollup := n.Commit.StatusCheckRollup; rollup != nil {
				contexts = append(contexts, rollup.Contexts.Nodes...)
				return rollup.Contexts.PageInfo, nil
			}
		}
		return prPageInfo{}, nil
	})
	if err != nil {
		if quota, ok := refusedQuota(err); ok && quota == quotaGraphQL {
			set, restErr := p.fetchCheckSetREST(ctx, repoPath, prID)
			if restErr == nil {
				return set, nil
			}
			err = fmt.Errorf("GraphQL read refused (%v); REST fallback: %w", err, restErr)
		}
		return vcs.CheckSet{}, fmt.Errorf("get check results: %w", err)
	}
	return p.buildCheckSet(checkRows(contexts)), nil
}

// GetPRMergeCommit returns the recorded merge SHA, or vcs.ErrPRNotMerged.
func (p *Provider) GetPRMergeCommit(ctx context.Context, repoPath string, prID int) (string, error) {
	ctx = withRoute(ctx, "GetPRMergeCommit", vcs.GitHubNWO(repoPath))
	args, ok := vcs.GitHubPRMergeCommitArgs(repoPath, prID)
	if !ok {
		return "", fmt.Errorf("get PR merge commit: invalid GitHub repo %q", repoPath)
	}
	out, err := p.gh(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("get PR merge commit: %w", err)
	}
	node, err := decodePRNode(out)
	if err != nil {
		return "", fmt.Errorf("parse PR merge commit: %w", err)
	}
	var pr struct {
		State       string `json:"state"`
		MergeCommit *struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal(node, &pr); err != nil {
		return "", fmt.Errorf("parse PR merge commit: %w", err)
	}
	if strings.ToUpper(pr.State) != "MERGED" {
		return "", fmt.Errorf("%w: state=%s", vcs.ErrPRNotMerged, pr.State)
	}
	if pr.MergeCommit == nil || pr.MergeCommit.OID == "" {
		return "", fmt.Errorf("%w: no merge commit recorded", vcs.ErrPRNotMerged)
	}
	return pr.MergeCommit.OID, nil
}
