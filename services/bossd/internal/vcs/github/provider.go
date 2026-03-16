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

// Provider implements vcs.Provider by shelling out to the gh CLI.
type Provider struct {
	logger zerolog.Logger
}

// New creates a new GitHub provider.
func New(logger zerolog.Logger) *Provider {
	return &Provider{logger: logger}
}

// CreateDraftPR pushes the head branch and creates a draft pull request.
func (p *Provider) CreateDraftPR(ctx context.Context, opts vcs.CreatePROpts) (*vcs.PRInfo, error) {
	args := []string{
		"pr", "create",
		"--repo", opts.RepoPath,
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
		"--repo", repoPath,
		"--json", "state,mergeable,title,headRefName,baseRefName",
	)
	if err != nil {
		return nil, fmt.Errorf("get PR status: %w", err)
	}

	var raw struct {
		State       string `json:"state"`
		Mergeable   string `json:"mergeable"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
		BaseRefName string `json:"baseRefName"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse PR status: %w", err)
	}

	status := &vcs.PRStatus{
		State:      parsePRState(raw.State),
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
func (p *Provider) GetCheckResults(ctx context.Context, repoPath string, prID int) ([]vcs.CheckResult, error) {
	out, err := p.runGH(ctx,
		"pr", "checks", strconv.Itoa(prID),
		"--repo", repoPath,
		"--json", "name,state,conclusion,workflowName",
	)
	if err != nil {
		return nil, fmt.Errorf("get check results: %w", err)
	}

	var raw []struct {
		Name         string `json:"name"`
		State        string `json:"state"`
		Conclusion   string `json:"conclusion"`
		WorkflowName string `json:"workflowName"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse check results: %w", err)
	}

	results := make([]vcs.CheckResult, len(raw))
	for i, r := range raw {
		results[i] = vcs.CheckResult{
			ID:         r.WorkflowName + "/" + r.Name,
			Name:       r.Name,
			Status:     parseCheckStatus(r.State),
			Conclusion: parseCheckConclusion(r.Conclusion),
		}
	}

	return results, nil
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

// parseCheckStatus converts a GitHub check state string to vcs.CheckStatus.
func parseCheckStatus(s string) vcs.CheckStatus {
	switch strings.ToUpper(s) {
	case "COMPLETED":
		return vcs.CheckStatusCompleted
	case "IN_PROGRESS":
		return vcs.CheckStatusInProgress
	case "QUEUED", "PENDING", "WAITING":
		return vcs.CheckStatusQueued
	default:
		return vcs.CheckStatusQueued
	}
}

// parseCheckConclusion converts a GitHub check conclusion string to *vcs.CheckConclusion.
func parseCheckConclusion(s string) *vcs.CheckConclusion {
	if s == "" {
		return nil
	}
	var c vcs.CheckConclusion
	switch strings.ToUpper(s) {
	case "SUCCESS":
		c = vcs.CheckConclusionSuccess
	case "FAILURE":
		c = vcs.CheckConclusionFailure
	case "NEUTRAL":
		c = vcs.CheckConclusionNeutral
	case "CANCELLED":
		c = vcs.CheckConclusionCancelled
	case "SKIPPED":
		c = vcs.CheckConclusionSkipped
	case "TIMED_OUT":
		c = vcs.CheckConclusionTimedOut
	default:
		return nil
	}
	return &c
}
