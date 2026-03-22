package main

import (
	"strings"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// parseDependabotLibrary extracts the library name from a dependabot PR
// branch or title. Dependabot branches follow the pattern:
//
//	dependabot/<ecosystem>/<library>-<version>
//
// For example: "dependabot/npm_and_yarn/lodash-4.17.21" → "lodash"
// If the branch doesn't match, falls back to extracting from the title
// which typically looks like "Bump lodash from 4.17.20 to 4.17.21".
func parseDependabotLibrary(pr *bossanovav1.PRSummary) string {
	if lib := libraryFromBranch(pr.GetHeadBranch()); lib != "" {
		return lib
	}
	return libraryFromTitle(pr.GetTitle())
}

// libraryFromBranch extracts the library name from a dependabot branch.
// Example: "dependabot/npm_and_yarn/lodash-4.17.21" → "lodash"
// Example: "dependabot/go_modules/golang.org/x/net-0.38.0" → "golang.org/x/net"
func libraryFromBranch(branch string) string {
	// Must start with "dependabot/"
	if !strings.HasPrefix(branch, "dependabot/") {
		return ""
	}

	// Split into ["dependabot", "<ecosystem>", "<library-version>"]
	parts := strings.SplitN(branch, "/", 3)
	if len(parts) < 3 {
		return ""
	}

	libVersion := parts[2]

	// Strip the trailing version: find the last hyphen that precedes a
	// digit, which marks the start of the version string.
	for i := len(libVersion) - 1; i >= 0; i-- {
		if libVersion[i] == '-' && i+1 < len(libVersion) && libVersion[i+1] >= '0' && libVersion[i+1] <= '9' {
			return libVersion[:i]
		}
	}

	// No version suffix found — return the whole thing.
	return libVersion
}

// libraryFromTitle extracts the library name from a dependabot PR title.
// Titles follow patterns like:
//   - "Bump lodash from 4.17.20 to 4.17.21"
//   - "Bump golang.org/x/net from 0.37.0 to 0.38.0"
//   - "Update lodash requirement from ~4.17.20 to ~4.17.21"
func libraryFromTitle(title string) string {
	lower := strings.ToLower(title)

	// Try "bump <lib> from"
	if idx := strings.Index(lower, "bump "); idx >= 0 {
		rest := title[idx+5:]
		if fromIdx := strings.Index(strings.ToLower(rest), " from "); fromIdx > 0 {
			return strings.TrimSpace(rest[:fromIdx])
		}
	}

	// Try "update <lib> requirement"
	if idx := strings.Index(lower, "update "); idx >= 0 {
		rest := title[idx+7:]
		if reqIdx := strings.Index(strings.ToLower(rest), " requirement"); reqIdx > 0 {
			return strings.TrimSpace(rest[:reqIdx])
		}
	}

	return ""
}

// isPreviouslyRejected checks whether a dependabot PR for the same library
// was previously closed without merging (i.e. rejected). It scans a list
// of recently-closed PRs for a matching library name with CLOSED state.
//
// closedPRs should contain recently-closed dependabot PRs for the same
// repo, fetched via ListClosedDependabotPRs.
func isPreviouslyRejected(pr *bossanovav1.PRSummary, closedPRs []*bossanovav1.PRSummary) bool {
	lib := parseDependabotLibrary(pr)
	if lib == "" {
		return false
	}

	for _, closed := range closedPRs {
		if closed.GetState() != bossanovav1.PRState_PR_STATE_CLOSED {
			continue
		}
		if parseDependabotLibrary(closed) == lib {
			return true
		}
	}
	return false
}
