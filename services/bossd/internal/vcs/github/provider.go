// Package github implements the vcs.Provider interface using the gh CLI.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/vcs"
)

// Compile-time interface check.
var _ vcs.Provider = (*Provider)(nil)

// Provider implements vcs.Provider by shelling out to the gh CLI.
type Provider struct {
	logger zerolog.Logger
}

// New creates a new GitHub provider.
func New(logger zerolog.Logger) *Provider {
	return &Provider{logger: logger}
}

// repoFlag converts a git origin URL to the owner/repo format expected by gh.
func repoFlag(repoPath string) string {
	if nwo := vcs.GitHubNWO(repoPath); nwo != "" {
		return nwo
	}
	return repoPath
}

// CreateDraftPR pushes the head branch and creates a draft pull request.
func (p *Provider) CreateDraftPR(ctx context.Context, opts vcs.CreatePROpts) (*vcs.PRInfo, error) {
	args := []string{
		"pr", "create",
		"--repo", repoFlag(opts.RepoPath),
		"--head", opts.HeadBranch,
		"--base", opts.BaseBranch,
		"--title", opts.Title,
		"--body", opts.Body,
	}
	if opts.Draft {
		args = append(args, "--draft")
	}

	out, err := p.runGH(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("create PR: %w", err)
	}

	// gh pr create outputs the PR URL on stdout.
	prURL := strings.TrimSpace(out)

	// Extract PR number from URL (e.g. https://github.com/owner/repo/pull/42).
	number, err := parsePRNumberFromURL(prURL)
	if err != nil {
		return nil, fmt.Errorf("parse PR URL %q: %w", prURL, err)
	}

	p.logger.Info().
		Int("number", number).
		Str("url", prURL).
		Msg("created draft PR")

	return &vcs.PRInfo{Number: number, URL: prURL}, nil
}

// GetPRStatus returns the current status of a pull request.
func (p *Provider) GetPRStatus(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error) {
	out, err := p.runGH(ctx,
		"pr", "view", strconv.Itoa(prID),
		"--repo", repoFlag(repoPath),
		"--json", "state,mergeable,isDraft,title,headRefName,baseRefName",
	)
	if err != nil {
		return nil, fmt.Errorf("get PR status: %w", err)
	}

	var raw struct {
		State       string `json:"state"`
		Mergeable   string `json:"mergeable"`
		IsDraft     bool   `json:"isDraft"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
		BaseRefName string `json:"baseRefName"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse PR status: %w", err)
	}

	status := &vcs.PRStatus{
		State:      parsePRState(raw.State),
		Draft:      raw.IsDraft,
		Title:      raw.Title,
		HeadBranch: raw.HeadRefName,
		BaseBranch: raw.BaseRefName,
	}

	if raw.Mergeable != "" && raw.Mergeable != "UNKNOWN" {
		m := raw.Mergeable == "MERGEABLE"
		status.Mergeable = &m
	}

	return status, nil
}

// GetCheckResults returns CI check results for a pull request.
//
// The gh CLI's "pr checks" command combines status and conclusion into a single
// "state" field (SUCCESS, FAILURE, PENDING, STARTUP_FAILURE, etc.) rather than
// exposing them separately. We map these combined states back to our Status +
// Conclusion model.
func (p *Provider) GetCheckResults(ctx context.Context, repoPath string, prID int) ([]vcs.CheckResult, error) {
	out, err := p.runGH(ctx,
		"pr", "checks", strconv.Itoa(prID),
		"--repo", repoFlag(repoPath),
		"--json", "name,state,workflow",
	)
	if err != nil {
		return nil, fmt.Errorf("get check results: %w", err)
	}

	var raw []struct {
		Name     string `json:"name"`
		State    string `json:"state"`
		Workflow string `json:"workflow"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse check results: %w", err)
	}

	results := make([]vcs.CheckResult, len(raw))
	for i, r := range raw {
		status, conclusion := parseCheckState(r.State)
		results[i] = vcs.CheckResult{
			ID:         r.Workflow + "/" + r.Name,
			Name:       r.Name,
			Status:     status,
			Conclusion: conclusion,
		}
	}

	return results, nil
}

// GetFailedCheckLogs returns the log output for a specific failed check run.
func (p *Provider) GetFailedCheckLogs(ctx context.Context, repoPath string, checkID string) (string, error) {
	// checkID is "workflow/job" — we use gh run view to get logs.
	// gh doesn't have a direct "get check logs" command, so we use the API.
	out, err := p.runGH(ctx,
		"api", fmt.Sprintf("repos/%s/actions/jobs/%s/logs", repoFlag(repoPath), checkID),
	)
	if err != nil {
		return "", fmt.Errorf("get check logs: %w", err)
	}
	return out, nil
}

// MarkReadyForReview transitions a draft PR to ready for review.
func (p *Provider) MarkReadyForReview(ctx context.Context, repoPath string, prID int) error {
	_, err := p.runGH(ctx,
		"pr", "ready", strconv.Itoa(prID),
		"--repo", repoFlag(repoPath),
	)
	if err != nil {
		return fmt.Errorf("mark ready for review: %w", err)
	}

	p.logger.Info().
		Int("number", prID).
		Msg("marked PR ready for review")

	return nil
}

// GetReviewComments returns review comments on a pull request.
func (p *Provider) GetReviewComments(ctx context.Context, repoPath string, prID int) ([]vcs.ReviewComment, error) {
	out, err := p.runGH(ctx,
		"api", fmt.Sprintf("repos/%s/pulls/%d/reviews", repoFlag(repoPath), prID),
	)
	if err != nil {
		return nil, fmt.Errorf("get reviews: %w", err)
	}

	var raw []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body  string `json:"body"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse reviews: %w", err)
	}

	comments := make([]vcs.ReviewComment, len(raw))
	for i, r := range raw {
		comments[i] = vcs.ReviewComment{
			Author: r.User.Login,
			Body:   r.Body,
			State:  parseReviewState(r.State),
		}
	}

	return comments, nil
}

// ListOpenPRs returns all open pull requests for a repository.
func (p *Provider) ListOpenPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error) {
	out, err := p.runGH(ctx,
		"pr", "list",
		"--repo", repoFlag(repoPath),
		"--state", "open",
		"--json", "number,title,headRefName,state",
	)
	if err != nil {
		return nil, fmt.Errorf("list open PRs: %w", err)
	}

	var raw []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse PRs: %w", err)
	}

	prs := make([]vcs.PRSummary, len(raw))
	for i, r := range raw {
		prs[i] = vcs.PRSummary{
			Number:     r.Number,
			Title:      r.Title,
			HeadBranch: r.HeadRefName,
			State:      parsePRState(r.State),
		}
	}

	return prs, nil
}

// UpdatePRTitle updates the title of an existing pull request.
func (p *Provider) UpdatePRTitle(ctx context.Context, repoPath string, prID int, title string) error {
	_, err := p.runGH(ctx,
		"pr", "edit", strconv.Itoa(prID),
		"--repo", repoFlag(repoPath),
		"--title", title,
	)
	if err != nil {
		return fmt.Errorf("update PR title: %w", err)
	}

	p.logger.Info().
		Int("number", prID).
		Str("title", title).
		Msg("updated PR title")

	return nil
}

// parseReviewState converts a GitHub API review state string to vcs.ReviewState.
func parseReviewState(s string) vcs.ReviewState {
	switch strings.ToUpper(s) {
	case "APPROVED":
		return vcs.ReviewStateApproved
	case "CHANGES_REQUESTED":
		return vcs.ReviewStateChangesRequested
	case "COMMENTED":
		return vcs.ReviewStateCommented
	case "DISMISSED":
		return vcs.ReviewStateDismissed
	default:
		return vcs.ReviewStateCommented
	}
}

// runGH executes a gh CLI command and returns stdout.
func (p *Provider) runGH(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// parsePRNumberFromURL extracts the PR number from a GitHub PR URL.
// Example: "https://github.com/owner/repo/pull/42" → 42
func parsePRNumberFromURL(url string) (int, error) {
	parts := strings.Split(strings.TrimRight(url, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "pull" {
		return 0, fmt.Errorf("unexpected PR URL format")
	}
	return strconv.Atoi(parts[len(parts)-1])
}

// parsePRState converts a GitHub API PR state string to vcs.PRState.
func parsePRState(s string) vcs.PRState {
	switch strings.ToUpper(s) {
	case "OPEN":
		return vcs.PRStateOpen
	case "CLOSED":
		return vcs.PRStateClosed
	case "MERGED":
		return vcs.PRStateMerged
	default:
		return vcs.PRStateOpen
	}
}

// parseCheckState converts a gh pr checks "state" field into a status and
// optional conclusion. The gh CLI combines status and conclusion into a single
// field: SUCCESS, FAILURE, PENDING, STARTUP_FAILURE, CANCELLED, SKIPPED, etc.
func parseCheckState(s string) (vcs.CheckStatus, *vcs.CheckConclusion) {
	switch strings.ToUpper(s) {
	case "SUCCESS":
		c := vcs.CheckConclusionSuccess
		return vcs.CheckStatusCompleted, &c
	case "FAILURE", "STARTUP_FAILURE", "STALE":
		c := vcs.CheckConclusionFailure
		return vcs.CheckStatusCompleted, &c
	case "NEUTRAL":
		c := vcs.CheckConclusionNeutral
		return vcs.CheckStatusCompleted, &c
	case "CANCELLED":
		c := vcs.CheckConclusionCancelled
		return vcs.CheckStatusCompleted, &c
	case "SKIPPED":
		c := vcs.CheckConclusionSkipped
		return vcs.CheckStatusCompleted, &c
	case "TIMED_OUT":
		c := vcs.CheckConclusionTimedOut
		return vcs.CheckStatusCompleted, &c
	case "IN_PROGRESS":
		return vcs.CheckStatusInProgress, nil
	case "QUEUED", "PENDING", "WAITING":
		return vcs.CheckStatusQueued, nil
	default:
		return vcs.CheckStatusQueued, nil
	}
}
