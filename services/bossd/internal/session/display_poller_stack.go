package session

import (
	"context"

	"github.com/recurser/bossalib/models"
)

// StackRefresher recomputes which open PRs a repository's sessions are
// stacked on, once per poll cycle (*prstack.Detector).
type StackRefresher interface {
	Refresh(ctx context.Context, repo *models.Repo, sessions []*models.Session)
}

// SetStackRefresher wires the stacked-PR detector. nil-safe: leaving it unset
// detects no stacks.
func (p *DisplayPoller) SetStackRefresher(r StackRefresher) {
	p.stacks = r
}
