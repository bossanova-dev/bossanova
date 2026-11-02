package server

import (
	"context"

	"github.com/recurser/bossalib/kit/vcs"
)

// requiredChecksSource resolves a repo reference (local path, origin URL or
// "owner/name") to the repo's configured required-check policy.
// checkpolicy.Resolver implements it.
type requiredChecksSource interface {
	RequiredChecks(ctx context.Context, repoRef string) vcs.RequiredChecks
}

// SetRequiredChecks wires the repo required-check policy into the display
// refresh and live merge-gate reads, so a non-draft head whose required checks
// have not run never displays Passing or Approved. nil-safe.
func (s *Server) SetRequiredChecks(src requiredChecksSource) {
	s.requiredChecks = src
}

// requiredChecksFor returns repoRef's policy, or no requirement when none is
// wired.
func (s *Server) requiredChecksFor(ctx context.Context, repoRef string) vcs.RequiredChecks {
	if s.requiredChecks == nil {
		return vcs.RequiredChecks{}
	}
	return s.requiredChecks.RequiredChecks(ctx, repoRef)
}
