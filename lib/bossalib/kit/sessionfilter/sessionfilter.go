// Package sessionfilter narrows a session list by its PR author and PR target
// branch. bossd applies it to ListSessions and bosso to ProxyListSessions, so
// the local and hosted session lists answer the same way.
package sessionfilter

import (
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// PR matches sessions whose PR author is any of a set of GitHub logins and
// whose PR target branch is any of a set of branches. An empty set does not
// constrain. Authors compare case-insensitively, since GitHub logins are;
// target branches compare exactly, since git branch names are case-sensitive.
// The zero value matches every session.
type PR struct {
	authors map[string]struct{}
	targets map[string]struct{}
}

// ByPR compiles the author and target filters. Blank entries are dropped, so
// a list of only blank entries does not constrain.
func ByPR(authors, targets []string) PR {
	return PR{
		authors: set(authors, strings.ToLower),
		targets: set(targets, func(s string) string { return s }),
	}
}

// Constrains reports whether the filter can reject a session.
func (f PR) Constrains() bool {
	return len(f.authors) > 0 || len(f.targets) > 0
}

// Matches reports whether s passes both filters. A session with no PR author
// or target never matches a filter that sets one.
func (f PR) Matches(s *pb.Session) bool {
	if len(f.authors) > 0 {
		if _, ok := f.authors[strings.ToLower(s.GetPrAuthorLogin())]; !ok {
			return false
		}
	}
	if len(f.targets) > 0 {
		if _, ok := f.targets[s.GetPrTargetBranch()]; !ok {
			return false
		}
	}
	return true
}

// Filter returns the sessions that match, in their original order. It returns
// sessions itself when the filter does not constrain, and otherwise a new
// slice, so it never mutates a list that other readers share.
func (f PR) Filter(sessions []*pb.Session) []*pb.Session {
	if !f.Constrains() {
		return sessions
	}
	out := make([]*pb.Session, 0, len(sessions))
	for _, s := range sessions {
		if s != nil && f.Matches(s) {
			out = append(out, s)
		}
	}
	return out
}

func set(values []string, key func(string) string) map[string]struct{} {
	var out map[string]struct{}
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if out == nil {
			out = make(map[string]struct{}, len(values))
		}
		out[key(v)] = struct{}{}
	}
	return out
}
