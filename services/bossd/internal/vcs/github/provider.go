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

// isReviewBotLogin reports whether a GitHub login belongs to a bot account whose
// COMMENTED reviews may be promoted to CHANGES_REQUESTED so they surface as
// "rejected" in the TUI. Bot code-review tools cannot always submit Request
// Changes reviews, so they post resolvable review threads instead.
//
// Promotion is conditional: a bot's COMMENTED review is only promoted if the bot
// has at least one unresolved review thread on the PR. When all threads are
// resolved, the review stays as COMMENTED (showing as "reviewed" rather than
// "rejected").
func isReviewBotLogin(login string) bool {
	return strings.HasSuffix(login, "[bot]")
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
// On any error it fails closed: returns nil so that no promotions fire.
// A missed promotion self-corrects on the next successful poll (2 min).
func (p *Provider) unresolvedThreadAuthors(ctx context.Context, repoPath string, prID int, botUsers map[string]bool) map[string]bool {
	nwo := repoFlag(repoPath)
	owner, repo, ok := splitNWO(nwo)
	if !ok {
		p.logger.Warn().Str("nwo", nwo).Msg("cannot split owner/repo for thread query, failing closed")
		return nil
	}

	query := `query($owner:String!, $repo:String!, $pr:Int!, $after:String) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$pr) {
      reviewThreads(first:100, after:$after) {
        pageInfo {
          hasNextPage
          endCursor
        }
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

	var result struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
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

	// GraphQL returns bare logins for bots ("cursor") while REST returns
	// the suffixed form ("cursor[bot]"). Build a lookup that handles both.
	graphqlToRest := make(map[string]string, len(botUsers)*2)
	for login := range botUsers {
		graphqlToRest[login] = login
		graphqlToRest[strings.TrimSuffix(login, "[bot]")] = login
	}

	unresolved := make(map[string]bool)
	after := ""
	for {
		args := []string{
			"api", "graphql",
			"-f", "query=" + query,
			"-f", "owner=" + owner,
			"-f", "repo=" + repo,
			"-F", fmt.Sprintf("pr=%d", prID),
		}
		if after != "" {
			args = append(args, "-f", "after="+after)
		}
		out, err := p.runGH(ctx, args...)
		if err != nil {
			p.logger.Warn().Err(err).Msg("GraphQL thread query failed, failing closed")
			return nil
		}
		result = struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						ReviewThreads struct {
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
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
		}{}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			p.logger.Warn().Err(err).Msg("failed to parse GraphQL thread response, failing closed")
			return nil
		}
		page := result.Data.Repository.PullRequest.ReviewThreads
		for _, thread := range page.Nodes {
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
		if !page.PageInfo.HasNextPage {
			return unresolved
		}
		if page.PageInfo.EndCursor == "" {
			p.logger.Warn().Msg("GraphQL thread response missing endCursor, failing closed")
			return nil
		}
		after = page.PageInfo.EndCursor
	}
}

// CreateDraftPR pushes the head branch and creates a draft pull request.
// It retries up to 3 times with exponential backoff when GitHub's API hasn't
// finished indexing the pushed branches (common in newly-created repositories).
func (p *Provider) CreateDraftPR(ctx context.Context, opts vcs.CreatePROpts) (*vcs.PRInfo, error) {
	repo := repoFlag(opts.RepoPath)
	if repo == "" {
		return nil, fmt.Errorf("repo path is empty; re-register the repository or configure its origin URL")
	}

	args := []string{
		"pr", "create",
		"--repo", repo,
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

		if isPRAlreadyExists(err) {
			return nil, fmt.Errorf("create PR: %w: %v", vcs.ErrPRAlreadyExists, err)
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

	return nil, fmt.Errorf("create draft PR: %w (last error: %v)", vcs.ErrRepoNotReady, lastErr)
}

// GetPRStatus returns the current status of a pull request.
func (p *Provider) GetPRStatus(ctx context.Context, repoPath string, prID int) (*vcs.PRStatus, error) {
	out, err := p.runGH(ctx,
		"pr", "view", strconv.Itoa(prID),
		"--repo", repoFlag(repoPath),
		"--json", "state,mergeable,mergeStateStatus,isDraft,title,headRefName,baseRefName,headRefOid,reviewDecision,reviews",
	)
	if err != nil {
		return nil, fmt.Errorf("get PR status: %w", err)
	}

	var raw struct {
		State            string `json:"state"`
		Mergeable        string `json:"mergeable"`
		MergeStateStatus string `json:"mergeStateStatus"`
		IsDraft          bool   `json:"isDraft"`
		Title            string `json:"title"`
		HeadRefName      string `json:"headRefName"`
		BaseRefName      string `json:"baseRefName"`
		HeadRefOid       string `json:"headRefOid"`
		ReviewDecision   string `json:"reviewDecision"`
		Reviews          []struct {
			State string `json:"state"`
		} `json:"reviews"`
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
		HeadSHA:    raw.HeadRefOid,
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
	for _, review := range raw.Reviews {
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

type pullRequestMergeability struct {
	Mergeable        *bool  `json:"mergeable"`
	MergeStateStatus string `json:"mergeable_state"`
	Rebaseable       *bool  `json:"rebaseable"`
}

func (p *Provider) getPRMergeability(ctx context.Context, repoPath string, prID int) (*pullRequestMergeability, error) {
	nwo := repoFlag(repoPath)
	if _, _, ok := splitNWO(nwo); !ok {
		return nil, fmt.Errorf("invalid GitHub repo: %s", nwo)
	}
	out, err := p.runGH(ctx, "api", fmt.Sprintf("repos/%s/pulls/%d", nwo, prID))
	if err != nil {
		return nil, err
	}
	var raw pullRequestMergeability
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse PR mergeability: %w", err)
	}
	return &raw, nil
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
		// A PR whose head commit has no check runs is a normal empty result,
		// not a failure. Returning it as an error here would make the display
		// poller bail before updating the tracker, freezing the row on its
		// previous status (e.g. a stale "draft" after the PR became ready).
		if isNoChecksReported(err) {
			return []vcs.CheckResult{}, nil
		}
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
		status, conclusion, recognized := parseCheckState(r.State)
		if !recognized {
			// An unrecognized state means GitHub introduced (or we missed)
			// a value we don't enumerate. parseCheckState fails safe by
			// treating it as a Failure so the repair plugin can still see
			// the PR; surface the gap so we can add it to the switch.
			p.logger.Warn().
				Str("state", r.State).
				Str("name", r.Name).
				Str("workflow", r.Workflow).
				Msg("unknown gh pr checks state; treating as failure")
		}
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
		ID   int64 `json:"id"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body  string `json:"body"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse reviews: %w", err)
	}

	// First pass: collect bot COMMENTED review authors. If none exist, skip the
	// GraphQL call entirely (zero overhead for non-bot PRs).
	botReviewUsers := make(map[string]bool)
	for _, r := range raw {
		if parseReviewState(r.State) == vcs.ReviewStateCommented && isReviewBotLogin(r.User.Login) {
			botReviewUsers[r.User.Login] = true
		}
	}

	var botsWithUnresolved map[string]bool
	if len(botReviewUsers) > 0 {
		botsWithUnresolved = p.unresolvedThreadAuthors(ctx, repoPath, prID, botReviewUsers)
	}

	comments := make([]vcs.ReviewComment, 0, len(raw))
	for _, r := range raw {
		state := parseReviewState(r.State)
		// Promote a bot COMMENTED review to CHANGES_REQUESTED only when it is
		// review-scoped actionable: the bot still has an unresolved thread on the
		// PR AND *this* review carries its own inline comments. The review's
		// summary body is NOT consulted — a substantive body cannot be told apart
		// from benign prose ("LGTM", "No issues found"), so promoting on body
		// alone false-blocks (BOS-254); only the review's own inline suggestions
		// are treated as actionable.
		//
		// An empty bot review (no inline comments of its own) is dropped from the
		// surfaced set even when the same bot has a separate unresolved/outdated
		// thread elsewhere on the PR. This is the BOS-254 fix:
		// chatgpt-codex-connector[bot] posts a byte-identical boilerplate
		// COMMENTED review on every pushed commit, and the old author-scoped
		// heuristic promoted every one of them off a single stale thread, wedging
		// the merge gate in a loop boss-repair could never clear.
		//
		// The empty review is dropped rather than appended as COMMENTED: because
		// ComputeDisplayStatus keys off each author's *latest* review state, an
		// appended trailing COMMENTED entry would overwrite an earlier promoted
		// CHANGES_REQUESTED from the same bot (the common codex flow: real inline
		// suggestions, then a later empty follow-up review) and silently unblock
		// genuinely-actionable feedback. Dropping it preserves the earlier
		// promotion while still not gating on the empty review itself.
		//
		// This feeds the display-poller path (ComputeDisplayStatus keys off
		// per-comment State); the realtime path performs an equivalent
		// review-scoped, inline-comment-based promotion in
		// upstream.WebhookDispatcher.enrichReviewComments. Keep both in sync.
		if state == vcs.ReviewStateCommented && botReviewUsers[r.User.Login] && botsWithUnresolved != nil {
			if !botsWithUnresolved[r.User.Login] {
				// This bot's threads are all resolved (or it has none): the
				// feedback has been addressed — never block, drop the review.
				continue
			}
			// The bot still has an unresolved thread. Fetch *this* review's own
			// inline comments so the promotion decision is review-scoped.
			var inlineComments []vcs.ReviewComment
			if r.ID != 0 {
				inlineComments, err = p.getInlineReviewComments(ctx, repoPath, prID, r.ID)
				if err != nil {
					return nil, err
				}
			}
			if len(inlineComments) == 0 {
				// No actionable inline content of its own: drop it (do not
				// promote on body substance, do not append as COMMENTED — see the
				// notes above).
				continue
			}
			comments = append(comments, vcs.ReviewComment{
				Author: r.User.Login,
				Body:   r.Body,
				State:  vcs.ReviewStateChangesRequested,
			})
			comments = append(comments, inlineComments...)
			continue
		}
		comments = append(comments, vcs.ReviewComment{
			Author: r.User.Login,
			Body:   r.Body,
			State:  state,
		})
		// A review GitHub already reports as CHANGES_REQUESTED carries its
		// actionable feedback as inline comments; surface them too.
		if state == vcs.ReviewStateChangesRequested && r.ID != 0 {
			inlineComments, err := p.getInlineReviewComments(ctx, repoPath, prID, r.ID)
			if err != nil {
				return nil, err
			}
			comments = append(comments, inlineComments...)
		}
	}

	return comments, nil
}

func (p *Provider) getInlineReviewComments(ctx context.Context, repoPath string, prID int, reviewID int64) ([]vcs.ReviewComment, error) {
	return p.GetReviewInlineComments(ctx, repoPath, prID, reviewID)
}

// GetReviewInlineComments returns inline comments attached to a single review.
func (p *Provider) GetReviewInlineComments(ctx context.Context, repoPath string, prID int, reviewID int64) ([]vcs.ReviewComment, error) {
	out, err := p.runGH(ctx,
		"api", fmt.Sprintf("repos/%s/pulls/%d/reviews/%d/comments", repoFlag(repoPath), prID, reviewID),
	)
	if err != nil {
		return nil, fmt.Errorf("get review comments: %w", err)
	}

	var raw []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body string  `json:"body"`
		Path *string `json:"path"`
		Line *int    `json:"line"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse review comments: %w", err)
	}

	comments := make([]vcs.ReviewComment, 0, len(raw))
	for _, r := range raw {
		comments = append(comments, vcs.ReviewComment{
			Author: r.User.Login,
			Body:   r.Body,
			State:  vcs.ReviewStateChangesRequested,
			Path:   r.Path,
			Line:   r.Line,
		})
	}
	return comments, nil
}

// ListOpenPRs returns all open pull requests for a repository.
func (p *Provider) ListOpenPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error) {
	return p.listPRsByState(ctx, repoPath, "list open PRs", "open", 300)
}

// ListClosedPRs returns recently-closed (not merged) pull requests.
func (p *Provider) ListClosedPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error) {
	return p.listPRsByState(ctx, repoPath, "list closed PRs", "closed", 50)
}

// listPRsByState lists pull requests in the given state (e.g. "open" or
// "closed"), capped at limit, and maps them to vcs.PRSummary. label provides
// the retry-logging description and error context for the caller.
func (p *Provider) listPRsByState(ctx context.Context, repoPath, label, state string, limit int) ([]vcs.PRSummary, error) {
	out, err := p.runGHWithTransientRetry(ctx, label,
		"pr", "list",
		"--repo", repoFlag(repoPath),
		"--state", state,
		"--json", "number,title,headRefName,state,author",
		"--limit", strconv.Itoa(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return decodePRList(out)
}

// decodePRList unmarshals `gh pr list --json number,title,headRefName,state,author`
// output into vcs.PRSummary values. Shared by listPRsByState and
// SearchPRsByTitleTag so the field set, the "parse PRs" error wrap, and the
// PRSummary mapping have a single source of truth rather than drifting copies.
func decodePRList(out string) ([]vcs.PRSummary, error) {
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

// SearchPRsByTitleTag returns PRs across all states whose title carries the
// bracketed tracker tag "[<tag>]". The tag argument is the bare identifier
// (e.g. "BOS-289"); the server-side search uses the bareword in:title (GitHub
// tokenizes bracket characters, so a "[BOS-289]" query would not narrow
// reliably), and the exact bracket match is enforced in Go afterwards.
func (p *Provider) SearchPRsByTitleTag(ctx context.Context, repoPath, tag string) ([]vcs.PRSummary, error) {
	out, err := p.runGHWithTransientRetry(ctx, "search PRs by title tag",
		"pr", "list",
		"--repo", repoFlag(repoPath),
		"--state", "all",
		"--search", tag+" in:title",
		"--json", "number,title,headRefName,state,author",
		"--limit", "100",
	)
	if err != nil {
		return nil, fmt.Errorf("search PRs by title tag: %w", err)
	}

	all, err := decodePRList(out)
	if err != nil {
		return nil, err
	}

	bracketed := "[" + tag + "]"
	prs := make([]vcs.PRSummary, 0, len(all))
	for _, pr := range all {
		if strings.Contains(pr.Title, bracketed) {
			prs = append(prs, pr)
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

	_, err := p.runGHWithTransientRetry(ctx, "merge PR",
		"pr", "merge", strconv.Itoa(prID),
		"--repo", repoFlag(repoPath),
		flag,
		"--delete-branch",
	)
	if err != nil {
		return fmt.Errorf("merge PR: %w", classifyMergeError(err, repoPath, prID))
	}

	p.logger.Info().
		Int("number", prID).
		Str("strategy", strategy).
		Msg("merged PR")

	return nil
}

// GetPRMergeCommit returns the merge commit SHA GitHub recorded for the PR.
// Returns vcs.ErrPRNotMerged if the PR is not in MERGED state.
func (p *Provider) GetPRMergeCommit(ctx context.Context, repoPath string, prID int) (string, error) {
	out, err := p.runGH(ctx,
		"pr", "view", strconv.Itoa(prID),
		"--repo", repoFlag(repoPath),
		"--json", "state,mergeCommit",
	)
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}

	var resp struct {
		State       string `json:"state"`
		MergeCommit *struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return "", fmt.Errorf("parse gh pr view JSON: %w", err)
	}

	if strings.ToUpper(resp.State) != "MERGED" {
		return "", fmt.Errorf("%w: state=%s", vcs.ErrPRNotMerged, resp.State)
	}
	if resp.MergeCommit == nil || resp.MergeCommit.OID == "" {
		return "", fmt.Errorf("%w: no merge commit recorded", vcs.ErrPRNotMerged)
	}
	return resp.MergeCommit.OID, nil
}

// GetAllowedMergeStrategies returns the strategies the GitHub repo has
// enabled, ordered "merge", "squash", "rebase" when present. Used as a
// fallback when the configured strategy is empty or disabled upstream.
func (p *Provider) GetAllowedMergeStrategies(ctx context.Context, repoPath string) ([]string, error) {
	nwo := repoFlag(repoPath)
	out, err := p.runGH(ctx,
		"api", "repos/"+nwo,
		"--jq", "{m:.allow_merge_commit,s:.allow_squash_merge,r:.allow_rebase_merge}",
	)
	if err != nil {
		return nil, fmt.Errorf("gh api repo: %w", err)
	}

	var resp struct {
		M bool `json:"m"`
		S bool `json:"s"`
		R bool `json:"r"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("parse allowed-strategies JSON: %w", err)
	}

	var allowed []string
	if resp.M {
		allowed = append(allowed, "merge")
	}
	if resp.S {
		allowed = append(allowed, "squash")
	}
	if resp.R {
		allowed = append(allowed, "rebase")
	}
	return allowed, nil
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

func parseReviewDecision(s string) vcs.ReviewState {
	switch strings.ToUpper(s) {
	case "APPROVED":
		return vcs.ReviewStateApproved
	case "CHANGES_REQUESTED":
		return vcs.ReviewStateChangesRequested
	case "REVIEW_REQUIRED":
		return vcs.ReviewStateRequired
	default:
		return vcs.ReviewStateUnspecified
	}
}

func parseMergeStateStatus(s string) vcs.MergeStateStatus {
	switch strings.ToUpper(s) {
	case "CLEAN":
		return vcs.MergeStateStatusClean
	case "BLOCKED":
		return vcs.MergeStateStatusBlocked
	case "BEHIND":
		return vcs.MergeStateStatusBehind
	case "DIRTY":
		return vcs.MergeStateStatusDirty
	case "DRAFT":
		return vcs.MergeStateStatusDraft
	case "HAS_HOOKS":
		return vcs.MergeStateStatusHasHooks
	case "UNKNOWN":
		return vcs.MergeStateStatusUnknown
	case "UNSTABLE":
		return vcs.MergeStateStatusUnstable
	default:
		return vcs.MergeStateStatusUnspecified
	}
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
	case "PENDING":
		return vcs.ReviewStateUnspecified
	default:
		return vcs.ReviewStateCommented
	}
}

// isRepoNotReady returns true when the gh CLI error indicates GitHub has not
// resolved the base/head refs or compare view yet. It gates the short
// (3-attempt, ~6s total) retry in CreateDraftPR. "No commits between" is
// treated as transient here because genuinely empty branches are caught before
// the API call by VerifyPushedBranchAheadOfBase; reaching this point with that
// error means GitHub has not yet indexed the freshly pushed head ref.
func isRepoNotReady(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Head sha can't be blank") ||
		strings.Contains(msg, "Base sha can't be blank") ||
		strings.Contains(msg, "No commits between")
}

// isPRAlreadyExists returns true when the gh CLI error indicates GitHub
// already has an open pull request for the requested head/base pair.
func isPRAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "a pull request for branch") &&
		strings.Contains(err.Error(), "already exists")
}

// isNoChecksReported reports whether a gh error indicates the PR's head commit
// simply has no check runs. `gh pr checks` exits non-zero in this case with
// "no checks reported on the '<branch>' branch", but it is a normal empty
// state — not a failure. Treating it as an error would freeze a PR's display
// status (e.g. a stale "draft") because the poller bails before recomputing.
func isNoChecksReported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no checks reported on the")
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

func (p *Provider) runGHWithTransientRetry(ctx context.Context, op string, args ...string) (string, error) {
	const maxAttempts = 4
	backoff := 30 * time.Second

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, err := p.runGH(ctx, args...)
		if err == nil {
			return out, nil
		}
		if !isGitHubTransient(err) {
			return "", err
		}
		lastErr = err
		if attempt == maxAttempts {
			break
		}

		p.logger.Warn().Err(err).
			Str("op", op).
			Int("attempt", attempt).
			Dur("backoff", backoff).
			Msg("github transient error, retrying")

		p.sleepFn(backoff)
		backoff *= 2

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
	}

	return "", lastErr
}

func isGitHubTransient(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	transientFragments := []string{
		"api rate limit",
		"secondary rate limit",
		"too many requests",
		"bad gateway",
		"service unavailable",
		"gateway timeout",
		"http 429",
		"http 502",
		"http 503",
		"http 504",
		"non-200 ok status code: 502",
		"non-200 ok status code: 503",
		"non-200 ok status code: 504",
	}
	for _, fragment := range transientFragments {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
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

// parseCheckState converts a gh pr checks "state" field into a status,
// optional conclusion, and a "recognized" flag. The gh CLI combines status
// and conclusion into a single field: SUCCESS, FAILURE, PENDING,
// STARTUP_FAILURE, CANCELLED, SKIPPED, ACTION_REQUIRED, ERROR, TIMED_OUT, etc.
//
// Unrecognized values are deliberately treated as Failure rather than Queued
// so the repair plugin (which only fires on FAILING/CONFLICT/REJECTED) can
// react. The recognized return lets the caller surface the unknown value
// for follow-up. Treating an unknown as "queued" silently masks real
// failures from auto-repair, which is the bug this case was added to fix.
func parseCheckState(s string) (vcs.CheckStatus, *vcs.CheckConclusion, bool) {
	switch strings.ToUpper(s) {
	case "SUCCESS":
		c := vcs.CheckConclusionSuccess
		return vcs.CheckStatusCompleted, &c, true
	case "FAILURE", "STARTUP_FAILURE", "STALE", "ACTION_REQUIRED", "ERROR":
		c := vcs.CheckConclusionFailure
		return vcs.CheckStatusCompleted, &c, true
	case "NEUTRAL":
		c := vcs.CheckConclusionNeutral
		return vcs.CheckStatusCompleted, &c, true
	case "CANCELLED":
		c := vcs.CheckConclusionCancelled
		return vcs.CheckStatusCompleted, &c, true
	case "SKIPPED":
		c := vcs.CheckConclusionSkipped
		return vcs.CheckStatusCompleted, &c, true
	case "TIMED_OUT":
		c := vcs.CheckConclusionTimedOut
		return vcs.CheckStatusCompleted, &c, true
	case "IN_PROGRESS":
		return vcs.CheckStatusInProgress, nil, true
	case "QUEUED", "PENDING", "WAITING":
		return vcs.CheckStatusQueued, nil, true
	default:
		c := vcs.CheckConclusionFailure
		return vcs.CheckStatusCompleted, &c, false
	}
}
