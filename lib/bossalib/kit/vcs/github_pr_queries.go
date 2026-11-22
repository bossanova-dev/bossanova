package vcs

import (
	"net/url"
	"strconv"
	"strings"
)

const gitHubPRStatusQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      state mergeable mergeStateStatus isDraft title body headRefName baseRefName headRefOid
      isCrossRepository reviewDecision additions deletions changedFiles
      reviews(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes { id state authorAssociation author { login } }
      }
    }
  }
}`

const gitHubPRChecksQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          __typename
          ... on CheckRun { name status conclusion startedAt checkSuite { workflowRun { event workflow { name } } } }
          ... on StatusContext { context state description }
        }
      } } } } }
    }
  }
}`

const gitHubPRMergeCommitQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) { state mergeCommit { oid } } }
}`

// GitHubRepoParts resolves a GitHub origin URL or owner/repo into GraphQL variables.
// URL origins retain their host, including GitHub Enterprise hosts.
func GitHubRepoParts(repoPath string) (owner, name, host string, ok bool) {
	nwo := GitHubNWO(repoPath)
	if nwo == "" {
		nwo = repoPath
	}
	if canonical := NormalizeRepoURL(repoPath); canonical != "" {
		u, _ := url.Parse(canonical)
		nwo, host = RepoSlug(canonical), u.Hostname()
	}
	owner, name, ok = strings.Cut(nwo, "/")
	ok = ok && owner != "" && name != "" && !strings.Contains(name, "/")
	return
}

// GitHubPRStatusArgs builds a paged status query for a repository origin.
func GitHubPRStatusArgs(repoPath string, number int, cursor string) ([]string, bool) {
	owner, name, host, ok := GitHubRepoParts(repoPath)
	if !ok {
		return nil, false
	}
	return GitHubPRStatusQueryArgs(owner, name, host, number, cursor), true
}

// GitHubPRChecksArgs builds a paged check-rollup query for a repository origin.
func GitHubPRChecksArgs(repoPath string, number int, cursor string) ([]string, bool) {
	owner, name, host, ok := GitHubRepoParts(repoPath)
	if !ok {
		return nil, false
	}
	return GitHubPRChecksQueryArgs(owner, name, host, number, cursor), true
}

// GitHubPRMergeCommitArgs builds a merge-commit query for a repository origin.
func GitHubPRMergeCommitArgs(repoPath string, number int) ([]string, bool) {
	owner, name, host, ok := GitHubRepoParts(repoPath)
	if !ok {
		return nil, false
	}
	return GitHubPRMergeCommitQueryArgs(owner, name, host, number), true
}

// GitHubPRStatusQueryArgs builds status argv from already resolved repository parts.
func GitHubPRStatusQueryArgs(owner, name, host string, number int, cursor string) []string {
	return gitHubPRQueryArgs(gitHubPRStatusQuery, owner, name, host, number, cursor)
}

// GitHubPRChecksQueryArgs builds check argv from already resolved repository parts.
func GitHubPRChecksQueryArgs(owner, name, host string, number int, cursor string) []string {
	return gitHubPRQueryArgs(gitHubPRChecksQuery, owner, name, host, number, cursor)
}

// GitHubPRMergeCommitQueryArgs builds merge-commit argv from resolved repository parts.
func GitHubPRMergeCommitQueryArgs(owner, name, host string, number int) []string {
	return gitHubPRQueryArgs(gitHubPRMergeCommitQuery, owner, name, host, number, "")
}

func gitHubPRQueryArgs(query, owner, name, host string, number int, cursor string) []string {
	args := []string{"api", "graphql", "-f", "query=" + query, "-f", "owner=" + owner, "-f", "name=" + name, "-F", "number=" + strconv.Itoa(number)}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	if cursor != "" {
		args = append(args, "-f", "cursor="+cursor)
	}
	return args
}
