// Package trackerref finds candidate tracker ticket ids (e.g. "BOS-1539") in
// facts a session already carries — an existing tracker id, its PR title, its
// branch name, its title. It is pure: it never resolves an id against a
// tracker, so every candidate it returns still needs confirmation from the
// repo's own tracker before it may become a link.
//
// Extraction is positional only: a leading "[ID]" in a title, or an id that
// is a whole segment of a branch name. Ids embedded in prose ("UTF-8",
// "GPT-4") never match, and the six-digit cap keeps unix-timestamp branch
// suffixes ("cron-bs-build-1791871203") from looking like tickets.
package trackerref

import (
	"regexp"
	"strconv"
	"strings"
)

// idPattern is the shape of one tracker id: a letter, up to six more letters
// or digits, a dash, and one to six digits.
const idPattern = `[A-Za-z][A-Za-z0-9]{0,6}-\d{1,6}`

var (
	fullIDRe         = regexp.MustCompile(`^` + idPattern + `$`)
	leadingBracketRe = regexp.MustCompile(`^\s*\[(` + idPattern + `)\]`)
	branchSegmentRe  = regexp.MustCompile(`(?:^|[/_-])(` + idPattern + `)(?:$|[/_-])`)
)

// Facts are the session facts candidates are extracted from.
type Facts struct {
	// TrackerID is the session's existing tracker id, if any. When set it is
	// the only candidate: a link is completed, never replaced.
	TrackerID string
	// PRTitle is the title of the session's open PR.
	PRTitle string
	// BranchName is the session's git branch.
	BranchName string
	// SessionTitle is the session's own title.
	SessionTitle string
}

// ParseID trims and upper-cases s and reports whether the whole string has
// the tracker id shape.
func ParseID(s string) (string, bool) {
	id := strings.ToUpper(strings.TrimSpace(s))
	if !fullIDRe.MatchString(id) {
		return "", false
	}
	return id, true
}

// Candidates returns the candidate ids in priority order (existing tracker
// id; else PR title, branch name, session title), upper-cased with
// duplicates removed. It returns nil when no source yields an id.
func Candidates(in Facts) []string {
	if strings.TrimSpace(in.TrackerID) != "" {
		if id, ok := ParseID(in.TrackerID); ok {
			return []string{id}
		}
		return nil
	}
	var out []string
	add := func(id string) {
		if id == "" {
			return
		}
		id = strings.ToUpper(id)
		for _, seen := range out {
			if seen == id {
				return
			}
		}
		out = append(out, id)
	}
	add(leadingBracketID(in.PRTitle))
	add(branchID(in.BranchName))
	add(leadingBracketID(in.SessionTitle))
	return out
}

func leadingBracketID(title string) string {
	if m := leadingBracketRe.FindStringSubmatch(title); m != nil {
		return m[1]
	}
	return ""
}

func branchID(branch string) string {
	if m := branchSegmentRe.FindStringSubmatch(branch); m != nil {
		return m[1]
	}
	return ""
}

// GitHubIssueRef identifies an issue, optionally scoped by its URL's repository.
type GitHubIssueRef struct {
	Number     int
	Repository string
}

var gitHubIssueIDRe = regexp.MustCompile(`(?i)^(?:GH-|#)?([1-9][0-9]*)$`)
var gitHubIssueURLRe = regexp.MustCompile(`^https?://([A-Za-z0-9.-]+/[A-Za-z0-9-]+/[A-Za-z0-9._-]+)/issues/([1-9][0-9]*)/?$`)

// ParseGitHubIssue accepts GH-N, #N, N or an HTTP(S) issue URL.
func ParseGitHubIssue(s string) (GitHubIssueRef, bool) {
	s = strings.TrimSpace(s)
	var repository, digits string
	if m := gitHubIssueIDRe.FindStringSubmatch(s); m != nil {
		digits = m[1]
	} else if m := gitHubIssueURLRe.FindStringSubmatch(s); m != nil {
		repository, digits = m[1], m[2]
	} else {
		return GitHubIssueRef{}, false
	}
	number, err := strconv.Atoi(digits)
	if err != nil {
		return GitHubIssueRef{}, false
	}
	return GitHubIssueRef{Number: number, Repository: repository}, true
}
