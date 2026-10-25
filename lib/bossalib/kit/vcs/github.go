package vcs

import "strings"

// IsGitHubURL reports whether a git origin's host is github.com or one of its
// subdomains. The host comes from parseRemote, never a substring match, so
// evilgithub.com and a path that names github.com do not count.
func IsGitHubURL(originURL string) bool {
	r, ok := parseRemote(originURL)
	return ok && IsGitHubHost(r.host)
}

// GitHubNWO extracts the "owner/repo" identifier from a GitHub origin URL.
// Only the first two path segments are returned, so extra path or query parts
// (e.g. /tree/main) are discarded. The origin is split by parseRemote and its
// host must satisfy IsGitHubHost. Returns empty string if owner/repo cannot be
// determined or the host is not GitHub.
//
// Examples:
//   - "git@github.com:owner/repo.git"                  → "owner/repo"
//   - "https://github.com/owner/repo.git"              → "owner/repo"
//   - "https://github.com/owner/repo/tree/main"        → "owner/repo"
//   - "ssh://git@github.com:22/owner/repo.git"         → "owner/repo"
//   - "https://user:token@github.com:443/owner/repo"   → "owner/repo"
//   - "git@github.com:22/repo.git"                     → "22/repo"
func GitHubNWO(originURL string) string {
	r, ok := parseRemote(originURL)
	if !ok || !IsGitHubHost(r.host) {
		return ""
	}
	return ownerRepo(r.path)
}

// ownerRepo returns the "owner/repo" prefix of a GitHub path, trimming a
// trailing .git and any deeper path segments. Returns "" unless both an owner
// and repo segment are present.
func ownerRepo(path string) string {
	path = strings.TrimSuffix(path, ".git")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
}

// ParseGitHubCheckState converts a GitHub check state into a status, an
// optional conclusion and a "recognized" flag. The input is the combined
// vocabulary `gh pr checks` reports (SUCCESS, FAILURE, PENDING,
// STARTUP_FAILURE, CANCELLED, SKIPPED, ACTION_REQUIRED, ERROR, TIMED_OUT, ...),
// which is also what a REST check run's conclusion (or, while it is not
// completed, its status) and a commit status's state spell, case aside.
//
// Unrecognized values are deliberately surfaced as completed but unclassified:
// they are neither green nor red, and the recognized return lets the caller
// preserve that distinction for the aggregate verdict (EvaluateChecks).
func ParseGitHubCheckState(s string) (CheckStatus, *CheckConclusion, bool) {
	switch strings.ToUpper(s) {
	case "SUCCESS":
		c := CheckConclusionSuccess
		return CheckStatusCompleted, &c, true
	case "FAILURE", "STARTUP_FAILURE", "STALE", "ACTION_REQUIRED", "ERROR":
		c := CheckConclusionFailure
		return CheckStatusCompleted, &c, true
	case "NEUTRAL":
		c := CheckConclusionNeutral
		return CheckStatusCompleted, &c, true
	case "CANCELLED":
		c := CheckConclusionCancelled
		return CheckStatusCompleted, &c, true
	case "SKIPPED":
		c := CheckConclusionSkipped
		return CheckStatusCompleted, &c, true
	case "TIMED_OUT":
		c := CheckConclusionTimedOut
		return CheckStatusCompleted, &c, true
	case "IN_PROGRESS":
		return CheckStatusInProgress, nil, true
	case "QUEUED", "PENDING", "WAITING":
		return CheckStatusQueued, nil, true
	default:
		return CheckStatusCompleted, nil, false
	}
}

// IsReviewBotLogin reports whether a GitHub login belongs to a bot account, as
// the REST API and webhook payloads spell it (`name[bot]`). The GraphQL review
// list reports the same account under a bare login, so this predicate only
// holds on REST/webhook-shaped data.
func IsReviewBotLogin(login string) bool {
	return strings.HasSuffix(login, "[bot]")
}

// IsTrustedAuthorAssociation reports whether a GitHub author_association grants
// a review the standing to drive unattended automation: the repository's
// owner, a member of its organization, or an invited collaborator. Everyone
// else — CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, FIRST_TIMER, MANNEQUIN, NONE, or
// an absent value — is untrusted.
func IsTrustedAuthorAssociation(association string) bool {
	switch strings.ToUpper(strings.TrimSpace(association)) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	default:
		return false
	}
}

// IsTrustedReviewAuthor reports whether a review or review comment may reach
// the agent automation that acts on review feedback. Trusted authors hold a
// trusted association (IsTrustedAuthorAssociation) or post as a bot
// (IsReviewBotLogin): review bots run as GitHub Apps, which must be installed
// on the repository to review at all, and GitHub reports them with
// association NONE. Anything else is a drive-by review that must never start
// a repair run, because its text is untrusted input to an agent that can push.
func IsTrustedReviewAuthor(login, association string) bool {
	return IsTrustedAuthorAssociation(association) || IsReviewBotLogin(login)
}
