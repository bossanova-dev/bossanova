package scenario

import (
	"slices"

	"github.com/recurser/boss/internal/realstack"
	"github.com/recurser/bossalib/kit/vcs"
)

// RemapPRQueries binds scenario-local per-PR fixtures to registered repo origins.
// Origins may be owner/repo keys, as persisted by Apply, or complete origin URLs.
func RemapPRQueries(response *realstack.GHResponse, doc Document, origins map[string]string) {
	for _, session := range doc.Sessions {
		if session.PR == nil {
			continue
		}
		origin, exists := origins[session.Repo]
		if !exists {
			continue
		}
		status := realstack.PRStatusResponse(session.Repo, session.PR.Number, "{}")
		checks := realstack.PRChecksResponse(session.Repo, session.PR.Number, "[]")
		isStatus, isChecks := slices.Equal(response.Args, status.Args), slices.Equal(response.Args, checks.Args)
		if !isStatus && !isChecks {
			continue
		}
		if vcs.NormalizeRepoURL(origin) == "" {
			origin = "https://github.com/" + origin + ".git"
		}
		build := vcs.GitHubPRChecksArgs
		if isStatus {
			build = vcs.GitHubPRStatusArgs
		}
		args, ok := build(origin, session.PR.Number, "")
		if ok {
			response.Args = args
		}
		return
	}
}
