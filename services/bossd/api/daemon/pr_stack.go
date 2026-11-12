package server

import (
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
// stack source's current result, or clears them when it has none.
func (s *Server) hydratePRStack(p *pb.Session, row *models.Session) {
	p.PrBaseBranch, p.RepoDefaultBranch, p.PrStackedOn = "", "", nil
	if s.stacks == nil || row == nil || row.PRNumber == nil {
		return
	}
	r, ok := s.stacks.Stack(row.ID)
	if !ok {
		return
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
