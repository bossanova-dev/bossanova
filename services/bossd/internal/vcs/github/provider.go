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
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/vcs"
)

// Compile-time interface check.
var _ vcs.Provider = (*Provider)(nil)

// reviewBotUsers lists bot accounts whose COMMENTED reviews may be promoted
// to CHANGES_REQUESTED so they surface as "rejected" in the TUI. Bot code-review
// tools cannot submit Request Changes reviews, so they post comments instead.
//
// Promotion is conditional: a bot's COMMENTED review is only promoted if the bot
// has at least one unresolved review thread on the PR. When all threads are
// resolved, the review stays as COMMENTED (showing as "reviewed" rather than
// "rejected").
var reviewBotUsers = map[string]bool{
	"cursor[bot]":       true,
	"cubic-dev-ai[bot]": true,
}

// ghFunc is the signature for executing gh CLI commands.
type ghFunc func(ctx context.Context, args ...string) (string, error)

// Provider implements vcs.Provider by shelling out to the gh CLI.
type Provider struct {
	logger  zerolog.Logger
	runGH   ghFunc
	sleepFn func(time.Duration)
}

// ProviderOption configures a Provider.
type ProviderOption func(*Provider)

// WithRunGH overrides the gh CLI executor (for testing).
func WithRunGH(f ghFunc) ProviderOption {
	return func(p *Provider) { p.runGH = f }
}

// WithSleepFunc overrides the sleep function used between retries (for testing).
func WithSleepFunc(f func(time.Duration)) ProviderOption {
	return func(p *Provider) { p.sleepFn = f }
}

// New creates a new GitHub provider.
func New(logger zerolog.Logger, opts ...ProviderOption) *Provider {
	p := &Provider{
		logger:  logger,
		runGH:   defaultRunGH,
		sleepFn: time.Sleep,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// repoFlag converts a git origin URL to the owner/repo format expected by gh.
func repoFlag(repoPath string) string {
	if nwo := vcs.GitHubNWO(repoPath); nwo != "" {
		return nwo
	}
	return repoPath
}

// splitNWO splits a "owner/repo" string into its two components.
func splitNWO(nwo string) (owner, repo string, ok bool) {
	parts := strings.SplitN(nwo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// unresolvedThreadAuthors queries GitHub's GraphQL API for review threads on a
// PR and returns the set of bot authors (from botUsers) that have at least one
// unresolved thread.
//
// On any error it fails open: returns all botUsers so that promotion still
// fires. A false "rejected" is safer than hiding real issues.
func (p *Provider) unresolvedThreadAuthors(ctx context.Context, repoPath string, prID int, botUsers map[string]bool) map[string]bool {
	nwo := repoFlag(repoPath)
	owner, repo, ok := splitNWO(nwo)
	if !ok {
		p.logger.Warn().Str("nwo", nwo).Msg("cannot split owner/repo for thread query, failing open")
		return botUsers
	}

	query := `query($owner:String!, $repo:String!, $pr:Int!) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$pr) {
      reviewThreads(first:100) {
        nodes {
          isResolved
          comments(first:1) {
            nodes { author { login } }
          }
        }
      }
    }
  }
}`

	out, err := p.runGH(ctx, "api", "graphql",
		"-f", "query="+query,
		"-f", "owner="+owner,
		"-f", "repo="+repo,
		"-F", fmt.Sprintf("pr=%d", prID),
	)
	if err != nil {
		p.logger.Warn().Err(err).Msg("GraphQL thread query failed, failing open")
		return botUsers
	}

	var result struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									Author struct {
										Login string `json:"login"`
									} `json:"author"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		p.logger.Warn().Err(err).Msg("failed to parse GraphQL thread response, failing open")
		return botUsers
	}

	// GraphQL returns bare logins for bots ("cursor") while REST returns
	// the suffixed form ("cursor[bot]"). Build a lookup that handles both.
	graphqlToRest := make(map[string]string, len(botUsers)*2)
	for login := range botUsers {
		graphqlToRest[login] = login
		graphqlToRest[strings.TrimSuffix(login, "[bot]")] = login
	}

	unresolved := make(map[string]bool)
	for _, thread := range result.Data.Repository.PullRequest.ReviewThreads.Nodes {
		if thread.IsResolved {
			continue
		}
		if len(thread.Comments.Nodes) == 0 {
			continue
		}
		author := thread.Comments.Nodes[0].Author.Login
		if restLogin, ok := graphqlToRest[author]; ok {
			unresolved[restLogin] = true
		}
	}
	return unresolved
}

// CreateDraftPR pushes the head branch and creates a draft pull request.
// It retries up to 3 times with exponential backoff when GitHub's API hasn't
// finished indexing the pushed branches (common in newly-created repositories).
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

	const maxAttempts = 3
	backoff := 2 * time.Second

	var lastErr error
	for attempt := range maxAttempts {
		out, err := p.runGH(ctx, args...)
		if err == nil {
			// gh pr create outputs the PR URL on stdout.
			prURL := strings.TrimSpace(out)

			// Extract PR number from URL (e.g. https://github.com/owner/repo/pull/42).
			number, parseErr := parsePRNumberFromURL(prURL)
			if parseErr != nil {
				return nil, fmt.Errorf("parse PR URL %q: %w", prURL, parseErr)
			}

			p.logger.Info().
				Int("number", number).
				Str("url", prURL).
				Msg("created draft PR")

			return &vcs.PRInfo{Number: number, URL: prURL}, nil
		}

		if !isRepoNotReady(err) {
			return nil, fmt.Errorf("create PR: %w", err)
		}

		lastErr = err

		// Don't sleep after the last attempt.
		if attempt < maxAttempts-1 {
			p.logger.Warn().
				Int("attempt", attempt+1).
				Dur("backoff", backoff).
				Msg("repo not ready, retrying PR creation")

			p.sleepFn(backoff)
			backoff *= 2

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
		}
	}

	_ = lastErr // preserve for debugging; sentinel conveys the meaning
	return nil, fmt.Errorf("%w", vcs.ErrRepoNotReady)
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

	// First pass: check if any bot COMMENTED reviews exist. If not, skip the
	// GraphQL call entirely (zero overhead for non-bot PRs).
	hasBotCommented := false
	for _, r := range raw {
		if parseReviewState(r.State) == vcs.ReviewStateCommented && reviewBotUsers[r.User.Login] {
			hasBotCommented = true
			break
		}
	}

	var botsWithUnresolved map[string]bool
	if hasBotCommented {
		botsWithUnresolved = p.unresolvedThreadAuthors(ctx, repoPath, prID, reviewBotUsers)
	}

	comments := make([]vcs.ReviewComment, len(raw))
	for i, r := range raw {
		state := parseReviewState(r.State)
		// Promote bot COMMENTED reviews to CHANGES_REQUESTED only when the bot
		// has unresolved review threads. This avoids false "rejected" status
		// when all review issues have been addressed.
		if state == vcs.ReviewStateCommented && reviewBotUsers[r.User.Login] && botsWithUnresolved[r.User.Login] {
			state = vcs.ReviewStateChangesRequested
		}
		comments[i] = vcs.ReviewComment{
			Author: r.User.Login,
			Body:   r.Body,
			State:  state,
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
		"--json", "number,title,headRefName,state,author",
		"--limit", "300",
	)
	if err != nil {
		return nil, fmt.Errorf("list open PRs: %w", err)
	}

	var raw []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
		State       string `json:"state"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
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
			Author:     r.Author.Login,
		}
	}

	return prs, nil
}

// ListClosedPRs returns recently-closed (not merged) pull requests.
func (p *Provider) ListClosedPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error) {
	out, err := p.runGH(ctx,
		"pr", "list",
		"--repo", repoFlag(repoPath),
		"--state", "closed",
		"--json", "number,title,headRefName,state,author",
		"--limit", "50",
	)
	if err != nil {
		return nil, fmt.Errorf("list closed PRs: %w", err)
	}

	var raw []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
		State       string `json:"state"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
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
			Author:     r.Author.Login,
		}
	}

	return prs, nil
}

// MergePR merges a pull request using the given strategy.
// Valid strategies are "rebase", "squash", and "merge". An empty string
// defaults to "merge" (GitHub's default).
func (p *Provider) MergePR(ctx context.Context, repoPath string, prID int, strategy string) error {
	flag := "--merge"
	switch strategy {
	case "rebase":
		flag = "--rebase"
	case "squash":
		flag = "--squash"
	}

	_, err := p.runGH(ctx,
		"pr", "merge", strconv.Itoa(prID),
		"--repo", repoFlag(repoPath),
		flag,
		"--delete-branch",
	)
	if err != nil {
		return fmt.Errorf("merge PR: %w", err)
	}

	p.logger.Info().
		Int("number", prID).
		Str("strategy", strategy).
		Msg("merged PR")

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

// isRepoNotReady returns true when the gh CLI error indicates the repository
// lacks enough commit history to create a pull request. This typically happens
// when a repo only has an --allow-empty initial commit with no real content.
func isRepoNotReady(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Head sha can't be blank") ||
		strings.Contains(msg, "Base sha can't be blank") ||
		strings.Contains(msg, "No commits between")
}

// defaultRunGH executes a gh CLI command and returns stdout.
func defaultRunGH(ctx context.Context, args ...string) (string, error) {
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
