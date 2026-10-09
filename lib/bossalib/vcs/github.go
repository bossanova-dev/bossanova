package vcs

import "strings"

// IsGitHubURL returns true if the given origin URL points to github.com.
// Supports both HTTPS and SSH formats:
//   - https://github.com/owner/repo.git
//   - git@github.com:owner/repo.git
func IsGitHubURL(originURL string) bool {
	s := strings.ToLower(strings.TrimSpace(originURL))
	if s == "" {
		return false
	}
	return strings.Contains(s, "github.com")
}

// GitHubNWO extracts the "owner/repo" identifier from a GitHub origin URL.
// Only the first two path segments are returned, so extra path or query parts
// (e.g. /tree/main) are discarded. Returns empty string if owner/repo cannot be
// determined.
//
// Examples:
//   - "git@github.com:owner/repo.git"          → "owner/repo"
//   - "https://github.com/owner/repo.git"      → "owner/repo"
//   - "https://github.com/owner/repo"          → "owner/repo"
//   - "https://github.com/owner/repo/tree/main" → "owner/repo"
func GitHubNWO(originURL string) string {
	s := strings.TrimSpace(originURL)
	if s == "" {
		return ""
	}

	// SSH format: git@github.com:owner/repo.git
	if _, after, ok := strings.Cut(s, "github.com:"); ok {
		return ownerRepo(after)
	}

	// HTTPS format: https://github.com/owner/repo.git
	if _, after, ok := strings.Cut(s, "github.com/"); ok {
		return ownerRepo(after)
	}

	return ""
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
