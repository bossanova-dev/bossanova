package session

import (
	"context"

	"github.com/recurser/bossalib/kit/vcs"
)

// RequiredChecksSource resolves a repo reference (local path, origin URL or
// "owner/name") to the repo's configured required-check policy.
// checkpolicy.Resolver implements it.
type RequiredChecksSource interface {
	RequiredChecks(ctx context.Context, repoRef string) vcs.RequiredChecks
}

// requiredChecksPolicy returns repoRef's policy, or no requirement when src is
// nil.
func requiredChecksPolicy(ctx context.Context, src RequiredChecksSource, repoRef string) vcs.RequiredChecks {
	if src == nil {
		return vcs.RequiredChecks{}
	}
	return src.RequiredChecks(ctx, repoRef)
}

// SetRequiredChecks wires the repo required-check policy, so a non-draft head
// whose required checks have not run never displays Passing or Approved (and
// so never auto-merges or clears a fix-loop block). Must be called before Run.
// nil-safe.
func (p *DisplayPoller) SetRequiredChecks(src RequiredChecksSource) {
	p.required = src
}
