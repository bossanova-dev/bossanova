package server

import (
	"cmp"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/prstack"
)

// StackSource serves a session's open-PR base branch and the open PRs it is
// stacked on. *prstack.Detector satisfies it.
type StackSource interface {
	Stack(sessionID string) (prstack.Result, bool)
}

// hydratePRStack replaces the session's PR base and stack fields with the
// stack source's current result, or clears them when it has none. The PR
// author and target start from the stored row and take the live result's
// non-empty values for the same PR, so a read never lags the poll that just
// landed and a merged, closed or restarted session keeps the stored ones.
func (s *Server) hydratePRStack(p *pb.Session, row *models.Session) {
	p.PrBaseBranch, p.RepoDefaultBranch, p.PrStackedOn = "", "", nil
	if row == nil {
		return
	}
	p.PrAuthorLogin, p.PrTargetBranch = row.PRAuthorLogin, row.PRTargetBranch
	if s.stacks == nil || row.PRNumber == nil {
		return
	}
	r, ok := s.stacks.Stack(row.ID)
	if !ok {
		return
	}
	if r.PRNumber == *row.PRNumber {
		p.PrAuthorLogin = cmp.Or(r.Author, p.PrAuthorLogin)
		p.PrTargetBranch = cmp.Or(r.BaseBranch, p.PrTargetBranch)
	}
	p.PrBaseBranch, p.RepoDefaultBranch = r.BaseBranch, r.DefaultBranch
	for _, parent := range r.StackedOn {
		p.PrStackedOn = append(p.PrStackedOn, &pb.StackedPullRequest{
			PrNumber:          clampInt32(parent.Number),
			Title:             parent.Title,
			Url:               parent.URL,
			HeadBranch:        parent.HeadBranch,
			SharedCommitCount: clampInt32(parent.SharedCommits),
			CommitCount:       clampInt32(parent.Commits),
			IsBaseBranch:      parent.IsBaseBranch,
		})
	}
}
